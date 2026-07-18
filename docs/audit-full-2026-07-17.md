# LLM API Gateway 全量审查报告（2026-07-17）

> 范围：对项目所有功能模块做静态审查 + 动态校验（编译 / `go vet` / 全量测试 / 针对性代码走查）。
> 环境说明：本环境 `TeamCreate` 不可用，无法拉起专用子代理（software-architect / software-qa-engineer），故由主理人直接使用本地工具链执行本次全量审计（架构静态审查 + QA 功能/边界/异常/资源审查二合一）。

## 0. 基线状态
- `CGO_ENABLED=0 /opt/homebrew/bin/go build ./...` ✅ 通过
- `CGO_ENABLED=0 /opt/homebrew/bin/go vet ./...` ✅ 无告警
- `CGO_ENABLED=0 /opt/homebrew/bin/go test ./internal/...` ✅ 12 个包全绿（含 #9 token 上限新增 11 用例）
- 资源释放规范：`sql.Rows`/`tx` 均有 `defer Close()`/`Rollback()`，未见泄漏。
- 并发安全：配额闸门为单条原子 `UPDATE ... WHERE`，无竞态；panic 有全局 `recoverMiddleware`；优雅关闭 30s。

## 1. 问题汇总（按严重程度）

| 编号 | 严重度 | 模块 | 标题 |
|------|--------|------|------|
| F1 | **High** | auth / admin | `expires_at` 裸日期/非 RFC3339 被静默当作"永不过期"（访问控制绕过） |
| F2 | Medium | handler | `/v1/quota` 的 `remaining` 未对超额做非负裁剪 |
| F3 | Medium | admin | 创建/编辑用户未校验配额字段为负（负值→用户被永久拦截） |
| F4 | Medium（建议核查） | web/admin | 模型名等外部字段渲染是否转义（潜在存储型 XSS） |
| F5 | Low/Medium | models | Token 计数器用 `int`，与 `call_logs.total_tokens`(int64) 不一致 |
| F6 | Low | handler | `/v1/quota` 忽略两条统计 `QueryRow` 的 `Scan` 错误 |
| F7 | Low | web/user | 5h/总配额上限为 0 时前端除零 |
| F8 | Low | handler | 两套 token 指标口径不同（展示一致性） |
| F9 | Low | handler | `/v1/quota` 每次多 2 条 `call_logs` 聚合查询（性能） |

---

## 2. 详细发现

### F1 — High — `expires_at` 非 RFC3339 被静默当作永久有效
- **位置**
  - `internal/auth/middleware.go:86-92`（鉴权过期判定）
  - `internal/admin/users.go:117`（创建直接落库 `req.ExpiresAt`）
  - `internal/admin/users.go:479`（`extend`：`newExpiresAt = req.Until` 直接落库）
- **描述**：中间件仅 `time.Parse(time.RFC3339, user.ExpiresAt)`，当 `ExpiresAt` 为非 RFC3339 字符串（如裸日期 `"2026-08-15"`）时 `err != nil`，整个过期判定被 `if err == nil && ...` 跳过 → 该 Key 被当作永不过期。Admin 创建/延期接口未对 `expires_at` 做规范化，裸日期会被原样入库。
- **复现路径**：
  1. 用 Admin API 创建用户：`POST /admin/api/users` `{"username":"t","expires_at":"2026-08-15", ...}`
  2. 用该 Key 调 `POST /v1/chat/completions` → **预期 403 `key_expired`，实际 200 成功**。
- **影响**：本应到期的 Key 无限可用，绕过有效期/配额控制（计费与访问控制边界失效）。与 #8 修复的"大仙"事件同源——当时用迁移硬编码 RFC3339 修了单个用户，但**通用解析未修**。
- **建议**：
  - 在 API 边界（create/update/extend）用 `models.NormalizeToShanghaiRFC3339` 同类逻辑把裸日期规范为 RFC3339（+08:00，建议取当日 `23:59:59` 作为到期时刻），或新增 `NormalizeExpiry`；
  - 中间件解析失败时**按"已过期"处理**而非跳过（fail-closed）。

### F2 — Medium — `/v1/quota` 的 remaining 未对超额裁剪
- **位置**：`internal/handler/quota.go:68,71,82`
- **描述**：`remaining = limit - used` 未做 `max(0, ...)`。Token 维度因"请求后记账"允许 `used >= limit`（软闸门，已确认接受），此时 `quota_token_total_remaining` 返回负数；5h/总维度因原子闸门一般不为负，但并发边界同。
- **复现**：设 token 上限 100，累计使 used=120 → `GET /v1/quota` → `quota_token_total_remaining = -20`。
- **影响**：API 契约返回负剩余值，消费方误用。
- **建议**：`remaining = max(0, limit-used)`；`limit<=0` 保持 0（前端据此判"无限"）。

### F3 — Medium — Admin 配额字段负值未校验
- **位置**：`internal/admin/users.go`（create/update 路径；当前仅校验了 `max_concurrency`）
- **描述**：`Quota5hLimit`/`QuotaTotalLimit`/`QuotaTokenTotalLimit` 接受负值。结合原子闸门 `used+delta <= 负数` 永不成立（或 token `used < 负数` 永不成立），该用户被**永久拦截**（任意请求 429）。
- **复现**：Admin 创建用户 `quota_token_total_limit = -1` → 该用户任意请求均 429。
- **影响**：管理员误操作即导致用户不可用，且无报错提示，难察觉。
- **建议**：校验配额字段 `>= 0`（token 上限 0=不限制，负值非法，返回 400）。

### F4 — Medium（建议核查）— Admin 前端外部字段转义
- **位置**：`web/admin/app.js`（调用统计 / 模型筛选下拉）
- **描述**：上游返回的 `model` 名写入 `call_logs` 并在 Admin 展示。若经 `innerHTML` 拼接且未转义，恶意上游可注入脚本（存储型 XSS）。用户自助面板（`web/user/index.html`）使用 `textContent`，安全；Admin 需核查。
- **核查点**：检查 `app.js` 中模型名/用户名插入是否走 `textContent` 或 HTML 转义。
- **影响**：管理员会话存在被劫持风险。
- **建议**：所有外部来源字段用 `textContent` 或转义后插入。

### F5 — Low/Medium — Token 计数器类型不一致
- **位置**：`internal/models/quota.go:17-18,32-33`；`internal/handler/quota.go:35-36`
- **描述**：`QuotaTokenTotalUsed/Limit` 为 `int`，而 `call_logs.total_tokens` 为 `int64`。x86_64（服务器）上 `int`=64 位，当前不溢出；但类型不统一，32 位构建会溢出，且两处"token"语义不同（见 F8），易致维护误解。
- **建议**：Token 计数器统一为 `int64`。

### F6 — Low — `/v1/quota` 忽略 Scan 错误
- **位置**：`internal/handler/quota.go:88-89`
- **描述**：`h.DB.QueryRow(...).Scan(&totalTokens)` 错误未检查，DB 异常时静默返回 0。
- **建议**：检查 `err` 并至少 `log`；失败时用 `quota_token_total_used` 兜底。

### F7 — Low — 用户面板 5h/总配额上限为 0 时除零
- **位置**：`web/user/index.html:341-342`
- **描述**：`pct5h = used/limit*100`，若 `quota_5h_limit==0`（无限）则除零 → `Infinity` → 进度条宽度/配色异常。Token 已做 `limit==0` 守卫，5h/总未做。
- **建议**：`limit<=0` 显示"无限"并隐藏进度条（同 token 处理）。

### F8 — Low — 两套 token 指标口径不同
- **位置**：`handler/quota.go:75-91`（配额口径 `prompt+completion`）vs `:88-89`（统计口径 `call_logs.total_tokens`=provider 总）
- **描述**："Token 总量"进度条用 `prompt+completion` 累加（与配额判定一致），而"累计 Token/今日 Token"用 provider 上报 `total_tokens`（可能 ≠ `prompt+completion`）。两数值会系统性偏离。
- **影响**：展示层面用户困惑。
- **建议**：统一口径并在 UI 标明。

### F9 — Low — `/v1/quota` 额外聚合查询
- **位置**：`handler/quota.go:88-89`
- **描述**：每次请求多 2 条 `call_logs` `SUM(total_tokens)` 聚合（且口径与配额不同，见 F8）。
- **建议**：可合并进配额查询或加缓存；低优先。

---

## 3. 模块覆盖清单（证明全覆盖）
- **proxy**：`handler.go`（鉴权→配额→429 分类→sync/stream 分发）、`stream.go`（SSE 解析 + token 记账）✅
- **auth**：`middleware.go`（SubKeyAuth 子Key→user.ID 归属正确；AdminSessionAuth）✅
- **models**：`quota.go`（AtomicDeductQuota OR/AND 闸门、AddTokenUsage、UpdateQuotaTokenTotalLimit）、`user.go`、`call_log.go`、`call_stats.go`（除零已守卫）、`session.go` ✅
- **db**：`migrations.go`（token 列幂等加列 + 历史回填；`expires_at` 列；大仙修正）✅
- **admin**：`users.go`（CRUD + token 接线 + 校验缺口 F3）、`calls_stats.go`（时区规范化）✅
- **handler**：`quota.go`（F2/F6/F8）、`calls.go` ✅
- **quota**：`multiplier.go`、`scheduler.go`、`checker.go`、`manager.go` ✅
- **provider**：`store.go`（CRUD/审计）、`router/selector.go`（时段路由）✅
- **config / main / router / security / timeutil**：配置消费完整一致；KEK Fatal 正确；优雅关闭/panic 恢复完整 ✅
- **web**：`admin/*`、`user/index.html`（token 进度条、limit=0 守卫；F7 除零）✅
- **CI / Makefile**：`go-version: 1.22.x` 与 `go.mod` 对齐；`make ci` = fmt→vet→test→build-linux→shellcheck ✅

## 4. 审计限制（非缺陷）
- `config.yaml` 不在仓库（部署于服务器 `/opt/llm-gateway/config.yaml`），无法核对实际配置值；代码侧消费完整一致。
- 本环境无法拉起专用审查子代理（TeamCreate 工具不可用），审查由主理人直接执行，结论基于代码走查 + 编译/vet/测试，未自动提交复现测试（避免污染仓库测试树）。

## 5. 优先修复建议
1. **F1（High）**：规范化 `expires_at` 解析（fail-closed）—— 影响访问控制，建议优先。
2. **F3（Medium）**：Admin 配额字段负值校验。
3. **F2（Medium）**：`/v1/quota` remaining 非负裁剪。
4. **F4（Medium）**：核查 Admin 前端外部字段转义。
5. 其余 Low 项按需处理。
