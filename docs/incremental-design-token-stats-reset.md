# 增量设计文档：管理员延期清零三档 Token 用量 + 月维度自然重置（滚动 30 天）

> 作者：高见远（software-architect）
> 关联：增量 PRD（许清楚产出，已与用户确认决策 1 / 决策 2）+ 代码现状精读
> 范围：自托管 LLM API 网关 shimiaocheng-llm（Go 1.22 零 CGO，modernc.org/sqlite，前端 `//go:embed` 编入二进制）
> 约定：列命名沿用 `quota_token_*` 前缀；软闸门语义沿用既有 token 上限；`limit=0` 表示不限制；时间一律 RFC3339 字符串（字符串序 = 时间序，与 `week_start` 一致）。
> 源码核实：`internal/models/quota.go`、`user.go`、`internal/admin/users.go`、`handler.go`、`internal/db/migrations.go`、`internal/quota/scheduler.go`、`web/admin/{index.html,app.js}`、`web/user/index.html` 均已精读。

---

## 1. 实现方案 + 框架选型

- **技术栈（沿用，无新增依赖）**：单体 Go 服务 + `modernc.org/sqlite`（纯 Go，零 CGO）+ 前端静态资源 `//go:embed` 编入单一二进制。本增量**不引入任何新第三方库**。
- **核心难点与对策**：
  1. **月维度自然重置**：在既有 `AtomicDeductQuota` 的「单条原子 UPDATE + WHERE」中，**仿照周窗口（`week_start`）新增月窗口（`month_start`）的惰性重置 CASE**（滚动 30 天）。原子、无额外语句，与周窗口完全对称。
  2. **延期清零三档 Token**：新增 `models.ResetTokenStats(db, userID, now)`，一条 `UPDATE` 将 `quota_token_5h_used` / `quota_token_week_used` / `quota_token_total_used` 归零，并把 **token-only 锚点** `week_start`、`month_start` bump 为 now；**绝不触碰** `window_start` / `quota_5h_used` / `quota_total_used`（满足「次数不重置」硬约束）。
  3. **5h Token 维度沿用的既有设计（关键）**：PR #27 已**刻意复用** 5h 调用次数窗口的 `window_start` 作为 5h Token 窗口，**未加独立列**；且 **gate 不惰性重置 5h Token**（仅由 cron `Reset5hQuota`/`CompensateQuotaReset` 每 5h 随 `window_start` 一并清零 `quota_token_5h_used`）。因此 admin reset 对 5h Token **只清零 used 即可立即解封**（gate 判 `(quota_token_5h_limit=0 OR quota_token_5h_used < limit)`，used=0 即放行），无需、也不应移动 `window_start`。详见 §R1。
  4. **计数口径不变**：请求时 gate 只校验（软闸门），**不累加 token**；billed token 在响应后由 `AddTokenUsage` 累加三档（delta 已含 multiplier）。本次**不改** `AddTokenUsage`。
- **架构模式**：沿用既有分层（models 数据层 / admin+handler 接口层 / proxy 网关层），纯增量，无架构变更。

---

## 2. 文件列表及相对路径（需改 / 新增）

| 文件 | 动作 | 关键改动 |
|---|---|---|
| `internal/db/migrations.go` | 改 | `RunMigrations` 幂等 ALTER 段新增 `month_start` 列（沿用 `week_start` 的 `DEFAULT ''` + 回填 `datetime('now')` 写法）；`CreateUser` 在 models 中写 `month_start` |
| `internal/models/quota.go` | 改 | `Quota` 增 `MonthStart` 字段；`GetQuota` SELECT/Scan 增列；`CreateUser` INSERT 写 `month_start`；`AtomicDeductQuota` 增月窗口惰性重置 CASE（SET + WHERE 同步）；**新增 `ResetTokenStats`**；`AddTokenUsage` / `Reset5hQuota` / `CompensateQuotaReset` 不变 |
| `internal/models/user.go` | 改 | `CreateUser` INSERT 写入 `month_start`（其余 `UserWithQuota`/`ListUsers` 无需扫描 `month_start`） |
| `internal/handler/quota.go` | 改 | `QuotaStatus` 增 `MonthResetAt`（= `month_start + 30d`，供前端可选展示「本月重置」）；`ServeHTTP` 填充 |
| `internal/admin/users.go` | 改 | `extendUserRequest` 增 `ResetTokenStats bool`；`ExtendUser` 延期后若 `true` → 调 `models.ResetTokenStats` + 写 `token_stats_reset` 审计（detail JSON 含 dimensions/trigger/operator） |
| `web/admin/index.html` | 改 | `#extend-user-modal` 内新增勾选框「重置 Token 统计（5 小时 / 本周 / 本月）」，默认 `checked` |
| `web/admin/app.js` | 改 | `extendUser` 默认勾选该框；`submitExtend` 发送 `reset_token_stats: checkbox.checked` |
| `web/user/index.html` | 改（可选增强） | `renderDashboard` 可选展示「本月重置」时间（取 `month_reset_at`）；「Token 月总量」行无需改动 |
| `docs/incremental-design-token-stats-reset.md` | 新增 | 本文档 |
| `docs/class-diagram-token-stats.mermaid` | 新增 | 类 / 结构图 |
| `docs/sequence-diagram-token-stats.mermaid` | 新增 | 时序图 |

> 注：类图与时序图另存为 `.mermaid` 文件（见上表后两行），本文第 3、4 节含其等价文字 / 表格描述。

---

## 3. 数据结构和接口（结构表 + 类图）

### 3.1 数据库增量列（`quotas` 表，幂等 ALTER）

| 列名 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `month_start` | TEXT NOT NULL | `''`（迁移时回填 `datetime('now')`；新建用户写本地 `now`） | **月 Token 桶锚点**（滚动 30 天），token-only，本次新增。空串 `''` 在闸门中视为「已过期」（安全首触，与 `week_start` 一致） |

> **不新增 `token_window_start` 列**（详见 §R1 决策 B）：5h Token 沿用 `window_start`（与 5h 调用次数共享），gate 不惰性重置 5h Token，由 cron 每 5h 清零。
> `week_start` 为既有周桶锚点（token-only），本次 admin reset 会 bump 它；`window_start` 为 5h 调用次数窗口（共享，**任何 token reset 路径都不可改**）。

### 3.2 `models.Quota` 结构增量字段

```go
MonthStart string `json:"month_start"` // 月 Token 桶锚点（RFC3339），本次新增
// 既有：WindowStart string `json:"window_start"`（5h 调用次数，与 5h Token 共享，不可改）
// 既有：WeekStart   string `json:"week_start"`（周 Token 桶锚点，token-only）
```

### 3.3 `models.QuotaStatus` 结构增量字段（`/v1/quota` 响应，可选增强）

```go
MonthResetAt string `json:"month_reset_at"` // = month_start + 30d，前端可选展示「本月重置」
```

### 3.4 新增模型函数签名

```go
// 延期清零三档 Token 用量 + bump token-only 锚点；绝不触碰 window_start/次数 used。
// now 为 RFC3339 本地时间串。
func ResetTokenStats(db *sql.DB, userID int64, now string) error
```

`ResetTokenStats` 的 SET 语义（一条 UPDATE，幂等安全）：

```
SET quota_token_5h_used   = 0,
    quota_token_week_used  = 0,
    quota_token_total_used = 0,
    week_start  = ?,   -- bump 到 now（token-only 锚点）
    month_start = ?    -- bump 到 now（token-only 锚点，新增）
WHERE user_id = ?
-- 明确不写：window_start / quota_5h_used / quota_total_used
```

### 3.5 `AtomicDeductQuota` 月窗口惰性重置（仿周窗口，SET + WHERE 同步）

```
monthCutoff := now - 30*24h
SET ...,
    quota_token_total_used = CASE WHEN month_start < ? THEN 0 ELSE quota_token_total_used END,
    month_start          = CASE WHEN month_start < ? THEN ? ELSE month_start END,
    ...
WHERE ... 
  AND (quota_token_total_limit = 0 OR (CASE WHEN month_start < ? THEN 0 ELSE quota_token_total_used END) < quota_token_total_limit)
-- 5h Token 维度：不加惰性重置 CASE（沿用 cron 每 5h 清零，与 PR #27 一致）
```

### 3.6 请求 / 响应 JSON 字段 & Admin API 增量入参

**POST /admin/api/users/{id}/extend（`extendUserRequest` 增量）**
- `reset_token_stats` : bool（缺省 false；true = 延期同时清零三档 Token 用量并 bump `week_start`/`month_start`）
- 既有 `days` / `until` 不变；响应不变（仅 `{expires_at, message}`）

**审计（audit_logs，新增 action）**
- `action = "token_stats_reset"`（既有 `action = "extend"` 仍写，二者并存）
- `target_type = "user"`，`target_id = <userID>`
- `detail`（JSON）：`{"dimensions":["5h","week","month"], "trigger":"extend", "operator":<admin 用户名或省略>}`
- `created_at = now`

### 3.7 类图（mermaid，另存 class-diagram-token-stats.mermaid）

要点：`Quota` 增 `MonthStart`；`QuotaStatus` 增 `MonthResetAt`；`extendUserRequest` 增 `ResetTokenStats`；`models` 包新增 `ResetTokenStats`；`Handler.ExtendUser` 编排 `ExtendUserExpiry` + `ResetTokenStats` + 两条审计；`quota.Checker.CheckAndDeduct` 经 `AtomicDeductQuota` 完成闸门（含月惰性重置）。

---

## 4. 程序调用流程（时序图 + 文字）

见 `docs/sequence-diagram-token-stats.mermaid`。两条链路：

1. **管理员提交延期（带 reset_token_stats）**：`Handler.ExtendUser` → `GetUserByID`（校验非 admin）→ `ExtendUserExpiry`（更新 `expires_at` + `status='active'`）→ 若 `reset_token_stats==true` 则 `ResetTokenStats`（清零三档 used + bump `week_start`/`month_start`，**不动 `window_start`/次数 used**）+ 写 `token_stats_reset` 审计 → 始终写既有 `extend` 审计 → 返回 200。处于 429 的用户因 `used` 已=0，下次请求即被 gate 放行（无需重启进程）。
2. **网关请求时 gate 月窗口惰性重置（滚动 30 天）**：`quota.Checker.CheckAndDeduct` → `models.AtomicDeductQuota` 单条原子 UPDATE，月维度用 `CASE WHEN month_start < (now-30d)` 完成清零 + bump；5h Token 维度**无**惰性重置（由 cron 每 5h 随 `window_start` 清零，沿用 PR #27）。

---

## 5. 任务列表（有序、含依赖、按实现顺序）

> 约束：≤5 个任务；每任务 ≥3 个相关文件；按层分组；T01 为「数据 / 基础」层（类比基础设施，最先落地，被其余任务依赖）。

### T01 — 数据层：迁移 + 模型 + 闸门月惰性重置 【P0】
- **涉及文件**：`internal/db/migrations.go`、`internal/models/quota.go`、`internal/models/user.go`、`internal/handler/quota.go`
- **依赖**：无
- **优先级**：P0
- **验收点**：
  - 迁移幂等（`columnExists` 守卫 + `month_start DEFAULT ''` 后回填 `datetime('now')`；存量行 `month_start=''` 被回填）；
  - `Quota` 含 `MonthStart`；`GetQuota` SELECT/Scan 含 `month_start`；`CreateUser` INSERT 写 `month_start`；
  - `AtomicDeductQuota` 的 SET 与 WHERE 均含月窗口 `CASE`（cutoff = now-30d），与周窗口对称；
  - **新增 `ResetTokenStats`**：清零三档 used + bump `week_start`/`month_start`，**明确不写** `window_start`/`quota_5h_used`/`quota_total_used`；
  - `AddTokenUsage` / `Reset5hQuota` / `CompensateQuotaReset` **不变**；
  - `QuotaStatus` 可选增 `MonthResetAt`（= `month_start + 30d`）；
  - `go build ./...` 通过。

### T02 — 后端延期接口 + 审计 【P0】
- **涉及文件**：`internal/admin/users.go`、`internal/admin/users_audit_test.go`、`internal/models/quota_test.go`、`internal/admin/handler.go`（确认路由 `POST /api/users/{id}/extend` 不变）
- **依赖**：T01
- **优先级**：P0
- **验收点**：
  - `extendUserRequest` 增 `ResetTokenStats bool`；`ExtendUser` 在延期成功后，若 `true` → 调 `models.ResetTokenStats` + 写 `token_stats_reset` 审计（`detail` JSON 含 `dimensions:["5h","week","month"]`、`trigger:"extend"`、best-effort `operator`）；
  - `reset_token_stats=false`（缺省）→ 仅延期、不清零、不写 `token_stats_reset`；
  - 单元测试断言：三档 used 归零 + `week_start`/`month_start`=now + 次数 used/`window_start` 不变 + 审计落库；
  - 既有 `extend` 审计始终保留。

### T03 — 前端：延期弹窗勾选框 + 用户面板展示 【P0】
- **涉及文件**：`web/admin/index.html`、`web/admin/app.js`、`web/user/index.html`
- **依赖**：T01（字段契约）、T02（API 契约）
- **优先级**：P0
- **验收点**：
  - `#extend-user-modal` 新增勾选框「重置 Token 统计（5 小时 / 本周 / 本月）」，**默认 `checked`**；
  - `extendUser` 默认勾选；`submitExtend` 发送 `reset_token_stats: checkboxEl.checked`（跟随勾选，可取消）；
  - 后端缺省 false、前端默认 true 的分工成立；
  - `web/user/index.html`「Token 月总量」行随重置即时归零显示（可选：展示「本月重置」时间取 `month_reset_at`）。

### T04 — 集成验证与回归 【P1】
- **涉及文件**：`internal/admin/users_test.go`、`internal/models/quota_test.go`、`docs/incremental-design-token-stats-reset.md`、`docs/sequence-diagram-token-stats.mermaid`、`docs/class-diagram-token-stats.mermaid`
- **依赖**：T01、T02、T03
- **优先级**：P1
- **验收点**：
  - `go test ./...` 全绿；既有 `migrations_token_window_test.go` / `token_total_recalc_test.go` / `quota_test.go` 不破；
  - 手测清单：建用户 → 拉满 5h/周/月 Token → 触发 429 → 管理员延期勾选重置 → 下次请求放行 → 审计含 `token_stats_reset` → 前端勾选框默认勾选且可取消 → 取消勾选仅延期不清零。

---

## 6. 依赖包列表

```
# 无新增第三方依赖（沿用现有）
modernc.org/sqlite   # 纯 Go SQLite 驱动（零 CGO），既有
database/sql          # 标准库，既有
encoding/json         # 标准库，审计 detail 序列化，既有
net/http             # 标准库，admin 接口，既有
time                  # 标准库，RFC3339 / cutoff 计算，既有
```

---

## 7. 共享知识（跨文件约定）

- **锚点列语义**：
  - `window_start` = 5h **调用次数**窗口（与 5h Token **共享**，token reset 路径**绝不可改**）；
  - `week_start` = **周 Token**桶锚点（token-only，可 bump）；
  - `month_start` = **月 Token**桶锚点（token-only，可 bump，本次新增）。
- **lazy reset 的 CASE SQL 模板**（周 / 月对称）：
  - SET：`col_used = CASE WHEN <anchor> < ? THEN 0 ELSE col_used END`，`<anchor> = CASE WHEN <anchor> < ? THEN ? ELSE <anchor> END`
  - WHERE：`(limit = 0 OR (CASE WHEN <anchor> < ? THEN 0 ELSE col_used END) < limit)`
  - cutoff：周 = `now-7d`，月 = `now-30d`。
- **时间约定**：一律 RFC3339 字符串；字符串序 = 时间序；空串 `''` 在闸门中视为「已过期」（安全首触，与 `week_start` 一致）。
- **硬约束**：`quota_5h_used` / `quota_total_used` / `window_start` 在任何 token reset 路径下**绝不**触碰（调用次数配额绝对不重置）。
- **`reset_token_stats` 分工**：后端缺省 `false`（仅延期）；前端默认勾选 `true`，随表单发送。
- **审计 action 常量**：`"extend"`（既有）、`"token_stats_reset"`（新增）；`detail` 用 JSON（含 `dimensions`/`trigger`/`operator`）；`audit_logs` 现有无 `operator` 列，operator 内嵌 `detail` 即可（见 §8）。
- **月窗口 = 滚动 30 天**（与周 7 天、上游月度额度 30 天一致）。
- **5h Token 维度无独立锚点列**（沿用 PR #27：复用 `window_start`，gate 不惰性重置 5h Token，由 cron 每 5h 清零）——admin reset 仅清零 `quota_token_5h_used`。

---

## 8. 待明确事项（需用户 / 主理人拍板）

1. **R1 最终取舍（本设计推荐「方案 B」）**：见下方 §R1。建议**不新增 `token_window_start` 列**；5h Token 仅清零 used，窗口沿用共享 5h 周期（与 PR #27 一致、零新增列 / 零 cron 改动、严格满足「次数不重置」）。若坚持「5h Token 锚点也 = now」，再考虑方案 A（需改 cron 写 `token_window_start`，净收益为 0）。**需用户 / 主理人最终确认**。
2. **审计 operator 来源**：`audit_logs` 现有无 `operator` 列且既有审计不记操作人；建议本增量将 `operator` 内嵌于 `token_stats_reset` 的 `detail` JSON（best-effort 取 admin session 用户名）。是否要标准化（另立 ADR 加 `operator` 列）待确认。
3. **存量用户 `month_start=now` 的副作用**：迁移仅置 `month_start=now`，**不清零** `quota_token_total_used`；已达 5h/周/月上限的存量用户需等到首个自然 30 天滚动或管理员显式 reset 才解封。是否可接受？（若不接受，可在迁移中一并清零存量 `quota_token_total_used`——但会改变既有累计语义，需确认。）
4. **用户面板是否展示「本月重置」时间**：建议 T01 在 `QuotaStatus` 输出 `month_reset_at`（= `month_start + 30d`），前端按需展示（可选增强，非 P0 必需）。
5. **cron 对 5h Token 的清零是否足够**：当前 5h Token 依赖 cron 每 5h 清零（无闸门惰性重置），与计数 5h 同一 cadence。如未来要求「cron 挂了也能惰性重置 5h Token」，则需回头引入 `token_window_start`（即方案 A）——本次不处理。

---

## §R1（关键设计风险）决策书

**背景**：`window_start` 是「5h 调用次数配额」与「5h Token 配额」的**共享锚点**（PR #27 明确「5h Token 复用现有 `window_start` 不加列」；cron `Reset5hQuota`/`CompensateQuotaReset` 每 5h 同时清 `quota_5h_used` 与 `quota_token_5h_used` 并重置 `window_start`）。若延期重置把 `window_start` 改为 now，会连带挪动调用次数 5h 窗口边界 → 违反「次数不重置」硬约束。

**两种方案**

- **方案 A（新增独立锚点列 `token_window_start`）**：为 5h Token 单开一列；cron 与 gate 的 5h Token 清零都改用它，与次数 5h 完全解耦；admin reset 设 `token_window_start=now` 真正「重启」5h Token 窗口。
- **方案 B（折中：只清零 `quota_token_5h_used`、不动 `window_start`）**：5h Token 窗口不重启、沿用共享 5h 周期；admin reset 仅把 used 归零。

**架构师推荐：方案 B**，理由如下（逐条硬证据，均来自代码核实）：

1. **gate 对 5h Token 根本不读锚点**：`AtomicDeductQuota` 的 5h Token 闸门是 `(quota_token_5h_limit = 0 OR quota_token_5h_used < quota_token_5h_limit)`，**没有任何 `window_start`/`token_window_start` 参与的 CASE**。换言之，5h Token 的「窗口」只由 `quota_token_5h_used` 决定，锚点在该维度对闸门**无影响**。
2. **锚点仅被 cron 消费**：`Reset5hQuota`/`CompensateQuotaReset` 用 `window_start` 决定何时把 `quota_token_5h_used` 与 `quota_5h_used` 一并清零。admin reset 把 `quota_token_5h_used` 归零后，下一次 cron（≤5h）会按 `window_start` 边界再次清零并重置——**无论方案 A 还是 B，5h Token 在下一个 cron tick 后行为完全收敛**。
3. **方案 A 的 `token_window_start=now` 在 gate 中无消费者**，且会被下一个 cron tick 直接用 `window_start` 边界覆盖；为它单开一列 + 改 cron SET 子句，**净收益为 0**，却引入额外列、迁移、cron 改动与回归面。违反 YAGNI。
4. **方案 B 严格满足硬约束**：`window_start`/`quota_5h_used`/`quota_total_used` 全程不触碰 → 「次数不重置」100% 成立；且 `used=0` 即令下次请求被 gate 放行（满足 P1「重置后 429 用户下次请求即放行」）。
5. **「重启窗口」语义的取舍**：对**周 / 月**两档，其锚点 `week_start`/`month_start` 是 token-only 且被 gate 惰性重置消费，故 admin reset bump 它们到 now = 真正「重启窗口」（语义完整）。唯独 5h Token 因 PR #27 的历史决策而**刻意与次数共享窗口**，其「重启」在 cron 驱动模型下等价于「立即清零 used + 在共享 5h 边界自然滚动」——这是该架构下唯一自洽的解释，而非缺陷。

**结论**：采用方案 B。增量仅新增 `month_start` 一列（周/月两档窗口锚点），**不新增 `token_window_start`**；cron 不改；5h Token 仅清零 used。R1 虽标为待确认，但本设计已据代码事实给出明确推荐（方案 B），请主理人 / 用户最终拍板；若仍选方案 A，需额外在 T01 增加 `token_window_start` 迁移 + cron SET 改动（不影响其余任务结构）。
