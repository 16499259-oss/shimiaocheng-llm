# QA 测试报告：上游 Provider 月度额度可见性

- **功能分支**：`feat/provider-monthly-quota`（commit `8eede80`）
- **测试工程师**：严过关（software-qa-engineer）
- **测试日期**：2026-07-19
- **测试性质**：独立验收（不信任工程师自测结论，独立重跑 + 代码审查 + 边界补测 + 线上/本地冒烟）
- **IS_PASS**：**YES（源码无 Bug）**

---

## 1. 测试概览

| 项目 | 结果 |
| --- | --- |
| 全量单测（12 包） | **195 + 2 补测 = 197 PASS / 0 FAIL** |
| `gofmt`（改动文件） | 无 diff（干净） |
| `go vet ./...` | 无输出（干净） |
| `CGO_ENABLED=0 go build -o /dev/null .` | BUILD_OK（干净） |
| 边界补测 | 2 个新增，全部 PASS |
| 线上/本地冒烟（真实 admin 会话） | PASS（含 401 鉴权拦截、404 未知 slug） |

> 工程师自报 "12 包 0 FAIL、IS_PASS:YES" —— **经独立重跑确认属实**（原 195 个测试函数，0 失败）。

---

## 2. 单测结果

命令：`export GATEWAY_KEK_ENV=test-kek-00000000000000000000000000 && go test ./... -v`

- 12 个含测试包的测试函数全部通过：**195 PASS / 0 FAIL**（新增边界测试后总计 197）。
- 关键测试：
  - `models`：`TestIsLowBalance`（含 90/100=0.9 边界标红）、`TestBuildProviderUsageView`（无限/限内/超限/空 provider）、`TestAggregateProviderUsage`、`TestGetProviderUsage`
  - `admin`：`TestHandleListProviderUsage`（{data:[...]} 含 token_used/call_used/remaining/low）、`TestHandleGetProviderUsage`（200 + 404 未知 slug）
  - `db`：`TestRunMigrations_Idempotent`（连跑两次 RunMigrations 不报错、种子规则不重复）

---

## 3. 代码审查结论

逐文件 Read 实际实现，按验收标准逐条核对：

### P0-1 providers 表双口径字段 + 迁移
- `migrations.go`：新增 `monthly_token_limit` / `monthly_call_limit`，均以 `columnExists()` 守卫 + `ALTER ... DEFAULT 0` 方式幂等添加（333–349 行）。✅
- `store.go`：`ListProviders` / `GetProvider` / `CreateProvider` / `UpdateProvider` / `SeedFromConfig` 均正确读写两列；`SeedFromConfig` 以 `0,0`（无限）落地。✅
- 幂等性：独立以内存 SQLite 连跑两次 `RunMigrations` 不报错（既有测试覆盖）。✅
- 未配置视为无限：`0` 语义正确；字段缺失读取不报错（`BuildProviderUsageView` 对 nil usage 按 0 处理，聚合结果为空 map 时按 0 用）。✅

### P0-2 滚动 30 天聚合
- `models/provider_usage.go` `AggregateProviderUsage`：`GROUP BY provider_id`，`SUM(prompt_tokens+completion_tokens)`→token 已用、`SUM(effective_calls)`→调用次数已用，`WHERE created_at >= ?` 按滚动窗口过滤。✅
- `RollingWindowStart()` = `now-30*24h`（Asia/Shanghai，RFC3339），**窗口滑动非自然月**。✅
- `BuildProviderUsageView`：`limit<=0` ⇒ `Unlimited=true`、`Remaining=-1`；空 provider `used=0`、`remaining=limit`；超限展示真实负值（仅可见）。✅
- **关键正确性核对**：文本比较 `created_at >= windowStart` 成立的前提是 `call_logs.created_at` 与 `windowStart` 同为 RFC3339(+08:00) 格式。经核查，`InsertCallLog`（`models/call_log.go:61`）以 `time.Now().In(ShanghaiTZ).Format(RFC3339)` 落库，且 `quota/manager.go` 等其它路径不写 call_logs，故生产中窗口过滤正确。✅

### P0-3 上游列表列展示
- `web/admin/index.html` 上游列表表头恰为 12 列（含「本月 Token」「本月 调用」），与 JS 行模板 12 `<td>` 及空态 `colspan="12"` 一致。✅
- `app.js loadProviders` 通过 `api/provider-usage` 合并按 slug，调用 `renderUsageCell` 渲染「已用/上限 + 进度条 + 百分比」；无限显示「不限制」；低余额加 `usage-low`（标红）。数据来自聚合接口。✅

### P0-4 开账号表单实时提示
- `fetchProviderUsage(slug, hintId)`：依 `fixed_provider` 实时 `GET api/providers/{slug}/usage`；未指定显示「未指定固定上游（全局路由，无额度提示）」；异常/无数据仅显示「获取失败」，**不阻断提交**。✅
- `index.html` 用户模态含「上游剩余额度（实时提示，仅供查看）」块；低余额标红（复用 `renderUsageCell`）。✅

### P0-5 低余额三处统一标红（仅视觉）
- 列表列（`usage-low`）、表单提示（`usage-low`）、仪表盘卡片（`usage-card-low`）三处均依据后端 `token_low`/`call_low` 标红，且均不改变任何写操作。✅

### P1-1 独立仪表盘页
- `handler.go:90` 注册 `GET /provider-usage`（挂载于 `/admin/provider-usage`，走 admin 鉴权）；`ServeProviderUsagePage` 注入 `window.__INIT_TAB__='provider-usage'` 深链直达。✅
- `app.js loadProviderUsage` 经 `api/provider-usage` 批量聚合渲染全部 provider 卡片（进度条 + 低余额标红）。`index.html` 含「上游额度」侧栏导航与 `tab-provider-usage` 区块。✅
- `style.css` 含 `usage-low` / `usage-bar` / `usage-fill` / `usage-card` 等样式。✅

### 路由与信封一致性
- `handler.go` 三条路由：`GET /api/provider-usage`、`GET /api/providers/{slug}/usage`、`GET /provider-usage`；前两条在 `/admin/` 子 mux 下，经 `AdminSessionAuthAPI` 鉴权。✅
- 响应统一 `{data: ...}` 信封（`HandleListProviderUsage` / `HandleGetProviderUsage`）。✅
- 前后端字段名一致：`slug` / `name` / `monthly_token_limit` / `monthly_call_limit` / `token_used` / `call_used` / `token_remaining` / `call_remaining` / `token_unlimited` / `call_unlimited` / `window_start` / `token_low` / `call_low`（冒烟实测 JSON 已确认）。✅

---

## 4. 边界验证（补充测试）

工程师单测覆盖了 `IsLowBalance` 0.9 边界（`{90,100,true}`）；但以下边界缺失，已补测（文件 `internal/models/provider_usage_edge_test.go`，gofmt 干净、PASS）：

1. **① 窗口边界**（新增 `TestAggregateProviderUsage_WindowBoundary`）
   - `created_at` 恰好 = `windowStart` → **计入**；=`windowStart` 前一秒 → **不计入**（验证 `>=` 含下界）。
   - 结果：`openai` token_used=300、call_used=5（仅"窗口内+边界"两行计入，前一秒行被排除）。
2. **③ 多 provider 各自归组**（新增 `TestAggregateProviderUsage_MultiProviderGrouping`）
   - openai（2 行窗口内）、zhipu（1 行窗口内）、anthropic（1 行窗口外）。
   - 结果：openai=300/5、zhipu=300/4，anthropic 不出现 —— GROUP BY 归组正确、无串扰。
3. **② `IsLowBalance` 边界 0.9**：工程师既有 `TestIsLowBalance` 已覆盖（`{90,100,true}` 与 `{900,1000,true}`），无需重复。

---

## 5. 前端核对

| 验收点 | 位置 | 结论 |
| --- | --- | --- |
| 上游列表两列 + colspan 修正 | index.html:107 表头 12 列 / app.js:151 `colspan="12"` | ✅ 一致 |
| 上游模态两 number 输入 | index.html:309 / 314 `prov-monthly-token-limit` / `prov-monthly-call-limit` | ✅ |
| 用户模态剩余提示块 | index.html:420-422 / 490-492 | ✅ |
| 侧栏「上游额度」导航 | index.html:46 | ✅ |
| 仪表盘 tab 区块 | index.html:159-166 | ✅ |
| `formatToken` / `renderUsageCell` / `fetchProviderUsage` | app.js:350 / 363 / 384 | ✅ |
| `loadProviderUsage` / `switchTab` + `__INIT_TAB__` | app.js:410 / 79 / 21 | ✅ |
| 样式类 `usage-low`/`usage-bar`/`usage-fill`/`usage-card` | style.css:426-454 | ✅ |

---

## 6. 受限项

**本次无受限项。** 工程师声称的"线上/本地冒烟"未提供证据，QA 已**实际搭建本地网关（测试 KEK + 临时 SQLite + 临时端口 127.0.0.1:8099）独立完成端到端冒烟**，全部通过：

- 无会话 `GET /admin/api/provider-usage` → **HTTP 401**（确认新路由被 `/admin/` 鉴权中间件正确拦截）。
- `POST /admin/api/login`（admin/admin123）→ `{"message":"Login successful"}`。
- 已登录 `GET /admin/api/provider-usage` → `{"data":[zhipu, openai]}`，字段齐全（含 `token_used`/`token_remaining`/`token_unlimited`/`window_start`/`token_low`/`call_low` 等）。
- 已登录 `GET /admin/api/providers/bad/usage` → **HTTP 404**。
- 已登录 `GET /admin/api/providers/zhipu/usage` → **HTTP 200** + 完整 ProviderUsageView。

> 说明：测试用临时 DB 已删除，未触碰任何生产/既有数据库，未 merge/push/deploy。

---

## 7. 问题清单

**无源码 Bug。** 以下为可选改进建议（非阻塞、非 Bug）：

1. **测试覆盖小缺口（非源码问题）**：`db/migrations_test.go` 的 `TestRunMigrations_AddsExpectedColumns` 未把 `monthly_token_limit` / `monthly_call_limit` 列入断言列。迁移本身正确（实跑 + 冒烟已证），仅测试列举不全。建议补两行断言。
2. **历史数据兼容性提示**：窗口过滤依赖 `call_logs.created_at` 为 RFC3339(+08:00)。当前唯一生产写入路径 `InsertCallLog` 已满足；若库中存在 D10 之前以其它格式写入的历史行，文本比较可能不符预期。新库无此问题。建议后续对存量库做一次 `created_at` 格式归一化（如确有历史数据）。

---

## 8. 总体结论

- **测试通过率**：197/197（100%），0 FAIL；gofmt / go vet / build 全绿。
- **IS_PASS**：**YES —— 源码无 Bug。** 所有 P0-1~P0-5、P1-1 验收标准均经独立代码审查 + 单测 + 边界补测 + 真实会话冒烟验证通过。
- **需工程师修复的问题**：**无。** （仅 2 条可选测试/数据兼容性增强建议，已在上文列出，不阻塞发布。）
- **遗留受限项**：**无。** 含鉴权拦截与 404 在内的端到端行为均已实跑验证。

**结论：功能可独立验收通过，建议进入合并评审。**
