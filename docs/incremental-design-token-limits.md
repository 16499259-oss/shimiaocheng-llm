# 增量设计文档：用户配额新增「5h Token 限制」与「周(滚动7天) Token 限制」

> 作者：高见远（software-architect）
> 关联：增量 PRD（许清楚产出，已确认）+ 代码现状精读
> 范围：自托管 LLM API 网关（Go 1.22 零 CGO，modernc.org/sqlite，前端 //go:embed 编入二进制）
> 约定：列命名采用 `quota_token_*` 前缀（对齐 `quota_token_total_limit`）；软闸门语义沿用 token 总量上限；limit=0 表示不限制。

---

## 1. 实现方案 + 框架选型

- **技术栈（沿用，无新增依赖）**：单体 Go 服务 + modernc.org/sqlite（纯 Go，零 CGO）+ 前端静态资源 `//go:embed` 编入单一二进制。本次增量**不引入任何新第三方库**。
- **核心难点与对策**：
  1. **闸门扩展**：现有 `AtomicDeductQuota` 用单条原子 `UPDATE ... WHERE` 同时校验 5h/总次数 + token 总量软闸门。新增 5h token、周 token 两条软闸门，沿用同一条原子 UPDATE，避免引入竞态。
  2. **5h token 窗口复用**：不新增窗口列，直接复用现有 5h 次数窗口的 `window_start` 与重置路径（`Reset5hQuota` / `CompensateQuotaReset`），在二者 `SET` 中一并 `quota_token_5h_used = 0`。
  3. **周 token 滚动7天近似**：采用「7天桶惰性重置」。在 `AtomicDeductQuota` 的同一原子 UPDATE 内，用 `CASE WHEN week_start < (now-7d) THEN 0 ELSE quota_token_week_used END` 完成清零 + `week_start` bump。**触发点 = 请求时闸门 UPDATE 内（原子、无额外语句）**，与 PRD「接受最多约7天滞后」一致。
  4. **计数口径**：请求时只校验（软闸门），**不累加 token**；billed token 在响应后由 `AddTokenUsage` 累加（与现有 token 总量一致），且 5h/周/总量三列同 delta 累加（delta 已含 multiplier）。
- **架构模式**：沿用既有分层（models 数据层 / handler+admin 接口层 / proxy 网关层），本次为纯增量，无架构变更。

---

## 2. 文件列表及相对路径（需改/新增）

| 文件 | 动作 | 关键改动 |
|---|---|---|
| `internal/db/migrations.go` | 改 | 在 `RunMigrations` 幂等 ALTER 段（L240-284 附近）新增 5 列迁移 |
| `internal/models/quota.go` | 改 | `Quota`/`QuotaStatus` 加字段；`GetQuota` SELECT；`AtomicDeductQuota` 加 5h/周 闸门 + 周内惰性重置；`AddTokenUsage` 三列累加；`Reset5hQuota`/`CompensateQuotaReset` 一并重置 5h token；新增 `UpdateQuotaTokenWindowLimits` |
| `internal/models/user.go` | 改 | `UserWithQuota` 加 4 字段；`CreateUser` INSERT 新列；`ListUsers` SELECT+Scan 新列 |
| `internal/handler/quota.go` | 改 | `ServeHTTP` 填充 `QuotaStatus` 的 5h/周 token limit/used/remaining |
| `internal/admin/users.go` | 改 | `createUserRequest`/`updateUserRequest` 加字段；`CreateUser` 校验+写入+响应；`UpdateUser` 校验+写入+响应 |
| `internal/proxy/handler.go` | 改 | `ServeHTTP` 超额分支（L437-464）三分类（5h/周/总量）+ 新鲜度判定 |
| `web/admin/index.html` | 改 | 用户表头加 2 窄列；创建/编辑表单加 2 个输入 |
| `web/admin/app.js` | 改 | `loadUsers` 渲染新列；`createUser`/`editUser`/`updateUser` 收发新字段；`editUser` onclick 传参扩展 |
| `web/user/index.html` | 改 | `renderDashboard` 新增 5h/周 token 两行进度条（0=无限） |
| `docs/incremental-design-token-limits.md` | 新增 | 本文档 |
| `docs/class-diagram-token-limits.mermaid` | 新增 | 类/结构图 |
| `docs/sequence-diagram-token-limits.mermaid` | 新增 | 时序图 |

> 注：类图与时序图另存为 `.mermaid` 文件（见上表后两行），本文第 3、4 节含其等价文字/表格描述。

---

## 3. 数据结构和接口（结构表 + 类图）

### 3.1 数据库增量列（`quotas` 表，幂等 ALTER）

| 列名 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `quota_token_5h_limit` | INTEGER NOT NULL | 0 | 5h Token 上限，0=不限制 |
| `quota_token_5h_used` | INTEGER NOT NULL | 0 | 5h Token 已用（随 5h 次数窗口惰性重置） |
| `quota_token_week_limit` | INTEGER NOT NULL | 0 | 周(滚动7天) Token 上限，0=不限制 |
| `quota_token_week_used` | INTEGER NOT NULL | 0 | 周 Token 已用 |
| `week_start` | TEXT NOT NULL | `''`（迁移时回填 `datetime('now')`；新建用户写本地 `now`） | 周桶锚点（RFC3339 或 SQLite `datetime('now')` 格式，`freshWindow` 二者均可解析） |

> 无回填：limit/used 默认 0（不限/未用）。`week_start` 实现上用 `DEFAULT ''`：原因见 §8 —— **SQLite 禁止对非空表 `ALTER TABLE ... ADD COLUMN` 加「非常量默认值」**，故迁移先加 `DEFAULT ''`，再 `UPDATE quotas SET week_start = (datetime('now')) WHERE week_start = ''` 回填存量行；新建用户在 `CreateUser` 直接写本地 `now`。空 `week_start` 在闸门中被当作「已过期」（`<` 任意 ISO 时间戳），属存量行首次触发的预期安全行为。首次请求若距今>7天则由闸门 CASE 清零。

### 3.2 `models.Quota` 结构新增字段
```go
QuotaToken5hLimit  int    `json:"quota_token_5h_limit"`  // 0 = unlimited
QuotaToken5hUsed   int    `json:"quota_token_5h_used"`
QuotaTokenWeekLimit int    `json:"quota_token_week_limit"` // 0 = unlimited
QuotaTokenWeekUsed  int    `json:"quota_token_week_used"`
WeekStart          string `json:"week_start"`
```

### 3.3 `models.QuotaStatus` 结构新增字段（/v1/quota 响应）
```go
QuotaToken5hLimit     int `json:"quota_token_5h_limit"`
QuotaToken5hUsed      int `json:"quota_token_5h_used"`
QuotaToken5hRemaining int `json:"quota_token_5h_remaining"` // limit>0 时 = max(0,limit-used)，否则 0（前端按无限渲染）
QuotaTokenWeekLimit     int `json:"quota_token_week_limit"`
QuotaTokenWeekUsed      int `json:"quota_token_week_used"`
QuotaTokenWeekRemaining int `json:"quota_token_week_remaining"`
```

### 3.4 `models.UserWithQuota` 结构新增字段（Admin 列表）
```go
QuotaToken5hLimit  int `json:"quota_token_5h_limit"`
QuotaToken5hUsed   int `json:"quota_token_5h_used"`
QuotaTokenWeekLimit int `json:"quota_token_week_limit"`
QuotaTokenWeekUsed  int `json:"quota_token_week_used"`
```

### 3.5 新增模型函数签名
```go
// 类比 UpdateQuotaTokenTotalLimit：写 5h/周 token 上限，不重置已用（与总量一致）
func UpdateQuotaTokenWindowLimits(db *sql.DB, userID int64, token5hLimit, tokenWeekLimit int) error
```

### 3.6 请求/响应 JSON 字段 & Admin API 新增入参

**POST /admin/api/users（createUserRequest 新增）**
- `quota_token_5h_limit` : int（默认 0 = 不限制，>=0，<0 → 400）
- `quota_token_week_limit`: int（默认 0 = 不限制，>=0，<0 → 400）
- 响应同步回显这两个字段。

**PUT /admin/api/users/{id}（updateUserRequest 新增）**
- `quota_token_5h_limit` : *int（nil=不改；0=不限制；<0 → 400）
- `quota_token_week_limit`: *int（nil=不改；0=不限制；<0 → 400）
- 响应在 `quota_token_total_limit/used` 旁回显这两个 limit。

**GET /admin/api/users（ListUsers 每项新增）**
- `quota_token_5h_limit` / `quota_token_5h_used` / `quota_token_week_limit` / `quota_token_week_used`

**GET /v1/quota（QuotaStatus 新增）**
- `quota_token_5h_limit` / `quota_token_5h_used` / `quota_token_5h_remaining`
- `quota_token_week_limit` / `quota_token_week_used` / `quota_token_week_remaining`

**429 错误 `error.type` 新增（沿用 `code`/`type` 同值）**
- `token_5h_quota_exceeded`（"5 小时内 Token 已超限"）
- `token_week_quota_exceeded`（"本周 Token 已超限"）
- `token_quota_exceeded`（"Token 额度已用尽"，沿用既有）

### 3.7 类图（mermaid，另存 class-diagram-token-limits.mermaid）
见 `docs/class-diagram-token-limits.mermaid`。要点：`Quota`/`QuotaStatus`/`UserWithQuota` 为数据实体（含 §3.2-3.4 新字段）；`models` 包提供 `GetQuota`/`AtomicDeductQuota`/`AddTokenUsage`/`UpdateQuotaTokenWindowLimits`/`Reset5hQuota`/`CompensateQuotaReset`；`createUserRequest`/`updateUserRequest` 为 Admin 入参；`quota.Checker.CheckAndDeduct` 封装 `AtomicDeductQuota`。

---

## 4. 程序调用流程（时序图 + 文字）

### 4.1 时序图（mermaid，另存 sequence-diagram-token-limits.mermaid）
见 `docs/sequence-diagram-token-limits.mermaid`。覆盖：① 创建用户写 limit；② 请求→CheckAndDeduct 含新闸门+周内惰性重置→响应后 AddTokenUsage 三列累加；③ 惰性重置触发点。

### 4.2 关键流程文字说明

**(A) 创建用户 → 写 limit**
1. Admin `POST /admin/api/users` → `admin.CreateUser` 解码 `createUserRequest`（含 `quota_token_5h_limit`/`quota_token_week_limit`）。
2. 校验：两个值 `<0` → 400；缺省按 0（不限制）。
3. `models.CreateUser` INSERT `quotas`，新列写入 `(0,0,0,0, week_start=now)`（5h/周 limit/used 默认 0）。
4. 随后 `models.UpdateQuotaTokenWindowLimits(db, userID, req.5h, req.week)` 写入管理员的限额（类比 `UpdateQuotaTokenTotalLimit`）。
5. 响应回显两字段。

**(B) 请求 → 闸门（含新闸门 + 周内惰性重置）→ 响应后累加**
1. `proxy.ServeHTTP` → `quota.Checker.CheckAndDeduct(userID, effectiveCalls)` → `models.AtomicDeductQuota`。
2. `AtomicDeductQuota` 单条原子 UPDATE：
   - **SET**：`quota_5h_used += calls`、`quota_total_used += calls`、**`quota_token_week_used = CASE WHEN week_start < (now-7d) THEN 0 ELSE quota_token_week_used END`**（仅惰性重置，不累加 calls）、**`week_start = CASE WHEN week_start < (now-7d) THEN now ELSE week_start END`**、`updated_at=now`。
     - 注意：**请求时不累加任何 Token 列（`quota_token_total_used` / `quota_token_5h_used` / `quota_token_week_used`）**（沿用既有 token 总量模式，留待响应后 `AddTokenUsage` 按 billed delta 三列同增）。
   - **WHERE 闸门**：保留 5h/总次数；新增两条软闸门：
     - `(quota_token_5h_limit = 0 OR quota_token_5h_used < quota_token_5h_limit)`
     - `(quota_token_week_limit = 0 OR (CASE WHEN week_start < (now-7d) THEN 0 ELSE quota_token_week_used END) < quota_token_week_limit)`
     - 保留既有 `(quota_token_total_limit = 0 OR quota_token_total_used < quota_token_total_limit)`。
   - `rowsAffected==1` 放行，否则拦截。
3. **拦截分支**（`!allowed`）：读回 `models.GetQuota`，按优先级 **5h → 周 → 总量** 判定 `error.type`（见 §4.3），写 429 + call_log。
4. **放行分支**：转发上游 → `handleSync` / `handleStream` 收响应后 `models.AddTokenUsage(db, userID, billedTokens)`：
   - `billedTokens = ceil((prompt+completion)*multiplier)`（与现有一致）。
   - `AddTokenUsage` 改为三列同 delta 累加：`quota_token_total_used += d`、`quota_token_5h_used += d`、`quota_token_week_used += d`（`d<=0` 仍 no-op）。

**(C) 惰性重置触发点（明确结论）**
- **5h token 重置**：复用现有 5h 次数窗口重置路径——`Reset5hQuota`（调度器每30s在边界触发）与 `CompensateQuotaReset`（启动补偿 + 调度）的 `SET` 中**一并 `quota_token_5h_used = 0`**，并随 `window_start` bump。不新增任何列/函数。触发时机 = 现有 5h 调度节奏（与次数配额完全一致，非回归）。
- **周 token 重置**：在 **`AtomicDeductQuota` 请求时闸门 UPDATE 内**用 `CASE` 原子完成（§4.2-B2）。无独立函数、无额外语句，边界重复重置为近似可接受。

### 4.3 429 三分类判定（proxy/handler.go，L437-464 替换）
读回 `q := GetQuota` 后（此时 used 尚未累加，因拦截在累加之前），按以下优先级 + **新鲜度** 判定，避免陈旧窗口误判：
```
errType, errMsg = "quota_exceeded", "Quota exceeded"   // 默认：次数配额(5h/总)耗尽
// 周 token：窗口未过期才算
if q.QuotaTokenWeekLimit != 0 && q.QuotaTokenWeekUsed >= q.QuotaTokenWeekLimit && fresh(week_start, 7d):
    errType, errMsg = "token_week_quota_exceeded", "本周 Token 已超限"
// 5h token：窗口未过期才算（now - window_start <= 5h）
else if q.QuotaToken5hLimit != 0 && q.QuotaToken5hUsed >= q.QuotaToken5hLimit && fresh(window_start, 5h):
    errType, errMsg = "token_5h_quota_exceeded", "5 小时内 Token 已超限"
// 总量 token
else if q.QuotaTokenTotalLimit != 0 && q.QuotaTokenTotalUsed >= q.QuotaTokenTotalLimit:
    errType, errMsg = "token_quota_exceeded", "Token 额度已用尽"
```
> 新鲜度 `fresh(ts, dur)` = `now.Sub(parse(ts)) <= dur`。这样当 5h/周 窗口已陈旧（本应被重置）但其 used 仍高时，不会误报为 token 维度耗尽，而正确回落到次数配额或总量判定。

---

## 5. 任务列表（有序、含依赖、按实现顺序）

> 说明：本任务分解遵循主理人齐活林在任务指派中明确的 9 步顺序（与通用「≤5任务」默认不同，以本次显式指派为准）。每个任务标注「文件 + 函数/结构 + 改动点（行号附近）」。

### T01 · DB 增量迁移（幂等）
- **文件/函数**：`internal/db/migrations.go` → `RunMigrations`（在 L240-284 幂等 ALTER 段之后追加）
- **改动**：仿 `quota_token_total_*` 范式，用 `columnExists` 守卫，依次 `ALTER TABLE quotas ADD COLUMN`：
  - `quota_token_5h_limit INTEGER NOT NULL DEFAULT 0`
  - `quota_token_5h_used INTEGER NOT NULL DEFAULT 0`
  - `quota_token_week_limit INTEGER NOT NULL DEFAULT 0`
  - `quota_token_week_used INTEGER NOT NULL DEFAULT 0`
  - `week_start TEXT NOT NULL DEFAULT ''` —— 随后在 `RunMigrations` 内 `UPDATE quotas SET week_start = (datetime('now')) WHERE week_start = ''` 幂等回填存量行（原因见 §8：SQLite 禁止对非空表 ALTER 加非常量默认值）
- **依赖**：无 ｜ **优先级**：P0

### T02 · models/quota.go（结构 + 闸门 + 累加 + 重置 + 新函数）
- **文件/函数**：`internal/models/quota.go`
  - `Quota` 结构（L10-22）加 5 字段（§3.2）。
  - `QuotaStatus` 结构（L24-40）加 6 字段（§3.3）。
  - `GetQuota`（L43-59）SELECT/Scan 加 5 新列。
  - `AtomicDeductQuota`（L90-115）WHERE 加 5h/周 两条软闸门；SET 加周内惰性重置 CASE（§4.2-B2），**不**在请求时累加 token 总量/5h used。
  - `AddTokenUsage`（L150-162）改为三列同 delta 累加（total/5h/week）。
  - `Reset5hQuota`（L181-192）与 `CompensateQuotaReset`（L195-208）SET 中加 `quota_token_5h_used = 0`（随 5h 窗口重置）。
  - 新增 `UpdateQuotaTokenWindowLimits(db, userID, token5hLimit, tokenWeekLimit int)`（类比 L168-178）。
- **依赖**：T01 ｜ **优先级**：P0

### T03 · models/user.go（UserWithQuota + CreateUser + ListUsers）
- **文件/函数**：`internal/models/user.go`
  - `UserWithQuota`（L47-59）加 4 字段（§3.4）。
  - `CreateUser`（L65-145）INSERT（L110-114）加 5 新列，值 `(0,0,0,0, calculateWindowStart 同口径的 now)`；返回结构体（L123-144）填充新字段。
  - `ListUsers`（L199-236）SELECT（L201-203）与 Scan（L220-226）加 4 新列。
- **依赖**：T02 ｜ **优先级**：P0

### T04 · handler/quota.go（QuotaStatus 填充）
- **文件/函数**：`internal/handler/quota.go` → `ServeHTTP`（L66-91）
- **改动**：构造 `QuotaStatus` 时填充 5h/周 token 的 limit/used；remaining = `limit>0 ? max(0,limit-used) : 0`（0 表示无限，前端隐藏进度条）。
- **依赖**：T02 ｜ **优先级**：P0

### T05 · admin/users.go（请求结构 + 校验 + 写入 + 响应）
- **文件/函数**：`internal/admin/users.go`
  - `createUserRequest`（L22-33）加 `QuotaToken5hLimit int`、`QuotaTokenWeekLimit int`（`json` 同名）。
  - `updateUserRequest`（L36-48）加 `QuotaToken5hLimit *int`、`QuotaTokenWeekLimit *int`。
  - `CreateUser`（L57-213）：在 token 总量校验（L151-161）旁，对两个新值 `<0 → 400`；非 0 时调用 `models.UpdateQuotaTokenWindowLimits(h.DB, user.ID, req.5h, req.week)`；响应 map（L190-207）回显两字段。
  - `UpdateUser`（L228-454）：在 token 总量校验（L272-298）旁，对两个新值 `!=nil && *v<0 → 400`；非 nil 时调用 `UpdateQuotaTokenWindowLimits` 并在响应回显 limit。
- **依赖**：T02, T03 ｜ **优先级**：P0

### T06 · proxy/handler.go（429 三分类）
- **文件/函数**：`internal/proxy/handler.go` → `ServeHTTP` 超额分支（L437-464）
- **改动**：将 L444-449 的单一 token 判定替换为 §4.3 的三分类（5h→周→总量 + 新鲜度），写对应 `error.type`/`message` + 429 + call_log。
- **依赖**：T02 ｜ **优先级**：P0

### T07 · 前端 Admin（表头 + 表单 + 列表 + JS）
- **文件**：`web/admin/index.html` + `web/admin/app.js`
  - `index.html` 表头（L88）在 `Token` 列后加 2 窄列（`5h·Token` / `周·Token`），`<td colspan>` 由 13 改 15（L89、L726 两处）。
  - `index.html` 创建表单（L436 后）加 `new-quota-token-5h`、`new-quota-token-week` 两个 `number`（默认0、min0）。
  - `index.html` 编辑表单（L516 后）加 `update-quota-token-5h`、`update-quota-token-week`。
  - `app.js` `loadUsers`（L728-778）：构造两个 token 单元格（used/limit，limit=0→`无限`），行模板（L770）插入 2 个 `<td>`。
  - `app.js` `createUser`（L787-847）：读取两输入，非空时写入 `body.quota_token_5h_limit`/`quota_token_week_limit`。
  - `app.js` `editUser`（L849-883）+ onclick（L774）：新增两参数（limit），回填两个 input。
  - `app.js` `updateUser`（L885+）：读取两输入，非空时写入 body 两字段（>=0）。
- **依赖**：T03, T05 ｜ **优先级**：P0（列表扩展 P1，表单 P0）

### T08 · 前端 User 面板（两行进度条）
- **文件**：`web/user/index.html`
- **改动**：`renderDashboard`（L401-420）在 token 总量行后新增「5小时 Token」「本周 Token」两行，采用 token 语义（limit=0 → `无限` 且隐藏进度条），插入 `quota-stats` innerHTML（L427-430）。
- **依赖**：T04 ｜ **优先级**：P0

### T09 · 测试覆盖
- **文件**：`internal/models/quota_coverage_test.go`（或新增 `token_window_test.go`）、`internal/proxy/`（新增分类单测）、迁移幂等验证
- **覆盖**：
  - `AtomicDeductQuota` 在 5h/周/总量 任一超限时拦截；周窗口陈旧时惰性重置并放行。
  - `AddTokenUsage` 三列同 delta 累加；`Reset5hQuota`/`CompensateQuotaReset` 一并清零 5h token。
  - `UpdateQuotaTokenWindowLimits` 写值正确。
  - proxy 三分类：构造不同 exhausted 维度 + 陈旧窗口，断言 `error.type` 命中预期。
  - 迁移：重复 `RunMigrations` 不报错且列存在（幂等）。
- **依赖**：T02, T06 ｜ **优先级**：P0

---

## 6. 依赖包列表

- **新增第三方包：无。** 全部沿用现有依赖（Go stdlib + `modernc.org/sqlite` + `modernc.org/sqlite/lib`）。前端无构建步骤（原生 HTML/JS，`//go:embed`），无新增 npm 包。

---

## 7. 共享约定（跨模块，供工程师统一遵循）

1. **limit=0 = 不限制**：`quota_token_5h_limit` / `quota_token_week_limit` / `quota_token_total_limit` 三处语义一致；0 表示无限，闸门 `OR limit=0` 直接放行。
2. **软闸门语义**：请求时 `used < limit` 放行，响应后 `AddTokenUsage` 累加 billed token（含 multiplier）。允许单请求小幅超额，下个请求被拦（与 token 总量一致，设计接受）。
3. **multiplier 作用于 billed token**：`billed = ceil((prompt+completion)*multiplier)`；5h/周/总量三列使用**同一 billed 值**累加（同口径）。
4. **JSON 字段命名**：后端出参用 `quota_token_5h_limit`/`quota_token_5h_used`/`quota_token_5h_remaining`/`quota_token_week_limit`/`quota_token_week_used`/`quota_token_week_remaining`；入参同名（`*int` 表示可选）。
5. **前端 0=无限渲染约定**：limit=0 时显示「无限」并隐藏进度条；否则 `used / limit` + 彩色条（>80% 红 / >50% 橙 / 否则 绿）。Admin 列表窄列同样适用。
6. **429 `error` 结构**：`{error:{message, type, code}}`，`type` 与 `code` 同值；新增 `token_5h_quota_exceeded` / `token_week_quota_exceeded`，沿用 `token_quota_exceeded`。
7. **时间格式**：`week_start` / `window_start` 存 RFC3339 字符串，字符串比较即时间序（与现有 `CompensateQuotaReset` 一致）。

---

## 8. 待明确事项（用户可推翻 / 新发现）

以下为第(三)节主理人「用户可推翻」项 + 精读代码后的新发现，请主理人/用户最终确认：

1. **列命名 `quota_token_*` 前缀**（已采纳，非 PRD 原 `quota_5h_token_*`）——用户若坚持原名需改全部引用。
2. **429 文案 + type 三套独立**（`token_5h_quota_exceeded` / `token_week_quota_exceeded` / `token_quota_exceeded`）——已采纳，文案措辞可微调。
3. **Admin 列表「加列」方案**（保留 Token 总量列，右侧新增 `5h`/`周` 窄列，窄屏横向滚动）——已采纳；替代方案为「悬浮 tooltip」或合并到现有 Token 列。
4. **面板剩余重置时间本期不做**（留 P2）——已采纳；若要做需给 `QuotaStatus` 加 `token_5h_reset_at` / `token_week_reset_at`。
5. **周窗口近似（7天桶，非真实滚动滑窗）**——已采纳；与「真实滚动7天」最多约7天滞后。
6. **【新发现】周内惰性重置采用「闸门 UPDATE 内 CASE 原子完成」**（非独立 `MaybeResetWeek` 函数）。优点是零额外语句、无竞态；边界处并发可能短暂重复重置，属近似可接受。若用户要求「独立重置函数 + 提前重置」可改，但会增大改动面。
7. **【新发现】5h token 重置完全依赖现有 5h 调度器**（`Reset5hQuota` 每30s边界 + `CompensateQuotaReset` 启动补偿）。若服务长期不跨 5h 边界或调度器未运行，5h token 不会重置——与现有 5h 次数配额行为一致（非回归）。如需「请求时 5h token 也惰性重置」需另加逻辑，建议保持现状以求一致。
8. **【新发现】429 三分类为「读回 quota + 优先级 + 新鲜度」**（沿用现有 token 总量 read-back 模式），优先级 5h→周→总量。若要求「精确最先触发维度」，需改 `quota.Checker.CheckAndDeduct` 返回原因枚举（更大改动，破坏现有 `bool` 契约），当前为范围克制方案。
9. **【实现修正】`week_start` 迁移默认值实际为 `DEFAULT ''`（非常量默认值）； `datetime('now')`（UTC）**，新建用户在 `CreateUser` 写本地 `now`；存量行空 `week_start` 在闸门中被当作「已过期」，首次请求即重置——安全且符合预期。字符串比较近似（受时区偏移≤数小时）影响可忽略，PRD 已接受约7天滞后。
10. **【新发现】Admin 表格 `colspan` 由 13 改为 15**（新增 2 列），需同步修改「加载中…」「暂无用户」两处 colspan，否则表格错位（已在 T07 标注）。

---

## 附：关键 SQL 速查（供工程师实现参考，非最终代码）

**AtomicDeductQuota（核心闸门 + 周内惰性重置）**
```sql
UPDATE quotas
SET quota_5h_used = quota_5h_used + ?,
    quota_total_used = quota_total_used + ?,
    quota_token_week_used = CASE WHEN week_start < ? THEN 0 ELSE quota_token_week_used END,
    week_start = CASE WHEN week_start < ? THEN ? ELSE week_start END,
    updated_at = ?
WHERE user_id = ?
  AND quota_5h_used + ? <= quota_5h_limit
  AND quota_total_used + ? <= quota_total_limit
  AND (quota_token_total_limit = 0 OR quota_token_total_used < quota_token_total_limit)
  AND (quota_token_5h_limit = 0 OR quota_token_5h_used < quota_token_5h_limit)
  AND (quota_token_week_limit = 0 OR (CASE WHEN week_start < ? THEN 0 ELSE quota_token_week_used END) < quota_token_week_limit)
```
> **关键纠正（主理人齐活林）**：Token 三列（total/5h/周）在请求时闸门内**绝不累加** `effectiveCalls`/calls。
> 闸门 ONLY 做原子校验 +（周桶）惰性重置；token 计数一律留到响应后由 `AddTokenUsage` 按 billed
> delta（已含 multiplier）三列同增。原稿 `quota_token_week_used = … + ?`、`… + calls` 属口径错误，已删除。
> 含义一致：请求时只校验 `used < limit`，放行后 `AddTokenUsage` 才把 token 计进去（允许单请求小幅超额，下请求被拦，设计接受）。
>
> 占位符顺序（共 10 个 `?`，与 `internal/models/quota.go` 一一对应）：
> `effectiveCalls, effectiveCalls`（SET 次数）, `weekCutoff`（SET week_used CASE 判定）, `weekCutoff, nowTime`（SET week_start CASE 判定 + bump）,
> `nowTime`（SET updated_at）, `userID`（WHERE）, `effectiveCalls, effectiveCalls`（WHERE 5h/总 次数闸门）, `weekCutoff`（WHERE 周 token 闸门 CASE 判定）。

**AddTokenUsage（响应后三列同 delta 累加）**
```sql
UPDATE quotas
SET quota_token_total_used = quota_token_total_used + ?,
    quota_token_5h_used = quota_token_5h_used + ?,
    quota_token_week_used = quota_token_week_used + ?
WHERE user_id = ?
```

**Reset5hQuota / CompensateQuotaReset（SET 中追加）**
```sql
-- 在现有 SET 基础上追加：
quota_token_5h_used = 0
```
