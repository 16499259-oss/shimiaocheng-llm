# ADR-0007: Provider Key Encryption at Rest + Database-First Hot Reload

- **日期**：2026-07-11
- **状态**：已采纳
- **取代**：ADR-0002（Key 仅内存存储策略，扩展为加密落库）
- **关联 PRD**：Admin 后台多上游动态管理

---

## 1. 背景

当前网关的 provider API Key 仅存储在环境变量中，启动时注入内存（ADR-0002 策略）。这导致：

1. **Key 无法通过 Admin 后台持久化管理**：新增/修改 Key 只能 SSH 到服务器修改环境变量或 config.yaml
2. **Router 无法感知运行时变更**：`NewRouter` 一次性构建 provider 映射，后续 Admin 操作不生效
3. **多 provider 扩展困难**：每个 provider 需要独立的环境变量和 config.yaml 条目

PRD 要求 Admin 后台支持完整的 Provider/ModelMapping/RoutingRule CRUD，且操作后立即生效（热加载）。

---

## 2. 决策

### 2.1 Key 加密落库

**选择 AES-256-GCM 加密 API Key 存入 SQLite**。

```
格式：[12-byte random nonce][GCM ciphertext + 16-byte tag]
存储：providers.encrypted_key (BLOB)
```

**KEK (Key Encryption Key) 派生**：

```go
kek := sha256.Sum256([]byte(os.Getenv("GATEWAY_KEK_ENV")))  // 32 bytes
```

- KEK 从 `GATEWAY_KEK_ENV` 环境变量 SHA-256 派生，确保任意长度输入 → 固定 32 字节 AES-256 密钥
- 启动时 `GATEWAY_KEK_ENV` 未设置 → **Fatal 退出**，绝不允许明文降级
- KEK 仅加载一次，通过参数注入 `ProviderStore`，不再读取环境变量

**替代方案及拒绝理由**：

| 方案 | 拒绝理由 |
|------|----------|
| AES-256-CBC | 无认证，容易被篡改；GCM 提供 AEAD（认证加密） |
| 明文存 DB | 严重安全风险；DB 文件泄露 = 所有 Key 泄露 |
| 第三方 KMS（Vault/HSM） | 引入额外依赖，不符合零 CGO 简单部署目标；P2 考虑 |
| 环境变量存 Key（现状） | 无法运行时通过 Admin 动态管理 |

### 2.2 DB 优先 + 原子热加载

**选择 `sync/atomic.Value` 持有 ProviderTable 快照**。

```
热路径（每请求）：
  table := r.table.Load().(*ProviderTable)  // 零锁读
  prov := table.Providers[slug]

冷路径（Admin CRUD 后）：
  newTable := store.BuildProviderTable()     // 从 DB 全量加载 + 解密
  r.table.Store(newTable)                    // 原子替换
```

**为什么不用 `sync.RWMutex`？**

- RWMutex 在高并发下仍有读锁开销（虽然共享），每请求获取/释放锁
- `atomic.Value` 零锁读，热路径无竞争；写路径仅在 Admin CRUD 时触发（低频）
- 快照模式天然避免读写冲突：永远读取完整一致的快照

**为什么不用 channel-based 通知？**

- 需要额外的 goroutine 和通知机制，增加复杂度
- atomic.Value 已是 Go 标准方案

### 2.3 前端保持原生 HTML/CSS/JS

延续现有 Admin 面板风格，不引入 React/Vue 等框架。理由：
- 改动范围可控：新增 4 个 Tab 页面（Provider/Mapping/Routing/Audit）
- 团队无前端构建工具链依赖
- 零构建步骤，embed 即可部署

---

## 3. 架构变更

### 变更文件清单

| 操作 | 文件 | 说明 |
|------|------|------|
| **新增** | `internal/security/encrypt.go` | AES-256-GCM 加解密 + KEK 派生 + Key 掩码 |
| **新增** | `internal/security/encrypt_test.go` | 加解密往返测试 |
| **新增** | `internal/models/provider.go` | ProviderRecord/ModelMappingRecord/AuditLogRecord |
| **新增** | `internal/provider/store.go` | ProviderStore：完整 CRUD + 种子迁移 + 快照构建 |
| **新增** | `internal/provider/store_test.go` | Store 单元测试 |
| **新增** | `internal/admin/providers.go` | Provider CRUD API handler |
| **新增** | `internal/admin/mappings.go` | ModelMapping CRUD API handler |
| **新增** | `internal/admin/routing.go` | RoutingRule CRUD API handler |
| **新增** | `internal/admin/audit.go` | AuditLog 查询 handler |
| **修改** | `internal/db/migrations.go` | 新增 providers/model_mappings/audit_logs 三表 DDL |
| **修改** | `internal/router/selector.go` | Router 改造为 atomic.Value + ProviderStore 驱动 |
| **修改** | `internal/config/config.go` | 移除 Load() 中的默认 provider 注入逻辑 |
| **修改** | `main.go` | 加载 KEK → ProviderStore → Seed → Router 接线 |
| **修改** | `internal/admin/handler.go` | Handler 新增 ProviderStore/Router 字段 + 路由注册 |
| **修改** | `web/admin/index.html` | 左侧导航重构 + 新增四个管理页面 |
| **修改** | `web/admin/app.js` | 新增 Provider/Mapping/Routing/Audit 前端逻辑 |
| **修改** | `web/admin/style.css` | 新增 Sidebar/Tab/Toast/Key 掩码样式 |

### 数据流

```
启动:
  main.go → DeriveKEK() → ProviderStore → SeedFromConfig(config.yaml)
  → NewRouter(store) → Reload() → atomic.Store(ProviderTable)

请求:
  POST /v1/chat/completions → Router.ResolveProvider()
  → table.Load().(*ProviderTable) → 零锁读 → 返回 Provider

Admin CRUD:
  POST /admin/api/providers → ProviderStore.CreateProvider()
  → Router.Reload() → BuildProviderTable() → atomic.Store()
```

---

## 4. 后果

### 正面影响

- ✅ Admin 后台可动态管理所有 provider/映射/路由规则，立即生效
- ✅ API Key 加密存储，DB 文件泄露不会直接暴露 Key
- ✅ 热路径零锁竞争，性能无退化
- ✅ 种子迁移幂等（多次启动不重复插入）
- ✅ 向后兼容：现有 config.yaml 在首次启动时自动种子化
- ✅ 删除安全检查：有路由规则/映射引用时拒绝删除

### 负面影响 / 风险

- ⚠️ KEK 轮换需离线脚本重新加密所有 Key（P2 不做）
- ⚠️ `GATEWAY_KEK_ENV` 泄露 = 所有存储 Key 泄露（密钥管理单一故障点）
- ⚠️ `CredentialStore` / `CredentialHolder` 保留但不再驱动路由（标记 Deprecated）
- ⚠️ 审计日志无自动清理（P2 后续加 TTL 策略）

### 迁移路径

1. 部署新版本到服务器
2. 确保 `GATEWAY_KEK_ENV` 环境变量已设置（systemd env）
3. 首次启动自动从 config.yaml 种子化 providers → DB
4. 之后通过 Admin 面板管理，不再依赖 config.yaml 的 providers 段
5. 可逐步删除 config.yaml 中的 providers/model_mappings 段（P2）

---

## 5. 验收标准

- [x] `make ci` 全绿（fmt/vet/test/build-linux/shellcheck）
- [x] `go test -race ./...` 全 PASS
- [x] 加密往返测试通过
- [x] 种子迁移幂等（两次启动不重复插入）
- [x] Router 热路径读取 atomic.Value 无锁
- [x] Admin CRUD 后立即生效（Reload 调用）
- [x] 前端所有页面可正常渲染
