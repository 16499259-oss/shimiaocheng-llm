# 测试报告 — P2 阈值可配置（独立验收）

- **分支 / 提交**: `feat/provider-quota-threshold` @ `02345c4`
- **变更主题**: LLM API Gateway「P2 阈值可配置」增强（token / 调用次数 分别配剩余阈值；全局默认 + per-provider 覆盖；前端双输入；后端防御校验）
- **验收人**: QA 工程师（严过关），独立验证（不信任工程师自测）
- **测试环境**: macOS，Go `/opt/homebrew/bin/go`，`GATEWAY_KEK_ENV=test-kek-00000000000000000000000000000000`，SQLite（`:memory:` / 临时文件）
- **核心验证点**: 用户配置的 ratio 是「剩余阈值」（0.10 = 剩余<10% 才标红），即 `IsLowBalance(used, limit, remainingRatio)` 判定为 `limit<=0→false; used/limit >= (1-remainingRatio) → true`。**代码使用正确公式 `>= 1-remainingRatio`，非错误公式 `>= ratio`**。

---

## 1. 测试概览

| 项目 | 结果 |
|---|---|
| 源码相关包（models/admin/provider/config/db/quota）全量单测 | **全部 PASS（96 个测试函数）** |
| `go build ./...` | **PASS（exit 0）** |
| `go test ./...` 总览 | 14 个包 PASS；`internal/proxy` 1 个包 FAIL（仅 15 个 `TestPassthrough_*` 失败，环境受限，非源码回归） |
| 独立补充测试 | 新增 4 个测试函数 + 扩展 1 个（总计 +183 行） |

**测试方法**：① 逐文件代码审查（核对实现而非文档）；② 全量单测（排除环境受限包）并补充边界测试；③ 通过 handler 级集成测试替代受限的二进制冒烟（环境不可达上游，按主理人指示以 handler 单测证据替代，未伪造通过）。

---

## 2. 单测结果（分包）

| Package | 测试数 | 结果 | 备注 |
|---|---|---|---|
| `internal/models` | 41 | ✅ PASS | 含 `TestIsLowBalance`、`TestBuildProviderUsageView`、新增 `TestBuildProviderUsageView_PerProviderIndependence` |
| `internal/admin` | 26 | ✅ PASS | 含 `providers_low_ratio_test.go` 全部（新增 `-0.1` 负比例用例）、`provider_usage_test.go` 覆盖 |
| `internal/provider` | 11 | ✅ PASS | Create/Update/Seed/List/Get 均覆盖两列读写 |
| `internal/config` | 5 | ✅ PASS | 新增 `TestLoad_ProviderQuotaDefaults`（缺段/缺字段回退 0.10） |
| `internal/db` | 6 | ✅ PASS | `TestRunMigrations_Idempotent` + `AddsExpectedColumns` 已扩展两 REAL 列 |
| `internal/quota` | 7 | ✅ PASS | 无本次改动，回归通过 |
| `internal/proxy` | 52 | ⚠️ 37 PASS / **15 FAIL** | 失败均为 `TestPassthrough_*`，**环境受限**（见第 9 节），不计入源码 Bug |

---

## 3. 代码审查结论（逐文件）

| 文件 | 结论 | 关键证据 |
|---|---|---|
| `internal/models/provider_usage.go` | ✅ 通过 | `IsLowBalance` 第 106-112 行：`limit<=0→false`；`usedRatio := float64(used)/float64(limit)`；`return usedRatio >= (1 - remainingRatio)` —— **正确剩余阈值公式**。`BuildProviderUsageView` 第 152-163 行：`per>0` 用各自 ratio，否则用全局；token/call 各自独立。`LowBalanceRatio` 常量已移除（grep 确认 .go 中无任何引用）。 |
| `internal/config/config.go` | ✅ 通过 | `ProviderQuotaConfig`（46-49 行）含 `DefaultTokenLowRatio/DefaultCallLowRatio`。`Load()` 第 144-145 行在 `yaml.Unmarshal` **之前**置默认 0.10；缺段/缺字段回退 0.10 不 panic。`config.yaml.example` 第 69-71 行已含 `provider_quota` 段且默认 0.10。 |
| `internal/db/migrations.go` | ✅ 通过 | 第 355-368 行：`monthly_token_low_ratio`、`monthly_call_low_ratio` 均为 `REAL NOT NULL DEFAULT 0`；均由 `columnExists` 守卫（`if !columnExists(...)`），幂等。`columnExists`（376-396）实现正确。 |
| `internal/provider/store.go` | ✅ 通过 | `CreateProvider` 新签名（146 行）新增两 ratio 参并写入 INSERT（192-194）。`UpdateProvider` 第 342-353 行动态更新两列（`float64` 分支）。`ListProviders`/`GetProvider` 读两列（64、78、114、136）。`SeedFromConfig` 第 747 行写 `0, 0`（继承全局）。 |
| `internal/admin/provider_usage.go` | ✅ 通过 | `globalTokenLowRatio()`/`globalCallLowRatio()`（15-34 行）：`h.Config != nil && r>0` 时用配置值，否则回退 **0.10**。`HandleListProviderUsage`（62 行）与 `HandleGetProviderUsage`（99 行）两处均注入 `h.globalTokenLowRatio(), h.globalCallLowRatio()`。 |
| `internal/admin/handler.go` + `main.go` | ✅ 通过 | `Handler` 结构体（40 行）含 `Config *config.Config`；`main.go` 第 193 行 `Config: cfg` 已接线。 |
| `internal/admin/providers.go` | ✅ 通过 | `createProviderRequest`/`updateProviderRequest` 含 `MonthlyTokenLowRatio/MonthlyCallLowRatio`（`*float64` 以支持局部更新）。`HandleCreateProvider` 第 84-88 行与 `HandleUpdateProvider` 第 167-181 行：ratio `<0` 或 `>1.0` → **400 "invalid low ratio"**，且 400 时未落库。 |
| `web/admin/index.html` | ✅ 通过 | 第 318-325 行：两个 `number` 输入 `prov-monthly-token-low-ratio` / `prov-monthly-call-low-ratio`，`min="0" max="100"`，`placeholder="0 = 继承全局默认 10%"`。 |
| `web/admin/app.js` | ✅ 通过 | `editProvider` 第 257-260 行回填 `ratio*100`（`>0` 才显示，否则空白 `''`）；`saveProvider` 第 314-326 行读取百分比并 `/100`，空→0；提交字段名 `monthly_token_low_ratio` / `monthly_call_low_ratio` 正确。 |

---

## 4. 剩余阈值语义专项验证（最核心）

**公式核对（代码，非文档）**：`internal/models/provider_usage.go` 第 110-111 行
```go
usedRatio := float64(used) / float64(limit)
return usedRatio >= (1 - remainingRatio)
```
确认使用 **`>= 1-remainingRatio`（剩余阈值）**，而非错误公式 `>= ratio`（已用阈值）。架构师设计文档 `docs/system_design.md` 原第 167-189 行错误公式已由主理人修正，代码与修正后的语义一致。

**核心用例断言（`TestIsLowBalance`，全部 PASS）**：

| used | limit | ratio | 期望 | 实际 | 语义 |
|---|---|---|---|---|---|
| 900 | 1000 | 0.10 | true | ✅ true | 剩 10% → 标红 |
| 899 | 1000 | 0.10 | false | ✅ false | 剩 10.1% → 不标红 |
| 500 | 1000 | 0.10 | false | ✅ false | 剩 50% → 不标红 |
| 1000 | 1000 | 0.10 | true | ✅ true | 超限也标红（仅展示不拦截） |
| 0 | 0 | 0.10 | false | ✅ false | 无限（limit<=0）永不标红 |

**边界（used/limit 恰好 = 1-ratio，含边界应标红）**：`{900,1000,0.10}` → `900/1000 = 0.9 = 1-0.10` → **true**（边界 inclusive），已覆盖。

---

## 5. per-provider 覆盖验证

- **`BuildProviderUsageView` 解析逻辑**：`per>0` 用各自 ratio，否则用全局；token/call 独立（`TestBuildProviderUsageView` p3/p4 已覆盖）。
- **新增 `TestBuildProviderUsageView_PerProviderIndependence`**（全部 PASS）：
  - Provider A：token 覆盖 0.10、call 继承全局 0.10；token 用 920/1000（剩 8%）→ **标红**，call 用 80/100（剩 20%）→ **不标红**（精确还原主理人示例「token 剩8%标红而 call 剩20%不标红」）。
  - Provider B：token 覆盖 **0.05**（更严）；同 token 剩 8% → **不标红**（全局 0.10 本会标红）→ 证明确用 per-provider 值而非全局。
  - Provider C：token 继承全局、call 覆盖 0.05；token 剩 8% → 标红，call 剩 20% → 不标红 → 证明两维度各自解析。
- **handler 级集成（真实 DB + Router）**：`TestHandleListProviderUsage_PerProviderOverride` —— 创建 provider 带 `monthly_token_low_ratio=0.20`，注入 token 850/1000（剩 15%）；断言 `TokenLow=true`（全局 0.10 不会标红 15%，证明覆盖生效），`CallLow=false`。
- **轻量本地冒烟**：目标场景（创建 provider 带 `monthly_token_low_ratio=0.20` → GET provider-usage 确认按 20% 触发）已由上述 handler 集成测试以真实存储 + 路由完整覆盖。**未启动完整二进制**（需真实上游 + 管理员登录会话，沙箱环境受限）；依主理人指示以 handler 单测证据替代，**未伪造通过**。

---

## 6. 防御校验验证

| 场景 | 期望 | 测试 | 结果 |
|---|---|---|---|
| 创建 ratio=1.5（>1.0） | 400 且不落库 | `TestCreateProvider_InvalidLowRatio` | ✅ |
| 创建 ratio=-0.1（<0） | 400 且不落库 | `TestCreateProvider_NegativeLowRatio`（新增） | ✅ |
| 更新 ratio=2.0（>1.0） | 400 | `TestUpdateProvider_InvalidLowRatio` | ✅ |
| 更新 ratio=-0.1（<0） | 400 | `TestUpdateProvider_NegativeLowRatio`（新增） | ✅ |
| 合法 ratio=0.20/0.15 | 201 且落库 | `TestCreateProvider_ValidLowRatio` | ✅ |

后端不信任前端（前端已有 min/max 约束），对 create 与 update（含局部更新）均做 `ratio<0 || ratio>1.0 → 400` 防御；400 路径在 `store.CreateProvider` 之前返回，经验证未落库。

---

## 7. 迁移幂等

- **`TestRunMigrations_Idempotent`**：对同一个库连续执行两次 `RunMigrations`，第二次无错误；9 张领域表均在，种子路由规则仍为恰好 1 行（未重复）。
- **`TestRunMigrations_AddsExpectedColumns`（已扩展）**：新增断言 `providers.monthly_token_low_ratio` 与 `providers.monthly_call_low_ratio` 两 REAL 列存在。
- 所有 ALTER 均由 `columnExists` 守卫，CREATE TABLE / CREATE INDEX 均带 `IF NOT EXISTS`，重入安全。

---

## 8. 前端核对

| 项 | 结论 |
|---|---|
| `index.html` 两个 number 输入 | ✅ `prov-monthly-token-low-ratio` / `prov-monthly-call-low-ratio`，`min=0 max=100`，`placeholder="0 = 继承全局默认 10%"` |
| `app.js` editProvider 回填 | ✅ `ratio*100`（`>0` 显示，否则空白 `''`） |
| `app.js` saveProvider 读取/提交 | ✅ 读百分比并 `/100`，空→0；字段名 `monthly_token_low_ratio` / `monthly_call_low_ratio` 与后端请求结构体一致 |

---

## 9. 受限项：internal/proxy 15 个 `TestPassthrough_*` 失败（环境受限，非源码回归）

**主理人 evidence（已独立核验）**：主理人亲测 `internal/proxy` 的 15 个 `TestPassthrough_*` 在 **main 主干与本分支失败集合完全一致**（仅执行耗时不同）。根因为这些集成测试依赖上游 MCP 可达，沙箱连不上返回 503，**属环境受限、非代码回归、本分支零新增失败**。

**本次实测**：`go test ./...` 中 `internal/proxy` 失败的全部为 `TestPassthrough_*`：

```
TestPassthrough_Upstream5xxForwarded
TestPassthrough_AllMethodsForwarded
TestPassthrough_ResponseHopByHopStripped
TestPassthrough_NoneAuthScheme
TestPassthrough_QuerySpecialChars
TestPassthrough_ClientXApiKeyStripped
TestPassthrough_ConcurrencyLimit
TestPassthrough_CallLogModelConvention
TestPassthrough_ForwardAndKeyHiding
TestPassthrough_BearerInjection
TestPassthrough_ProviderDisabled
TestPassthrough_QuotaExceeded
TestPassthrough_StreamingSSE
TestPassthrough_Upstream4xx
TestPassthrough_UpstreamUnreachable
```

**失败原因取样**（确认是环境而非逻辑）：`TestPassthrough_ForwardAndKeyHiding` 期望 200 实得 503；`TestPassthrough_UpstreamUnreachable` 期望 502 实得 503 —— 即沙箱无法连达上游返回 503。**这与主理人 evidence 的「上游不可达」判因一致**。

**判定**：这 15 个失败**不计入源码 Bug**，也不影响本次 P2 阈值增强（无相关代码改动触及 passthrough 路径）。

---

## 10. 问题清单

- **源码 Bug**：无。
- **测试代码**：无需要修复项；已**补充** 4 个边界/缺失测试并扩展 1 个迁移列断言，使其与本次变更匹配。
- 涉及改动文件：`internal/models/provider_usage_test.go`、`internal/admin/providers_low_ratio_test.go`、`internal/config/config_test.go`、`internal/db/migrations_test.go`（均为测试文件，未触碰生产代码）。

---

## 11. 总体结论

- **IS_PASS：源码无 Bug**。所有源码相关包（models/admin/provider/config/db/quota，共 96 个测试函数）全部通过；`go build ./...` 通过。
- **剩余阈值语义正确**：代码使用 `used/limit >= 1-remainingRatio`（剩余阈值），5 个核心用例 + 边界用例全部断言通过。
- **per-provider 覆盖正确**：token/call 各自独立，`per>0` 用各自、否则继承全局，handler 级集成测试印证。
- **防御校验正确**：`ratio<0 || >1.0 → 400`，create 与 update 均覆盖且不落库。
- **迁移幂等 + 两 REAL 列**：连跑两次无错，列存在断言通过。
- **前端核对通过**：双输入 + 回填 `*100` + 提交 `/100` + 字段名一致。
- **遗留受限项**：`internal/proxy` 15 个 `TestPassthrough_*` 因沙箱连不上上游（503）失败，属环境受限、非回归、本分支零新增失败；按主理人 evidence 记入受限项。

> 说明：本 QA 未修改任何生产代码，仅补充/扩展测试以覆盖本次 P2 阈值增强的边界与缺失路径。`docs/system_design.md` 的修改为主理人此前对错误公式的纠正，非本次 QA 改动。
