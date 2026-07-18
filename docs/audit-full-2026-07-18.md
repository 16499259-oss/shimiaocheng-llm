# 全量功能审计报告

- 日期：2026-07-18
- 代码基线：`main` @ `17560eb`（含 PR #9 token 总额、#10 审计 F1-F2-F3、#11 路由优先级、#12 同步 Token 记账、#13 倍率 Token 记账，均已合并并部署生产）
- 审计方法：静态走查 + `go build/vet/test` 全量基线 + 生产库抽样核对（本环境 Agent/TeamCreate 不可用，由主理人直接执行）

## 1. 基线结果

| 检查项 | 结果 |
|--------|------|
| `go build ./...` | ✅ 通过 |
| `go vet ./...` | ✅ 通过 |
| `go test ./...` | ✅ 全过（13 个包，proxy 4.3s 含新增回归测试） |
| `gofmt -l` | ✅ 无未格式化文件 |

> 说明：GitHub Actions 的 `make ci` 仍为 3 秒红灯（`proxy.golang.org` 拉工具链时段故障，非代码问题）；本地 go1.26.5 与 Docker golang:1.22 + `GOPROXY=off` 双重复现均全绿。

## 2. 本轮变更代码复核（PR #9–#13）

逐文件审查最近几轮改动，未发现新增功能性 bug：

| PR | 关键改动 | 复核结论 |
|----|----------|----------|
| #11 路由优先级 | `sortRoutingRules`：priority DESC → 窗口更窄优先 → id ASC | ✅ 稳健。复制快照避免并发修改；`sort.SliceStable`；`windowMinutes` 对非法时间窗回退最大宽度、跨午夜正确处理，**不会 panic** |
| #12 同步记账 | `handler.go` 同步路径补 `AddTokenUsage` | ✅ 与 `stream.go` 一致（fire-and-forget，delta≤0 为 no-op）；测试 `TestHandler_ServeHTTPSync_AccumulatesTokenUsage` 卡住回归 |
| #13 倍率记账 | 两路径 `AddTokenUsage(userID, ceil((prompt+completion)*multiplier))` | ✅ 无回归。`GetEffectiveMultiplier` 无窗口时返回 **1.0**（非 0），故非倍率时段 Token 按 ×1.0 计，未打回 #12 修复 |

### 倍率系统健壮性（重点验证）

- **负倍率退款漏洞：不存在。**
  - `fixed_multiplier`：admin 创建/编辑接口强制校验 `0.1 ≤ x ≤ 100.0`，否则 400（`internal/admin/users.go:380`）。
  - `time_multipliers`（全局时段倍率）：`MultiplierEngine.Create/Update` 本身**不校验范围**，但 `GetEffectiveMultiplier` 以 `maxMultiplier := 1.0` 起算、仅接受 `rule.Multiplier > maxMultiplier` 的规则。负数倍率永远无法覆盖 1.0 基线，被静默视为 1.0，既不会退款也不会产生负 token。
  - 结论：任意输入下 `effectiveCalls ≥ 1` 且 `billedTokens ≥ 0`，无配额反向加回风险。

## 3. 上一轮遗留问题状态（F1–F9）

| 编号 | 原级别 | 问题 | 当前状态 |
|------|--------|------|----------|
| F1 | High | `expires_at` 裸日期被静默当永久有效 | ✅ 已修复（PR #10）：`NormalizeExpiry`/`ParseExpiry` + auth 中间件 fail-closed |
| F2 | Medium | `/v1/quota` remaining 负数 | ✅ 已修复（PR #10）：`max(0, limit-used)`，limit=0 强制 remaining=0（`handler/quota.go:81-87`） |
| F3 | Medium | Admin 配额负值校验 | ✅ 已修复（PR #10）：`quota_5h/total/token_total` 负值均 400（`admin/users.go:249-260`） |
| F4 | Medium | admin 前端 XSS | 🟡 已基本缓解：所有用户可控字段均经 `escapeHtml`；`onclick` 内 `escapeAttr` 先转义 `\` 再转义 `'`，顺序正确，防 JS 注入。残留：admin 自填的时间字段（`start_time`/`end_time`）未转义，仅管理员自 XSS，且为格式校验的 HH:MM，风险极低 |
| F5 | Low | int64 Token 溢出 | 🟢 实际非问题：字段为 Go `int`（64 位平台即 int64），SQLite INTEGER；现实用量（亿级）远低于 9.2e18 阈值；`billedTokens` 的 float64 转换仅在 >9e15 token 时失精，不现实 |
| F7 | Low | 前端除零 | 🟡 部分修复：Token 行 `limit=0 → 无限` 已处理；但 `pct5h`/`pctTotal` 在 count 配额 limit=0 时仍除零（见 L2） |

## 4. 本轮新发现

未发现 High/Medium 级 bug。以下为 Low/Info 级观察项：

### L1（Low）— Token 计费闸门与后记账的超额偏差（PR #13 放大）
- 现象：`AtomicDeductQuota` 的 Token 门槛是纯列比较 `quota_token_total_used < quota_token_total_limit`（`models/quota.go:76`），**不乘倍率**；而 `AddTokenUsage` 加的是已乘倍率的 `billedTokens`。倍率窗口内单个请求可把 used 推过 limit 最多一个请求的 billed 量（×3 窗口更明显），下一请求才被拦。
- 影响：既有的"Token 后记账"设计与 count 原子扣减的不对称，PR #13 放大了该偏差，但属预期软闸门（注释已说明）。
- 建议（可选）：闸门也按 multiplier 预判（在 WHERE 中比较 `used + ceil(estimate*mult)`），可降低超额幅度。非必须。

### L2（Low）— 用户面板 count 配额 limit=0 时除零
- 现象：`web/user/index.html:341-342` `pct5h = used/quota_5h_limit`、`pctTotal = used/quota_total_limit`。若管理员将 count 配额设为 0，`limit=0` → `Infinity`/`NaN`，进度条宽度异常（不崩）。
- 连锁：edit 接口允许 `quota_5h_limit=0`（仅拒绝负值），而闸门 `used + calls <= 0` 实际会**锁死该用户**。
- 建议：① 前端对 count 配额也做 `|| 1` 保护或 `limit=0 → 无限`；② 后端 edit 接口对 count 配额 0 要么拒绝、要么与 Token 一致视为"无限"。

### L3（Info）— 时段倍率不支持 < 1.0 折扣
- 现象：`GetEffectiveMultiplier` 以 1.0 为基线，倍率 < 1.0 的规则被静默忽略（永远 ≤ 1.0 基线）。`MultiplierEngine.Create/Update` 不校验倍率范围（依赖 `> baseline` 守卫 + admin 前端校验）。
- 影响：无安全/正确性问题（无折扣能力而已）。若未来要支持"折扣时段"，需同步放开引擎基线逻辑。

### L4（Info）— Token 上限下调到已用量以下会立即全拦
- 现象：若管理员把某用户 `quota_token_total_limit` 设为低于当前 `quota_token_total_used`，闸门 `used < limit` 立即为假 → 该用户**所有请求**返回 `token_quota_exceeded`，直到用量自然下降。属 self-consistent（代码注释已说明），但需告知管理员这一行为。

## 5. 各模块检查清单

| 模块 | 检查点 | 结论 |
|------|--------|------|
| proxy/handler.go | 倍率解析、配额闸门、同步记账、usage 解析健壮性 | ✅ |
| proxy/stream.go | 流式记账、resp.Body 关闭（defer）、SSE 解析 | ✅ |
| router/selector.go | 排序并发安全、跨午夜、非法输入不 panic | ✅ |
| quota/manager.go, checker.go | 原子扣减、倍率引擎 | ✅ |
| quota/multiplier.go | 负倍率防护、时区锁定 Shanghai | ✅ |
| models/quota.go, expiry.go | Token 字段类型、AddTokenUsage no-op、expiry fail-closed | ✅ |
| admin/users.go, routing.go | 负值校验、倍率范围校验、priority 持久化 | ✅ |
| auth/middleware.go | expires_at fail-closed | ✅ |
| handler/quota.go | remaining 裁剪、扫描错误 non-fatal | ✅ |
| db/migrations.go | 幂等加列（priority / token_total_*）、回填 | ✅（部署验证通过） |
| web/admin/app.js | 用户字段 XSS 转义、innerHTML 注入 | 🟡 仅 admin 自填时间字段未转义（L4 已述，极低） |
| web/user/index.html | Token 除零处理 | 🟡 count 配额 limit=0 除零（L2） |

## 6. 结论与建议

- **整体健康**：生产代码基线全绿，最近 5 轮改动（路由优先级、同步记账、倍率记账）经逐项复核均无回归，倍率系统对负/异常输入具备多重防护。
- **无需立即修复项**：L1–L4 均为 Low/Info，不影响功能正确性与安全，可排入后续优化。
- **建议排期**：
  1. （Low）前端 count 配额 `|| 1` 除零保护 + 后端 edit 接口对 count 配额 0 与 Token 一致处理（拒绝或视作无限）。
  2. （Low，可选）Token 闸门按倍率预判，缩小高倍率窗口的超额幅度。
  3. （Info）在 Admin 文档/提示中明确"Token 上限下调到已用量以下会立即全拦"的行为。
  4. （Info）若需支持折扣时段，放开倍率引擎的 1.0 基线逻辑。

> 注：本报告为只读审计，未修改任何代码或生产数据。如需就 L1/L2 出修复 PR，告知即可。
