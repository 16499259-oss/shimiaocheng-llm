# AGENTS.md — LLM API Gateway

项目级 AI 协作手册。**任何 AI agent（包括本项目的 multi-agent SOP 团队）开工前必读。**
目的是让纯 AI 驱动的开发保持上下文连贯、不重复踩坑、改动可回归。

---

## 1. 项目简介

LLM API Gateway：对接上游**智谱 GLM（Coding Plan endpoint）**，向下游多用户发放**子 Key**，
做配额代理 + 时段倍率 + Admin 管理后台 + 用户自助面板。单二进制部署（Go + SQLite），
外层 nginx 反代。

- 线上域名：`https://ai.shimiaocheng.top`
- 隐蔽管理路径：`/m-7xa2/`（→ 内部 `/admin/`）
- 用户面板：`/user/`
- 部署服务器：`115.190.223.216`（root）

---

## 2. 技术栈

- **Go 1.22**，**零 CGO**（`modernc.org/sqlite` 纯 Go SQLite 驱动，driver name `"sqlite"`）
- SQLite 单文件数据库（`llm_gateway.db`）
- 前端：原生 HTML/JS，**嵌入二进制**（`web/admin`、`web/user`，经 `embed.go` 的 `//go:embed`）
- 反向代理：nginx（隐蔽管理路径 + TLS + 限速）
- 部署：systemd 服务 + nginx 反代
- 跨平台编译：本地无 go 时用 `/tmp/go/bin/go`（或 `go install` 的受管版本），
  `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`

---

## 3. 目录结构

| 路径 | 职责 |
|------|------|
| `main.go` | 入口；`router.CredentialStore`/`Router` 构建与接线、路由注册 |
| `internal/auth` | 子 Key 认证、session、middleware、keygen |
| `internal/proxy` | `/v1/chat/completions` 代理、SSE 透传、content 归一化、模型名重写、`/v1/models` |
| `internal/admin` | 管理后台 handler（用户/倍率/设置/登录） |
| `internal/models` | 数据模型与 DB 访问（users、quotas、call_logs、multipliers、session） |
| `internal/quota` | 配额检查器 + 时段倍率引擎（时区锁定 Asia/Shanghai） |
| `internal/router` | 多上游按时间段路由选择、凭证内存态存储、模型名重写（见 ADR-0006） |
| `internal/timeutil` | 时区单例 `ShanghaiTZ` + `IsInRange`/`MatchDay`（全项目时段判定统一） |
| `internal/handler` | `/v1/quota`、`/v1/calls`、`/v1/models` 等公开 handler |
| `internal/config` | YAML 加载（支持 `ZHIPU_API_KEY` 环境变量覆盖 yaml） |
| `internal/db` | SQLite 打开与迁移 |
| `web/admin`, `web/user` | 前端（嵌入二进制，改完需重新编译） |
| `deploy/` | nginx.conf 模板、systemd service 模板 |
| `docs/` | `system_design.md`、mermaid 图 |
| `AGENTS.md` | 本文件 |

---

## 4. 构建 / 测试 / 部署

```bash
# 有 go 环境
make build            # 本地构建
make build-linux      # 交叉编译 linux/amd64
make test             # 跑单测（必须全绿）
make vet / make fmt   # 静态检查 / 格式化
make lint             # gofmt 检查 + go vet
make ci               # fmt && vet && test && build-linux

# 本地无 go（受管路径）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 /tmp/go/bin/go build -o llm_api_gateway_linux .
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 /tmp/go/bin/go test ./...
```

**部署流程（改后端后）**：
1. 交叉编译 → 产物 `llm_api_gateway_linux`
2. `scp llm_api_gateway_linux root@115.190.223.216:/opt/llm-gateway/`
3. `ssh root@... "sudo mv ...new ... ; sudo chmod +x; sudo chown llm-gw:llm-gw; sudo systemctl restart llm-gateway"`
4. 前端改动：重新编译（前端嵌入二进制）→ 同上重启；nginx 配置变更：`nginx -t && systemctl reload nginx`

---

## 5. 🔒 密钥管理铁律（最高优先级，违反即安全事件）

- `config.yaml` **不提交 git**（`.gitignore` 已忽略；只有 `config.yaml.example` 入库）
- 上游智谱 Key **仅通过 systemd 环境变量 `ZHIPU_API_KEY` 注入**，
  不在任何磁盘文件明文存储（`/etc/systemd/system/llm-gateway.service` 的 `Environment=`）
- admin 后台改 Key 只更新内存（`apiKeyHolder.Set`），**绝不回写明文到 config.yaml**
- **绝不允许把明文 API Key 写回任何磁盘文件**（含 yaml/db/日志）
- 子 Key 用 **SHA256 + salt** 哈希存储，明文仅创建/重新生成时一次性返回前端
- **多上游 B 凭据铁律**：除智谱外若启用第二上游（如 OpenAI），其 Key（`OPENAI_API_KEY` 等）
  **仅在服务启动时由对应 `providers[].api_key_env` 环境变量注入内存**（`router.CredentialStore`），
  **绝不写回 yaml/db/日志，admin 后台也无改 B Key 的入口**。沿用 ADR-0002「绝不落盘明文」。

> 历史事故：早期 `persistToYAML` 会把明文 key 写回 config.yaml。已在 security-hardening 分支修复
> （改 Key 时 `delete(apiSection, "zhipu_api_key")`），并把磁盘明文 key 迁到 systemd 环境变量。
> 重启服务后 key 从环境变量加载，config.yaml 仅保留 endpoint。

---

## 6. 关键不变量（改动前必读，改错会破坏业务）

- **配额按调用次数计量**：`effective_calls = ceil(1 × multiplier)`。token **仅统计不计量**。
  （不要用 token 当主计量单位，这是 v1.2 的设计决策。）
- **时段倍率**：按 `星期 + 时间段` 匹配，多规则取 **MAX**。
  高峰期 **14:00–18:00 (UTC+8)** 倍率 **3×**，其余时间 1×。
- **隐蔽管理路径**：外部 `/m-7xa2/` → 内部 `/admin/`。**禁止暴露真实 `/admin/` 路径**（防暴力破解）。
- **SSE 流式透传**：逐行转发，检测 `r.Context().Done()` 断连；流式 client 整体超时 10 分钟（防上游挂起）。
- **`/v1/models` 公开端点**：Cursor / Continue 等 OpenAI 兼容客户端发现模型用，必须保留。
- **前端分享文案**：Base URL 用 `https://ai.shimiaocheng.top/v1`
  （**不是**完整 `/v1/chat/completions`，否则 Cursor 拼接成
  `/v1/chat/completions/chat/completions` 导致 404）。
- **禁用/删除用户三重拦截**：SQL 过滤（`status != 'deleted'`）+ 中间件（`disabled`/`deleted` 返回 403）+ 列表排除。
- **请求体限制（per-user）**：用户表 `max_body_size` 列（字节，默认 1MB）控制每个用户的单次请求体上限；鉴权后由 Go `http.MaxBytesReader` 按该值执行，未设置回落 1MB、超过 32MB 自动封顶。nginx `/v1/` 维持 `client_max_body_size 32m` 作为**全局通道天花板**（go 无法超过此值），管理后台/UI 路径仍保留 1MB server 级守卫。滥用量由「每用户调用配额 + 每用户请求体上限」共同控制，非全局一刀切。
- **时段路由：命中即走，绝不回退**（见 ADR-0006）：按 `provider_routing_rules` 的「时间段 + 星期」
  判定，窗口命中某 provider（如 openai）即转发该上游；**若其故障返回 502，绝不静默回退默认
  provider（如 zhipu）**。路由规则表空 / DB 错时回退默认 provider，但不 panic。
- **时区锁定 Asia/Shanghai**：所有时段窗口、倍率窗口、路由判定统一用 `timeutil.ShanghaiTZ`
  （`.In(timeutil.ShanghaiTZ)`），**禁止裸 `time.Now()` 本地时区做窗口/倍率比较**（避免跨时区部署
  错位）。`internal/timeutil` 已 `import _ "time/tzdata"` 保证离线时区可用。
- **模型名重写 passthrough**：窗口命中后按 `model_mappings` 把 external 模型名重写为目标上游真实名；
  缺失映射时**原样透传 external 名，不报错**。`/v1/models` 仍返回单一 external 列表，不随窗口变化。

---

## 7. 🚫 禁区

- 不要改配额计量口径（次数 ↔ token）
- 不要暴露 `/admin/` 真实路径，或去掉隐蔽路径
- 不要把明文 Key 落盘（见第 5 节）
- 不要破坏 `/v1/models` 端点
- 不要移除请求体限制（per-user `max_body_size` 仍须保留默认 1MB 守卫；nginx `/v1/` 的 32m 是通道天花板，勿降到低于你想分配给任何用户的最大值，也勿整体取消后台/UI 的 1MB）
- 不要改变子 Key 的 SHA256+salt 哈希方式
- 不要做时段路由回退（窗口命中 B 故障即 502，绝不可静默重试默认 A）
- 不要用裸本地时区（`time.Now()`）做时段/倍率窗口比较，必须 `.In(timeutil.ShanghaiTZ)`
- 不要把第二上游（如 OpenAI）明文 Key 落盘或打印（见第 5 节铁律）

---

## 8. 安全加固现状（2026-07-11，分支 security-hardening）

- **nginx 限速**：`login_limit`(5r/m)，zone 定义在
  `/etc/nginx/conf.d/rate-limit.conf`（http 块 include）；`/m-7xa2/` 套 login_limit 防护后台爆破。`/v1/` **无 nginx 限速**（纯中转，上游自带限速；2026-07-13 决策移除 `api_limit`）
- **API Key**：内存态 + systemd 环境变量持久化（不落盘明文）
- **请求体**：per-user `max_body_size`（默认 1MB，后台可调 500KB/1/4/8/16/32MB）；nginx `/v1/` 维持 32m 通道天花板（Go 读取上限），Go 按用户执行。`compaction: "trim"`（默认）下，请求超过用户上限时**自动裁剪历史对话**（保留 system + 最近轮次）后转发，**不再 413**；`compaction: "off"` 恢复旧行为（超限即 413）。无论哪种，超过 32MB 绝对天花板仍 413（滥用防护）。管理后台/UI 路径仍 1MB（nginx server 级 + Go `MaxBytesReader` 双保险）。
- **流式**：10 分钟整体超时
- **前端**：`escapeAttr` 反斜杠转义（防 self-XSS）
- **部署模板**：`deploy/nginx.conf` 已同步为脱敏真实配置（`/v1/` 无 api_limit 限速，login_limit 注释保留）

---

## 9. Git 分支约定

- `main`：稳定版
- 功能/修复：从 `main`（或当前开发分支）`checkout -b <feat>` → 提交 → PR/合并
- 历史分支：`security-hardening`（安全加固）、`quality-guardrails`（测试护栏+AGENTS.md）

---

## 10. AI 协作约定（本项目特有）

本项目用 **multi-agent SOP** 开发（主理人齐活林协调：产品经理/架构师/工程师/QA）。
任何成员改动后必须：
1. 编译通过（`make build-linux`）
2. 单测通过（`make test`）
3. 关键安全相关改动（密钥/路径/配额）需对照本文件第 5–7 节自查
4. 部署到服务器后实际验证（curl 或日志），不要只凭代码存在就认为完成
