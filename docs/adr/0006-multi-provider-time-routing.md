# ADR-0006: 多上游按时间段自动路由

- 状态（Status）：Accepted（已落地）
- 日期：2026-07-11
- 决策人：高见远（架构师）/ 主理人齐活林

## 背景（Context）

网关原本只对接单一上游（智谱 GLM），所有 `/v1/chat/completions` 请求都转发到
固定的 Zhipu endpoint。随着业务需要（成本优化、容量兜底、灰度新模型），我们希望在
**业务高峰期（如 14:00–18:00 UTC+8）自动把流量切到另一个上游（如 OpenAI）**，其余时间
仍走默认上游 Zhipu，且这种切换对下游用户透明——用户始终用同一个 Base URL 和同一个
external 模型名（如 `glm-5.2`）调用，由网关在内部完成 provider 选择与模型名重写。

现有约束与痛点：

- 上游 Key 必须**仅内存态 + 环境变量注入**（ADR-0002），新增的 B 上游 Key 同样不能落盘。
- 时段倍率引擎（`internal/quota/multiplier.go`）原本用本地时区（`time.Now()` 的本地表示）
  比较时段窗口，存在隐蔽的时区 bug（跨时区部署时倍率窗口错位）。路由也必须统一时区。
- `/v1/models` 必须保持**单一 external 模型列表、不随窗口变化**（AGENTS.md §6 不变量）。
- 路由规则应是**配置化 / 数据化**的（N 个 provider、按天+时间段匹配），而非硬编码。

## 决策（Decision）

1. **时区单例（`internal/timeutil`）**：新增 `timeutil.ShanghaiTZ`（`time.LoadLocation("Asia/Shanghai")`，
   失败回退 `FixedZone("Asia/Shanghai", 8*3600)`），并提供 `IsInRange(start,end,now)`（左闭右开
   `[start,end)`、支持跨夜区间）与 `MatchDay(day, weekMask)`。必须 `import _ "time/tzdata"`
   把 IANA 时区库嵌入二进制，保证离线也能解析 `Asia/Shanghai`。全项目时段/倍率判定统一
   `.In(timeutil.ShanghaiTZ)`，**禁止裸 `time.Now()` 做窗口/倍率比较**。

2. **配置层（N Provider 配置化）**：`internal/config` 新增
   - `ProviderConfig{ID, Endpoint, APIKeyEnv, IsDefault}`，`Config.Providers []ProviderConfig`；
   - `ModelMapping{External, PerProvider}`，`Config.ModelMappings []ModelMapping`。
   `Load` 在 `Providers` 为空时默认注入单个 Zhipu provider（endpoint 取 `api.zhipu_endpoint`）。
   每个 provider 的 Key 由 `APIKeyEnv` 指定的环境变量在启动时注入内存。

3. **DB 规则表（`provider_routing_rules`）**：新增表，字段对齐 `time_multipliers`
   （`id/provider_id/start_time/end_time/days_of_week/timezone/enabled/default_provider_id`）。
   默认植入一条 `14:00–18:01 → openai`（仅当表空时）。`default_provider_id` 列仅作 schema 预留，
   全局默认 provider 取 `config.Providers[IsDefault]`（按规则指定回退留 P2）。

4. **路由组件（`internal/router/selector.go`）**：
   - `CredentialHolder`（RWMutex + key，`Get`/`Set`）、`CredentialStore`（map[providerID]*Holder，
     `Get`/`Set`/`Holder`）。**同一 provider 的 Holder 实例全局共享**，admin 热更新与路由读取同源。
   - `Router.ResolveProvider(now)`：查 `provider_routing_rules`（enabled=1），按 `timeutil.IsInRange`
     + `MatchDay` 判定；**窗口命中即返回该 provider，绝不回退默认 A**；全未命中返回 config 默认
     provider；表空/DB 错 → 回退默认、绝不 panic；全无 provider 配置才 error。
   - `Router.RewriteModel(external, providerID)`：查 `mappings[external][providerID]`，缺失则
     **passthrough 原 external 名（不报错）**。
   - B 上游 Key（`OPENAI_API_KEY`）仅在启动时从环境变量注入内存，**绝不落盘、绝不打印明文**（沿用
     ADR-0002）。

5. **Handler 接入**：`proxy.Handler` 增加 `Router` 字段。`ServeHTTP` 解析 provider 一次 →
   用 `Router.RewriteModel` 重写 body 的 `model` → 把 endpoint/key/providerID 透传给
   `handleSync`/`handleStream`。`Router == nil` 时走旧的 `APIKeyGetter`/`EndpointGetter` 闭包兜底，
   保证可灰度。`call_logs` 增加 `provider_id` 列（默认 `'zhipu'`，窗口内记录 `'openai'`）。

6. **倍率引擎时区统一**：`internal/quota/multiplier.go` 删除本地 `isInTimeRange`/`matchDay`，
   改用 `timeutil.*`；`GetEffectiveMultiplier` 开头 `now = now.In(timeutil.ShanghaiTZ)`（修本地时区
   bug）。倍率行为（多规则取 MAX、1× 基线）保持不变，仅做时区修正。

7. **`/v1/models` 保持不变**：仍返回单一 external 模型列表，不随窗口变化；`model_mappings` 仅用于
   请求内的模型名重写，不影响模型发现端点。

## 后果（Consequences）

**正面：**

- 多上游按时间段自动路由，对下游用户完全透明（同一 Base URL + external 模型名）。
- 路由规则数据化在 `provider_routing_rules` 表，可随运营需要增删，无需改代码。
- 时区统一锁定 Asia/Shanghai，路由与倍率窗口不再受部署主机本地时区影响，消除隐蔽 bug。
- 密钥管理铁律延续：B 上游 Key 仅内存态 + 环境变量，不落盘、不打印。
- 模型名重写缺失即 passthrough，向后兼容历史 external 模型名。
- 灰度安全：`Router == nil` 走旧兜底路径，可逐步上线。

**负面 / 取舍：**

- 窗口内命中 B 后**绝不回退 A**：若 B 故障，请求返回 502，不会静默重试 A。这是有意为之的
  "严格路由"语义（避免倍率/成本口径被绕过），运维需保证 B 的可用性（或临时禁用该规则行）。
- `default_provider_id` 列本期未启用（全局默认取 config），按规则指定回退留 P2。
- `external` 唯一键本期未强制校验（重复 external 取首个匹配），留 P2。
- B endpoint 默认值（`https://api.openai.com/v1/chat/completions`）写在 `config.yaml.example`，
  未硬编码到代码逻辑外。
- admin 后台仍只管理 Zhipu Key；B Key 仅由 systemd 环境变量注入，无后台改 Key 入口（符合铁律）。

**后续注意事项：**

- 禁止重新引入裸本地时区比较（见 AGENTS.md §6 不变量）。
- 禁止在窗口命中 B 后回退 A（502 即终态）。
- B 凭据绝不落盘/打印（见 AGENTS.md §5 铁律）。
- 路由相关改动需对照本文与 AGENTS.md §3/§5/§6 自查。
