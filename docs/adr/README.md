# ADR 索引（Architecture Decision Records）

本项目（LLM API Gateway）的关键架构 / 安全决策记录。所有 ADR 均为 **Accepted（已落地）**，用于固化「为什么这么做」，让后续（尤其是 AI 协作）开发有章可循。

> 日期：2026-07-11　决策人：高见远（架构师）/ 主理人齐活林

| 编号 | 标题 | 一句话摘要 |
|------|------|-----------|
| [ADR-0001](./0001-zero-cgo-single-binary.md) | 零 CGO 单二进制部署 | 用 `modernc.org/sqlite` 纯 Go 驱动 + `CGO_ENABLED=0`，产出无外部依赖的静态二进制。 |
| [ADR-0002](./0002-api-key-in-memory.md) | 上游 API Key 仅内存态 + 环境变量持久化 | 改 Key 只更新内存、绝不落盘明文；持久化靠 systemd `Environment=ZHIPU_API_KEY`。 |
| [ADR-0003](./0003-nginx-fronting-gateway.md) | nginx 作为网关前置反向代理 | 隐蔽管理路径 `/m-7xa2/`（→ `/admin/`）+ 登录/API 限速 + 1MB 体限 + 安全头。 |
| [ADR-0004](./0004-stream-overall-timeout.md) | SSE 流式上游整体超时 10 分钟 | 流式转发 `http.Client{Timeout: 10m}`（整体而非 idle），防上游挂起拖死连接。 |
| [ADR-0005](./0005-escapeattr-backslash.md) | 前端 escapeAttr 反斜杠转义 | 先转义 `\` 再转义引号，消除 admin 面板属性注入隐患（self-XSS）。 |

## 新增决策流程

当需要记录一项新的架构 / 安全决策时，请按以下流程操作（全程不直接改 `main`）：

1. **复制模板**：将 [`TEMPLATE.md`](./TEMPLATE.md) 复制为 `00NN-title.md`，编号顺延现有 ADR（当前最大为 0005，下一个新决策即 0006、0007……），文件名小写为英文连字符。
2. **填写元信息**：在文件顶部填好 `状态（Status）`、`日期`、`决策人` 三项（状态取值：Proposed / Accepted / Deprecated）。
3. **写满三节**：完成 `背景（Context）` / `决策（Decision）` / `后果（Consequences）`，其中后果须含 `正面` / `负面·取舍` / `后续注意事项` 三段。
4. **更新索引表**：在本文件顶部索引表补一行（编号、标题、一句话摘要）。
5. **提交分支**：在 `chore/adr` 类分支提交（如 `chore/adr-0006-xxx`），由主理人统一合并推送。

模板本身的格式与现有 5 份 ADR（0001~0005）保持一致，可直接套用骨架，降低记录门槛。

## 关联文档

- `AGENTS.md`：项目级 AI 协作手册（密钥铁律、关键不变量、禁区）。
- `docs/system_design.md`：系统总体设计。
