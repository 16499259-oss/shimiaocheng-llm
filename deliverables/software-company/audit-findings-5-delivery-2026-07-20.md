# 交付报告：审计 5 处前端问题修复（PR #24）

- **项目**：shimiaocheng-llm（LLM API Gateway）
- **分支/PR**：`fix/audit-findings-5` → **PR #24** → 合并 main（merge commit `db1b159`）
- **提交**：`7e00287`（4 文件，+190 行）
- **日期**：2026-07-20
- **交付总监**：齐活林（Qi）

---

## TL;DR

全项目功能审计发现的 5 个问题（F5/F1–F4）+ 2 个缺失单测已全部修复，已提交、交叉编译部署上线（PID 759713）、通过「线上实时 JS 资产抓取 + 服务健康 + 单测」验证，并合并至 main。修复代码已确证真实上线。

---

## 一、问题来源

源自 2026-07-19 全项目功能审计（团队 `software-full-audit`），QA 工程师严过关静态走查 + 全量测试后汇总出 5 个问题：

| ID | 严重度 | 问题 | 根因 |
|----|--------|------|------|
| F5 | 🟡 中等 | 上游用量单元格用「用量子查询是否成功」误判「无限额」——`api/provider-usage` 偶发失败时，把有额度上限的上游误显成「不限制」 | `loadProviders` 仅以 `usageMap[slug]` 是否存在判无限 |
| F1 | 🟢 轻微 | 用户表空态 `colspan=11`（应为 13） | 与 thead 列数不一致 |
| F2 | 🟢 轻微 | 调用记录表初始 `colspan=9`（应为 10） | 含 PR #20 新增「用户名」列后未同步 |
| F3 | 🟢 轻微 | 超额时显示负数「剩余」未钳制 | `renderUsageCell` 直接 `limit - used` |
| F4 | 🟢 轻微 | 无限额上游「已分配」列不展示已分配量 | `renderAllocationCell` 在 `limit<=0` 时直接返回「不限制」 |

---

## 二、修复明细

### F5（优先，web/admin/app.js · `loadProviders`）
判断「真无限」改用上游**自身** limit 字段（`api/providers` 已返回 `monthly_token_limit` / `monthly_call_limit`）：
- `p.monthly_token_limit <= 0` → 显示「不限制」；
- `> 0` 但用量数据缺失（`u` 为 undefined）→ 显示中性占位「获取失败」，**绝不**显示「不限制」。
- call 列同理用 `p.monthly_call_limit`。
- **纯前端修复，未动后端**。

### F3（web/admin/app.js · `renderUsageCell`）
```js
const remaining = Math.max(0, limit - used);   // 原：const remaining = limit - used;
```
超额时剩余钳制为 0，不再出现负数；进度条 pct 逻辑不变。

### F4（web/admin/app.js · `renderAllocationCell`）
`limit <= 0` 时不再隐藏已分配量，改为仍展示并标注：
```js
return `<div class="usage-cell"><span class="usage-nums">${numFmt(allocated)}</span> <span class="usage-unlimited">（无限上限）</span></div>`;
```

### F1（web/admin/app.js · `loadUsers` 空态）
`colspan="11"` → `colspan="13"`（对齐用户表 13 列 thead）。

### F2（web/admin/index.html · `cs-table` 加载态）
`colspan="9"` → `colspan="10"`（对齐调用记录表 10 列 thead）。
- 额外核对：`calls-tbody` 的 `colspan="8"` 属另一张 8 列表，按指示保持不变；`cs-tbody` 的 JS 空态原已是 `colspan="10"`，无需改动。

---

## 三、测试补充（2 处）

| 文件 | 测试 | 目的 |
|------|------|------|
| `internal/admin/providers_test.go`（新建） | `TestHandleListProviders_ReturnsMonthlyLimits` | 校验 `GET /api/providers` 响应对象**必含** `monthly_token_limit` / `monthly_call_limit`（F5 降级判断的后端契约保障） |
| `internal/admin/users_test.go`（追加） | `TestAdminUpdateUser_ZeroTokenTotalLimitAllowed` | token 配额 `0`=无限 → HTTP 200（正向用例，complement count 配额 `0`→400 拒绝契约） |

> 注：`quota_5h_limit:0` 与 `quota_total_limit:0` → 400 的两条用例 `TestAdminUpdateUser_Zero5hLimitRejected` / `ZeroTotalLimitRejected` 已存在于 `users_test.go`，本次运行确认 PASS，未重复创建。

---

## 四、质量关卡（双层核验）

主理人未直接采信工程师结论，亲自以 `git diff` + 重编译 + 重跑测试核验：

### 编译
```
CGO_ENABLED=0 /opt/homebrew/bin/go build -o /dev/null .   → BUILD_OK
```

### 单测（4 个相关用例全 PASS）
```
TestHandleListProviders_ReturnsMonthlyLimits         PASS
TestAdminUpdateUser_ZeroTokenTotalLimitAllowed       PASS
TestAdminUpdateUser_Zero5hLimitRejected              PASS
TestAdminUpdateUser_ZeroTotalLimitRejected           PASS
go test ./...  → 全量 ok（13 包）
```

### 部署前 git diff 核验
改动与工程师汇报一致（`web/admin/app.js`、`web/admin/index.html`、`internal/admin/users_test.go` 修改 + `internal/admin/providers_test.go` 新建未跟踪）。

---

## 五、部署与验证

### 部署动作
1. `git checkout -b fix/audit-findings-5` + commit `7e00287`
2. 交叉编译：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 /opt/homebrew/bin/go build -ldflags="-s -w" -o llm_api_gateway .`
3. scp → `cp .bak` → `mv .new` → `chown llm-gw:llm-gw` → `systemctl restart llm-gateway` → `nginx -t && reload`
4. 新进程 **PID 759713**（10:34:34 重启，`Database migrations completed successfully`）

### 线上验证（强验证：实时抓取服务器资产）
从线上服务器实际抓取 `/m-7xa2/static/app.js`（72KB, HTTP 200），5 处修复关键字全部命中：

| 修复 | 关键字 | 线上 JS 命中 |
|------|--------|------|
| F5 | `获取失败` | ×6 |
| F3 | `Math.max(0` | ×1 |
| F4 | `无限上限` | ×1 |
| F1 | `colspan="13"` | ×1 |
| F2 | `colspan="10"` | ×1 |

### 服务健康
| 检查 | 结果 |
|------|------|
| 新进程 / migrations | ✅ PID 759713, migrations OK |
| `/user/` | ✅ 200 |
| `/m-7xa2/` | ✅ 303 → login |

### 合并
- `git push -u origin fix/audit-findings-5` → `gh pr create --base main` → **PR #24**
- `gh pr merge 24 --merge` → merge commit `db1b159`
- 本地 main fast-forward 同步；本地 + 远程分支已删除
- **main 现与线上部署版本完全一致**

---

## 六、交付概览

| 项 | 状态 |
|----|------|
| 交付状态 | ✅ 已提交、已部署、已合并 main |
| 测试通过率 | 4/4 相关用例 PASS；`go test ./...` 全绿 |
| 已知阻断问题 | 0（5 个问题全部修复） |
| 遗留非缺陷项 | 历史 2 个 P3（文档/命名偏差，不影响功能） |

---

## 七、文件清单

| 文件 | 状态 | 关联问题 |
|------|------|----------|
| `web/admin/app.js` | 修改 | F5 / F3 / F4 / F1 |
| `web/admin/index.html` | 修改 | F2 |
| `internal/admin/users_test.go` | 修改 | 测试缺口 |
| `internal/admin/providers_test.go` | 新建 | 测试缺口（F5 契约） |

---

## 八、剩余建议

1. **浏览器眼检（待办）**：当前环境无浏览器自动化技能，admin 登录后的上游管理页交互渲染（F5「获取失败」/F4 已分配量等）建议真人走查一次，或提供 admin 密码由主理人代验。
2. **一致性**：main 已与线上部署版本一致，无需额外 re-deploy。
3. **旧团队清理**：历史审计团队 `software-full-audit-5f1e`（报告曾在消息通道丢失）已清理。
