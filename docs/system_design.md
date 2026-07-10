# LLM API Gateway — 系统架构设计

> 版本：v1.2 | 日期：2025-07-09 | 作者：高见远（架构师）
> 
> **v1.2 变更**: 配额主单位从 token 改为**调用次数**；token 保留为辅助统计字段
> **v1.1 变更**: ①配额由软限制改为硬限制；②新增时段倍率系统

---

## 目录

1. [整体架构](#1-整体架构)
2. [技术栈选型](#2-技术栈选型)
3. [数据库表结构](#3-数据库表结构)
4. [API 接口设计](#4-api-接口设计)
5. [安全策略](#5-安全策略)
6. [SSE 流式响应透传方案](#6-sse-流式响应透传方案)
7. [5h 配额自动重置方案](#7-5h-配额自动重置方案)
8. [配额模型详解](#8-配额模型详解)
9. [时段倍率系统](#9-时段倍率系统)
10. [子 Key 生成与管理](#10-子-key-生成与管理)
11. [项目文件结构](#11-项目文件结构)
12. [依赖包列表](#12-依赖包列表)
13. [部署方案](#13-部署方案)
14. [任务列表](#14-任务列表)
15. [待明确事项](#15-待明确事项)

---

## 1. 整体架构

```mermaid
graph TB
    subgraph External["外部"]
        U[终端用户<br/>使用子 Key]
        A[管理员<br/>Web 面板]
        Z[智谱 GLM-5.2 API<br/>api.zhipuai.cn]
    end

    subgraph Server["单机云服务器"]
        N[Nginx :443<br/>TLS 终止 + 反代]

        subgraph Gateway["LLM API Gateway<br/>Go 单二进制 :8080"]
            direction TB
            MUX[net/http ServeMux]
            MW_AUTH[AuthMiddleware<br/>子 Key 鉴权]
            MW_QUOTA[QuotaMiddleware<br/>配额检查]
            PROXY[ProxyHandler<br/>/v1/chat/completions]
            ADMIN[AdminHandler<br/>/admin/*]
            QUOTA_H[QuotaHandler<br/>/v1/quota]
            SCHED[QuotaScheduler<br/>5h 窗口重置]
        end

        DB[(SQLite<br/>llm_gateway.db)]
        STATIC[静态资源<br/>管理面板 HTML/JS]
    end

    U -->|Bearer sk-xxx| N
    A -->|HTTPS| N
    N -->|:8080| MUX
    MUX --> MW_AUTH --> MW_QUOTA
    MW_QUOTA --> PROXY
    PROXY -->|真实 API Key| Z
    MW_QUOTA --> ADMIN
    MW_QUOTA --> QUOTA_H
    PROXY --> DB
    ADMIN --> DB
    QUOTA_H --> DB
    SCHED --> DB
```

### 架构分层

| 层 | 职责 | 组件 |
|---|---|---|
| **接入层** | TLS 终止、反向代理、静态资源 | Nginx |
| **中间件层** | 鉴权、配额检查、日志 | AuthMiddleware、QuotaMiddleware |
| **核心服务层** | 代理转发、管理 API、配额查询 | ProxyHandler、AdminHandler、QuotaHandler |
| **定时任务层** | 5h 窗口配额重置、周期性清理 | QuotaScheduler |
| **数据层** | 用户、配额、调用记录、会话 | SQLite |

### 数据流（一次 LLM 代理请求）

```
用户 → Nginx(:443) → Gateway(:8080)
  → AuthMiddleware: 解析子 Key → 查 DB 验证 → 获取 user_id
  → QuotaMiddleware: 计算时段倍率 → 查 DB 配额 → 5h_used + effective_calls ≤ 5h_limit? total_used + effective_calls ≤ total_limit?
    → 准入: 原子扣减 effective_calls 次 → ProxyHandler → 智谱 API → 记录调用日志（token 统计）
    → 拒绝: 429 Too Many Requests
```

---

## 2. 技术栈选型

| 组件 | 选型 | 版本 | 理由 |
|---|---|---|---|
| **语言** | Go | 1.22+ | 单二进制编译、原生并发、零运行时依赖 |
| **HTTP 路由** | `net/http` 标准库 | 1.22 | Go 1.22 已支持路径参数（`{param}`），无需第三方路由库 |
| **数据库** | SQLite | — | 单机部署首选，无需独立数据库进程 |
| **SQLite 驱动** | `modernc.org/sqlite` | latest | **纯 Go 实现，零 CGO 依赖**。不依赖系统 libsqlite3，真正单二进制 |
| **配置解析** | `gopkg.in/yaml.v3` | v3 | YAML 配置，可读性强 |
| **密码哈希** | `golang.org/x/crypto` | latest | bcrypt 标准实现 |
| **前端** | 原生 HTML + Vanilla JS | — | 管理面板：无框架依赖，内嵌到二进制 |
| **嵌入资源** | `embed` 标准库 | 1.22 | 前端静态文件编译进二进制 |
| **模板渲染** | `html/template` | 标准库 | Admin 页面渲染 |
| **反向代理** | Nginx | 任意 | TLS 终止、静态资源、SSE 代理 |
| **进程管理** | systemd | — | 开机自启、崩溃重启 |

### 依赖总结

```
直接依赖数: 3
├── modernc.org/sqlite    (纯 Go SQLite)
├── gopkg.in/yaml.v3      (YAML 配置)
└── golang.org/x/crypto   (bcrypt)
```

---

## 3. 数据库表结构

### 3.1 ER 图

```
┌──────────────┐       ┌──────────────────┐
│    users     │ 1──1  │     quotas       │
├──────────────┤       ├──────────────────┤
│ id (PK)      │       │ id (PK)          │
│ username     │       │ user_id (FK, UQ) │
│ password_hash│       │ quota_5h_limit   │
│ sub_key_hash │       │ quota_5h_used    │
│ sub_key_preview│      │ quota_total_limit│
│ status       │       │ quota_total_used │
│ created_at   │       │ window_start     │
│ updated_at   │       │ updated_at       │
└──────────────┘       └──────────────────┘
       │
       │ 1──N
       ▼
┌──────────────────┐    ┌──────────────────┐    ┌──────────────────┐
│    call_logs     │    │  admin_sessions  │    │ time_multipliers │
├──────────────────┤    ├──────────────────┤    ├──────────────────┤
│ id (PK)          │    │ id (PK)          │    │ id (PK)          │
│ user_id (FK)     │    │ session_token    │    │ start_time       │
│ model            │    │ created_at       │    │ end_time         │
│ prompt_tokens    │    │ expires_at       │    │ multiplier       │
│ completion_tokens│    └──────────────────┘    │ days_of_week     │
│ total_tokens     │                            │ enabled          │
│ effective_calls  │                            │ created_at       │
│ multiplier_used  │                            └──────────────────┘
│ status_code      │
│ latency_ms       │
│ error_msg        │
│ created_at       │
└──────────────────┘
```

### 3.2 完整 DDL

```sql
-- 用户表
CREATE TABLE IF NOT EXISTS users (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    username        TEXT    NOT NULL UNIQUE,
    password_hash   TEXT    NOT NULL,             -- bcrypt 哈希（仅 Admin 有密码，普通用户可为空）
    sub_key_hash    TEXT    NOT NULL UNIQUE,       -- SHA256(子Key) 哈希，用于验证
    sub_key_preview TEXT    NOT NULL,              -- 子 Key 前缀展示（sk-3f8a2...）
    role            TEXT    NOT NULL DEFAULT 'user', -- 'admin' | 'user'
    status          TEXT    NOT NULL DEFAULT 'active', -- 'active' | 'disabled'
    created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_users_sub_key_hash ON users(sub_key_hash);
CREATE INDEX idx_users_status ON users(status);

-- 配额表（与用户 1:1）
CREATE TABLE IF NOT EXISTS quotas (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id           INTEGER NOT NULL UNIQUE,
    quota_5h_limit    INTEGER NOT NULL DEFAULT 100,      -- 5h 窗口调用次数上限
    quota_5h_used     INTEGER NOT NULL DEFAULT 0,        -- 当前窗口已使用次数
    quota_total_limit INTEGER NOT NULL DEFAULT 10000,    -- 总调用次数上限
    quota_total_used  INTEGER NOT NULL DEFAULT 0,        -- 累计已使用次数
    window_start      TEXT    NOT NULL,                   -- 当前 5h 窗口起始时间
    updated_at        TEXT    NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX idx_quotas_window_start ON quotas(window_start);

-- 调用记录表（配额主单位：次数；token 为辅助统计）
CREATE TABLE IF NOT EXISTS call_logs (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id           INTEGER NOT NULL,
    model             TEXT    NOT NULL DEFAULT 'glm-5.2',
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,        -- 统计：prompt token
    completion_tokens INTEGER NOT NULL DEFAULT 0,        -- 统计：completion token
    total_tokens      INTEGER NOT NULL DEFAULT 0,        -- 统计：上游原始 token
    effective_calls   INTEGER NOT NULL DEFAULT 1,        -- 实际消耗次数 = 1 × multiplier（进位取整）
    multiplier_used   REAL    NOT NULL DEFAULT 1.0,       -- 生效的时段倍率
    status_code       INTEGER NOT NULL,                   -- 200/429/500
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    error_msg         TEXT,
    created_at        TEXT    NOT NULL DEFAULT (datetime('now')),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX idx_call_logs_user_id    ON call_logs(user_id);
CREATE INDEX idx_call_logs_created_at ON call_logs(created_at);

-- Admin 会话表
CREATE TABLE IF NOT EXISTS admin_sessions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_token   TEXT    NOT NULL UNIQUE,
    created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
    expires_at      TEXT    NOT NULL                   -- 24h 过期
);

CREATE INDEX idx_admin_sessions_token ON admin_sessions(session_token);

-- 时段倍率表（全局规则，非用户级别）
CREATE TABLE IF NOT EXISTS time_multipliers (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    start_time    TEXT    NOT NULL,              -- 时段开始 "14:00"
    end_time      TEXT    NOT NULL,              -- 时段结束 "18:00"
    multiplier    REAL    NOT NULL DEFAULT 1.0,   -- 倍率，如 2.0 表示消耗翻倍
    days_of_week  TEXT    NOT NULL DEFAULT '*',   -- 生效星期：'*'=每天, '1,2,3,4,5'=工作日
    enabled       INTEGER NOT NULL DEFAULT 1,     -- 是否启用
    created_at    TEXT    NOT NULL DEFAULT (datetime('now')),
    CONSTRAINT chk_multiplier CHECK (multiplier >= 1.0),
    CONSTRAINT chk_time CHECK (start_time < end_time)
);

CREATE INDEX idx_time_multipliers_enabled ON time_multipliers(enabled);
```

---

## 4. API 接口设计

### 4.1 端点总览

| # | 路由 | 方法 | 鉴权 | 说明 |
|---|---|---|---|---|
| 1 | `/v1/chat/completions` | POST | 子 Key | LLM 代理（同步 + SSE 流式） |
| 2 | `/v1/quota` | GET | 子 Key | 用户自助查询剩余配额 |
| 3 | `/admin/login` | POST | 无 | Admin 登录 |
| 4 | `/admin/logout` | POST | Admin Session | Admin 登出 |
| 5 | `/admin/` | GET | Admin Session | 管理面板首页（Dashboard） |
| 6 | `/admin/api/users` | GET | Admin Session | 用户列表 |
| 7 | `/admin/api/users` | POST | Admin Session | 创建用户 |
| 8 | `/admin/api/users/{id}` | PUT | Admin Session | 更新用户（配额、状态、重新生成子 Key） |
| 9 | `/admin/api/users/{id}/calls` | GET | Admin Session | 用户调用记录 |
| 10 | `/admin/api/overview` | GET | Admin Session | Dashboard 概览 |
| 11 | `/admin/api/multipliers` | GET | Admin Session | 时段倍率规则列表 |
| 12 | `/admin/api/multipliers` | POST | Admin Session | 创建时段倍率规则 |
| 13 | `/admin/api/multipliers/{id}` | PUT | Admin Session | 更新时段倍率规则 |
| 14 | `/admin/api/multipliers/{id}` | DELETE | Admin Session | 删除时段倍率规则 |

### 4.2 接口详细设计

#### 4.2.1 请求头规范

```
# 下游鉴权
Authorization: Bearer sk-<sub_key>

# Admin 鉴权
Cookie: admin_session=<session_token>

# Content-Type
Content-Type: application/json
```

#### 4.2.2 `/v1/chat/completions` — LLM 代理

```
POST /v1/chat/completions
Authorization: Bearer sk-3f8a2b1c4d5e6f7a8b9c0d1e2f3a4b5c
Content-Type: application/json

请求体（透传智谱标准格式）:
{
  "model": "glm-5.2",
  "messages": [
    {"role": "user", "content": "Hello"}
  ],
  "stream": false
}

成功响应:
HTTP/1.1 200 OK
{
  "id": "chatcmpl-xxx",
  "object": "chat.completion",
  "created": 1712345678,
  "model": "glm-5.2",
  "choices": [...],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 50,
    "total_tokens": 60
  }
}

配额耗尽:
HTTP/1.1 429 Too Many Requests
{
  "error": {
    "message": "Quota exceeded. 5h limit: 100 calls, used: 100. Resets at 15:00",
    "type": "quota_exceeded",
    "code": "quota_exceeded"
  }
}

子 Key 无效:
HTTP/1.1 401 Unauthorized
{
  "error": {
    "message": "Invalid API key",
    "type": "invalid_api_key",
    "code": "invalid_api_key"
  }
}

子 Key 已禁用:
HTTP/1.1 403 Forbidden
{
  "error": {
    "message": "API key has been disabled",
    "type": "key_disabled",
    "code": "key_disabled"
  }
}
```

#### 4.2.3 `/admin/api/users` — 创建用户

```
POST /admin/api/users
Cookie: admin_session=xxx
Content-Type: application/json

请求体:
{
  "username": "zhangsan",
  "quota_5h_limit": 100,        // 5h 调用次数上限
  "quota_total_limit": 10000    // 总调用次数上限
}

成功响应 (201):
{
  "id": 2,
  "username": "zhangsan",
  "sub_key": "sk-3f8a2b1c4d5e6f7a8b9c0d1e2f3a4b5c",
  "sub_key_preview": "sk-3f8a2...",
  "quota_5h_limit": 100,        // 单位：次数
  "quota_5h_used": 0,
  "quota_total_limit": 10000,
  "quota_total_used": 0,
  "status": "active",
  "created_at": "2025-07-09T16:30:00Z"
}

⚠️ 子 Key 明文仅在创建响应中返回一次，之后无法再次获取（数据库仅存哈希）
```

#### 4.2.4 `/admin/api/users/{id}` — 更新用户

```
PUT /admin/api/users/2
Cookie: admin_session=xxx
Content-Type: application/json

请求体（所有字段可选）:
{
  "quota_5h_limit": 2000000,    // 修改 5h 配额上限
  "quota_total_limit": 200000000, // 修改总配额上限
  "status": "disabled",         // 启用/禁用
  "regenerate_key": true        // 重新生成子 Key
}

regenerate_key=true 时的响应:
{
  ...
  "new_sub_key": "sk-9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d",
  "old_key_disabled": true
}
```

#### 4.2.5 `/admin/api/users/{id}/calls` — 调用记录

```
GET /admin/api/users/2/calls?from=2025-07-01&to=2025-07-09&page=1&limit=20

响应:
{
  "data": [
    {
      "id": 1234,
      "model": "glm-5.2",
      "total_tokens": 1240,
      "status_code": 200,
      "latency_ms": 823,
      "created_at": "2025-07-09T14:32:00Z"
    },
    ...
  ],
  "pagination": {
    "page": 1,
    "limit": 20,
    "total": 156
  }
}
```

#### 4.2.6 `/v1/quota` — 用户自助查询

```
GET /v1/quota
Authorization: Bearer sk-3f8a2...

响应:
{
  "quota_5h_limit": 100,
  "quota_5h_used": 45,
  "quota_5h_remaining": 55,
  "quota_total_limit": 10000,
  "quota_total_used": 2300,
  "quota_total_remaining": 7700,
  "window_reset_at": "2025-07-09T15:00:00Z",
  "status": "active"
}
```

#### 4.2.7 `/admin/api/multipliers` — 时段倍率管理

```
GET /admin/api/multipliers
响应:
{
  "data": [
    {
      "id": 1,
      "start_time": "14:00",
      "end_time": "18:00",
      "multiplier": 2.0,
      "days_of_week": "2,4",
      "enabled": true
    }
  ]
}

POST /admin/api/multipliers
请求体:
{
  "start_time": "14:00",
  "end_time": "18:00",
  "multiplier": 2.0,
  "days_of_week": "2,4"
}
响应 (201): { "id": 1, ... }

PUT /admin/api/multipliers/1
请求体（所有字段可选）:
{
  "enabled": false,      // 禁用该规则
  "multiplier": 3.0      // 修改倍率
}

DELETE /admin/api/multipliers/1
响应 (204): No Content
```

---

## 5. 安全策略

### 5.1 子 Key 安全

```
生成: sub_key = "sk-" + hex.EncodeToString(sha256.Sum256(secret + userID + timestamp + cryptoRand(32)))[:32]

存储: 数据库仅存 SHA256(sub_key)，不存明文
验证: 请求时 SHA256(请求中的 sub_key) 与数据库 hash 比对

特点:
- 子 Key 无法逆向推导真实 API Key
- 数据库泄露不会暴露子 Key 明文
- 子 Key 明文仅创建时展示一次（Admin 自行分发）
```

### 5.2 真实 API Key 保护

```
┌────────────────────────────────────────────────────┐
│ 原则：真实 API Key 零持久化，仅内存存在              │
├────────────────────────────────────────────────────┤
│ 1. 从配置文件/环境变量读取 → 仅加载进内存变量        │
│ 2. 不入数据库                                       │
│ 3. 不出现在日志中（日志输出时替换为 "****"）          │
│ 4. 不出现在任何 API 响应中                           │
│ 5. 不传递给任何中间件                                │
│ 6. 仅 ProxyHandler 内部访问                         │
│                                                    │
│ 配置方式（按优先级）:                                │
│   1. 环境变量 ZHIPU_API_KEY（推荐生产）              │
│   2. 配置文件 config.yaml 中的 api_key 字段         │
│   3. 命令行参数 -api-key                            │
└────────────────────────────────────────────────────┘
```

### 5.3 Admin 鉴权

```
登录流程:
  1. Admin POST /admin/login {username, password}
  2. 服务端 bcrypt.CompareHashAndPassword 验证
  3. 生成 session_token = hex(sha256(cryptoRand(64)))
  4. 写入 admin_sessions 表，expires_at = now + 24h
  5. 返回 Set-Cookie: admin_session=<token>; HttpOnly; SameSite=Strict; Max-Age=86400

Cookie 安全属性:
  - HttpOnly: JS 无法读取
  - SameSite=Strict: 防止 CSRF
  - Secure: 生产环境通过 Nginx TLS 后设置（Go 端检测 X-Forwarded-Proto）
  - Path=/admin: 仅管理路径携带

每次 Admin API 请求:
  1. 从 Cookie 提取 admin_session
  2. 查 admin_sessions WHERE session_token=? AND expires_at > now
  3. 存在 → 放行；不存在 → 401
```

### 5.4 防重放与限流（P2）

```
- 请求幂等：同一请求 body + 同一 sub_key 在 5s 内不重复计配额（基于请求哈希去重）
- 限流：单用户 10 req/s（P2 实现）
- IP 白名单：可选配置 ADMIN_IPS 限制管理面板访问来源
```

### 5.5 服务器加固

```bash
# 防火墙：仅开放 443（Nginx），8080 仅监听 127.0.0.1
ufw allow 443/tcp
ufw default deny incoming

# 文件权限
chmod 600 config.yaml       # 包含 API Key
chmod 600 llm_gateway.db    # 数据库
```

---

## 6. SSE 流式响应透传方案

### 6.1 整体流程

```
                     Gateway
客户端                代理层                  智谱 API
  │                    │                       │
  │ POST /v1/chat/     │                       │
  │ stream=true        │                       │
  │───────────────────▶│                       │
  │                    │ (鉴权 + 配额预留)       │
  │                    │ POST /v1/chat/        │
  │                    │ stream=true           │
  │                    │──────────────────────▶│
  │                    │                       │
  │                    │  data: {"id":...}     │
  │                    │◀──────────────────────│
  │  data: {"id":...}  │                       │
  │◀───────────────────│                       │
  │                    │  data: {"choices":...}│
  │                    │◀──────────────────────│
  │  data: {"choices": │                       │
  │  ...}              │                       │
  │◀───────────────────│                       │
  │                    │       ...             │
  │                    │  data: [DONE]         │
  │                    │◀──────────────────────│
  │  data: [DONE]      │                       │
  │◀───────────────────│                       │
  │                    │ (解析 usage → 配额确认) │
```

### 6.2 Go 实现要点

```go
// proxy.go — SSE 透传核心
func (h *ProxyHandler) handleStream(w http.ResponseWriter, upstreamResp *http.Response, userID int64) {
    // 1. 设置 SSE 响应头
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")
    w.WriteHeader(http.StatusOK)

    flusher := w.(http.Flusher)
    scanner := bufio.NewScanner(upstreamResp.Body)
    scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 大缓冲区

    var totalTokens int64
    for scanner.Scan() {
        line := scanner.Text()

        // 2. 透传每一行到客户端
        fmt.Fprintf(w, "%s\n", line)
        flusher.Flush()

        // 3. 解析 usage 行（最后一帧）
        if strings.HasPrefix(line, "data:") && strings.Contains(line, "total_tokens") {
            var sseData struct {
                Usage struct {
                    TotalTokens int64 `json:"total_tokens"`
                } `json:"usage"`
            }
            json.Unmarshal([]byte(line[5:]), &sseData) // 去掉 "data:" 前缀
            totalTokens = sseData.Usage.TotalTokens
        }
    }

    // 4. 流结束后，以实际 token 确认配额
    h.quotaManager.ConfirmStream(userID, totalTokens)
}
```

### 6.3 配额两阶段策略（次数计量 + 硬限制）

配额耗尽即**硬性拒绝**，流式请求同样执行：

```
阶段1 — 预留（请求开始时）:
  计算本次消耗次数: effective_calls = ceil(1 × multiplier)

  读取当前配额:
    available_5h   = 5h_limit - 5h_used
    available_total = total_limit - total_used

  IF available_5h < effective_calls OR available_total < effective_calls:
    → 429 硬拒绝，不发起上游请求

  → quota_5h_used += effective_calls（预占，原子操作）
  → 发起上游流式请求

阶段2 — 确认（流结束后）:
  流结束，配额已预占 effective_calls 次
  解析 upstream 响应中的 total_tokens → 写入 call_logs（统计用途）
  不需要回滚（次数固定 = 1 × multiplier，可预知）

关键设计决策：
- 流式每次请求固定消耗 effective_calls 次（可预知），不依赖 token 量
- 预占即可确认，无需回滚逻辑 → 比 token 方案更简单
- 上游返回的 token 仅记录统计，不参与配额计算
```

### 6.4 Nginx 配置（SSE 兼容）

```nginx
location /v1/ {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Connection '';
    proxy_buffering off;           # ← 关键：SSE 必须关闭缓冲
    proxy_cache off;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

---

## 7. 5h 配额自动重置方案

### 7.1 固定窗口方案

```
窗口划分:
┌──────────┬──────────┬──────────┬──────────┬──────────┐
│ 00:00    │ 05:00    │ 10:00    │ 15:00    │ 20:00    │
│   ~      │   ~      │   ~      │   ~      │   ~      │
│ 04:59    │ 09:59    │ 14:59    │ 19:59    │ 23:59    │
└──────────┴──────────┴──────────┴──────────┴──────────┘
窗口边界: 00:00, 05:00, 10:00, 15:00, 20:00

边界到达时: 所有用户的 quota_5h_used 重置为 0
```

### 7.2 调度器实现

```go
// scheduler.go
type QuotaScheduler struct {
    db *sql.DB
    resetHour int // 5（每5小时）
}

func (s *QuotaScheduler) Start() {
    // 1. 启动时检查是否需要立即补偿重置
    s.compensate()

    // 2. 启动 30s ticker 定时检查
    ticker := time.NewTicker(30 * time.Second)
    go func() {
        for range ticker.C {
            s.tryReset()
        }
    }()
}

func (s *QuotaScheduler) tryReset() {
    now := time.Now()
    hours := now.Hour()

    // 判断是否到达窗口边界
    if hours % s.resetHour == 0 && now.Minute() == 0 {
        // 使用互斥锁防止并发重置
        s.mu.Lock()
        defer s.mu.Unlock()

        // 批量归零所有用户的 5h 配额
        _, err := s.db.Exec(`
            UPDATE quotas
            SET quota_5h_used = 0,
                window_start = ?,
                updated_at = ?
        `, now.Format(time.RFC3339), now.Format(time.RFC3339))

        if err != nil {
            log.Printf("quota reset failed: %v", err)
        } else {
            log.Printf("quota reset completed at %s", now.Format(time.RFC3339))
        }
    }
}

func (s *QuotaScheduler) compensate() {
    // 启动补偿：如果窗口起始时间不在当前窗口内，立即重置
    now := time.Now()
    currentWindowStart := now.Truncate(s.resetHour * time.Hour)
    // 检查 window_start < currentWindowStart 的用户并重置
    s.db.Exec(`
        UPDATE quotas
        SET quota_5h_used = 0, window_start = ?, updated_at = ?
        WHERE window_start < ?
    `, now.Format(time.RFC3339), currentWindowStart.Format(time.RFC3339))
}
```

### 7.3 设计要点

| 要点 | 方案 |
|---|---|
| 重置粒度 | 30s 定时检查（窗口边界最多 30s 延迟） |
| 并发安全 | 互斥锁 + SQLite WAL 模式 |
| 启动补偿 | 服务重启后检查 window_start，可能跨窗口 → 自动补偿 |
| 重置原子性 | 单条 SQL UPDATE 语句，SQLite 保证原子性 |
| 重置条件 | `hour % 5 == 0 AND minute == 0`，忽略秒 |

---

## 8. 配额模型详解

### 8.1 配额检查流程（次数计量 + 时段倍率）

```
请求到达
  │
  ▼
1. 鉴权 → 获取 user_id
  │
  ▼
2. 计算时段倍率
   GET * FROM time_multipliers WHERE enabled=1
   判断当前时间是否命中任一规则
   effective_multiplier = MAX(匹配规则的 multiplier)
  │
  ▼
3. 计算本次消耗次数
   effective_calls = ceil(1 × effective_multiplier)
   （默认 1 次/请求，倍率时段如 ×2.0 = 消耗 2 次）
  │
  ▼
4. 读取 quotas WHERE user_id = ?
  │
  ├── 5h_used + effective_calls > 5h_limit ?
  │     └── YES → 429 "5h quota exceeded. Resets at {next_window}"
  │
  ├── total_used + effective_calls > total_limit ?
  │     └── YES → 429 "Total quota exceeded. Contact admin"
  │
  └── PASS → 执行代理请求
        │
        ├── stream=false (同步):
        │     proxy → parse response
        │     quota_5h_used   += effective_calls
        │     quota_total_used += effective_calls
        │     记录 call_logs: total_tokens=X, effective_calls=N, multiplier_used=M
        │
        └── stream=true (流式):
              预占 effective_calls 次 → 同样按次数计
              （见 6.3 节硬限制策略，单位改为次数）
```

### 8.2 并发配额管理（SQLite）

```sql
-- 原子配额更新（次数计量，利用 SQLite 行锁）
UPDATE quotas
SET quota_5h_used = quota_5h_used + ?,
    quota_total_used = quota_total_used + ?,
    updated_at = ?
WHERE user_id = ?
  AND quota_5h_used + ? <= quota_5h_limit
  AND quota_total_used + ? <= quota_total_limit;

-- 检查 RowsAffected:
--   = 1 → 更新成功，配额准入
--   = 0 → 配额不足，返回 429
```

### 8.3 默认配额值

| 配额类型 | 默认值 | 说明 |
|---|---|---|
| `quota_5h_limit` | 100 次 | 每 5h 窗口上限 |
| `quota_total_limit` | 10,000 次 | 账户总上限 |

---

## 9. 时段倍率系统

### 9.1 功能说明

Admin 可设置**时段倍率规则**：在指定时间段内，每次调用消耗的次数乘以指定倍率计入配额。

**使用场景示例**：
| 规则 | 说明 |
|---|---|
| 周二/周四 14:00-18:00，倍率 2.0 | 高峰期每次调用消耗 2 次配额 |
| 每天 22:00-06:00，倍率 1.0 | 夜间不翻倍 |
| 周三全天，倍率 3.0 | 某天每次调用消耗 3 次配额 |

### 9.2 数据模型

```sql
-- time_multipliers 表（全局规则，Admin 级别管理）
CREATE TABLE time_multipliers (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    start_time    TEXT    NOT NULL,              -- "14:00"（HH:MM 格式）
    end_time      TEXT    NOT NULL,              -- "18:00"
    multiplier    REAL    NOT NULL DEFAULT 1.0,   -- 倍率 ≥ 1.0
    days_of_week  TEXT    NOT NULL DEFAULT '*',   -- '*':每天, '1,2,3,4,5':工作日, '0,6':周末
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT    NOT NULL DEFAULT (datetime('now')),
    CONSTRAINT chk_multiplier CHECK (multiplier >= 1.0),
    CONSTRAINT chk_time CHECK (start_time < end_time)
);
```

### 9.3 倍率匹配算法

```go
// multiplier.go — 时段倍率引擎
func (m *MultiplierEngine) GetEffectiveMultiplier(now time.Time) float64 {
    rules, _ := m.FindAllEnabled()

    currentTime := now.Format("15:04")
    weekday := strconv.Itoa(int(now.Weekday())) // 0=Sun, 1=Mon, ...

    maxMultiplier := 1.0
    for _, rule := range rules {
        // 1. 检查星期匹配
        if !m.matchDay(rule.DaysOfWeek, weekday) {
            continue
        }

        // 2. 检查时间段匹配
        inRange := false
        if rule.StartTime <= rule.EndTime {
            // 正常时段：14:00-18:00
            inRange = currentTime >= rule.StartTime && currentTime < rule.EndTime
        } else {
            // 跨天时段：22:00-06:00
            inRange = currentTime >= rule.StartTime || currentTime < rule.EndTime
        }

        // 3. 命中 → 取最大值
        if inRange && rule.Multiplier > maxMultiplier {
            maxMultiplier = rule.Multiplier
        }
    }
    return maxMultiplier
}

// 调用方使用: effectiveCalls = int(math.Ceil(1 * multiplier))
```

### 9.4 配额计算公式

```
effective_calls = ceil(1 × effective_multiplier)

示例:
- 不在任何倍率时段 → 消耗 1 次
- 时段倍率 = 2.0 → 消耗 2 次
- 同时命中两条规则（2.0 和 3.0）→ 取 MAX = 3.0 → 消耗 3 次

注意：multiplier 下限为 1.0，即最少消耗 1 次/请求
```

### 9.5 call_logs 记录字段

```
call_logs 表配额相关字段:
- total_tokens:      原始 token（上游响应解析，纯统计用途）
- effective_calls:   实际消耗次数 = ceil(1 × multiplier)
- multiplier_used:   命中的倍率值
```

### 9.6 Admin API

#### 创建倍率规则

```
POST /admin/api/multipliers
{
  "start_time": "14:00",
  "end_time": "18:00",
  "multiplier": 2.0,
  "days_of_week": "2,4"     // 周二、周四
}

响应 (201):
{
  "id": 1,
  "start_time": "14:00",
  "end_time": "18:00",
  "multiplier": 2.0,
  "days_of_week": "2,4",
  "enabled": true
}
```

#### 列表/更新/删除

```
GET    /admin/api/multipliers          → [{...}, ...]
PUT    /admin/api/multipliers/1        → 更新规则
DELETE /admin/api/multipliers/1        → 删除规则（硬删除）
```

---

## 10. 子 Key 生成与管理

### 10.1 生成算法

```go
func GenerateSubKey(userID int64) string {
    // 混合因素确保唯一性
    input := fmt.Sprintf("%s:%d:%d:%s",
        secretSalt,       // 服务端固定密钥
        userID,
        time.Now().UnixNano(),
        cryptoRandHex(32), // 32 字节随机数
    )
    hash := sha256.Sum256([]byte(input))
    return "sk-" + hex.EncodeToString(hash[:])[:32]
}

// 示例输出: sk-3f8a2b1c4d5e6f7a8b9c0d1e2f3a4b5c
```

### 10.2 存储策略

```sql
-- 创建用户时
INSERT INTO users (username, sub_key_hash, sub_key_preview)
VALUES ('zhangsan', SHA256('sk-3f8a2...'), 'sk-3f8a2...');
--                            ↑ 存哈希             ↑ 存预览前缀（用于 UI 展示）
```

### 10.3 鉴权流程

```
1. 提取 Authorization: Bearer sk-xxx
2. keyHash = SHA256(sk-xxx)
3. SELECT id, status FROM users WHERE sub_key_hash = ?
4. 无结果 → 401 Invalid API key
5. status='disabled' → 403 Key disabled
6. 通过 → 进入配额检查
```

---

## 11. 项目文件结构

```
llm_api_gateway/
├── main.go                    # 入口：初始化 + 路由注册 + 服务启动
├── go.mod                     # Go module
├── go.sum
├── config.yaml                # 配置文件（模板）
├── config.yaml.example        # 示例配置
├── Makefile                   # 编译/测试/安装
├── install.sh                 # 一键部署脚本
│
├── internal/
│   ├── config/
│   │   └── config.go          # YAML 配置解析
│   │
│   ├── db/
│   │   ├── db.go              # SQLite 连接初始化 + WAL 模式
│   │   └── migrations.go      # 自动建表
│   │
│   ├── models/
│   │   ├── user.go            # User 结构体 + CRUD
│   │   ├── quota.go           # Quota 结构体 + 原子更新
│   │   ├── call_log.go        # CallLog 结构体
│   │   └── session.go         # AdminSession 结构体
│   │
│   ├── auth/
│   │   ├── keygen.go          # 子 Key 生成
│   │   └── middleware.go      # 鉴权中间件（子Key + Admin Session）
│   │
│   ├── quota/
│   │   ├── checker.go         # 配额准入检查 + 原子扣减（含时段倍率应用）
│   │   ├── manager.go         # 配额确认/回滚（流式硬限制）
│   │   ├── scheduler.go       # 5h 窗口定时重置
│   │   └── multiplier.go      # 时段倍率引擎 + CRUD
│   │
│   ├── proxy/
│   │   ├── handler.go         # /v1/chat/completions 代理处理器
│   │   ├── stream.go          # SSE 流式透传
│   │   └── upstream.go        # 上游请求构建（注入真实 Key）
│   │
│   ├── admin/
│   │   ├── handler.go         # Admin API 路由处理
│   │   ├── login.go           # 登录/登出
│   │   ├── users.go           # 用户 CRUD
│   │   ├── calls.go           # 调用记录查询
│   │   └── overview.go        # Dashboard 数据
│   │
│   └── handler/
│       └── quota.go           # /v1/quota 自助查询
│
├── web/
│   └── admin/                 # 管理面板前端（嵌入到二进制）
│       ├── index.html         # Dashboard 页面
│       ├── login.html         # 登录页面
│       ├── style.css          # 样式（Minimal CSS）
│       └── app.js             # 前端逻辑（Vanilla JS + fetch）
│
├── embed.go                   # //go:embed web/admin/* 嵌入静态资源
│
└── deploy/
    ├── llm-gateway.service    # systemd 服务文件
    └── nginx.conf             # Nginx 配置示例
```

---

## 12. 依赖包列表

### go.mod

```
module github.com/yourname/llm_api_gateway

go 1.22.0

require (
    github.com/golang-modern/csqlite v1.x     // 内部路径别名
    modernc.org/sqlite v1.30.0               // 纯 Go SQLite
    gopkg.in/yaml.v3 v3.0.1                   // YAML 解析
    golang.org/x/crypto v0.24.0               // bcrypt
)
```

### 间接依赖

```
modernc.org/sqlite 的传递依赖（纯 Go 实现，自动拉取）:
- modernc.org/libc
- modernc.org/mathutil
- modernc.org/memory
- modernc.org/ccgo/v4
- modernc.org/cc/v4
- modernc.org/tcl
- modernc.org/opt
- modernc.org/strutil

总依赖数: ~15（全部为 modernc 生态，零 C 依赖）
```

---

## 13. 部署方案

### 12.1 部署架构

```
┌─────────────────────────────────┐
│         云服务器 (2C4G)          │
│                                 │
│  ┌───────────────────────────┐  │
│  │ Nginx (:443)              │  │
│  │ - TLS (Let's Encrypt)     │  │
│  │ - 反向代理 → :8080        │  │
│  │ - 静态资源 /admin/        │  │
│  └───────────┬───────────────┘  │
│              │                   │
│  ┌───────────▼───────────────┐  │
│  │ LLM API Gateway (:8080)   │  │
│  │ - 仅监听 127.0.0.1        │  │
│  │ - systemd 管理            │  │
│  └───────────┬───────────────┘  │
│              │                   │
│  ┌───────────▼───────────────┐  │
│  │ SQLite (llm_gateway.db)   │  │
│  └───────────────────────────┘  │
└─────────────────────────────────┘
```

### 12.2 systemd 服务

```ini
# /etc/systemd/system/llm-gateway.service
[Unit]
Description=LLM API Gateway
After=network.target

[Service]
Type=simple
User=llm
Group=llm
WorkingDirectory=/opt/llm-gateway
ExecStart=/opt/llm-gateway/llm_api_gateway -config /opt/llm-gateway/config.yaml
Restart=always
RestartSec=5

# 安全加固
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/opt/llm-gateway

# 环境变量（或从配置文件读取 API Key）
Environment="ZHIPU_API_KEY=your-real-api-key"

[Install]
WantedBy=multi-user.target
```

### 12.3 Nginx 配置

```nginx
# /etc/nginx/sites-available/llm-gateway
server {
    listen 443 ssl http2;
    server_name llm-gateway.example.com;

    ssl_certificate     /etc/letsencrypt/live/llm-gateway.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/llm-gateway.example.com/privkey.pem;

    # 管理面板
    location /admin/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # LLM API（支持 SSE 流式）
    location /v1/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection '';
        proxy_buffering off;
        proxy_cache off;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # 超时设置（LLM 响应可能较长）
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
}

# HTTP → HTTPS 重定向
server {
    listen 80;
    server_name llm-gateway.example.com;
    return 301 https://$server_name$request_uri;
}
```

### 12.4 一键部署脚本（install.sh）

```bash
#!/bin/bash
set -e

APP_DIR="/opt/llm-gateway"
SERVICE_NAME="llm-gateway"

echo "=== LLM API Gateway 部署 ==="

# 1. 编译
echo "[1/6] 编译..."
CGO_ENABLED=0 go build -ldflags="-s -w" -o llm_api_gateway .

# 2. 创建目录
echo "[2/6] 创建目录..."
sudo mkdir -p "$APP_DIR"

# 3. 复制文件
echo "[3/6] 复制文件..."
sudo cp llm_api_gateway "$APP_DIR/"
sudo cp config.yaml.example "$APP_DIR/config.yaml"
sudo cp deploy/llm-gateway.service /etc/systemd/system/

# 4. 创建用户
echo "[4/6] 创建 llm 用户..."
id -u llm &>/dev/null || sudo useradd -r -s /bin/false llm
sudo chown -R llm:llm "$APP_DIR"

# 5. 启动服务
echo "[5/6] 启动服务..."
sudo systemctl daemon-reload
sudo systemctl enable "$SERVICE_NAME"
sudo systemctl start "$SERVICE_NAME"

# 6. 设置 Admin 密码
echo "[6/6] 初始化 Admin..."
echo -n "请输入 Admin 密码: "
read -s ADMIN_PASS
echo
"$APP_DIR/llm_api_gateway" -config "$APP_DIR/config.yaml" -passwd "$ADMIN_PASS"

echo "=== 部署完成 ==="
echo "管理面板: https://<server-ip>/admin/"
echo "子 Key 分发后，用户 API 端点: https://<server-ip>/v1/chat/completions"
```

---

## 14. 任务列表

### T01 — 项目基础设施

| 优先级: P0 | 预估文件: 6 |
|---|---|

| 文件 | 说明 |
|---|---|
| `go.mod` | Module 初始化，核心依赖声明 |
| `internal/config/config.go` | YAML 配置解析（监听地址、DB路径、API Key路径） |
| `internal/db/db.go` | SQLite 连接（WAL 模式、连接池） |
| `internal/db/migrations.go` | 5 张表自动建表 |
| `internal/models/user.go` | User 模型 + CRUD（创建/查询/更新/列表） |
| `internal/models/quota.go` | Quota 模型 + 原子更新操作 |
| `internal/models/call_log.go` | CallLog 模型（含 effective_tokens + multiplier_used 字段）+ 插入/分页查询 |
| `config.yaml.example` | 配置模板 |

### T02 — 核心代理与鉴权

| 优先级: P0 | 依赖: T01 | 预估文件: 5 |

| 文件 | 说明 |
|---|---|
| `internal/auth/keygen.go` | 子 Key 生成（SHA256 + salt） |
| `internal/auth/middleware.go` | 鉴权中间件（子 Key → userID，A→ Admin Session） |
| `internal/proxy/handler.go` | `/v1/chat/completions` 同步代理（非流式） |
| `internal/proxy/stream.go` | SSE 流式透传 + 两阶段配额 |
| `internal/proxy/upstream.go` | 上游请求构建（注入真实 API Key + 请求头） |

### T03 — 配额系统 + 时段倍率

| 优先级: P0 | 依赖: T01, T02 | 预估文件: 5 |

| 文件 | 说明 |
|---|---|
| `internal/quota/checker.go` | 配额准入检查 + 原子扣减（含倍率乘法） |
| `internal/quota/manager.go` | 流式配额预留/确认（硬限制策略） |
| `internal/quota/scheduler.go` | 5h 窗口定时重置 + 启动补偿 |
| `internal/quota/multiplier.go` | 时段倍率引擎（匹配算法 + CRUD） |
| `internal/handler/quota.go` | `/v1/quota` 自助查询端点（返回倍率信息） |

### T04 — 管理面板后端

| 优先级: P1 | 依赖: T01, T02 | 预估文件: 6 |

| 文件 | 说明 |
|---|---|
| `internal/admin/handler.go` | Admin 路由注册（`/admin/api/*`） |
| `internal/admin/login.go` | 登录/登出 + Cookie Session |
| `internal/admin/users.go` | 用户 CRUD API（创建/列表/更新/禁用/重新生成 Key） |
| `internal/admin/calls.go` | 调用记录分页查询（含 effective_tokens 字段） |
| `internal/admin/overview.go` | Dashboard 概览数据聚合 |
| `internal/admin/multipliers.go` | 时段倍率规则 CRUD |
| `internal/models/session.go` | AdminSession 模型 |

### T05 — 管理面板前端 + 部署

| 优先级: P2 | 依赖: T04 | 预估文件: 10 |

| 文件 | 说明 |
|---|---|
| `web/admin/login.html` | Admin 登录页 |
| `web/admin/index.html` | Dashboard 主页（用户列表 + 统计卡片 + 创建弹窗） |
| `web/admin/style.css` | 全局样式（Minimal） |
| `web/admin/app.js` | 前端 JS（fetch API、表格渲染、分页） |
| `embed.go` | `//go:embed` 嵌入前端静态资源 |
| `main.go` | 入口整合：路由注册 + 中间件链 + 调度器 + 服务启动 |
| `Makefile` | 编译/运行/测试目标 |
| `deploy/llm-gateway.service` | systemd 服务文件 |
| `deploy/nginx.conf` | Nginx 配置示例 |
| `install.sh` | 一键部署脚本 |

### 依赖图

```
T01 (基础设施)
 ├──▶ T02 (代理+鉴权) ──┐
 │                       ├──▶ T03 (配额系统)
 │                       │
 ├──▶ T04 (管理后端) ────┤
                         │
                         ▼
                        T05 (前端+部署)
```

### 实现顺序

```
1. T01 → 基础设施就绪
2. T02 + T04 可并行 → 代理鉴权 + 管理 API 独立开发
3. T03 → 配额系统（依赖 T02）
4. T05 → 前端 + 集成 + 部署
```

---

## 15. 待明确事项

| # | 事项 | 影响范围 | 建议方案 |
|---|---|---|---|
| A1 | ✅ **已确认：配额硬限制** | — | 配额耗尽一律拒绝，不做软限制。流式采用预占+硬限策略 |
| A2 | **Admin 密码初始化** | 部署 | 建议通过 `-passwd` 命令行参数或环境变量在首次部署时设置 |
| A3 | **Call Log 保留策略** | 数据库 | MVP 暂不清理，P2 增加按时间清理（如保留 30 天） |
| A4 | **子 Key 明文分发方式** | 安全 | Admin 创建后手动复制/发送；前端仅展示一次 |
| A5 | **多模型路由** | 架构扩展 | T02 预留 model 字段，P2 增加模型→上游 URL 映射 |
| A6 | **Admin 密码修改** | Admin 功能 | P2 增加修改密码 API + 前端页面 |
| A7 | **单 Admin → 多 Admin 迁移** | 用户系统 | 当前 users.role 字段已预留，P2 增加 role 管理 API |

---

> **文档版本**: v1.1 · **最后更新**: 2025-07-09 · **架构师**: 高见远
