# ADR-0003: nginx 作为网关前置反向代理

- 状态（Status）：Accepted（已落地）
- 日期：2026-07-11
- 决策人：高见远（架构师）/ 主理人齐活林

## 背景（Context）

Go 网关（`llm_api_gateway`）本身以 `127.0.0.1:8080` 仅监听本地，不直面公网。直接把 Go 服务暴露到公网会带来一系列风险与体验问题：

- 管理后台 `/admin/` 若直连公网，容易被爆破、扫描；
- 缺少传输层限速，登录接口可能被暴力破解，API 接口可能被滥用刷量；
- 缺少统一的安全响应头（HSTS、防点击劫持、MIME sniff 防护等）；
- Go 侧缺少对请求体尺寸的硬上限时，超大请求体会占用内存。

因此需要在 Go 之前放置一层成熟、高性能的反向代理，承担 TLS 终结、限速、安全头、请求体裁剪等公共职责。

## 决策（Decision）

以 **nginx** 作为网关前置反代（部署模板见 `deploy/nginx.conf`），并落实以下具体规则：

1. **隐蔽管理路径**：对外暴露名 `/m-7xa2/`，nginx 内部 `proxy_pass` 映射到 Go 的 `/admin/`。真实 `/admin/` 路径不直接对外暴露，降低管理面板被扫描/爆破的暴露面（`AGENTS.md` 第 6 节明确为关键不变量）。
2. **登录限速 `login_limit`**：`limit_req_zone $binary_remote_addr zone=login_limit:10m rate=5r/m;`（定义在 http 块，burst 5，nodelay），套在 `/m-7xa2/`。
3. **API 限速 `api_limit`**：`limit_req_zone $binary_remote_addr zone=api_limit:10m rate=10r/s;`（定义在 http 块，burst 20，nodelay），套在 `/v1/`。
4. **请求体上限 1MB**：`client_max_body_size 1m`；Go 侧再以 `http.MaxBytesReader` 做双保险（见 `AGENTS.md` 第 6 节）。
5. **安全响应头**（各 location 统一添加）：
   - `Strict-Transport-Security "max-age=31536000; includeSubDomains"`（HSTS）
   - `X-Content-Type-Options: nosniff`
   - `X-Frame-Options: DENY`
   - `Referrer-Policy: no-referrer`
   - `server_tokens off`（关闭 nginx 版本号暴露，置于 http/server 层级）
6. **SSE 支持**：`/v1/` 开启 `proxy_http_version 1.1`、`proxy_buffering off`、长 `proxy_read_timeout 300s`，保证流式透传不被缓冲截断。
7. HTTP→HTTPS 强制跳转（`listen 80` → 301 到 `https://$server_name$request_uri`）。

## 后果（Consequences）

**正面：**

- 暴力破解与滥用在到达 Go 之前就被 nginx 拦截：超限请求直接由 nginx 返回 `503`（限速）/ `413`（超体），Go 进程零负担。
- 隐蔽路径 + 限速叠加，大幅抬高管理面板被攻破的门槛。
- 统一安全头提升整体防护基线（传输安全、点击劫持、MIME 嗅探）。
- 请求体 1MB 双保险，遏制大 payload 滥用与内存占用。

**负面 / 取舍：**

- 增加一层运维组件（nginx 配置、限速 zone 需在 http 块中一致定义，否则 reload 失败）。
- 限速阈值（5r/m、10r/s）为经验值，若下游合法突发流量超过 burst，会出现正常请求被 `503` 误伤，需按监控调整。
- `server_tokens off`、限速 zone 定义等属于 http 块级配置，不体现在 `deploy/nginx.conf` 站点模板内，部署时需同步在 `/etc/nginx/nginx.conf` 的 http 块中维护（模板顶部注释已说明）。

**后续注意事项：**

- 限速 zone 定义与站点 `limit_req` 引用必须配对，改动后务必 `nginx -t && systemctl reload nginx`。
- 禁止移除 1MB 请求体限制、禁止暴露真实 `/admin/` 路径（见 `AGENTS.md` 第 7 节禁区）。
- 若新增公网 location，应默认继承安全头与相应限速策略。
