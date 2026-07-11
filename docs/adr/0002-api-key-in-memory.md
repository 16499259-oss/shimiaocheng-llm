# ADR-0002: 上游 API Key 仅内存态 + 环境变量持久化（绝不落盘明文）

- 状态（Status）：Accepted（已落地）
- 日期：2026-07-11
- 决策人：高见远（架构师）/ 主理人齐活林

## 背景（Context）

网关需要持有上游智谱 GLM 的 API Key（`ZHIPU_API_KEY`），用于在转发 `/v1/chat/completions` 请求时注入 `Authorization: Bearer <real_key>`。早期版本在 admin 后台「修改上游 Key」时，会把明文 Key 回写到 `config.yaml`（即便该文件权限为 `0600`）。这留下严重的落盘明文风险：

- 明文 Key 存在于磁盘配置文件，一旦服务器被入侵或备份泄露即直接失陷；
- `config.yaml` 虽被 `.gitignore` 忽略，但运维误操作、日志、镜像打包仍可能把明文带出；
- 历史事故已发生：早期 `persistToYAML` 会把明文 key 写回 `config.yaml`，在 `security-hardening` 分支修复。

同时，admin 需要支持「运行时热更新 Key」而不重启服务。因此必须在不写盘明文的前提下，既支持内存热更新，又能在重启后恢复 Key。

## 决策（Decision）

- **内存态为唯一可信源**：admin 后台修改上游 Key 时，只调用 `apiKeyHolder.Set(...)` 更新内存态，**绝不回写明文到 `config.yaml` 或任何磁盘文件**（含 yaml / db / 日志）。
- **持久化靠 systemd 环境变量**：上游 Key 的持久化载体是 systemd 单元 `Environment=ZHIPU_API_KEY=...`（`/etc/systemd/system/llm-gateway.service`）。服务启动后由 `internal/config` 通过 `os.Getenv("ZHIPU_API_KEY")` 读取并覆盖 yaml 中的值（`config.go` 中 `if envKey := os.Getenv("ZHIPU_API_KEY"); envKey != "" { cfg.API.ZhipuAPIKey = envKey }`）。
- **`config.yaml` 不写明文 Key**：配置文件中不再保存上游明文 Key（仅保留 `endpoint` 等）；且 `config.yaml` 本身已被 `.gitignore` 忽略，仅 `config.yaml.example` 入库。
- 运维更换 Key 的标准动作：改 systemd 单元的 `Environment=`，`systemctl daemon-reload && systemctl restart llm-gateway`，而非编辑配置文件。

## 后果（Consequences）

**正面：**

- 磁盘上没有任何明文上游密钥，显著降低密钥泄露面（入侵者即便拿到服务器文件也拿不到上游 Key）。
- 满足「密钥管理铁律」（见 `AGENTS.md` 第 5 节）：明文 Key 不落盘、仅一次性在创建/重新生成时返回前端（子 Key 用 SHA256+salt 哈希存储）。
- 内存热更新 Key 不中断服务，配合环境变量可在重启后自动恢复，运维路径清晰。

**负面 / 取舍：**

- 重启服务必须依赖环境变量注入；若 systemd 单元未配置 `Environment=`，服务将丢失上游 Key（或回退到 yaml 中的空值），需在部署清单中显式保证。
- Key 不再「可见」于配置文件，排查问题时要到 systemd 单元而非 yaml 去找，对运维的约定要求更高。
- 明文 Key 仍会在某次 `Set` 调用后存在于进程内存中（无法避免），但不落盘已消除最主要的泄露途径。

**后续注意事项：**

- 禁止重新引入任何把明文 Key 写回磁盘的逻辑（见 `AGENTS.md` 第 7 节禁区）。
- 日志、错误返回、panic 堆栈中不得打印上游明文 Key（上游请求构造见 `internal/proxy/upstream.go`，注释已强调 never logged）。
- 更换上游 Key 的 SOP 文档应指向 systemd 单元修改，而非 `config.yaml`。
