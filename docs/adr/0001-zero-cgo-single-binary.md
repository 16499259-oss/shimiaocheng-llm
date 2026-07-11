# ADR-0001: 零 CGO 单二进制部署

- 状态（Status）：Accepted（已落地）
- 日期：2026-07-11
- 决策人：高见远（架构师）/ 主理人齐活林

## 背景（Context）

LLM API Gateway 是一个部署在单台服务器（`115.190.223.216`）上的配额代理网关，对外提供 OpenAI 兼容的 `/v1/` 接口，向下游多用户发放子 Key。早期版本若使用 cgo 版 SQLite 驱动（`mattn/go-sqlite3`），则 `go build` 时必须开启 CGO（`CGO_ENABLED=1`），并依赖目标机安装了对应的 C 工具链与 SQLite 共享库。

这带来几个现实问题：

- 交叉编译到 `linux/amd64` 时，本机若无对应 C 交叉工具链则无法编译；
- 交付物不是真正的静态二进制，运行时依赖目标机的 `libc` / `libsqlite3`，部署环境稍有差异就可能因缺库而启动失败；
- 在受限的线上容器/最小化系统中，安装 C 库增加运维复杂度与攻击面。

项目需要一种「拷贝即运行、零外部依赖」的交付方式。

## 决策（Decision）

- 选用纯 Go 实现的 SQLite 驱动 **`modernc.org/sqlite`**（driver name `"sqlite"`），完全替代 cgo 版驱动。
- 构建时统一设置 **`CGO_ENABLED=0`**，产出单个静态二进制 `llm_api_gateway`，目标平台 `GOOS=linux GOARCH=amd64`。
- 该二进制内嵌前端静态资源（`web/admin`、`web/user`，经 `embed.go` 的 `//go:embed`），无需额外的 web 根目录。
- 部署形态为：单二进制 + `llm_gateway.db`（SQLite 单文件）+ 外层 nginx 反代 + systemd 托管。

相关构建命令见 `Makefile`（`make build-linux`）与 `AGENTS.md` 第 4 节：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o llm_api_gateway .
```

## 后果（Consequences）

**正面：**

- 交叉编译简单：在任意平台（macOS / Windows）即可直出 linux/amd64 静态二进制，无需 C 交叉工具链。
- 部署干净：目标机只需放置一个二进制文件，无需安装任何 C 库或 SQLite 运行时，降低了环境差异导致的「在我机器上能跑」问题。
- 体积虽比 cgo 版略大（纯 Go SQLite 实现更重），但本项目量级完全可接受。
- 与 systemd + nginx 的部署方式契合，更新即「替换二进制 + restart」。

**负面 / 取舍：**

- SQLite 走纯 Go 实现（`modernc.org/sqlite`），在极高并发写场景下的吞吐低于 cgo 原生绑定；但本项目为低频配额代理（QPS 量级低），性能完全足够。
- 一旦更换数据库驱动，需同步保证 `database/sql` 的 `Open("sqlite", ...)` 调用、迁移脚本、查询语法与纯 Go 驱动兼容（避免 cgo 专有 PRAGMA/语法）。
- 后续若引入任何依赖 cgo 的库，会破坏「零 CGO」承诺，须在选型时显式评估。

**后续注意事项：**

- 任何新依赖若引入 cgo，必须在 PR 中说明并重新评估本决策。
- CI（`make ci` / `make build-linux`）应持续以 `CGO_ENABLED=0` 构建，作为该约束的回归保障。
