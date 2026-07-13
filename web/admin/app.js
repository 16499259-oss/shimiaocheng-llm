// LLM API Gateway — Admin Panel JavaScript

let currentCallsUserId = null;
let currentCallsPage = 1;
let currentAuditPage = 1;
let providerMap = {}; // slug -> name for dropdowns

// ===== Initialization =====
document.addEventListener('DOMContentLoaded', () => {
    loadOverview();
    setupEventListeners();

    // Restore tab from URL hash, default to dashboard
    const hash = location.hash.replace(/^#/, '');
    const initialTab = VALID_TABS.includes(hash) ? hash : 'dashboard';
    switchTab(initialTab);
});

// ===== Event Listeners =====
function setupEventListeners() {
    document.getElementById('logout-btn').addEventListener('click', logout);
    document.getElementById('create-user-btn').addEventListener('click', () => showModal('create-user-modal'));
    document.getElementById('refresh-users-btn').addEventListener('click', refreshUsers);
    document.getElementById('create-user-form').addEventListener('submit', createUser);
    document.getElementById('update-user-form').addEventListener('submit', updateUser);
    document.getElementById('extend-user-form').addEventListener('submit', submitExtend);
    document.getElementById('close-calls-btn').addEventListener('click', () => {
        document.getElementById('calls-section').style.display = 'none';
    });
    document.getElementById('create-multiplier-btn').addEventListener('click', () => showModal('create-multiplier-modal'));
    document.getElementById('create-multiplier-form').addEventListener('submit', createMultiplier);
    document.getElementById('provider-form').addEventListener('submit', saveProvider);
    document.getElementById('mapping-form').addEventListener('submit', saveMapping);
    document.getElementById('routing-form').addEventListener('submit', saveRoutingRule);
    // Show/hide custom date picker in create user modal
    document.getElementById('new-expiry-type').addEventListener('change', function() {
        document.getElementById('new-expiry-date-group').style.display = this.value === 'custom' ? '' : 'none';
    });
}

// ===== Tab Switching =====
const VALID_TABS = ['dashboard', 'users', 'providers', 'mappings', 'routing', 'multipliers', 'audit'];

function switchTab(tabName) {
    // Update nav active state
    document.querySelectorAll('.nav-item').forEach(el => el.classList.remove('active'));
    const navItem = document.querySelector(`.nav-item[data-tab="${tabName}"]`);
    if (navItem) navItem.classList.add('active');

    // Show/hide content
    document.querySelectorAll('.tab-content').forEach(el => el.classList.remove('active'));
    const content = document.getElementById('tab-' + tabName);
    if (content) content.classList.add('active');

    // Lazy load tab data
    switch (tabName) {
        case 'dashboard': loadOverview(); break;
        case 'users': loadUsers(); break;
        case 'providers': loadProviders(); break;
        case 'mappings': loadMappings(); break;
        case 'routing': loadRoutingRules(); break;
        case 'multipliers': loadMultipliers(); break;
        case 'audit': loadAuditLogs(); break;
    }

    // Persist current tab in URL hash for refresh resilience
    location.hash = tabName;
}

// ===== API Helpers =====
async function apiFetch(url, options = {}) {
    const resp = await fetch(url, { credentials: 'same-origin', ...options });
    if (resp.status === 401) { window.location.href = 'login'; throw new Error('Not authenticated'); }
    if (resp.status === 204) return null;
    return resp.json();
}

// ===== Toast =====
function showToast(message, type) {
    type = type || 'info';
    const container = document.getElementById('toast-container');
    const toast = document.createElement('div');
    toast.className = 'toast toast-' + type;
    toast.textContent = message;
    container.appendChild(toast);
    setTimeout(() => { toast.style.opacity = '0'; setTimeout(() => toast.remove(), 300); }, 3000);
}

// ===== Overview =====
async function loadOverview() {
    try {
        const data = await apiFetch('api/overview');
        document.getElementById('stat-total-users').textContent = data.total_users;
        document.getElementById('stat-active-users').textContent = data.active_users;
        document.getElementById('stat-total-calls').textContent = data.total_calls;
        document.getElementById('stat-calls-today').textContent = data.total_calls_today;
        document.getElementById('stat-tokens-today').textContent = data.total_tokens_today.toLocaleString();
        document.getElementById('stat-avg-latency').textContent = data.avg_latency_ms + ' ms';
        document.getElementById('stat-expiring-soon').textContent = data.expiring_soon || 0;
    } catch (err) { console.error('Failed to load overview:', err); }
}

// ===== Providers =====
async function loadProviders() {
    try {
        const data = await apiFetch('api/providers');
        const tbody = document.getElementById('providers-tbody');
        if (!data || !data.data || data.data.length === 0) {
            tbody.innerHTML = '<tr><td colspan="9" class="text-center">暂无上游</td></tr>';
            return;
        }
        providerMap = {};
        data.data.forEach(p => { providerMap[p.slug] = p.name; });

        tbody.innerHTML = data.data.map(p => {
            const defBadge = p.is_default ? ' <span class="badge badge-success">默认</span>' : '';
            const statusBadge = p.enabled
                ? '<span class="badge badge-active">启用</span>'
                : '<span class="badge badge-disabled">禁用</span>';
            return `<tr>
                <td>${p.id}</td>
                <td>${escapeHtml(p.name)}${defBadge}</td>
                <td><code>${escapeHtml(p.slug)}</code></td>
                <td><span class="endpoint-text" title="${escapeHtml(p.endpoint)}">${escapeHtml(truncateUrl(p.endpoint))}</span></td>
                <td><span class="key-masked">${escapeHtml(p.masked_key)}</span></td>
                <td>${p.is_default ? '✅' : '-'}</td>
                <td>${statusBadge}</td>
                <td>${formatDate(p.created_at)}</td>
                <td>
                    <div class="btn-group">
                        <button class="btn btn-outline btn-sm" onclick="editProvider('${escapeAttr(p.slug)}','${escapeAttr(p.name)}','${escapeAttr(p.endpoint)}',${p.is_default},${p.enabled})">编辑</button>
                        <button class="btn btn-danger btn-sm" onclick="deleteProvider('${escapeAttr(p.slug)}','${escapeAttr(p.name)}')">删除</button>
                    </div>
                </td>
            </tr>`;
        }).join('');
    } catch (err) { console.error('Failed to load providers:', err); }
}

function openProviderModal(slug) {
    document.getElementById('provider-modal-title').textContent = slug ? '编辑上游' : '新增上游';
    document.getElementById('prov-edit-slug').value = slug || '';
    if (!slug) {
        document.getElementById('prov-name').value = '';
        document.getElementById('prov-slug').value = '';
        document.getElementById('prov-slug').disabled = false;
        document.getElementById('prov-endpoint').value = '';
        document.getElementById('prov-apikey').value = '';
        document.getElementById('prov-is-default').checked = false;
    }
    showModal('provider-modal');
}

function editProvider(slug, name, endpoint, isDefault, enabled) {
    document.getElementById('provider-modal-title').textContent = '编辑上游';
    document.getElementById('prov-edit-slug').value = slug;
    document.getElementById('prov-name').value = name;
    document.getElementById('prov-slug').value = slug;
    document.getElementById('prov-slug').disabled = true;
    document.getElementById('prov-endpoint').value = endpoint;
    document.getElementById('prov-apikey').value = '';
    document.getElementById('prov-apikey').placeholder = '留空则不修改';
    document.getElementById('prov-is-default').checked = isDefault;
    showModal('provider-modal');
}

async function saveProvider(e) {
    e.preventDefault();
    const editSlug = document.getElementById('prov-edit-slug').value;
    const isEdit = !!editSlug;
    const name = document.getElementById('prov-name').value.trim();
    const slug = document.getElementById('prov-slug').value.trim();
    const endpoint = document.getElementById('prov-endpoint').value.trim();
    const apiKey = document.getElementById('prov-apikey').value.trim();
    const isDefault = document.getElementById('prov-is-default').checked;

    if (!name || !endpoint) { showToast('名称和端点为必填项', 'error'); return; }
    if (!isEdit && !slug) { showToast('Slug 为必填项', 'error'); return; }

    try {
        if (isEdit) {
            const body = { name, endpoint };
            if (apiKey) body.api_key = apiKey;
            if (isDefault) body.is_default = true;
            await apiFetch('api/providers/' + encodeURIComponent(editSlug), {
                method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
            });
            showToast('上游已更新', 'success');
        } else {
            await apiFetch('api/providers', {
                method: 'POST', headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ name, slug, endpoint, api_key: apiKey, is_default: isDefault }),
            });
            showToast('上游已创建', 'success');
        }
        closeModal('provider-modal');
        loadProviders();
    } catch (err) { showToast('操作失败: ' + err.message, 'error'); }
}

async function deleteProvider(slug, name) {
    if (!confirm(`确定要删除上游「${name}」吗？如有路由规则引用将无法删除。`)) return;
    try {
        await apiFetch('api/providers/' + encodeURIComponent(slug), { method: 'DELETE' });
        showToast('上游已删除', 'success');
        loadProviders();
    } catch (err) { showToast('删除失败: ' + err.message, 'error'); }
}

// ===== Model Mappings =====
async function loadMappings() {
    try {
        const data = await apiFetch('api/mappings');
        const tbody = document.getElementById('mappings-tbody');
        if (!data || !data.data || data.data.length === 0) {
            tbody.innerHTML = '<tr><td colspan="6" class="text-center">暂无映射</td></tr>';
            return;
        }
        tbody.innerHTML = data.data.map(m => `<tr>
            <td>${m.id}</td>
            <td><code>${escapeHtml(m.external)}</code></td>
            <td><span class="badge badge-enabled">${escapeHtml(m.provider_id)}</span></td>
            <td><code>${escapeHtml(m.real_model)}</code></td>
            <td>${formatDate(m.created_at)}</td>
            <td><button class="btn btn-danger btn-sm" onclick="deleteMapping(${m.id})">删除</button></td>
        </tr>`).join('');
    } catch (err) { console.error('Failed to load mappings:', err); }
}

function openMappingModal() {
    // Populate provider dropdown
    const sel = document.getElementById('map-provider');
    sel.innerHTML = Object.entries(providerMap).map(([slug, name]) =>
        `<option value="${escapeHtml(slug)}">${escapeHtml(name)}</option>`
    ).join('');
    document.getElementById('map-external').value = '';
    document.getElementById('map-real-model').value = '';
    showModal('mapping-modal');
}

async function saveMapping(e) {
    e.preventDefault();
    const body = {
        external: document.getElementById('map-external').value.trim(),
        provider_id: document.getElementById('map-provider').value,
        real_model: document.getElementById('map-real-model').value.trim(),
    };
    if (!body.external || !body.provider_id || !body.real_model) {
        showToast('所有字段为必填项', 'error'); return;
    }
    try {
        await apiFetch('api/mappings', {
            method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
        });
        showToast('映射已创建', 'success');
        closeModal('mapping-modal');
        loadMappings();
    } catch (err) { showToast('创建失败: ' + err.message, 'error'); }
}

async function deleteMapping(id) {
    if (!confirm('确定删除此映射吗？')) return;
    try {
        await apiFetch('api/mappings/' + id, { method: 'DELETE' });
        showToast('映射已删除', 'success');
        loadMappings();
    } catch (err) { showToast('删除失败: ' + err.message, 'error'); }
}

// ===== Routing Rules =====
async function loadRoutingRules() {
    try {
        const data = await apiFetch('api/routing-rules');
        const tbody = document.getElementById('routing-rules-tbody');
        if (!data || !data.data || data.data.length === 0) {
            tbody.innerHTML = '<tr><td colspan="7" class="text-center">暂无路由规则</td></tr>';
            return;
        }
        tbody.innerHTML = data.data.map(r => {
            const s = r.enabled
                ? '<span class="badge badge-active">启用</span>'
                : '<span class="badge badge-disabled">禁用</span>';
            return `<tr>
                <td>${r.id}</td>
                <td><span class="badge badge-enabled">${escapeHtml(r.provider_id)}</span></td>
                <td>${r.start_time}</td>
                <td>${r.end_time}</td>
                <td>${formatDaysOfWeek(r.days_of_week)}</td>
                <td>${s}</td>
                <td>
                    <div class="btn-group">
                        <button class="btn btn-outline btn-sm" onclick="toggleRoutingRule(${r.id}, ${!r.enabled})">${r.enabled ? '禁用' : '启用'}</button>
                        <button class="btn btn-danger btn-sm" onclick="deleteRoutingRule(${r.id})">删除</button>
                    </div>
                </td>
            </tr>`;
        }).join('');
    } catch (err) { console.error('Failed to load routing rules:', err); }
}

function openRoutingRuleModal() {
    const sel = document.getElementById('routing-provider');
    sel.innerHTML = Object.entries(providerMap).map(([slug, name]) =>
        `<option value="${escapeHtml(slug)}">${escapeHtml(name)}</option>`
    ).join('');
    document.getElementById('routing-start').value = '';
    document.getElementById('routing-end').value = '';
    document.getElementById('routing-days').value = '*';
    showModal('routing-modal');
}

async function saveRoutingRule(e) {
    e.preventDefault();
    const body = {
        provider_id: document.getElementById('routing-provider').value,
        start_time: document.getElementById('routing-start').value.trim(),
        end_time: document.getElementById('routing-end').value.trim(),
        days_of_week: document.getElementById('routing-days').value,
        enabled: true,
    };
    if (!body.provider_id || !body.start_time || !body.end_time) {
        showToast('Provider、开始时间和结束时间为必填项', 'error'); return;
    }
    try {
        await apiFetch('api/routing-rules', {
            method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
        });
        showToast('路由规则已创建', 'success');
        closeModal('routing-modal');
        loadRoutingRules();
    } catch (err) { showToast('创建失败: ' + err.message, 'error'); }
}

async function toggleRoutingRule(id, enable) {
    try {
        await apiFetch('api/routing-rules/' + id, {
            method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: enable }),
        });
        loadRoutingRules();
    } catch (err) { showToast('操作失败: ' + err.message, 'error'); }
}

async function deleteRoutingRule(id) {
    if (!confirm('确定删除此路由规则吗？')) return;
    try {
        await apiFetch('api/routing-rules/' + id, { method: 'DELETE' });
        showToast('路由规则已删除', 'success');
        loadRoutingRules();
    } catch (err) { showToast('删除失败: ' + err.message, 'error'); }
}

// ===== Audit Logs =====
async function loadAuditLogs(page) {
    currentAuditPage = page || 1;
    try {
        const data = await apiFetch('api/audit-logs?page=' + currentAuditPage + '&limit=20');
        const tbody = document.getElementById('audit-tbody');
        if (!data || !data.data || data.data.length === 0) {
            tbody.innerHTML = '<tr><td colspan="6" class="text-center">暂无审计记录</td></tr>';
            document.getElementById('audit-pagination').innerHTML = '';
            return;
        }
        tbody.innerHTML = data.data.map(l => `<tr>
            <td>${l.id}</td>
            <td><span class="badge badge-warning">${escapeHtml(l.action)}</span></td>
            <td>${escapeHtml(l.target_type)}</td>
            <td>${escapeHtml(l.target_id)}</td>
            <td><span class="audit-detail">${escapeHtml(l.detail || '-')}</span></td>
            <td>${formatDate(l.created_at)}</td>
        </tr>`).join('');

        const totalPages = Math.ceil(data.total / data.limit);
        document.getElementById('audit-pagination').innerHTML = `
            <button ${currentAuditPage <= 1 ? 'disabled' : ''} onclick="loadAuditLogs(${currentAuditPage - 1})">上一页</button>
            <span>第 ${data.page} / ${totalPages} 页 (共 ${data.total} 条)</span>
            <button ${currentAuditPage >= totalPages ? 'disabled' : ''} onclick="loadAuditLogs(${currentAuditPage + 1})">下一页</button>
        `;
    } catch (err) { console.error('Failed to load audit logs:', err); }
}

// ===== Users (keep existing) =====
async function shareUser(username, id) {
    if (!confirm(`查看 ${username} 的 Key 需要重新生成密钥（旧 Key 将立即失效），确定继续？`)) return;
    document.getElementById('subkey-value').textContent = '正在生成新 Key...';
    document.getElementById('share-all-text').value = '加载中...';
    showModal('subkey-modal');
    try {
        const data = await apiFetch('api/users/' + id, {
            method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ regenerate_key: true }),
        });
        if (data && data.new_sub_key) {
            document.getElementById('subkey-value').textContent = data.new_sub_key;
            document.getElementById('share-all-text').value = buildShareText(data.new_sub_key);
        } else {
            document.getElementById('subkey-value').textContent = '生成失败，请重试';
        }
        loadUsers();
    } catch (err) { document.getElementById('subkey-value').textContent = '生成失败: ' + err.message; }
}

function buildShareText(subKey) {
    return ['你的 LLM API 账户已开通！', '', 'Key：' + subKey, '',
        'Base URL：https://ai.shimiaocheng.top/v1', '   （Cursor / ChatBox / 所有 OpenAI 客户端均填此项）', '',
        '模型名称：GLM-5.2', '', '自助面板（查余量、看用法）：', '   https://ai.shimiaocheng.top/user/'].join('\n');
}

async function loadUsers() {
    try {
        const data = await apiFetch('api/users');
        const tbody = document.getElementById('users-tbody');
        if (!data.data || data.data.length === 0) { tbody.innerHTML = '<tr><td colspan="10" class="text-center">暂无用户</td></tr>'; return; }
        const now = new Date();
        tbody.innerHTML = data.data.map(u => {
            const quota5h = `${u.quota_5h_used} / ${u.quota_5h_limit}`;
            const quotaTotal = `${u.quota_total_used.toLocaleString()} / ${u.quota_total_limit.toLocaleString()}`;
            const tokens = (u.total_tokens || 0).toLocaleString();
            let s = u.status === 'active' ? '<span class="badge badge-active">启用</span>' : '<span class="badge badge-disabled">禁用</span>';
            // Expiry cell
            let expiryHtml = '永久';
            let rowClass = '';
            if (u.expires_at) {
                const expDate = new Date(u.expires_at);
                expiryHtml = expDate.toLocaleDateString('zh-CN');
                if (expDate < now) {
                    rowClass = ' class="row-expired"';
                    s = '<span class="badge badge-error">已过期</span>';
                } else if ((expDate - now) < 7 * 86400000) {
                    expiryHtml = `<span class="text-warning">${expiryHtml}</span>`;
                }
            }
            return `<tr${rowClass}>
                <td>${u.id}</td><td>${escapeHtml(u.username)}</td><td><code>${escapeHtml(u.sub_key_preview)}</code></td>
                <td>${quota5h}</td><td>${quotaTotal}</td><td>${tokens}</td><td>${expiryHtml}</td><td>${s}</td><td>${formatDate(u.created_at)}</td>
                <td><div class="btn-group">
                    <button class="btn btn-outline btn-sm" onclick="extendUser(${u.id},'${escapeAttr(u.username)}','${escapeAttr(u.expires_at || '')}')">🕐 延期</button>
                    <button class="btn btn-outline btn-sm" onclick="shareUser('${escapeAttr(u.username)}',${u.id})">📋 分享</button>
                    <button class="btn btn-outline btn-sm" onclick="editUser(${u.id},'${escapeAttr(u.status)}',${u.quota_5h_limit},${u.quota_total_limit})">编辑</button>
                    <button class="btn btn-outline btn-sm" onclick="viewCalls(${u.id},'${escapeAttr(u.username)}')">记录</button>
                    <button class="btn btn-danger btn-sm" onclick="deleteUser(${u.id},'${escapeAttr(u.username)}')">删除</button>
                </div></td>
            </tr>`;
        }).join('');
    } catch (err) { console.error('Failed to load users:', err); }
}

function refreshUsers() {
    loadUsers();
}

async function createUser(e) {
    e.preventDefault();
    const username = document.getElementById('new-username').value.trim();
    const q5 = parseInt(document.getElementById('new-quota-5h').value) || 100;
    const qt = parseInt(document.getElementById('new-quota-total').value) || 10000;
    // Calculate expires_at
    const expiryType = document.getElementById('new-expiry-type').value;
    let expiresAt = '';
    if (expiryType === '7' || expiryType === '30') {
        const days = parseInt(expiryType);
        expiresAt = new Date(Date.now() + days * 86400000).toISOString();
    } else if (expiryType === 'custom') {
        const dateVal = document.getElementById('new-expiry-date').value;
        if (dateVal) {
            expiresAt = new Date(dateVal + 'T00:00:00+08:00').toISOString();
        }
    }
    try {
        const data = await apiFetch('api/users', { method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ username, quota_5h_limit: q5, quota_total_limit: qt, expires_at: expiresAt }) });
        if (data.sub_key) {
            document.getElementById('subkey-value').textContent = data.sub_key;
            document.getElementById('share-all-text').value = buildShareText(data.sub_key);
            closeModal('create-user-modal'); showModal('subkey-modal');
            document.getElementById('create-user-form').reset();
        } else if (data.error) {
            const resultEl = document.getElementById('create-user-result');
            resultEl.textContent = '创建失败: ' + data.error;
            resultEl.classList.remove('hidden');
            return;
        }
        loadUsers(); loadOverview();
    } catch (err) { document.getElementById('create-user-result').textContent = '创建失败: ' + err.message; document.getElementById('create-user-result').classList.remove('hidden'); }
}

function editUser(id, status, q5, qt) {
    document.getElementById('update-user-id').value = id;
    document.getElementById('update-quota-5h').value = q5;
    document.getElementById('update-quota-total').value = qt;
    document.getElementById('update-status').value = '';
    document.getElementById('update-regenerate-key').checked = false;
    document.getElementById('update-user-result').classList.add('hidden');
    showModal('update-user-modal');
}

async function updateUser(e) {
    e.preventDefault();
    const id = document.getElementById('update-user-id').value;
    const body = {};
    const q5 = document.getElementById('update-quota-5h').value;
    if (q5) body.quota_5h_limit = parseInt(q5);
    const qt = document.getElementById('update-quota-total').value;
    if (qt) body.quota_total_limit = parseInt(qt);
    const st = document.getElementById('update-status').value;
    if (st) body.status = st;
    body.regenerate_key = document.getElementById('update-regenerate-key').checked;
    if (body.regenerate_key && !confirm('确定要重新生成 Key 吗？旧 Key 将立即失效。')) return;
    try {
        const data = await apiFetch('api/users/' + id, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        if (data && data.new_sub_key) {
            document.getElementById('subkey-value').textContent = data.new_sub_key;
            document.getElementById('share-all-text').value = buildShareText(data.new_sub_key);
            closeModal('update-user-modal'); showModal('subkey-modal');
        } else { closeModal('update-user-modal'); }
        loadUsers(); loadOverview();
    } catch (err) { document.getElementById('update-user-result').textContent = '更新失败: ' + err.message; document.getElementById('update-user-result').classList.remove('hidden'); }
}

async function deleteUser(id, username) {
    if (!confirm(`确定要删除用户「${username}」吗？`)) return;
    try { await apiFetch('api/users/' + id, { method: 'DELETE' }); loadUsers(); loadOverview(); }
    catch (err) { alert('删除失败: ' + err.message); }
}

// ===== Extend User Expiry =====
function extendUser(id, username, currentExpiry) {
    document.getElementById('extend-user-id').value = id;
    document.getElementById('extend-username').textContent = username;
    const expiryDisplay = currentExpiry ? formatDate(currentExpiry) : '永久';
    document.getElementById('extend-current-expiry').textContent = expiryDisplay;
    // Store current expiry for preview calculation (P1-2).
    window._extendCurrentExpiry = currentExpiry || '';
    // Reset to default: +30 days
    const radio30 = document.querySelector('input[name="extend-type"][value="30"]');
    if (radio30) radio30.checked = true;
    document.getElementById('extend-custom-date').value = '';
    document.getElementById('extend-custom-date-group').style.display = 'none';
    updateExtendPreview();
    showModal('extend-user-modal');
}

function updateExtendPreview() {
    const typeEl = document.querySelector('input[name="extend-type"]:checked');
    if (!typeEl) return;
    const type = typeEl.value;
    const customDateGroup = document.getElementById('extend-custom-date-group');
    const preview = document.getElementById('extend-preview-text');

    if (type === 'custom') {
        customDateGroup.style.display = '';
        const dateVal = document.getElementById('extend-custom-date').value;
        preview.textContent = dateVal || '请选择日期';
    } else {
        customDateGroup.style.display = 'none';
        const days = parseInt(type);
        // P1-2: base on existing expires_at if available, otherwise NOW.
        const currentExpiry = window._extendCurrentExpiry || '';
        const baseDate = currentExpiry ? new Date(currentExpiry) : new Date();
        const newDate = new Date(baseDate.getTime() + days * 86400000);
        preview.textContent = newDate.toLocaleDateString('zh-CN');
    }
}

async function submitExtend(e) {
    e.preventDefault();
    const id = document.getElementById('extend-user-id').value;
    const typeEl = document.querySelector('input[name="extend-type"]:checked');
    if (!typeEl) { showToast('请选择延期方式', 'error'); return; }
    const type = typeEl.value;

    let body = {};
    if (type === 'custom') {
        const dateVal = document.getElementById('extend-custom-date').value;
        if (!dateVal) { showToast('请选择日期', 'error'); return; }
        body.until = new Date(dateVal + 'T00:00:00+08:00').toISOString();
    } else {
        body.days = parseInt(type);
    }

    try {
        const data = await apiFetch('api/users/' + id + '/extend', {
            method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
        });
        closeModal('extend-user-modal');
        showToast(data.message || '延期成功', 'success');
        loadUsers(); loadOverview();
    } catch (err) { showToast('延期失败: ' + err.message, 'error'); }
}

async function viewCalls(userId, username) {
    currentCallsUserId = userId; currentCallsPage = 1;
    document.getElementById('calls-username').textContent = username;
    document.getElementById('calls-section').style.display = '';
    await loadCalls();
}

async function loadCalls() {
    if (!currentCallsUserId) return;
    try {
        const data = await apiFetch('api/users/' + currentCallsUserId + '/calls?page=' + currentCallsPage + '&limit=20');
        const tbody = document.getElementById('calls-tbody');
        if (!data.data || data.data.length === 0) { tbody.innerHTML = '<tr><td colspan="8" class="text-center">暂无调用记录</td></tr>'; document.getElementById('calls-pagination').innerHTML = ''; return; }
        tbody.innerHTML = data.data.map(c => `<tr>
            <td>${c.id}</td><td>${escapeHtml(c.model)}</td><td>${c.total_tokens.toLocaleString()}</td>
            <td>${c.effective_calls}</td><td>${c.multiplier_used.toFixed(1)}x</td>
            <td>${c.status_code}</td><td>${c.latency_ms}</td><td>${formatDate(c.created_at)}</td></tr>`).join('');
        const pag = data.pagination;
        const tp = Math.ceil(pag.total / pag.limit);
        document.getElementById('calls-pagination').innerHTML =
            `<button ${pag.page <= 1 ? 'disabled' : ''} onclick="goToCallsPage(${pag.page - 1})">上一页</button>
             <span>第 ${pag.page} / ${tp} 页 (共 ${pag.total} 条)</span>
             <button ${pag.page >= tp ? 'disabled' : ''} onclick="goToCallsPage(${pag.page + 1})">下一页</button>`;
    } catch (err) { console.error('Failed to load calls:', err); }
}

function goToCallsPage(page) { currentCallsPage = page; loadCalls(); }

// ===== Multipliers =====
async function loadMultipliers() {
    try {
        const data = await apiFetch('api/multipliers');
        const tbody = document.getElementById('multipliers-tbody');
        if (!data.data || data.data.length === 0) { tbody.innerHTML = '<tr><td colspan="7" class="text-center">暂无倍率规则</td></tr>'; return; }
        tbody.innerHTML = data.data.map(m => {
            const s = m.enabled ? '<span class="badge badge-active">启用</span>' : '<span class="badge badge-disabled">禁用</span>';
            return `<tr>
                <td>${m.id}</td><td>${m.start_time}</td><td>${m.end_time}</td><td>${m.multiplier.toFixed(1)}x</td>
                <td>${formatDaysOfWeek(m.days_of_week)}</td><td>${s}</td>
                <td><div class="btn-group">
                    <button class="btn btn-outline btn-sm" onclick="toggleMultiplier(${m.id},${!m.enabled})">${m.enabled ? '禁用' : '启用'}</button>
                    <button class="btn btn-danger btn-sm" onclick="deleteMultiplier(${m.id})">删除</button>
                </div></td></tr>`;
        }).join('');
    } catch (err) { console.error('Failed to load multipliers:', err); }
}

async function createMultiplier(e) {
    e.preventDefault();
    const body = { start_time: document.getElementById('mult-start-time').value, end_time: document.getElementById('mult-end-time').value,
        multiplier: parseFloat(document.getElementById('mult-multiplier').value), days_of_week: document.getElementById('mult-days').value };
    try {
        await apiFetch('api/multipliers', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        closeModal('create-multiplier-modal'); document.getElementById('create-multiplier-form').reset(); loadMultipliers();
    } catch (err) { alert('创建失败: ' + err.message); }
}

async function toggleMultiplier(id, enable) {
    try { await apiFetch('api/multipliers/' + id, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: enable }) }); loadMultipliers(); }
    catch (err) { alert('操作失败: ' + err.message); }
}

async function deleteMultiplier(id) {
    if (!confirm('确定删除此规则吗？')) return;
    try { await apiFetch('api/multipliers/' + id, { method: 'DELETE' }); loadMultipliers(); }
    catch (err) { alert('删除失败: ' + err.message); }
}

// ===== Auth =====
async function logout() {
    try { await apiFetch('logout', { method: 'POST' }); } catch (_) {}
    window.location.href = 'login';
}

// ===== Modal Helpers =====
function showModal(id) { document.getElementById(id).classList.remove('hidden'); }
function closeModal(id) { document.getElementById(id).classList.add('hidden'); }

// ===== Utilities =====
function escapeHtml(str) { const div = document.createElement('div'); div.textContent = str; return div.innerHTML; }
function escapeAttr(str) { return str.replace(/\\/g, "\\\\").replace(/'/g, "\\'").replace(/"/g, '&quot;'); }
function formatDate(s) { if (!s) return '-'; try { return new Date(s).toLocaleString('zh-CN'); } catch (_) { return s; } }
function formatDaysOfWeek(d) {
    if (d === '*') return '每天';
    const m = {'0':'日','1':'一','2':'二','3':'三','4':'四','5':'五','6':'六'};
    return d.split(',').map(x => '周'+m[x.trim()]).join(', ');
}
function truncateUrl(url) { return url.length > 50 ? url.substring(0, 47) + '...' : url; }

function copyShareAll() {
    const ta = document.getElementById('share-all-text'); ta.select();
    navigator.clipboard.writeText(ta.value).then(() => {
        const btn = document.querySelector('#subkey-modal .btn-primary');
        btn.textContent = '✅ 已复制！'; setTimeout(() => btn.textContent = '📋 一键复制全部', 2000);
    });
}

function copyToClipboard(eid) {
    const el = document.getElementById(eid);
    navigator.clipboard.writeText(el.textContent || el.value).then(() => {
        const btn = el.parentElement.querySelector('button');
        if (btn) { btn.textContent = '✅'; setTimeout(() => btn.textContent = '📋 复制 Key', 1500); }
    });
}

document.addEventListener('click', (e) => { if (e.target.classList.contains('modal')) e.target.classList.add('hidden'); });
