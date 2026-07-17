# 增量架构设计：用户级「累计 Token 总量上限」

| 项 | 内容 |
|---|---|
| 项目 | LLM API Gateway（Go 1.22 / SQLite `modernc.org/sqlite` / nginx 前置） |
| 文档类型 | 增量设计 + 任务分解（仅描述本次变更，不重写完整架构） |
| 架构师 | 高见远（Gao） |
| 关联 PRD | `docs/prd-token-total-limit.md`（许清楚） |
| 核心目标 | 在现有「5h 窗口次数 + 累计总次数」之上，新增第三限额维度「累计 Token 总量上限」（用户级，与次数维度 OR；上限 0 = 不限制） |

---

## 1. 实现方案 + 框架选型（沿用现有栈，最小侵入点）

**技术栈**：完全沿用现有栈——Go 1.22、零 CGO、`modernc.org/sqlite`、分层 `models / quota / proxy / admin / handler`、前端 `//go:embed` 编入二进制。**不引入任何新依赖**。

**关键设计决策（按 PRD 待确认问题逐一拍板）**

| # | 待确认问题 | 决策 |
|---|---|---|
| 1 | 子 Key 是否独立限 Token | **否，仅用户级**。认证中间件已把 sub-key 解析为所属 `user_id`；`quotas` 以 `user_id` 为键 → 子 Key 请求天然计入所属用户，无需额外代码。 |
| 2 | 拦截状态码 / 文案 | **HTTP 429**；`type` 区分 `quota_exceeded`（次数）vs `token_quota_exceeded`（Token）；文案「Token 额度已用尽」。 |
| 3 | 存量回填口径 | `SUM(prompt_tokens + completion_tokens)`（与本次计量口径一致），来自 `call_logs`。 |
| 4 | 上限设低于已用量 | **允许**，下次请求立即被拦（自洽）。 |
| 5 | 计数口径 | 以 `quotas.quota_token_total_used` 为 Token 唯一口径（请求后累加 = `prompt + completion`）；`call_logs` 仅作历史统计展示。 |
| 6 | 全局默认 token 上限 | **本次不做（P2）**，新建用户默认 0（不限制）。 |

**最小侵入点拆解**

- **数据层**：`ALTER` 加两列（幂等 `columnExists` 守卫，沿用现有风格）+ 一次性回填。**不改动 `models.CreateUser` 签名**——新列 `DEFAULT 0` 天然保证「默认不限制」，从而避免改动约 10 处调用点（`main.go` 种子 + 多个 `*_test.go`）与全部相关测试。
- **限额逻辑**：在现有 `AtomicDeductQuota` 的 `WHERE` 增补 Token 维度（纯列比较，**无新增绑定参数**）。
- **记账**：新增 `models.AddTokenUsage(db, userID, delta)`，**在 `handleSync` / `handleStream` 响应结束后调用**（与现有 `models.InsertCallLog` 同位置，沿用「handler 直接调 models」的模式）。**不启用 `Manager.ConfirmStream`**（保持 no-op，避免重复累加）。
- **拦截**：在 `ServeHTTP` 现有 429 分支增补维度判定——原子 `UPDATE` 失败后用 `GetQuota` 读回分类，返回对应 `type`。
- **重置窗口 (`Reset5hQuota` / `CompensateQuotaReset`)**：仅重置 `quota_5h_used`，**不触及** `quota_token_total_used`（Token 总量为终身累计，符合「累计」语义）。

---

## 2. 文件列表（标注 新增 / 修改）

| 文件 | 状态 | 改动要点 |
|---|---|---|
| `internal/db/migrations.go` | **修改** | `quotas` 表 `ALTER` 加 `quota_token_total_limit` / `quota_token_total_used`（幂等）；一次性回填 `used`。 |
| `internal/models/quota.go` | **修改** | `Quota` / `QuotaStatus` struct +2/+3 字段；`GetQuota` SELECT 加列；`AtomicDeductQuota` WHERE 加 Token 维度；新增 `AddTokenUsage` / `UpdateQuotaTokenTotalLimit`。 |
| `internal/models/user.go` | **修改** | `UserWithQuota` +2 字段；`ListUsers` SELECT + Scan 加两列。（`CreateUser` 签名**不变**） |
| `internal/proxy/handler.go` | **修改** | `ServeHTTP`：429 分支按维度分类（`token_quota_exceeded`）；`handleSync` 结尾调 `AddTokenUsage`。 |
| `internal/proxy/stream.go` | **修改** | `handleStream` 结尾调 `AddTokenUsage`。 |
| `internal/admin/users.go` | **修改** | `createUserRequest` / `updateUserRequest` + `QuotaTokenTotalLimit *int`；`CreateUser` / `UpdateUser` 按需调 `UpdateQuotaTokenTotalLimit`；响应体返回新字段。 |
| `internal/handler/quota.go` | **修改** | `QuotaStatus` 组装补充 `quota_token_total_limit` / `quota_token_total_used`（及 remaining）。 |
| `web/admin/index.html` | **修改** | 用户表头 Token 列语义；创建/编辑弹窗新增「Token 总量上限」字段。 |
| `web/admin/app.js` | **修改** | `loadUsers` Token 单元格改 quota 口径；`createUser` / `editUser` / `updateUser` 传递新字段。 |
| `web/user/index.html` | **修改** | `renderDashboard` 新增「Token 总量」行 + 进度条（上限 0 显示「无限」）。 |
| `internal/quota/manager.go` | **不改** | `ConfirmStream` 保持 no-op（本次不接线，避免重复累加）。 |

---

## 3. 数据结构和接口变更

### 3.1 Struct 变更（`internal/models/quota.go` / `user.go`）

```go
// Quota — quotas 表映射（新增 2 字段，默认 0）
type Quota struct {
    ID                  int64           `json:"id"`
    UserID              int64           `json:"user_id"`
    Quota5hLimit        int             `json:"quota_5h_limit"`
    Quota5hUsed         int             `json:"quota_5h_used"`
    QuotaTotalLimit     int             `json:"quota_total_limit"`
    QuotaTotalUsed      int             `json:"quota_total_used"`
    WindowStart         string          `json:"window_start"`
    UpdatedAt           string          `json:"updated_at"`
    FixedMultiplier     sql.NullFloat64 `json:"fixed_multiplier"`
    QuotaTokenTotalLimit int            `json:"quota_token_total_limit"` // 新增：0 = 不限制
    QuotaTokenTotalUsed  int            `json:"quota_token_total_used"`  // 新增：累计已用
}

// QuotaStatus — /v1/quota 响应（新增 3 字段）
type QuotaStatus struct {
    // ... 原有 5h / total / total_tokens / window_reset_at / status ...
    QuotaTokenTotalLimit     int `json:"quota_token_total_limit"`
    QuotaTokenTotalUsed      int `json:"quota_token_total_used"`
    QuotaTokenTotalRemaining int `json:"quota_token_total_remaining"` // limit==0 时前端按无限处理
}

// UserWithQuota — 列表/详情响应（新增 2 字段，向后兼容）
type UserWithQuota struct {
    User
    Quota5hLimit         int      `json:"quota_5h_limit"`
    Quota5hUsed          int      `json:"quota_5h_used"`
    QuotaTotalLimit      int      `json:"quota_total_limit"`
    QuotaTotalUsed       int      `json:"quota_total_used"`
    TotalTokens          int64    `json:"total_tokens"` // 保留（call_logs 统计），前端不再用于 Token 列
    QuotaTokenTotalLimit int      `json:"quota_token_total_limit"` // 新增
    QuotaTokenTotalUsed  int      `json:"quota_token_total_used"`  // 新增
    SubKey               string   `json:"sub_key,omitempty"`
    FixedMultiplier      *float64 `json:"fixed_multiplier"`
}
```

### 3.2 关键函数（新增 / 修改）

```go
// 修改：原子预扣 WHERE 增补 Token 维度（无新增绑定参数，参数顺序不变）
func AtomicDeductQuota(db *sql.DB, userID int64, effectiveCalls int) (bool, error) {
    now := time.Now().Format(time.RFC3339)
    result, err := db.Exec(
        `UPDATE quotas
         SET quota_5h_used = quota_5h_used + ?,
             quota_total_used = quota_total_used + ?,
             updated_at = ?
         WHERE user_id = ?
           AND quota_5h_used + ? <= quota_5h_limit
           AND quota_total_used + ? <= quota_total_limit
           AND (quota_token_total_limit = 0 OR quota_token_total_used < quota_token_total_limit)`,
        effectiveCalls, effectiveCalls, now,
        userID,
        effectiveCalls, effectiveCalls,
    )
    // rowsAffected == 1 → 放行；== 0 → 任一维度达上限，拦截
}

// 新增：请求后累加 Token 用量（用户级；delta = prompt + completion）
func AddTokenUsage(db *sql.DB, userID int64, delta int) error {
    if delta <= 0 { return nil }
    _, err := db.Exec(
        `UPDATE quotas SET quota_token_total_used = quota_token_total_used + ? WHERE user_id = ?`,
        delta, userID,
    )
    return err
}

// 新增：设置用户 Token 总量上限（0 = 不限制）
func UpdateQuotaTokenTotalLimit(db *sql.DB, userID int64, limit int) error {
    now := time.Now().Format(time.RFC3339)
    _, err := db.Exec(
        `UPDATE quotas SET quota_token_total_limit = ?, updated_at = ? WHERE user_id = ?`,
        limit, now, userID,
    )
    return err
}
```

### 3.3 类图（Mermaid）

见 `docs/design-token-total-limit-class.mermaid`。

---

## 4. 程序调用流程（时序图）

见 `docs/design-token-total-limit-seq.mermaid`。要点：

- **请求前拦截**：`CheckAndDeduct` → 原子 `UPDATE`（WHERE 含 5h AND total AND token 三维度）→ `rowsAffected==0` 即拦截。拦截后用 `GetQuota` 读回，按 `token_limit!=0 && token_used>=token_limit` 判定返回 `token_quota_exceeded`，否则 `quota_exceeded`。
- **请求后记账（sync）**：`handleSync` 转发上游 → `ExtractTokenUsage` → `InsertCallLog` → `AddTokenUsage(userID, prompt+completion)`。
- **请求后记账（stream）**：`handleStream` 解析 SSE `usage` → `InsertCallLog` → `AddTokenUsage(userID, prompt+completion)`。
- 两条记账路径调用同一个 `models.AddTokenUsage`，**不在 `Manager.ConfirmStream` 重复记账**。

---

## 5. 任务列表（有序 + 依赖 + 标注）

> 依赖关系：`T1 → T2 → {T3, T4, T5, T6} → {T7(T5), T8(T6)}`。T3/T4/T5/T6 互不依赖，可并行实现。

| ID | 任务 | 优先级 | 新增/修改文件 + 动作 | 依赖 | 验收要点 |
|---|---|---|---|---|---|
| **T1** | 迁移：加列 + 存量回填 | P0 | `internal/db/migrations.go`（修改）：`columnExists` 守卫下 `ALTER` 加 `quota_token_total_limit INTEGER NOT NULL DEFAULT 0`、`quota_token_total_used INTEGER NOT NULL DEFAULT 0`；建列时一次性 `UPDATE ... SET quota_token_total_used = COALESCE((SELECT SUM(prompt_tokens+completion_tokens) FROM call_logs WHERE call_logs.user_id=quotas.user_id),0)`。 | — | 重启后列存在；存量用户 `used` = 历史求和；二次重启不覆盖 live 增量。 |
| **T2** | 模型与限额逻辑 | P0 | `internal/models/quota.go`（修改）：`Quota`+2 字段；`GetQuota` SELECT/Scan +2 列；`AtomicDeductQuota` WHERE 加 Token 维度；`QuotaStatus`+3 字段；新增 `AddTokenUsage`、`UpdateQuotaTokenTotalLimit`。`internal/models/user.go`（修改）：`UserWithQuota`+2 字段；`ListUsers` SELECT/Scan +2 列（`CreateUser` 签名不变）。 | T1 | `AtomicDeductQuota` 在 token 达上限/上限=0 时行为正确；单测覆盖「token 达上限拦截」「上限=0 不拦截」。 |
| **T3** | 请求前拦截（含差异文案） | P0/P1 | `internal/proxy/handler.go`（修改）：`ServeHTTP` 429 分支读 `GetQuota` 分类，`token_quota_exceeded`/「Token 额度已用尽」vs `quota_exceeded`；`CallLog.ErrorMsg` 同步。 | T2 | token 达上限 → 429 + `type=token_quota_exceeded`；次数达上限 → `quota_exceeded`；上限=0 永不因 token 拦截。 |
| **T4** | 请求后记账（sync + stream） | P0 | `internal/proxy/handler.go`（修改 `handleSync` 结尾）、`internal/proxy/stream.go`（修改 `handleStream` 结尾）：响应结束后 `models.AddTokenUsage(userID, promptTokens+completionTokens)`（置于 `InsertCallLog` 之后，错误仅 log 不中断响应）。 | T2 | sync/stream 各成功响应后 `quota_token_total_used` 正确累增；上游错误/客户端断连不致命。 |
| **T5** | Admin API：token 上限读写 | P0 | `internal/admin/users.go`（修改）：`createUserRequest`/`updateUserRequest` 加 `QuotaTokenTotalLimit *int`；`CreateUser` 创建后若非空调 `UpdateQuotaTokenTotalLimit` 并写入响应；`UpdateUser` 若非空调之并回写 `response["quota_token_total_limit"]`；`ListUsers` 响应经模型自动带新字段。 | T2 | 创建/编辑传 `quota_token_total_limit` 生效；列表返回 `quota_token_total_limit`/`quota_token_total_used`。 |
| **T6** | /v1/quota 返回 token 字段 | P0 | `internal/handler/quota.go`（修改）：`QuotaStatus` 组装补 `QuotaTokenTotalLimit`、`QuotaTokenTotalUsed`、Remaining（`limit==0` 时 remaining 置 0，由前端判无限）。 | T2 | `GET /v1/quota` 响应含两个新字段；历史 `total_tokens` 保留。 |
| **T7** | Admin 前端：列 + 弹窗 | P1 | `web/admin/index.html`（修改）：表头 Token 列；创建/编辑弹窗加「Token 总量上限」（number, 默认 0, 占位「0 = 不限制」）。`web/admin/app.js`（修改）：`loadUsers` Token 单元格改 `used/limit`（0→「无限」+进度色）；`createUser`/`updateUser` body 传 `quota_token_total_limit`（非空才传）；`editUser` 签名+回填新值。 | T5 | 列表 Token 列显示 quota 口径；创建/编辑可设上限并回填。 |
| **T8** | User 面板：Token 进度条 | P1 | `web/user/index.html`（修改）：`renderDashboard` 在「总配额」进度条下新增「Token 总量」行 + 进度条（`used/limit`；`limit==0` 显示「无限」并隐藏进度条）。 | T6 | 面板展示 token 用量/上限；0 上限时显示「无限」无进度条。 |

---

## 6. 依赖包列表

**无新增第三方依赖。** 全部沿用现有 `go.mod`（`modernc.org/sqlite` 等）。前端无新库。

---

## 7. 共享知识（跨文件约定）

- **表字段名（唯一口径）**：`quotas.quota_token_total_limit`（0=不限制）、`quotas.quota_token_total_used`（累计已用，口径 = `prompt_tokens + completion_tokens`）。所有 Token 计量必须走这两个字段，禁止再用 `call_logs.total_tokens` 作为限额判定。
- **错误 `type` 常量（proxy 拦截）**：
  - 次数达上限：`quota_exceeded`（文案 `"Quota exceeded"`）
  - Token 达上限：`token_quota_exceeded`（文案 `"Token 额度已用尽"`）
  - 二者均 HTTP 429，JSON 结构 `{error:{message,type,code}}`，`type` 与 `code` 同值。
- **原子 UPDATE 片段（务必命中 `user_id` 唯一索引）**：
  ```sql
  AND (quota_token_total_limit = 0 OR quota_token_total_used < quota_token_total_limit)
  ```
  严格 `<`：恰好等于上限即拦截（符合「达上限即拦」）。
- **记账 SQL 片段**：`UPDATE quotas SET quota_token_total_used = quota_token_total_used + ? WHERE user_id = ?`（用户级，子 Key 经 auth 已是 user_id）。
- **0 值语义**：`quota_token_total_limit = 0` ⇔ 不限制，等同于现状，绝不触发 Token 拦截。
- **重置窗口不碰 Token**：`Reset5hQuota` / `CompensateQuotaReset` 仅清 `quota_5h_used`，`quota_token_total_used` 终身累计。
- **前端默认值**：Admin 弹窗 token 上限默认 `0`；User 面板 `limit==0` 显示「无限」。
- **结构体兼容**：`Quota` / `QuotaStatus` / `UserWithQuota` 均为具名字段字面量，新增字段向后兼容；`models.CreateUser` 签名本次保持不变（默认 0 由列 DEFAULT 保证）。

---

## 8. 待明确事项 / 回归风险

1. **拦截分类优先级（已决策，提示风险）**：当次数与 Token 同时达上限时，读回分类会优先报 `token_quota_exceeded`（因 `token_used >= limit` 为真）。该判定基于「原子 UPDATE 已做出拦截决策、读回仅做标注」，是 race-free 的；极端并发下文案可能偏向 Token，但拦截本身永远正确。可接受。
2. **非位置式字面量**：已确认三处 struct 字面量均为具名字段（`&Quota{}`、`&UserWithQuota{...}`、`models.QuotaStatus{...}`），新增字段不破坏编译。实现时仍须 `grep` 确认无遗漏的位置式字面量。
3. **单条超大请求超出**：如 PRD 所述，单条请求 `prompt+completion` 可能使 `used` 越过上限（少量），下一条请求必拦；属已接受行为。
4. **`Manager.ConfirmStream` 弃用**：保持 no-op，本次不接线；若后续想统一记账入口，再单独重构（避免与 `AddTokenUsage` 重复累加）。
5. **审计日志（P2-11）**：本次 Token 上限变更不写 `audit_logs`（与现有 route/multiplier 审计不一致，留待 P2）。
6. **存量回填幂等**：回填置于「`quota_token_total_used` 列首次创建」分支内，天然仅执行一次；若运维手动 `UPDATE` 过该列，重启不会再次覆盖（因列已存在）。
7. **回归范围**：现有次数限额行为完全不变（默认 `token_total_limit=0` → WHERE 的 Token 子句恒真）；建议 QA 重点验证「老用户不被 Token 拦截」「上限=0 行为与升级前一致」。
