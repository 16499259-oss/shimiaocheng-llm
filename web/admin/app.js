// LLM API Gateway — Admin Panel JavaScript

let currentCallsUserId = null;
let currentCallsPage = 1;
let currentAuditPage = 1;
let providerMap = {}; // slug -> name for dropdowns
let providerDetail = {}; // slug -> full provider object (for edit modal)

// ===== Initialization =====
document.addEventListener('DOMContentLoaded', () => {
    loadOverview();
    loadProviders(); // preload provider map for route mode dropdowns
    setupEventListeners();

    // Restore tab from URL hash (or the injected __INIT_TAB__ on the deep-link
    // dashboard page /admin/provider-usage), default to dashboard.
    const hash = location.hash.replace(/^#/, '');
    const initTab = (window.__INIT_TAB__ && VALID_TABS.includes(window.__INIT_TAB__))
        ? window.__INIT_TAB__
        : (VALID_TABS.includes(hash) ? hash : 'dashboard');
    switchTab(initTab);
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
    // Route mode toggle: show/hide fixed provider selector
    document.getElementById('new-route-mode').addEventListener('change', function() {
        const isFixed = this.value === 'fixed';
        document.getElementById('new-fixed-provider-group').style.display = isFixed ? '' : 'none';
        if (isFixed) fetchProviderUsage(this.form.querySelector('#new-fixed-provider').value, 'new-provider-usage');
        else { const h = document.getElementById('new-provider-usage'); if (h) h.innerHTML = ''; }
    });
    document.getElementById('update-route-mode').addEventListener('change', function() {
        const isFixed = this.value === 'fixed';
        document.getElementById('update-fixed-provider-group').style.display = isFixed ? '' : 'none';
        if (isFixed) fetchProviderUsage(this.form.querySelector('#update-fixed-provider').value, 'update-provider-usage');
        else { const h = document.getElementById('update-provider-usage'); if (h) h.innerHTML = ''; }
    });
    // Live quota hint when the fixed provider selection changes.
    document.getElementById('new-fixed-provider').addEventListener('change', function() {
        fetchProviderUsage(this.value, 'new-provider-usage');
    });
    document.getElementById('update-fixed-provider').addEventListener('change', function() {
        fetchProviderUsage(this.value, 'update-provider-usage');
    });

    // Call-stats panel filter bar (bound once; elements live in the DOM at load).
    document.getElementById('cs-time').addEventListener('change', onCsTimeChange);
    document.getElementById('cs-user').addEventListener('change', onCsFilterChange);
    document.getElementById('cs-provider').addEventListener('change', onCsFilterChange);
    document.getElementById('cs-model').addEventListener('change', onCsFilterChange);
    document.getElementById('cs-from').addEventListener('change', onCsFilterChange);
    document.getElementById('cs-to').addEventListener('change', onCsFilterChange);
    document.getElementById('cs-query').addEventListener('click', () => { csPage = 1; reloadCs(); });
    document.getElementById('cs-reset').addEventListener('click', onCsReset);
}

// ===== Tab Switching =====
const VALID_TABS = ['dashboard', 'users', 'providers', 'provider-usage', 'mappings', 'routing', 'multipliers', 'audit', 'callstats'];

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
        case 'provider-usage': loadProviderUsage(); break;
        case 'callstats': initCallStatsTab(); break;
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
        // Parallel fetch: provider list + rolling-window usage, merged by slug.
        const [provData, usageData] = await Promise.all([
            apiFetch('api/providers'),
            apiFetch('api/provider-usage').catch(() => ({ data: [] })),
        ]);
        const tbody = document.getElementById('providers-tbody');
        const provs = (provData && provData.data) || [];
        if (provs.length === 0) {
            tbody.innerHTML = '<tr><td colspan="14" class="text-center">暂无上游</td></tr>';
            return;
        }

        // Merge usage by slug (usage may be empty for providers with no calls).
        const usageMap = {};
        ((usageData && usageData.data) || []).forEach(u => { usageMap[u.slug] = u; });

        providerMap = {};
        providerDetail = {};
        provs.forEach(p => { providerMap[p.slug] = p.name; providerDetail[p.slug] = p; });

        const localeFmt = n => (Number(n) || 0).toLocaleString();
        tbody.innerHTML = provs.map(p => {
            const defBadge = p.is_default ? ' <span class="badge badge-success">默认</span>' : '';
            const statusBadge = p.enabled
                ? '<span class="badge badge-active">启用</span>'
                : '<span class="badge badge-disabled">禁用</span>';
            const passBadge = p.allow_passthrough
                ? '<span class="badge badge-success">✅</span>'
                : '<span class="badge badge-disabled">-</span>';
            const u = usageMap[p.slug];
            // F5 fix: judge "unlimited" by the provider's OWN limit field (always
            // returned by the list API), NOT by whether the usage subquery
            // succeeded. If usage fetch failed or a slug is missing from
            // usageMap, show a neutral "获取失败" placeholder instead of falsely
            // reporting "不限制" for a provider that actually has a real cap.
            const isTokenUnlimited = (p.monthly_token_limit || 0) <= 0;
            const isCallUnlimited = (p.monthly_call_limit || 0) <= 0;
            const tokenCell = isTokenUnlimited
                ? '<span class="usage-unlimited">不限制</span>'
                : (u ? renderUsageCell(u.token_used, u.monthly_token_limit, u.token_low) : '<span class="usage-unlimited">获取失败</span>');
            const allocTokenCell = u
                ? renderAllocationCell(u.allocated_tokens, u.monthly_token_limit, u.allocation_low)
                : '<span class="usage-unlimited">-</span>';
            const callCell = isCallUnlimited
                ? '<span class="usage-unlimited">不限制</span>'
                : (u ? renderUsageCell(u.call_used, u.monthly_call_limit, u.call_low, localeFmt) : '<span class="usage-unlimited">获取失败</span>');
            const allocCallCell = u
                ? renderAllocationCell(u.allocated_calls, u.monthly_call_limit, u.allocation_low, localeFmt)
                : '<span class="usage-unlimited">-</span>';
            return `<tr>
                <td>${p.id}</td>
                <td>${escapeHtml(p.name)}${defBadge}</td>
                <td><code>${escapeHtml(p.slug)}</code></td>
                <td>${p.cycle_start_date ? formatDate(p.cycle_start_date) : '-'}</td>
                <td>${tokenCell}</td>
                <td>${allocTokenCell}</td>
                <td>${callCell}</td>
                <td>${allocCallCell}</td>
                <td>${statusBadge}</td>
                <td>${passBadge}</td>
                <td><span class="endpoint-text" title="${escapeHtml(p.endpoint)}">${escapeHtml(truncateUrl(p.endpoint))}</span></td>
                <td><span class="key-masked">${escapeHtml(p.masked_key)}</span></td>
                <td>${formatDate(p.created_at)}</td>
                <td>
                    <div class="btn-group">
                        <button class="btn btn-outline btn-sm" onclick="editProvider('${escapeAttr(p.slug)}')">编辑</button>
                        <button class="btn btn-danger btn-sm" onclick="deleteProvider('${escapeAttr(p.slug)}','${escapeAttr(p.name)}')">删除</button>
                    </div>
                </td>
            </tr>`;
        }).join('');

        // Refresh provider dropdowns for user create/edit modals.
        refreshProviderDropdowns();
    } catch (err) { console.error('Failed to load providers:', err); }
}

function refreshProviderDropdowns() {
    const opts = Object.entries(providerMap).map(([slug, name]) =>
        `<option value="${escapeHtml(slug)}">${escapeHtml(name)}</option>`
    ).join('');
    const newSel = document.getElementById('new-fixed-provider');
    if (newSel) newSel.innerHTML = opts;
    const updSel = document.getElementById('update-fixed-provider');
    if (updSel) updSel.innerHTML = opts;
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
        // Passthrough defaults (off / chat-compatible auth).
        document.getElementById('prov-allow-passthrough').checked = false;
        document.getElementById('prov-auth-header').value = '';
        document.getElementById('prov-auth-scheme').value = 'bearer';
        document.getElementById('prov-extra-headers').innerHTML = '';
        // Monthly quota defaults (0 = unlimited).
        document.getElementById('prov-monthly-token-limit').value = '0';
        document.getElementById('prov-monthly-call-limit').value = '0';
        // Low-balance thresholds: empty = inherit global default (rendered as %).
        document.getElementById('prov-monthly-token-low-ratio').value = '';
        document.getElementById('prov-monthly-call-low-ratio').value = '';
        // Cycle start date: empty = today (backend default).
        document.getElementById('prov-cycle-start-date').value = '';
    }
    showModal('provider-modal');
}

function editProvider(slug) {
    const p = providerDetail[slug];
    if (!p) { showToast('未找到上游信息', 'error'); return; }
    document.getElementById('provider-modal-title').textContent = '编辑上游';
    document.getElementById('prov-edit-slug').value = slug;
    document.getElementById('prov-name').value = p.name || '';
    document.getElementById('prov-slug').value = slug;
    document.getElementById('prov-slug').disabled = true;
    document.getElementById('prov-endpoint').value = p.endpoint || '';
    document.getElementById('prov-apikey').value = '';
    document.getElementById('prov-apikey').placeholder = '留空则不修改';
    document.getElementById('prov-is-default').checked = !!p.is_default;
    // Passthrough fields.
    document.getElementById('prov-allow-passthrough').checked = !!p.allow_passthrough;
    document.getElementById('prov-auth-header').value = p.auth_header || '';
    document.getElementById('prov-auth-scheme').value = p.auth_scheme || 'bearer';
    // Monthly quota fields.
    document.getElementById('prov-monthly-token-limit').value = (p.monthly_token_limit != null) ? p.monthly_token_limit : 0;
    document.getElementById('prov-monthly-call-limit').value = (p.monthly_call_limit != null) ? p.monthly_call_limit : 0;
    // Low-balance thresholds: stored as a ratio (0.10 = 10%); render as % and
    // show empty when 0 (= inherit global default).
    document.getElementById('prov-monthly-token-low-ratio').value =
        (p.monthly_token_low_ratio > 0) ? (p.monthly_token_low_ratio * 100) : '';
    document.getElementById('prov-monthly-call-low-ratio').value =
        (p.monthly_call_low_ratio > 0) ? (p.monthly_call_low_ratio * 100) : '';
    // Cycle start date.
    document.getElementById('prov-cycle-start-date').value = p.cycle_start_date || '';
    // Rebuild extra_headers rows from the stored JSON string.
    document.getElementById('prov-extra-headers').innerHTML = '';
    let extra = {};
    try { extra = p.extra_headers ? JSON.parse(p.extra_headers) : {}; } catch (e) { extra = {}; }
    const keys = Object.keys(extra);
    if (keys.length === 0) {
        addExtraHeaderRow('', '');
    } else {
        keys.forEach(k => addExtraHeaderRow(k, extra[k]));
    }
    showModal('provider-modal');
}

// addExtraHeaderRow appends one key/value row to the extra_headers editor.
function addExtraHeaderRow(key = '', value = '') {
    const container = document.getElementById('prov-extra-headers');
    const row = document.createElement('div');
    row.className = 'extra-header-row';
    row.innerHTML = `<input type="text" class="eh-key" placeholder="Header 名（如 anthropic-version）" value="${escapeAttr(key)}">` +
        `<input type="text" class="eh-value" placeholder="值（如 2023-06-01）" value="${escapeAttr(value)}">` +
        `<button type="button" class="btn btn-outline btn-sm" onclick="this.parentNode.remove()">✕</button>`;
    container.appendChild(row);
}

// collectExtraHeaders reads the extra_headers editor rows into a plain map,
// skipping rows with an empty key. Returns {} when nothing is configured.
function collectExtraHeaders() {
    const map = {};
    document.querySelectorAll('#prov-extra-headers .extra-header-row').forEach(row => {
        const k = row.querySelector('.eh-key').value.trim();
        const v = row.querySelector('.eh-value').value.trim();
        if (k !== '') map[k] = v;
    });
    return map;
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
    const allowPassthrough = document.getElementById('prov-allow-passthrough').checked;
    const authHeader = document.getElementById('prov-auth-header').value.trim();
    const authScheme = document.getElementById('prov-auth-scheme').value;
    const extraHeaders = collectExtraHeaders();
    const monthlyTokenLimit = parseInt(document.getElementById('prov-monthly-token-limit').value, 10) || 0;
    const monthlyCallLimit = parseInt(document.getElementById('prov-monthly-call-limit').value, 10) || 0;
    // Low-balance thresholds: read as % from the input, submit as ratio
    // (value / 100). Empty or 0 -> 0 (inherit global default).
    const tkLowRaw = document.getElementById('prov-monthly-token-low-ratio').value.trim();
    const clLowRaw = document.getElementById('prov-monthly-call-low-ratio').value.trim();
    const monthlyTokenLowRatio = tkLowRaw === '' ? 0 : (parseFloat(tkLowRaw) / 100);
    const monthlyCallLowRatio = clLowRaw === '' ? 0 : (parseFloat(clLowRaw) / 100);
    const cycleStartDate = document.getElementById('prov-cycle-start-date').value;

    if (!name || !endpoint) { showToast('名称和端点为必填项', 'error'); return; }
    if (!isEdit && !slug) { showToast('Slug 为必填项', 'error'); return; }

    try {
        if (isEdit) {
            const body = { name, endpoint, allow_passthrough: allowPassthrough, auth_header: authHeader, auth_scheme: authScheme, extra_headers: extraHeaders,
                monthly_token_limit: monthlyTokenLimit, monthly_call_limit: monthlyCallLimit,
                monthly_token_low_ratio: monthlyTokenLowRatio, monthly_call_low_ratio: monthlyCallLowRatio };
            if (cycleStartDate) body.cycle_start_date = cycleStartDate;
            if (apiKey) body.api_key = apiKey;
            if (isDefault) body.is_default = true;
            await apiFetch('api/providers/' + encodeURIComponent(editSlug), {
                method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
            });
            showToast('上游已更新', 'success');
        } else {
            await apiFetch('api/providers', {
                method: 'POST', headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    name, slug, endpoint, api_key: apiKey, is_default: isDefault,
                    allow_passthrough: allowPassthrough, auth_header: authHeader,
                    auth_scheme: authScheme, extra_headers: extraHeaders,
                    monthly_token_limit: monthlyTokenLimit, monthly_call_limit: monthlyCallLimit,
                    monthly_token_low_ratio: monthlyTokenLowRatio, monthly_call_low_ratio: monthlyCallLowRatio,
                    cycle_start_date: cycleStartDate,
                }),
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

// ===== Provider Monthly Usage (上游额度可见性) =====

// formatToken abbreviates large token counts: K/M/亿 with up to 2 decimals
// (trailing zeros removed). Mirrors the backend's human-readable convention.
function trimTrailingZeros(s) {
    return s.indexOf('.') >= 0 ? s.replace(/\.?0+$/, '') : s;
}
function formatToken(n) {
    n = Number(n) || 0;
    if (n < 1000) return String(n);
    if (n < 1e6) return trimTrailingZeros((n / 1e3).toFixed(1)) + 'K';
    if (n < 1e8) return trimTrailingZeros((n / 1e6).toFixed(2)) + 'M';
    return trimTrailingZeros((n / 1e8).toFixed(2)) + '亿';
}

// renderUsageCell renders a single usage column (token or call). It shows
// used/limit, a progress bar, and the remaining percentage; an unlimited
// limit (limit<=0) shows "不限制" with no bar. `low` adds the red class.
// `numFmt` formats the numbers (defaults to formatToken; pass toLocaleString
// for call counts).
function renderUsageCell(used, limit, low, numFmt) {
    numFmt = numFmt || formatToken;
    used = Number(used) || 0;
    if (!limit || limit <= 0) {
        return `<div class="usage-cell${low ? ' usage-low' : ''}"><span class="usage-unlimited">不限制</span></div>`;
    }
    const remaining = Math.max(0, limit - used); // F3 fix: clamp negative remaining to 0 when over quota
    const pct = Math.min(100, Math.round(used / limit * 100));
    const barCls = pct > 80 ? 'bad' : (pct > 50 ? 'warn' : 'good');
    const lowCls = low ? ' usage-low' : '';
    return `<div class="usage-cell${lowCls}">
        <div class="usage-nums"><span>${numFmt(used)}</span> / <span>${numFmt(limit)}</span></div>
        <div class="usage-bar"><div class="usage-fill ${barCls}" style="width:${pct}%"></div></div>
        <div class="usage-meta">剩余 ${numFmt(remaining)} · ${pct}%</div>
    </div>`;
}

// renderAllocationCell renders an allocation column (allocated vs limit).
// Simpler than renderUsageCell: shows the allocated number; red if allocation_low.
function renderAllocationCell(allocated, limit, low, numFmt) {
    numFmt = numFmt || formatToken;
    allocated = Number(allocated) || 0;
    if (!limit || limit <= 0) {
        // F4 fix: an unlimited-cap provider still exposes an allocated total
        // (sum of per-user quotas), which is meaningful regardless of the
        // provider's own cap. Show it instead of hiding behind a bare "不限制".
        const allocCls = low ? ' allocation-low' : '';
        return `<div class="usage-cell${allocCls}"><span class="usage-nums">${numFmt(allocated)}</span> <span class="usage-unlimited">（无限上限）</span></div>`;
    }
    const lowCls = low ? ' allocation-low' : '';
    const pct = limit > 0 ? Math.round(allocated / limit * 100) : 0;
    return `<div class="usage-cell${lowCls}">
        <div class="usage-nums"><span>${numFmt(allocated)}</span> / <span>${numFmt(limit)}</span></div>
        <div class="usage-meta">${pct}%</div>
    </div>`;
}

// fetchProviderUsage pulls a single provider's rolling-window usage to show a
// live hint in the account-creation form. It NEVER blocks form submission:
// on any error it only displays "获取失败". `hintId` selects which hint block
// (create vs edit modal) to populate.
async function fetchProviderUsage(slug, hintId) {
    const hintEl = document.getElementById(hintId);
    if (!hintEl) return;
    if (!slug) {
        hintEl.innerHTML = '<span class="usage-hint-text">未指定固定上游（全局路由，无额度提示）</span>';
        return;
    }
    try {
        const data = await apiFetch('api/providers/' + encodeURIComponent(slug) + '/usage');
        const u = data && data.data;
        if (!u) {
            hintEl.innerHTML = '<span class="usage-hint-text">获取失败</span>';
            return;
        }
        const localeFmt = n => (Number(n) || 0).toLocaleString();
        const allocInfo = (u.allocated_tokens > 0 || u.allocated_calls > 0 || u.unlimited_user_count > 0)
            ? `<div class="usage-hint-row"><span class="usage-hint-label">已分配</span><span class="usage-hint-text">Token ${localeFmt(u.allocated_tokens)} · 调用 ${localeFmt(u.allocated_calls)}${u.unlimited_user_count > 0 ? ' · 无限用户 ' + u.unlimited_user_count : ''}</span></div>`
            : '';
        hintEl.innerHTML = `
            <div class="usage-hint-row"><span class="usage-hint-label">本月 Token</span>${renderUsageCell(u.token_used, u.monthly_token_limit, u.token_low)}</div>
            <div class="usage-hint-row"><span class="usage-hint-label">本月 调用</span>${renderUsageCell(u.call_used, u.monthly_call_limit, u.call_low, localeFmt)}</div>${allocInfo}`;
    } catch (err) {
        // Read-only hint only — must not block the create/edit submit.
        hintEl.innerHTML = '<span class="usage-hint-text">获取失败</span>';
        console.error('fetch provider usage failed:', err);
    }
}

// loadProviderUsage renders the standalone quota dashboard cards.
async function loadProviderUsage() {
    const grid = document.getElementById('provider-usage-cards');
    if (!grid) return;
    try {
        const data = await apiFetch('api/provider-usage');
        const views = (data && data.data) || [];
        if (views.length === 0) {
            grid.innerHTML = '<p class="text-center">暂无上游</p>';
            return;
        }
        const localeFmt = n => (Number(n) || 0).toLocaleString();
        const now = new Date().toLocaleString('zh-CN');
        grid.innerHTML = views.map(u => {
            const tokenCell = renderUsageCell(u.token_used, u.monthly_token_limit, u.token_low);
            const callCell = renderUsageCell(u.call_used, u.monthly_call_limit, u.call_low, localeFmt);
            const allocTokenCell = renderAllocationCell(u.allocated_tokens, u.monthly_token_limit, u.allocation_low);
            const allocCallCell = renderAllocationCell(u.allocated_calls, u.monthly_call_limit, u.allocation_low, localeFmt);
            const lowCls = (u.token_low || u.call_low || u.allocation_low) ? ' usage-card-low' : '';
            // Cycle info
            const cycleDaysCls = (u.cycle_days_remaining >= 0 && u.cycle_days_remaining <= 3) ? ' cycle-expiring' : '';
            const cycleInfo = u.cycle_start
                ? `<span>周期 ${u.cycle_start} ~ ${u.cycle_end} · <span class="${cycleDaysCls ? 'cycle-expiring' : ''}">剩余 ${u.cycle_days_remaining} 天</span></span>`
                : '';
            return `<div class="usage-card${lowCls}">
                <div class="usage-card-head">
                    <span class="usage-card-name">${escapeHtml(u.name)}</span>
                    <code class="usage-card-slug">${escapeHtml(u.slug)}</code>
                </div>
                <div class="usage-card-row"><span class="usage-card-label">本月 Token</span>${tokenCell}</div>
                <div class="usage-card-row"><span class="usage-card-label">已分配 Token</span>${allocTokenCell}</div>
                <div class="usage-card-row"><span class="usage-card-label">本月 调用</span>${callCell}</div>
                <div class="usage-card-row"><span class="usage-card-label">已分配 调用</span>${allocCallCell}</div>
                <div class="usage-card-foot">${cycleInfo} · 更新于 ${escapeHtml(now)}</div>
            </div>`;
        }).join('');
    } catch (err) {
        console.error('Failed to load provider usage:', err);
        grid.innerHTML = '<p class="text-center">加载失败</p>';
    }
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
            tbody.innerHTML = '<tr><td colspan="8" class="text-center">暂无路由规则</td></tr>';
            return;
        }
        tbody.innerHTML = data.data.map(r => {
            const s = r.enabled
                ? '<span class="badge badge-active">启用</span>'
                : '<span class="badge badge-disabled">禁用</span>';
            return `<tr>
                <td>${r.id}</td>
                <td><span class="badge badge-enabled">${escapeHtml(r.provider_id)}</span></td>
                <td>${escapeHtml(r.start_time)}</td>
                <td>${escapeHtml(r.end_time)}</td>
                <td>${formatDaysOfWeek(r.days_of_week)}</td>
                <td>${r.priority ?? 0}</td>
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
    const prioEl = document.getElementById('routing-priority');
    if (prioEl) prioEl.value = '0';
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
    const prioEl = document.getElementById('routing-priority');
    if (prioEl && prioEl.value !== '') {
        const p = parseInt(prioEl.value, 10);
        if (!Number.isNaN(p)) body.priority = p;
    }
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
    return ['你的 GLM API 账户已开通！', '', 'Key：' + subKey, '',
        'Base URL：https://ai.shimiaocheng.top/v1', '',
        '模型名称：GLM-5.2', '', '自助面板（查余量、看用法）：', '   https://ai.shimiaocheng.top/user/'].join('\n');
}

async function loadUsers() {
    try {
        const data = await apiFetch('api/users');
        const tbody = document.getElementById('users-tbody');
        if (!data.data || data.data.length === 0) { tbody.innerHTML = '<tr><td colspan="15" class="text-center">暂无用户</td></tr>'; return; }
        const now = new Date();
        tbody.innerHTML = data.data.map(u => {
            const quota5h = `${u.quota_5h_used} / ${u.quota_5h_limit}`;
            const quotaTotal = `${u.quota_total_used.toLocaleString()} / ${u.quota_total_limit.toLocaleString()}`;
            // Cumulative Token usage cell (quota 口径). 0 cap => unlimited.
            const tokenLimit = u.quota_token_total_limit || 0;
            const tokenUsed = u.quota_token_total_used || 0;
            let tokenCell;
            if (tokenLimit === 0) {
                tokenCell = '<span class="infinite">无限</span>';
            } else {
                const pct = Math.min(100, Math.round(tokenUsed / tokenLimit * 100));
                const cls = pct > 80 ? 'bad' : (pct > 50 ? 'warn' : 'good');
                tokenCell = `${tokenUsed.toLocaleString()} / ${tokenLimit.toLocaleString()}` +
                    `<div class="token-progress"><div class="${cls}" style="width:${pct}%"></div></div>`;
            }
            // 5h-window Token cell. 0 cap => unlimited.
            const token5hLimit = u.quota_token_5h_limit || 0;
            const token5hUsed = u.quota_token_5h_used || 0;
            let token5hCell;
            if (token5hLimit === 0) {
                token5hCell = '<span class="infinite">无限</span>';
            } else {
                const pct5h = Math.min(100, Math.round(token5hUsed / token5hLimit * 100));
                const cls5h = pct5h > 80 ? 'bad' : (pct5h > 50 ? 'warn' : 'good');
                token5hCell = `${token5hUsed.toLocaleString()} / ${token5hLimit.toLocaleString()}` +
                    `<div class="token-progress"><div class="${cls5h}" style="width:${pct5h}%"></div></div>`;
            }
            // Weekly (rolling 7d) Token cell. 0 cap => unlimited.
            const tokenWeekLimit = u.quota_token_week_limit || 0;
            const tokenWeekUsed = u.quota_token_week_used || 0;
            let tokenWeekCell;
            if (tokenWeekLimit === 0) {
                tokenWeekCell = '<span class="infinite">无限</span>';
            } else {
                const pctWeek = Math.min(100, Math.round(tokenWeekUsed / tokenWeekLimit * 100));
                const clsWeek = pctWeek > 80 ? 'bad' : (pctWeek > 50 ? 'warn' : 'good');
                tokenWeekCell = `${tokenWeekUsed.toLocaleString()} / ${tokenWeekLimit.toLocaleString()}` +
                    `<div class="token-progress"><div class="${clsWeek}" style="width:${pctWeek}%"></div></div>`;
            }
            let s = u.status === 'active' ? '<span class="badge badge-active">启用</span>' : '<span class="badge badge-disabled">禁用</span>';
            // Route mode badge
            let routeHtml = '';
            const rm = u.route_mode || 'auto';
            if (rm === 'fixed') {
                routeHtml = `<span class="badge badge-warning">📌 ${escapeHtml(u.fixed_provider || '?')}</span>`;
            } else {
                routeHtml = '<span class="badge" style="background:#e5e7eb;color:#6b7280;">auto</span>';
            }
            if (u.fixed_multiplier != null) {
                routeHtml += `<br><span class="badge badge-info" style="font-size:0.75rem;">${u.fixed_multiplier.toFixed(1)}x</span>`;
            }
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
                <td>${quota5h}</td><td>${quotaTotal}</td><td class="token-cell">${tokenCell}</td><td class="token-cell">${token5hCell}</td><td class="token-cell">${tokenWeekCell}</td><td>${routeHtml}</td><td>${formatBodySize(u.max_body_size)}</td><td>${u.max_concurrency > 0 ? u.max_concurrency : '不限'}</td><td>${expiryHtml}</td><td>${s}</td><td>${formatDate(u.created_at)}</td>
                <td><div class="btn-group">
                    <button class="btn btn-outline btn-sm" onclick="extendUser(${u.id},'${escapeAttr(u.username)}','${escapeAttr(u.expires_at || '')}')">🕐 延期</button>
                    <button class="btn btn-outline btn-sm" style="color:var(--color-warning,#e6a817);" onclick="openResetModal(${u.id},'${escapeAttr(u.username)}')">🔄 重置</button>
                    <button class="btn btn-outline btn-sm" onclick="shareUser('${escapeAttr(u.username)}',${u.id})">📋 分享</button>
                    <button class="btn btn-outline btn-sm" onclick="editUser(${u.id},'${escapeAttr(u.status)}',${u.quota_5h_limit},${u.quota_total_limit},'${escapeAttr(u.route_mode || 'auto')}','${escapeAttr(u.fixed_provider || '')}',${u.fixed_multiplier != null ? u.fixed_multiplier : 'null'},${u.max_body_size ? u.max_body_size : 1048576},${u.quota_token_total_limit || 0},${u.quota_token_total_used || 0},${u.max_concurrency != null ? u.max_concurrency : 10},${u.quota_token_5h_limit || 0},${u.quota_token_week_limit || 0})">编辑</button>
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
    // Route mode fields
    const routeMode = document.getElementById('new-route-mode').value;
    const fixedProvider = routeMode === 'fixed' ? document.getElementById('new-fixed-provider').value : '';
    const fmRaw = document.getElementById('new-fixed-multiplier').value;
    const fixedMultiplier = fmRaw ? parseFloat(fmRaw) : null;
    const mbs = parseFloat(document.getElementById('new-max-body-size').value) || 1;

    const body = { username, quota_5h_limit: q5, quota_total_limit: qt, expires_at: expiresAt, route_mode: routeMode, fixed_provider: fixedProvider };
    if (fixedMultiplier != null) body.fixed_multiplier = fixedMultiplier;
    body.max_body_size = mbs * 1048576;
    // Per-user concurrency cap:
    //   - 留空 (empty) → 不发送该字段，后端按默认上限 (10) 处理
    //   - 0            → 发送 0（不限）
    //   - 正数 (>0)    → 发送该上限
    //   - 负数（理论上已被 input min=0 拦截）→ 仍发送，交由后端返回 400
    const mcRaw = document.getElementById('new-max-concurrency').value.trim();
    if (mcRaw !== '') {
        body.max_concurrency = parseInt(mcRaw, 10);
    }
    // Cumulative Token cap: only send when the field is non-empty (default 0 = unlimited).
    const qttRaw = document.getElementById('new-quota-token-total').value.trim();
    if (qttRaw !== '') {
        const qtt = parseInt(qttRaw);
        if (!isNaN(qtt) && qtt >= 0) body.quota_token_total_limit = qtt;
    }
    // 5h-window Token cap: only send when non-empty (default 0 = unlimited).
    const q5hRaw = document.getElementById('new-quota-token-5h').value.trim();
    if (q5hRaw !== '') {
        const q5h = parseInt(q5hRaw);
        if (!isNaN(q5h) && q5h >= 0) body.quota_token_5h_limit = q5h;
    }
    // Weekly (rolling 7d) Token cap: only send when non-empty (default 0 = unlimited).
    const qWeekRaw = document.getElementById('new-quota-token-week').value.trim();
    if (qWeekRaw !== '') {
        const qWeek = parseInt(qWeekRaw);
        if (!isNaN(qWeek) && qWeek >= 0) body.quota_token_week_limit = qWeek;
    }

    try {
        const data = await apiFetch('api/users', { method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body) });
        if (data.sub_key) {
            document.getElementById('subkey-value').textContent = data.sub_key;
            document.getElementById('share-all-text').value = buildShareText(data.sub_key);
            closeModal('create-user-modal'); showModal('subkey-modal');
            document.getElementById('create-user-form').reset();
            document.getElementById('new-fixed-provider-group').style.display = 'none';
        } else if (data.error) {
            const resultEl = document.getElementById('create-user-result');
            resultEl.textContent = '创建失败: ' + data.error;
            resultEl.classList.remove('hidden');
            return;
        }
        loadUsers(); loadOverview();
    } catch (err) { document.getElementById('create-user-result').textContent = '创建失败: ' + err.message; document.getElementById('create-user-result').classList.remove('hidden'); }
}

function editUser(id, status, q5, qt, routeMode, fixedProvider, fixedMultiplier, maxBodySize, tokenLimit, tokenUsed, maxConcurrency, token5hLimit, tokenWeekLimit) {
    document.getElementById('update-user-id').value = id;
    document.getElementById('update-quota-5h').value = q5;
    document.getElementById('update-quota-total').value = qt;
    document.getElementById('update-quota-token-total').value = tokenLimit || 0;
    document.getElementById('update-quota-token-5h').value = token5hLimit || 0;
    document.getElementById('update-quota-token-week').value = tokenWeekLimit || 0;
    document.getElementById('update-status').value = '';
    document.getElementById('update-regenerate-key').checked = false;
    document.getElementById('update-route-mode').value = '';
    document.getElementById('update-fixed-provider').value = '';
    document.getElementById('update-fixed-provider-group').style.display = 'none';
    document.getElementById('update-fixed-multiplier').value = '';
    // Reflect the stored value on the matching dropdown option (supports the
    // 512 KB / 0.5 MB step, not just whole MB).
    const mbVal = (maxBodySize || 1048576) / 1048576;
    document.getElementById('update-max-body-size').value = String(Math.round(mbVal * 10) / 10);
    document.getElementById('update-max-concurrency').value = (maxConcurrency && maxConcurrency > 0) ? maxConcurrency : '0';
    document.getElementById('update-user-result').classList.add('hidden');
    // Pre-fill existing route mode info (display only, user can change)
    if (routeMode && routeMode !== 'null') {
        document.getElementById('update-route-mode').value = routeMode;
        if (routeMode === 'fixed') {
            document.getElementById('update-fixed-provider-group').style.display = '';
            if (fixedProvider && fixedProvider !== 'null' && fixedProvider !== '') {
                document.getElementById('update-fixed-provider').value = fixedProvider;
                fetchProviderUsage(fixedProvider, 'update-provider-usage');
            }
        }
    }
    if (fixedMultiplier && fixedMultiplier !== 'null') {
        document.getElementById('update-fixed-multiplier').value = parseFloat(fixedMultiplier);
    }
    window._editFixedMultiplier = (fixedMultiplier && fixedMultiplier !== 'null') ? parseFloat(fixedMultiplier) : null;
    refreshProviderDropdowns();
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
    const uttRaw = document.getElementById('update-quota-token-total').value.trim();
    if (uttRaw !== '') {
        const utt = parseInt(uttRaw);
        if (!isNaN(utt) && utt >= 0) body.quota_token_total_limit = utt;
    }
    // 5h-window Token cap: empty = unchanged; 0 = unlimited.
    const u5hRaw = document.getElementById('update-quota-token-5h').value.trim();
    if (u5hRaw !== '') {
        const u5h = parseInt(u5hRaw);
        if (!isNaN(u5h) && u5h >= 0) body.quota_token_5h_limit = u5h;
    }
    // Weekly (rolling 7d) Token cap: empty = unchanged; 0 = unlimited.
    const uWeekRaw = document.getElementById('update-quota-token-week').value.trim();
    if (uWeekRaw !== '') {
        const uWeek = parseInt(uWeekRaw);
        if (!isNaN(uWeek) && uWeek >= 0) body.quota_token_week_limit = uWeek;
    }
    const st = document.getElementById('update-status').value;
    if (st) body.status = st;
    body.regenerate_key = document.getElementById('update-regenerate-key').checked;
    // Route mode fields
    const rm = document.getElementById('update-route-mode').value;
    if (rm) {
        body.route_mode = rm;
        body.fixed_provider = rm === 'fixed' ? document.getElementById('update-fixed-provider').value : '';
    }
    const fmRaw = document.getElementById('update-fixed-multiplier').value;
    if (fmRaw !== '') {
        body.fixed_multiplier = parseFloat(fmRaw) || null;
    } else if (window._editFixedMultiplier != null) {
        // Input cleared but user previously had a value → send explicit clear signal
        body.fixed_multiplier_clear = true;
    }
    const umbs = document.getElementById('update-max-body-size').value;
    if (umbs) body.max_body_size = parseFloat(umbs) * 1048576;
    const umc = document.getElementById('update-max-concurrency').value.trim();
    if (umc !== '') body.max_concurrency = parseInt(umc);
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
    body.reset_token_stats = document.getElementById('reset-token-stats').checked;

    try {
        const data = await apiFetch('api/users/' + id + '/extend', {
            method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
        });
        closeModal('extend-user-modal');
        showToast(data.message || '延期成功', 'success');
        loadUsers(); loadOverview();
    } catch (err) { showToast('延期失败: ' + err.message, 'error'); }
}

let _resetUserId = null;
let _resetUsername = '';

function openResetModal(id, username) {
    _resetUserId = id;
    _resetUsername = username;
    document.getElementById('reset-username').textContent = username;
    showModal('reset-usage-modal');
}

async function confirmResetUsage() {
    const id = _resetUserId;
    closeModal('reset-usage-modal');
    try {
        const data = await apiFetch('api/users/' + id + '/reset-usage', { method: 'POST' });
        showToast(data.message || '重置成功', 'success');
        loadUsers(); loadOverview();
    } catch (err) { showToast('重置失败: ' + err.message, 'error'); }
}

async function viewCalls(userId, username) {
    // Jump to callstats tab with the user preselected.
    // Store user info in sessionStorage so initCallStatsTab can pick it up.
    try {
        sessionStorage.setItem('cs_preselected_user', JSON.stringify({id: userId, name: username}));
        // Drop any stale call-stats filter (e.g. a previously saved custom time
        // range whose upper bound is in the past). Without this, the preselected
        // user's records — which may all be recent — could be hidden behind an
        // outdated range, showing "暂无调用记录" even though rows exist. This
        // only affects the jump-from-user-management flow; the manual filter on
        // the call-stats tab itself is untouched and still persists normally.
        sessionStorage.removeItem(CS_STORAGE_KEY);
    } catch (_) {}
    switchTab('callstats');
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

// ===== Call Stats Panel (调用记录汇总) =====
// Pure read-only analytics over all call logs. Time boundaries are computed in
// Asia/Shanghai and sent as RFC3339; the backend re-normalizes defensively.
const CS_STORAGE_KEY = 'callstats_filter';
let csPage = 1;
let csFilter = { user_id: '', provider_id: '', model: '', time: '7d', customFrom: '', customTo: '' };

// Render any Date instant as Asia/Shanghai wall-clock RFC3339 (+08:00).
function toShanghaiRFC3339(date) {
    const parts = new Intl.DateTimeFormat('en-US', {
        timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit',
        hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
    }).formatToParts(date).reduce((m, p) => { m[p.type] = p.value; return m; }, {});
    let hour = parts.hour === '24' ? '00' : parts.hour;
    return `${parts.year}-${parts.month}-${parts.day}T${hour}:${parts.minute}:${parts.second}+08:00`;
}
// Calendar date (Asia/Shanghai) of a Date instant, e.g. "2026-07-14".
function shDateString(date) {
    return new Intl.DateTimeFormat('en-CA', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit' }).format(date);
}
// Midnight (00:00:00 SH) of a "YYYY-MM-DD" date, as RFC3339.
function shMidnightRFC3339(yyyymmdd) {
    return toShanghaiRFC3339(new Date(yyyymmdd + 'T00:00:00+08:00'));
}
// Current instant as RFC3339 in SH (rolling upper bound, includes in-progress day).
function nowShRFC3339() {
    return toShanghaiRFC3339(new Date());
}

function computeCsRange() {
    const now = new Date();
    const today = shDateString(now);
    let from, to;
    const t = csFilter.time;
    if (t === 'today') {
        from = shMidnightRFC3339(today);
        to = nowShRFC3339();
    } else if (t === '7d' || t === '30d') {
        const days = t === '7d' ? 7 : 30;
        const fromDate = new Date(today + 'T00:00:00+08:00');
        fromDate.setDate(fromDate.getDate() - (days - 1)); // start-of-window in SH
        from = toShanghaiRFC3339(fromDate);
        to = nowShRFC3339();
    } else { // custom
        const start = csFilter.customFrom || today;
        from = shMidnightRFC3339(start);
        const end = csFilter.customTo || today;
        // End day < today(SH): up to 23:59:59.999 SH. End day == today: now SH.
        if (end >= today) {
            to = nowShRFC3339();
        } else {
            to = toShanghaiRFC3339(new Date(end + 'T23:59:59.999+08:00'));
        }
    }
    return { from, to };
}

function buildCsQuery(page) {
    const { from, to } = computeCsRange();
    const q = new URLSearchParams();
    if (csFilter.user_id) q.set('user_id', csFilter.user_id);
    if (csFilter.provider_id) q.set('provider_id', csFilter.provider_id);
    if (csFilter.model) q.set('model', csFilter.model);
    if (from) q.set('from', from);
    if (to) q.set('to', to);
    if (page) { q.set('page', page); q.set('limit', 20); }
    return q.toString();
}

function saveCsFilter() {
    try { sessionStorage.setItem(CS_STORAGE_KEY, JSON.stringify({ ...csFilter, page: csPage })); } catch (_) {}
}
function restoreCsFilter() {
    try {
        const raw = sessionStorage.getItem(CS_STORAGE_KEY);
        if (raw) {
            const o = JSON.parse(raw);
            csFilter = { user_id: '', provider_id: '', model: '', time: '7d', customFrom: '', customTo: '', ...o };
            csPage = o.page || 1;
            return true;
        }
    } catch (_) {}
    return false;
}
function applyCsFilterToDOM() {
    document.getElementById('cs-time').value = csFilter.time;
    document.getElementById('cs-user').value = csFilter.user_id;
    document.getElementById('cs-provider').value = csFilter.provider_id;
    document.getElementById('cs-model').value = csFilter.model;
    document.getElementById('cs-from').value = csFilter.customFrom;
    document.getElementById('cs-to').value = csFilter.customTo;
    document.getElementById('cs-custom-range').style.display = csFilter.time === 'custom' ? '' : 'none';
}
function collectCsFilter() {
    csFilter.user_id = document.getElementById('cs-user').value;
    csFilter.provider_id = document.getElementById('cs-provider').value;
    csFilter.model = document.getElementById('cs-model').value.trim();
    csFilter.customFrom = document.getElementById('cs-from').value;
    csFilter.customTo = document.getElementById('cs-to').value;
}
function onCsTimeChange() {
    csFilter.time = document.getElementById('cs-time').value;
    document.getElementById('cs-custom-range').style.display = csFilter.time === 'custom' ? '' : 'none';
    csPage = 1;
    reloadCs();
}
function onCsFilterChange() {
    collectCsFilter();
    csPage = 1;
    reloadCs();
}
function onCsReset() {
    csFilter = { user_id: '', provider_id: '', model: '', time: '7d', customFrom: '', customTo: '' };
    csPage = 1;
    applyCsFilterToDOM();
    saveCsFilter();
    loadCallStats(1).catch(e => console.error(e));
}
function reloadCs() {
    collectCsFilter();
    saveCsFilter();
    loadCallStats(csPage).catch(e => console.error(e));
}

// Entry point for the call-stats tab: restore filter, fill dropdowns, load.
async function initCallStatsTab() {
    restoreCsFilter();
    applyCsFilterToDOM();
    // If arriving from user-management "记录" button, preselect that user
    // and clear the flag so subsequent tab switches don't override the filter.
    try {
        const raw = sessionStorage.getItem('cs_preselected_user');
        if (raw) {
            const u = JSON.parse(raw);
            if (u.id) {
                csFilter.user_id = String(u.id);
                // Reset the time range so the preselected user's records are
                // guaranteed visible. A previously-saved custom range (upper
                // bound in the past) would otherwise hide all of this user's
                // (possibly recent) call logs. '7d' covers recent activity;
                // the admin can still narrow it manually afterward.
                csFilter.time = '7d';
                csFilter.customFrom = '';
                csFilter.customTo = '';
                applyCsFilterToDOM();
                document.getElementById('cs-user').value = csFilter.user_id;
            }
            sessionStorage.removeItem('cs_preselected_user');
        }
    } catch (_) {}
    await Promise.all([loadCallStatsUsers(), loadCallModels()]);
    await loadCallStats(csPage);
    // 优先恢复上次选中的 sub-tab；首次访问默认「按用户明细」。
    const savedCsView = localStorage.getItem('csActiveView') || 'user';
    switchCsView(savedCsView);
}

async function loadCallStatsUsers() {
    try {
        const data = await apiFetch('api/users');
        const sel = document.getElementById('cs-user');
        const cur = csFilter.user_id;
        sel.innerHTML = '<option value="">全部用户</option>' + (data.data || []).map(u =>
            `<option value="${u.id}">${escapeHtml(u.username)}</option>`).join('');
        sel.value = cur;
    } catch (e) { console.error('load users for callstats:', e); }
}

async function loadCallModels() {
    try {
        const data = await apiFetch('api/calls/models');
        const list = document.getElementById('cs-model-list');
        const models = (data.data || []);
        list.innerHTML = '<option value=""></option>' + models.map(m =>
            `<option value="${escapeHtml(m)}"></option>`).join('');
    } catch (e) { console.error('load call models:', e); }
}

// Parallel fetch of stats + list; each resilient to the other failing.
async function loadCallStats(page) {
    csPage = page || 1;
    const qs = buildCsQuery(csPage);
    saveCsFilter();
    const [sRes, cRes] = await Promise.allSettled([
        apiFetch('api/calls/stats?' + qs),
        apiFetch('api/calls?' + qs),
    ]);
    if (sRes.status === 'fulfilled') renderStatsCards(sRes.value);
    else console.error('load call stats failed:', sRes.reason);
    if (cRes.status === 'fulfilled') renderCsCalls(cRes.value);
    else console.error('load calls failed:', cRes.reason);
}

function renderStatsCards(stats) {
    stats = stats || {};
    const tokens = stats.tokens || {};
    const success = stats.success || {};
    const total = stats.total_calls || 0;
    document.getElementById('cs-total-calls').textContent = total.toLocaleString();
    document.getElementById('cs-tok-prompt').textContent = (tokens.prompt || 0).toLocaleString();
    document.getElementById('cs-tok-completion').textContent = (tokens.completion || 0).toLocaleString();
    document.getElementById('cs-tok-total').textContent = (tokens.total || 0).toLocaleString();
    document.getElementById('cs-effective-calls').textContent = (stats.effective_calls || 0).toLocaleString();
    document.getElementById('cs-success-rate').textContent = total > 0
        ? (success.success_rate || 0).toFixed(1) + '%' : '-';
    document.getElementById('cs-success-count').textContent = (success.success_count || 0).toLocaleString();
    document.getElementById('cs-error-count').textContent = (success.error_count || 0).toLocaleString();
    document.getElementById('cs-note').textContent = '基于当前筛选 ' + total.toLocaleString() + ' 条';

    // Render per-model breakdown table
    const byModel = stats.by_model || [];
    const bmTbody = document.getElementById('cs-by-model-tbody');
    if (byModel.length === 0) {
        bmTbody.innerHTML = '<tr><td colspan="5" class="text-center">暂无数据</td></tr>';
    } else {
        bmTbody.innerHTML = byModel.map(m => {
            const mt = m.tokens || {};
            return `<tr>
                <td><code>${escapeHtml(m.model)}</code></td>
                <td>${(m.calls || 0).toLocaleString()}</td>
                <td>${(mt.prompt || 0).toLocaleString()}</td>
                <td>${(mt.completion || 0).toLocaleString()}</td>
                <td>${(mt.total || 0).toLocaleString()}</td>
            </tr>`;
        }).join('');
    }

    // Render per-user breakdown table (按用户明细)
    const byUser = stats.by_user || [];
    const buTbody = document.getElementById('cs-by-user-tbody');
    if (byUser.length === 0) {
        buTbody.innerHTML = '<tr><td colspan="6" class="text-center">暂无数据</td></tr>';
    } else {
        buTbody.innerHTML = byUser.map(u => {
            const ut = u.tokens || {};
            const name = u.username ? escapeHtml(u.username) : ('用户#' + u.user_id);
            return `<tr>
                <td><code>${name}</code></td>
                <td>${(u.calls || 0).toLocaleString()}</td>
                <td>${(ut.prompt || 0).toLocaleString()}</td>
                <td>${(ut.completion || 0).toLocaleString()}</td>
                <td>${(ut.total || 0).toLocaleString()}</td>
                <td>${(u.effective_calls || 0).toLocaleString()}</td>
            </tr>`;
        }).join('');
    }
}

function switchCsView(view) {
    document.getElementById('cs-view-model').style.display  = view === 'model'  ? '' : 'none';
    document.getElementById('cs-view-user').style.display   = view === 'user'   ? '' : 'none';
    document.getElementById('cs-view-detail').style.display = view === 'detail' ? '' : 'none';
    document.querySelectorAll('.cs-subtab').forEach(b => {
        b.classList.toggle('active', b.getAttribute('data-view') === view);
    });
    localStorage.setItem('csActiveView', view);
}

function renderCsCalls(calls) {
    calls = calls || {};
    const tbody = document.getElementById('cs-tbody');
    const data = calls.data || [];
    if (data.length === 0) {
        tbody.innerHTML = '<tr><td colspan="10" class="text-center">暂无调用记录</td></tr>';
        document.getElementById('cs-pagination').innerHTML = '';
        return;
    }
    tbody.innerHTML = data.map(c => {
        const prov = providerMap[c.provider_id] || c.provider_id || '-';
        const mult = (c.multiplier_used != null) ? c.multiplier_used.toFixed(1) + 'x' : '-';
        return `<tr>
            <td>${c.id}</td>
            <td>${escapeHtml(c.username || '-')}</td>
            <td>${escapeHtml(c.model)}</td>
            <td>${(c.total_tokens || 0).toLocaleString()}</td>
            <td>${c.effective_calls}</td>
            <td>${mult}</td>
            <td>${c.status_code}</td>
            <td>${c.latency_ms}</td>
            <td>${formatDateSH(c.created_at)}</td>
            <td>${escapeHtml(prov)}</td>
        </tr>`;
    }).join('');
    const pag = calls.pagination || {};
    const tp = Math.ceil((pag.total || 0) / (pag.limit || 20));
    document.getElementById('cs-pagination').innerHTML =
        `<button ${pag.page <= 1 ? 'disabled' : ''} onclick="loadCallStats(${pag.page - 1})">上一页</button>
         <span>第 ${pag.page} / ${tp} 页 (共 ${(pag.total || 0).toLocaleString()} 条)</span>
         <button ${pag.page >= tp ? 'disabled' : ''} onclick="loadCallStats(${pag.page + 1})">下一页</button>`;
}

// Show created_at in Asia/Shanghai specifically for this panel (PRD D6: SH only here).
function formatDateSH(s) {
    if (!s) return '-';
    try {
        return new Date(s).toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai' });
    } catch (_) { return s; }
}

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
function formatDate(s) { return formatDateSH(s); }

// formatBodySize renders a per-user body cap (bytes) as a human label,
// e.g. 524288 -> "512 KB", 1048576 -> "1 MB", 4194304 -> "4 MB".
function formatBodySize(bytes) {
    bytes = bytes || 1048576;
    if (bytes < 1048576) {
        return Math.round(bytes / 1024) + ' KB';
    }
    const mb = bytes / 1048576;
    return (Math.round(mb * 100) / 100) + ' MB';
}
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
