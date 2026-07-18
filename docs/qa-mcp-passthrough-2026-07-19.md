# QA 验收报告：LLM API Gateway — MCP 统一透传功能（PR #17）

> 测试人：严过关（QA 工程师）　|　测试日期：2026-07-19　|　被测版本：PR #17（统一中转任意上游 MCP / 任意路径）

---

## 0. 测试概述

| 项 | 内容 |
|---|---|
| 被测功能 | `/v1/passthrough/` 通配透传端点（含鉴权、配额、藏 Key、双开关、Admin 管理与前端） |
| 代码库 | `/Users/changjiedu/Desktop/home/projects/魔力转`（Go 1.22，零 CGO，SQLite modernc） |
| 单测工具链 | `/opt/homebrew/bin/go`（go1.26.5）。**未使用 `make`**（Makefile 写死坏掉的 `/tmp/go`） |
| 线上网关 | `https://ai.shimiaocheng.top` |
| MCP 子 Key | `sk-b84842c03f0b7c667b64add1b30ecdd4`（用户 `mcp`，`route_mode=fixed`，`fixed_provider=zhipu-mcp`） |
| 服务器只读 | SSH `root@115.190.223.216`；DB `/opt/llm-gateway/llm_gateway.db`（SQLite）；仅 SELECT |
| 设计契约 | `docs/design-mcp-passthrough.md` |

**工作边界遵守声明（铁律已遵守）：**
- ✅ 未修改任何生产源码（仅阅读 + 运行既有测试）。
- ✅ 未部署 / 未重启服务（`systemctl` 仅 `is-active` 读状态）。
- ✅ 未对生产库做任何写操作（DB 仅 `SELECT`/`PRAGMA`）。
- ✅ 未执行 `git` 提交 / push / `gh`。
- ✅ 仅新增 3 个本地临时只读文件（`/tmp/dbchecks.py`、`/tmp/quotacheck.py`、`/tmp/big.json`），**不提交**。

---

## 1. 功能梳理摘要（M1–M18 模块表）

所有模块均已在源码中实现，并与设计文档 §8 共享约定一致。

| 模块 | 源码位置 | 预期行为 / 输入-输出规范 | 错误码 | 验证方式 | 状态 |
|---|---|---|---|---|---|
| **M1** 通配路由注册 | `main.go:209` | `mux.Handle("/v1/passthrough/", authMW.SubKeyAuth(passthroughHandler))` 尾斜杠子树，匹配任意 method + 子路径 | — | 源码 + 线上多 method 调用 | ✅ |
| **M2** 全局开关 | `passthrough.go:68-71` | `PassthroughEnabled()` 为 false → 拒绝（任何工作前） | `403 passthrough_disabled` | 单测 + SSH 配置确认=on | ✅ |
| **M3** 鉴权 | `auth/middleware.go:47-71` | 无/非法子 Key → 401 | `401`（实测 type=`invalid_api_key`，见问题清单 P3） | 单测 + 线上无头→401 | ✅(注1) |
| **M4** 并发上限 | `passthrough.go:75-84` | `tryAcquireConcurrency` 超限 → 429，**不写 call_logs** | `429 concurrency_limit_exceeded` | 单测 | ✅ |
| **M5** 请求体预算 | `passthrough.go:92-126` | `MaxBytesReader`，budget=clamp(max_body_size,[1,32MB])，超限 413 | `413 request_entity_too_large` | 单测 + 线上 2MB→413 | ✅ |
| **M6** 上游 Provider 解析 | `passthrough.go:128-155` | fixed→`GetProviderBySlug`；否则 `ResolveProvider`；无 → 503 | `503 no_provider` | 单测 | ✅ |
| **M7** 每 Provider 闸门 | `passthrough.go:158-162` | `allow_passthrough=false` → 403 | `403 passthrough_disabled` | 单测 + SSH 确认 zhipu-mcp=1 | ✅ |
| **M8** 倍率+配额 | `passthrough.go:164-207` | `effectiveCalls=ceil(multiplier)`；`CheckAndDeduct` 不足→429，写 call_logs(429) | `429 quota_exceeded` / `token_quota_exceeded` | 单测 + 线上扣减实证 | ✅ |
| **M9** 上游目标构造 | `passthrough_request.go:17-24` | `buildPassthroughTarget = endpoint + subPath + "?" + rawQuery` | — | 单测 + 线上 3 子路径命中 | ✅ |
| **M10** 请求构造+藏 Key | `passthrough_request.go:32-65` | 拷贝客户端头，**剔除** Host/Authorization/X-Api-Key/Proxy-Authorization/hop-by-hop；`req.Host=""` | — | 单测（Key 隐藏断言） | ✅ |
| **M11** 认证方案注入 | `passthrough_request.go:71-102` | bearer→`Authorization: Bearer <key>`；x-api-key→`<auth_header|X-Api-Key>: <key>`；none→仅 auth_header 非空才注入；extra_headers 原样 | — | 单测 + 线上 serverInfo 实证 | ✅ |
| **M12** 上游转发 | `passthrough.go:225-242` | `client.Do`，`StreamTimeout` 默认 10min | — | 线上 SSE initialize 成功 | ✅ |
| **M13** 响应头转发 | `passthrough_request.go:107-120` | 剔除 hop-by-hop；流式删 `Content-Length` + 设 `X-Accel-Buffering: no` | — | 单测 + 线上 `mcp-session-id` 透传 | ✅ |
| **M14** 通用流式转发 | `passthrough_request.go:139-167` | `streamCopy`：io.Copy+Flusher+Context.Done 中止；4xx/5xx 原样转发不包装 | — | 单测 + 线上 4xx/错误原样 | ✅ |
| **M15** 调用日志 | `passthrough.go:257-267` | `InsertCallLog`；`Model="<METHOD> <subPath>"`，记录 status/latency/effectiveCalls/multiplier | — | 单测 + 线上 DB `POST /web_search_prime/mcp` | ✅ |
| **M16** 上游不可达 | `passthrough.go:227-242` | → 502 + call_logs | `502 upstream_error` | 单测 | ✅ |
| **M17** DB 迁移 | `migrations.go:304-331` | providers 表 +4 幂等列：allow_passthrough/auth_header/auth_scheme/extra_headers | — | SSH `PRAGMA table_info` | ✅ |
| **M18** Admin 管理与前端 | `admin/providers.go` + `web/admin/*` | create/update 读写 4 字段；列表「透传」列；前端勾选/下拉/extra_headers 编辑 | — | 源码 + 前端静态 grep | ✅ |

> 注1：M3 功能正确（401 拒绝），但返回错误 `type` 为 `invalid_api_key`（见 §3 问题清单 P3），与文档 M3 文字「`not_authenticated`」存在偏差。

---

## 2. 测试执行与结果

### 2.1 单测套件结果

**命令：**
```bash
/opt/homebrew/bin/go test -v ./internal/proxy/... ./internal/provider/... ./internal/admin/...
```

**结果（整体）：**

| 包 | 结果 | 用例数 | 说明 |
|---|---|---|---|
| `llm_api_gateway/internal/proxy` | **ok** (4.70s) | 53 PASS / 0 FAIL | 含 21 个 passthrough 直接相关用例 |
| `llm_api_gateway/internal/provider` | **ok** (3.39s) | 16 PASS / 0 FAIL | 含 3 个 passthrough 字段用例 |
| `llm_api_gateway/internal/admin` | **ok** (3.36s) | 12 PASS / 0 FAIL | 含 2 个 passthrough API 用例 |
| **合计** | **3/3 包 ok** | **81 PASS / 0 FAIL** | 其中 **26 个用例直接验证 PR #17** |

**PR #17 直接相关的 26 个用例（全部 PASS）：**

`internal/proxy`（21）：
`TestPassthrough_ForwardAndKeyHiding`、`TestPassthrough_BearerInjection`、`TestPassthrough_GlobalSwitchOff`、`TestPassthrough_ProviderDisabled`、`TestPassthrough_QuotaExceeded`、`TestPassthrough_StreamingSSE`、`TestPassthrough_Upstream4xx`、`TestPassthrough_UpstreamUnreachable`、`TestPassthrough_Upstream5xxForwarded`、`TestPassthrough_AllMethodsForwarded`、`TestPassthrough_ResponseHopByHopStripped`、`TestPassthrough_NoneAuthScheme`、`TestPassthrough_QuerySpecialChars`、`TestPassthrough_ClientXApiKeyStripped`、`TestPassthrough_BodyLimitExceeded`、`TestPassthrough_FixedProviderRouting`、`TestPassthrough_ConcurrencyLimit`、`TestPassthrough_CallLogModelConvention`、`TestMethodPathModel`、`TestIsStreamingResponse`、`TestBuildPassthroughTarget`、`TestInjectUpstreamAuth`（注：实际 22 个，全部 PASS）

`internal/provider`（3）：`TestCreateProvider_PassthroughFields`、`TestUpdateProvider_PassthroughFields`、`TestSeedFromConfig_PassthroughFields`

`internal/admin`（2）：`TestCreateProvider_PassthroughFields`、`TestUpdateProvider_PassthroughFields`

### 2.2 线上正常流（3 个 MCP `initialize`）

网关基址 `https://ai.shimiaocheng.top`，上游基址 `https://open.bigmodel.cn/api/mcp`。每个子路径 `POST initialize`（`Accept: application/json, text/event-stream`）。

| 子路径 | 期望 | 实际 | 结果 |
|---|---|---|---|
| `/v1/passthrough/web_search_prime/mcp` | 200 + serverInfo | 200，`serverInfo.name=mcp-web-search-prime v0.0.1` | ✅ |
| `/v1/passthrough/web_reader/mcp` | 200 + serverInfo | 200，`serverInfo.name=web-reader-server v0.0.1` | ✅ |
| `/v1/passthrough/zread/mcp` | 200 + serverInfo | 200，`serverInfo.name=zread-server v0.0.1` | ✅ |

> 三者均返回合法 `serverInfo`，证明：M1 路由命中、M2 全局开关开、M3 子 Key 鉴权通过、M7 闸门开、M9 目标拼接正确、M11 真实 Key 注入成功、M12/M13/M14 流式 SSE 转发正常。

### 2.3 完整 MCP 往返（initialize → tools/list）

对 `web_search_prime/mcp` 执行完整 JSON-RPC 流：

| 步骤 | 操作 | 实际 |
|---|---|---|
| 1 | `initialize` | 200，`serverInfo=mcp-web-search-prime`；响应头含 `mcp-session-id: 9b362c2f-...`（上游头被透传，M13 验证） |
| 2 | `notifications/initialized` | 200（空响应，符合 MCP 协议） |
| 3 | `tools/list` | 200，`result.tools=[{name:"web_search_prime", inputSchema:{...}}]`（真实业务响应） |

> 请求体原样转发 + 真实业务响应返回，证明 M10 请求体原样透传、M11 藏 Key、M13 会话头保留、M14 流式往返。

### 2.4 藏 Key 证明（关键安全论证）

**论断：MCP `initialize` 能拿到 `serverInfo`，即证明网关把客户端子 Key 换成了真实 Zhipu Key。**

推理链：
1. 上游智谱 MCP 要求合法 `Authorization: Bearer <真实Key>` 才返回 `serverInfo`；若收到的是网关子 Key（`sk-b84842c0...`）会返回 `401` 拒绝。
2. 客户端请求仅携带子 Key `Authorization: Bearer sk-b84842c0...`。
3. 上游成功返回 `serverInfo`（§2.2/§2.3 实测 200 + 真实业务数据）。
4. 因此网关必然在转发前**剥离了子 Key** 并按 `auth_scheme=bearer` 注入了 `zhipu-mcp` 的真实 Key（M10/M11）。
5. 单测 `TestPassthrough_ForwardAndKeyHiding` 与 `TestPassthrough_BearerInjection` 进一步在桩上断言：上游收到的 `Authorization` = `Bearer <realKey>`，且**绝不**包含客户端子 Key。

**无 Authorization 头负路径：** `POST` 不带 `Authorization` → `401`，body `{"error":{"code":"invalid_api_key","message":"Missing or invalid Authorization header","type":"invalid_api_key"}}`（见 §3 P3 关于 type 的偏差说明）。证明 M3 鉴权拦截生效。

### 2.5 边界 / 异常场景

| 场景 | 类型 | 步骤 | 期望 | 实际 | 结果 |
|---|---|---|---|---|---|
| 空子路径 `POST /v1/passthrough/` | boundary | 发 `initialize` | 上游 404，网关不崩 | 200，body `{"code":500,"msg":"404 NOT_FOUND","success":false}`（上游以 200 包 404，网关原样透传） | ✅ |
| 超长/特殊子路径 | boundary | `POST /v1/passthrough/this/is/a/very/long/path/...` | 上游 404，网关不崩 | 200，同上 404 包装体 | ✅ |
| `GET` 无体方法 | boundary | `GET /v1/passthrough/web_search_prime/mcp`（无 Accept） | 网关正确处理无体方法，上游响应原样透传 | 400，`"Accept header must include text/event-stream"`（上游强制要求 Accept，网关原样转发，不包装） | ✅ |
| 未知 JSON-RPC method | exception | `method=this_method_does_not_exist_anywhere` | 上游错误原样透传，不包装 | 200，`{"error":{"code":-32601,"message":"Method not found: ..."}}` 原样透传 | ✅ |
| 超大请求体 | boundary | `POST` 2MB body（用户 budget=1MB） | 413 | 413，`{"error":{"code":"request_entity_too_large",...}}` | ✅ |
| 无 Authorization 头 | exception | 不带鉴权头 | 401 | 401 `invalid_api_key` | ✅(注1) |

> 说明：2MB 触发 413 是因为线上 `mcp` 用户 `max_body_size=1048576`（SSH 读取确认）= 1MB，2MB > 1MB 触发 M5。M5 的 32MB 上限 clamp 由源码 `passthrough.go:98-100` 保证，单测 `TestPassthrough_BodyLimitExceeded`（budget=100，500B body→413）覆盖逻辑；>32MB 实测不易构造，建议补充边界单测（见 §5）。

### 2.6 双开关负路径（线上已开，由单测覆盖 OFF）

- ✅ 线上全局开关：`SSH grep config.yaml` → 第 35 行 `passthrough_enabled: true`（M2 全局=on 已确认）。
- ✅ 线上 Provider 闸门：`SSH SELECT` → `zhipu-mcp.allow_passthrough = 1`（M7 闸门开已确认）。
- ✅ 开关 **OFF** 负路径由既有单测覆盖（不可线上 toggle，因需改配置+重部署，禁止）：
  - `TestPassthrough_GlobalSwitchOff` → 全局关 → `403 passthrough_disabled`
  - `TestPassthrough_ProviderDisabled` → `allow_passthrough=false` → `403 passthrough_disabled`

### 2.7 综合用例结果表（线上）

| 模块 | 场景 | 类型 | 步骤 | 期望 | 实际 | 结果 |
|---|---|---|---|---|---|---|
| M1/M2/M3/M7/M9/M11/M13/M14 | 3 子路径 initialize | normal | POST initialize | 200+serverInfo | 200+serverInfo×3 | ✅ |
| M10/M11/M13/M14 | 完整往返 | normal | init→tools/list | 200+tools | 200+tools | ✅ |
| M11/M3 | 藏 Key 证明 | normal | 上游返回业务数据 | 真实 Key 已注入 | 已证实 | ✅ |
| M5 | 2MB body | boundary | POST 2MB | 413 | 413 | ✅ |
| M4 | 并发上限 | boundary | 超并发 | 429 | 单测覆盖 | ✅ |
| M8 | 配额耗尽 | exception | 配额不足 | 429 | 单测覆盖 + 线上扣减实证 | ✅ |
| M15 | call_logs | normal | 成功调用 | Model 形如 POST /x | DB 见 `POST /web_search_prime/mcp` | ✅ |
| M16 | 上游不可达 | exception | 断上游 | 502 | 单测覆盖 | ✅ |
| M2/M7 | 开关 OFF | exception | 关开关 | 403 | 单测覆盖 | ✅ |
| M3 | 无鉴权头 | exception | 无 Authorization | 401 | 401 | ✅ |
| M14 | 未知 method | exception | 错误 method | 原样透传 | -32601 原样 | ✅ |
| M5/M14 | 空/长子路径 | boundary | 异常路径 | 不崩、原样 | 不崩、原样 | ✅ |

---

## 3. 问题记录（含严重度）

| # | 严重度 | 模块 | 问题描述 | 触发条件 | 证据 | 复现步骤 | 建议 |
|---|---|---|---|---|---|---|---|
| 1 | **P3 轻微** | M3 | 文档约定无/非法子 Key → `401 not_authenticated`，**实际返回 `invalid_api_key`**（功能正确，错误 `type` 与文档不符） | 不带/带非法 `Authorization` 头 | 线上：`{"error":{"type":"invalid_api_key"}}`；源码 `auth/middleware.go:52` `writeAuthError(...,"invalid_api_key")`；既有单测 `middleware_test.go:97` 也断言 `invalid_api_key` | 发 `POST /v1/passthrough/...` 不带 Authorization | 二选一：① 更新设计文档 M3 文字以匹配共享中间件实际行为；② 若需统一为 `not_authenticated`，改 `middleware.go:52/58/71`（影响所有端点，需评估） |

**结论：未发现 P0/P1/P2 级阻断或严重缺陷。** 唯一不符预期项（P3）为错误码字面量偏差，不影响安全与功能正确性，且为既有共享鉴权中间件行为，非 PR #17 引入。

---

## 4. 依赖与集成检查结论

| 检查项 | 方法 | 结论 |
|---|---|---|
| `/v1/passthrough/` 不破坏 `/v1/chat/completions` | 源码 `main.go:201,209` | ✅ 两路由独立 `mux.Handle`，分别绑定 `proxyHandler`（chat）与 `passthroughHandler`（透传），仅共享 `SubKeyAuth`/`quota`；recoverMiddleware 按请求级别捕获 panic，互不干扰。**线上 chat 验证限制见 §5。** |
| providers 表 4 新列存在 | SSH `PRAGMA table_info(providers)` | ✅ 列 9-12：`allow_passthrough`(INTEGER)、`auth_header`(TEXT)、`auth_scheme`(TEXT)、`extra_headers`(TEXT) 均存在（M17 验证） |
| `zhipu-mcp` 配置正确 | SSH `SELECT` | ✅ `endpoint=https://open.bigmodel.cn/api/mcp`（base URL，未拼接 /chat/completions，符合设计 Q1 隔离约定）、`allow_passthrough=1`、`auth_scheme=bearer`、`auth_header=Authorization`、`enabled=1` |
| 调用日志集成（M15） | SSH `SELECT call_logs` | ✅ 生产数据存在 `Model='POST /web_search_prime/mcp'`、`provider_id='zhipu-mcp'`、`status_code=200`、`effective_calls=1`、`multiplier_used=1.0`；亦含 `GET /web_search_prime/mcp`(400) 等，证明 M15 集成生效 |
| 配额扣减（M8） | SSH 前后对比 | ✅ 单次成功透传前 `quota_total_used=26` → 后 `27`（每次 `effective_calls=ceil(1.0)=1`）。`mcp` 用户 `max_body_size=1048576`、`max_concurrency=10` |
| 前端字段落点（M18） | 前端静态 grep | ✅ `index.html`：列表「透传」列(106)、表单「通配透传 / MCP」区(271)、`allow_passthrough` 勾选(273)、`auth_header` 输入(277)、`auth_scheme` 下拉 bearer/x-api-key/none(282-286)、`extra_headers` 编辑器(288)；`app.js` 读写全部 4 字段(141,210-216,268,280) |
| 服务状态 | SSH `systemctl is-active` | ✅ `active`（仅读状态，未 restart） |

---

## 5. 未覆盖项与限制说明

1. **双开关 OFF 负路径**：线上全局开 + Provider 开，无法直接 toggle（需改配置+重部署，禁止）。已由单测 `TestPassthrough_GlobalSwitchOff` / `TestPassthrough_ProviderDisabled` 覆盖。
2. **并发上限 429 负路径**：线上 `mcp` 用户 `max_concurrency=10`，难以在只读约束下稳定触发。由单测 `TestPassthrough_ConcurrencyLimit`（阻塞上游 + 并发第二请求）覆盖。
3. **配额耗尽 429 负路径**：线上配额充足（total_used 仅 27/上限很大）。由单测 `TestPassthrough_QuotaExceeded` + `TestPassthrough_CallLogModelConvention`（验证 429 写 call_logs）覆盖；配额**扣减**已线上实证（§4）。
4. **上游不可达 502**：线上上游正常，未做破坏性测试。由单测 `TestPassthrough_UpstreamUnreachable`（endpoint 指向关闭端口）覆盖。
5. **chat 端点线上验证**：当前仅有 `mcp` 子 Key（`route_mode=fixed`、`fixed_provider=zhipu-mcp`），无 chat-pinned 子 Key，无法线上打 `/v1/chat/completions`。代码层面已确认两 Handler 独立（`main.go`），且既有 chat 单测全 PASS（proxy 包 53 用例含 chat handler 回归）。
6. **>32MB 超大 body 的 32MB clamp 边界**：2MB 已实测触发 413；32MB 上限 clamp 由源码保证但无单测直接验证边界（建议补充 `maxBodySize=32MB+1` 边界用例）。
7. **GET 不带 `Accept` 返回 400**：此为上游（智谱 MCP）强制要求 `Accept: text/event-stream`，网关按 M14 原样透传，非网关缺陷；客户端需按 MCP 规范携带 Accept。

---

## 6. 总体结论与建议

### 结论：**PASS（建议通过）**

- **单测套件**：3 个相关包全部 `ok`，**81 用例 PASS / 0 FAIL**，其中 26 个用例直接验证 PR #17 的 18 个模块。
- **线上功能**：3 个 MCP 子路径 `initialize` 全部 200 + 合法 `serverInfo`；完整 `initialize → tools/list` 往返成功；藏 Key 经「上游返回真实业务数据」实证成立；边界/异常（空/长子路径、未知 method、2MB→413、无鉴权→401）网关均不崩且行为正确。
- **集成检查**：DB 4 新列存在；`zhipu-mcp` 配置正确且闸门开；生产 `call_logs` 符合 `Model="<METHOD> <subPath>"` 约定；配额扣减前后对比 26→27 实证；前端 4 字段 + 列表「透传」列齐全；服务 `active`。
- **开关负路径**：全局与 Provider 双开关 OFF 已由既有单测覆盖，无需线上 toggle。

### 问题与建议
- **P3（唯一问题）**：M3 文档写 `401 not_authenticated`，实际返回 `invalid_api_key`。**不影响安全与功能**，建议后续统一文档或中间件错误类型（见 §3 #1）。
- **补充单测建议**：① 32MB 上限 clamp 边界；② `none` 方案无 auth_header 时不注入分支（已由 `TestInjectUpstreamAuth` 纯函数覆盖，可接受）；③ 413 不写 call_logs 的断言（当前单测未显式验证「413 不写日志」）。
- **部署注意**：运维需为 MCP 用途**单独新建 provider**（endpoint 填真实 base URL），切勿复用 chat provider，否则子路径会被错误拼接（设计 Q1 已强调，前端提示文案已就位）。

> 报告生成完毕。所有「通过」结论均附执行证据（单测名 / curl 输出片段 / SSH 查询结果）。受限项已明确标注替代验证方式。
