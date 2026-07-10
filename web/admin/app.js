// LLM API Gateway — Admin Panel JavaScript

let currentCallsUserId = null;
let currentCallsPage = 1;

// ===== Initialization =====
document.addEventListener('DOMContentLoaded', () => {
    loadOverview();
    loadUsers();
    loadMultipliers();
    setupEventListeners();
});

// ===== Event Listeners =====
function setupEventListeners() {
    // Logout
    document.getElementById('logout-btn').addEventListener('click', logout);

    // Create User
    document.getElementById('create-user-btn').addEventListener('click', () => {
        showModal('create-user-modal');
    });
    document.getElementById('create-user-form').addEventListener('submit', createUser);

    // Update User
    document.getElementById('update-user-form').addEventListener('submit', updateUser);

    // Close calls section
    document.getElementById('close-calls-btn').addEventListener('click', () => {
        document.getElementById('calls-section').style.display = 'none';
        document.getElementById('users-section').style.display = '';
    });

    // Create Multiplier
    document.getElementById('create-multiplier-btn').addEventListener('click', () => {
        showModal('create-multiplier-modal');
    });
    document.getElementById('create-multiplier-form').addEventListener('submit', createMultiplier);
}

// ===== API Helpers =====
async function apiFetch(url, options = {}) {
    const resp = await fetch(url, {
        credentials: 'same-origin',
        ...options,
    });

    if (resp.status === 401) {
        window.location.href = 'login';
        throw new Error('Not authenticated');
    }

    if (resp.status === 204) {
        return null;
    }

    return resp.json();
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
    } catch (err) {
        console.error('Failed to load overview:', err);
    }
}

// ===== Share existing user =====
async function shareUser(username, id) {
    // Key is hashed, must regenerate to show plaintext
    if (!confirm(`查看 ${username} 的 Key 需要重新生成密钥（旧 Key 将立即失效），确定继续？`)) {
        return;
    }

    // Show modal with loading state
    document.getElementById('subkey-value').textContent = '正在生成新 Key...';
    document.getElementById('share-all-text').value = '加载中...';
    showModal('subkey-modal');

    try {
        const data = await apiFetch(`api/users/${id}`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ regenerate_key: true }),
        });

        if (data && data.new_sub_key) {
            const subKey = data.new_sub_key;
            document.getElementById('subkey-value').textContent = subKey;

            const shareText = [
                '🎉 你的 LLM API 账户已开通！',
                '',
                '🔑 Key：' + subKey,
                '',
                '📡 Base URL：https://ai.shimiaocheng.top/v1',
                '   （Cursor / ChatBox / 所有 OpenAI 客户端均填此项）',
                '',
                '模型名称：GLM-5.2',
                '',
                '📖 自助面板（查余量、看用法）：',
                '   https://ai.shimiaocheng.top/user/'
            ].join('\n');
            document.getElementById('share-all-text').value = shareText;
        } else {
            document.getElementById('subkey-value').textContent = '生成失败，请重试';
            document.getElementById('share-all-text').value = '生成失败，请重试';
        }

        loadUsers();
    } catch (err) {
        document.getElementById('subkey-value').textContent = '生成失败: ' + err.message;
        document.getElementById('share-all-text').value = '错误: ' + err.message;
    }
}

// ===== Users =====
async function loadUsers() {
    try {
        const data = await apiFetch('api/users');
        const tbody = document.getElementById('users-tbody');

        if (!data.data || data.data.length === 0) {
            tbody.innerHTML = '<tr><td colspan="8" class="text-center">暂无用户</td></tr>';
            return;
        }

        tbody.innerHTML = data.data.map(u => {
            const quota5h = `${u.quota_5h_used} / ${u.quota_5h_limit}`;
            const quotaTotal = `${u.quota_total_used.toLocaleString()} / ${u.quota_total_limit.toLocaleString()}`;
            const tokens = (u.total_tokens || 0).toLocaleString();
            const statusBadge = u.status === 'active'
                ? '<span class="badge badge-active">启用</span>'
                : '<span class="badge badge-disabled">禁用</span>';

            return `
                <tr>
                    <td>${u.id}</td>
                    <td>${escapeHtml(u.username)}</td>
                    <td><code>${escapeHtml(u.sub_key_preview)}</code></td>
                    <td>${quota5h}</td>
                    <td>${quotaTotal}</td>
                    <td>${tokens}</td>
                    <td>${statusBadge}</td>
                    <td>${formatDate(u.created_at)}</td>
                    <td>
                        <div class="btn-group">
                            <button class="btn btn-outline btn-sm" onclick="shareUser('${escapeAttr(u.username)}', ${u.id})">📋 分享</button>
                            <button class="btn btn-outline btn-sm" onclick="editUser(${u.id}, '${escapeAttr(u.status)}', ${u.quota_5h_limit}, ${u.quota_total_limit})">编辑</button>
                            <button class="btn btn-outline btn-sm" onclick="viewCalls(${u.id}, '${escapeAttr(u.username)}')">记录</button>
                            <button class="btn btn-danger btn-sm" onclick="deleteUser(${u.id}, '${escapeAttr(u.username)}')">删除</button>
                        </div>
                    </td>
                </tr>
            `;
        }).join('');
    } catch (err) {
        console.error('Failed to load users:', err);
    }
}

async function createUser(e) {
    e.preventDefault();
    const username = document.getElementById('new-username').value.trim();
    const quota5hLimit = parseInt(document.getElementById('new-quota-5h').value) || 100;
    const quotaTotalLimit = parseInt(document.getElementById('new-quota-total').value) || 10000;

    try {
        const data = await apiFetch('api/users', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                username,
                quota_5h_limit: quota5hLimit,
                quota_total_limit: quotaTotalLimit,
            }),
        });

        if (data.sub_key) {
            // Fill subkey modal with full share info
            const username = data.username;
            const subKey = data.sub_key;
            document.getElementById('subkey-value').textContent = subKey;

            const shareText = [
                '🎉 你的 LLM API 账户已开通！',
                '',
                '🔑 Key：' + subKey,
                '',
                '📡 Base URL：https://ai.shimiaocheng.top/v1',
                '   （Cursor / ChatBox / 所有 OpenAI 客户端均填此项）',
                '',
                '模型名称：GLM-5.2',
                '',
                '📖 自助面板（查余量、看用法）：',
                '   https://ai.shimiaocheng.top/user/'
            ].join('\n');
            document.getElementById('share-all-text').value = shareText;

            closeModal('create-user-modal');
            showModal('subkey-modal');
            document.getElementById('create-user-form').reset();
        }

        loadUsers();
        loadOverview();
    } catch (err) {
        const resultEl = document.getElementById('create-user-result');
        resultEl.textContent = '创建失败: ' + err.message;
        resultEl.classList.remove('hidden');
    }
}

function editUser(id, status, quota5hLimit, quotaTotalLimit) {
    document.getElementById('update-user-id').value = id;
    document.getElementById('update-quota-5h').value = quota5hLimit;
    document.getElementById('update-quota-total').value = quotaTotalLimit;
    document.getElementById('update-status').value = '';
    document.getElementById('update-regenerate-key').checked = false;
    document.getElementById('update-user-result').classList.add('hidden');
    showModal('update-user-modal');
}

async function updateUser(e) {
    e.preventDefault();
    const id = document.getElementById('update-user-id').value;
    const body = {};

    const quota5h = document.getElementById('update-quota-5h').value;
    if (quota5h) body.quota_5h_limit = parseInt(quota5h);

    const quotaTotal = document.getElementById('update-quota-total').value;
    if (quotaTotal) body.quota_total_limit = parseInt(quotaTotal);

    const status = document.getElementById('update-status').value;
    if (status) body.status = status;

    body.regenerate_key = document.getElementById('update-regenerate-key').checked;

    if (body.regenerate_key && !confirm('确定要重新生成 Key 吗？旧 Key 将立即失效。')) {
        return;
    }

    try {
        const data = await apiFetch(`api/users/${id}`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
        });

        if (data && data.new_sub_key) {
            const subKey = data.new_sub_key;
            const username = data.username || ('user-' + id);
            document.getElementById('subkey-value').textContent = subKey;

            const shareText = [
                '🎉 你的 LLM API Key 已更新！',
                '',
                '🔑 新 Key：' + subKey,
                '',
                '📡 Base URL：https://ai.shimiaocheng.top/v1',
                '   （Cursor / ChatBox / 所有 OpenAI 客户端均填此项）',
                '',
                '模型名称：GLM-5.2',
                '',
                '📖 自助面板：https://ai.shimiaocheng.top/user/'
            ].join('\n');
            document.getElementById('share-all-text').value = shareText;

            closeModal('update-user-modal');
            showModal('subkey-modal');
        } else {
            closeModal('update-user-modal');
        }

        loadUsers();
        loadOverview();
    } catch (err) {
        const resultEl = document.getElementById('update-user-result');
        resultEl.textContent = '更新失败: ' + err.message;
        resultEl.classList.remove('hidden');
    }
}

async function deleteUser(id, username) {
    if (!confirm(`确定要删除用户「${username}」吗？删除后该用户的 Key 将立即失效，此操作不可撤销。`)) {
        return;
    }

    try {
        await apiFetch(`api/users/${id}`, { method: 'DELETE' });
        loadUsers();
        loadOverview();
    } catch (err) {
        alert('删除失败: ' + err.message);
    }
}

// ===== Call Logs =====
async function viewCalls(userId, username) {
    currentCallsUserId = userId;
    currentCallsPage = 1;
    document.getElementById('calls-username').textContent = username;
    document.getElementById('users-section').style.display = 'none';
    document.getElementById('calls-section').style.display = '';
    await loadCalls();
}

async function loadCalls() {
    if (!currentCallsUserId) return;

    try {
        const data = await apiFetch(
            `api/users/${currentCallsUserId}/calls?page=${currentCallsPage}&limit=20`
        );

        const tbody = document.getElementById('calls-tbody');

        if (!data.data || data.data.length === 0) {
            tbody.innerHTML = '<tr><td colspan="8" class="text-center">暂无调用记录</td></tr>';
            document.getElementById('calls-pagination').innerHTML = '';
            return;
        }

        tbody.innerHTML = data.data.map(c => `
            <tr>
                <td>${c.id}</td>
                <td>${escapeHtml(c.model)}</td>
                <td>${c.total_tokens.toLocaleString()}</td>
                <td>${c.effective_calls}</td>
                <td>${c.multiplier_used.toFixed(1)}x</td>
                <td>${c.status_code}</td>
                <td>${c.latency_ms}</td>
                <td>${formatDate(c.created_at)}</td>
            </tr>
        `).join('');

        // Pagination
        const pag = data.pagination;
        const totalPages = Math.ceil(pag.total / pag.limit);
        document.getElementById('calls-pagination').innerHTML = `
            <button ${pag.page <= 1 ? 'disabled' : ''} onclick="goToCallsPage(${pag.page - 1})">上一页</button>
            <span>第 ${pag.page} / ${totalPages} 页 (共 ${pag.total} 条)</span>
            <button ${pag.page >= totalPages ? 'disabled' : ''} onclick="goToCallsPage(${pag.page + 1})">下一页</button>
        `;
    } catch (err) {
        console.error('Failed to load calls:', err);
    }
}

function goToCallsPage(page) {
    currentCallsPage = page;
    loadCalls();
}

// ===== Multipliers =====
async function loadMultipliers() {
    try {
        const data = await apiFetch('api/multipliers');
        const tbody = document.getElementById('multipliers-tbody');

        if (!data.data || data.data.length === 0) {
            tbody.innerHTML = '<tr><td colspan="7" class="text-center">暂无倍率规则</td></tr>';
            return;
        }

        tbody.innerHTML = data.data.map(m => {
            const statusBadge = m.enabled
                ? '<span class="badge badge-enabled">启用</span>'
                : '<span class="badge badge-disabled-rule">禁用</span>';

            return `
                <tr>
                    <td>${m.id}</td>
                    <td>${m.start_time}</td>
                    <td>${m.end_time}</td>
                    <td>${m.multiplier.toFixed(1)}x</td>
                    <td>${formatDaysOfWeek(m.days_of_week)}</td>
                    <td>${statusBadge}</td>
                    <td>
                        <div class="btn-group">
                            <button class="btn btn-outline btn-sm" onclick="toggleMultiplier(${m.id}, ${!m.enabled})">
                                ${m.enabled ? '禁用' : '启用'}
                            </button>
                            <button class="btn btn-danger btn-sm" onclick="deleteMultiplier(${m.id})">删除</button>
                        </div>
                    </td>
                </tr>
            `;
        }).join('');
    } catch (err) {
        console.error('Failed to load multipliers:', err);
    }
}

async function createMultiplier(e) {
    e.preventDefault();
    const body = {
        start_time: document.getElementById('mult-start-time').value,
        end_time: document.getElementById('mult-end-time').value,
        multiplier: parseFloat(document.getElementById('mult-multiplier').value),
        days_of_week: document.getElementById('mult-days').value,
    };

    try {
        await apiFetch('api/multipliers', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
        });
        closeModal('create-multiplier-modal');
        document.getElementById('create-multiplier-form').reset();
        loadMultipliers();
    } catch (err) {
        alert('创建失败: ' + err.message);
    }
}

async function toggleMultiplier(id, enable) {
    try {
        await apiFetch(`api/multipliers/${id}`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled: enable }),
        });
        loadMultipliers();
    } catch (err) {
        alert('操作失败: ' + err.message);
    }
}

async function deleteMultiplier(id) {
    if (!confirm('确定要删除此规则吗？')) return;

    try {
        await apiFetch(`api/multipliers/${id}`, { method: 'DELETE' });
        loadMultipliers();
    } catch (err) {
        alert('删除失败: ' + err.message);
    }
}

// ===== Settings =====
async function openSettings() {
    showModal('settings-modal');
    loadSettings();
}

async function loadSettings() {
    try {
        const data = await apiFetch('api/settings');
        const statusEl = document.getElementById('api-key-status');
        const endpoint = data.endpoint || '';
        document.getElementById('endpoint-input').value = endpoint;
        if (data.api_key_configured) {
            statusEl.textContent = '已配置 ✅ (端点: ' + endpoint + ')';
            statusEl.className = 'badge badge-success';
            document.getElementById('api-key-input').placeholder = '已配置，输入新 Key 覆盖';
        } else {
            statusEl.textContent = '未配置 ⚠️';
            statusEl.className = 'badge badge-warning';
            document.getElementById('api-key-input').placeholder = '输入你的智谱 API Key';
        }
    } catch (err) {
        document.getElementById('api-key-status').textContent = '查询失败';
        document.getElementById('api-key-status').className = 'badge badge-error';
    }
}

async function saveSettings() {
    const apiKey = document.getElementById('api-key-input').value.trim();
    const endpoint = document.getElementById('endpoint-input').value.trim();

    if (!apiKey && !endpoint) {
        alert('请至少输入 API Key 或端点地址');
        return;
    }

    const resultBox = document.getElementById('settings-result');
    const btn = document.getElementById('save-settings-btn');
    btn.disabled = true;
    btn.textContent = '保存中...';

    try {
        const body = {};
        if (apiKey) body.zhipu_api_key = apiKey;
        if (endpoint) body.zhipu_endpoint = endpoint;

        const data = await apiFetch('api/settings', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
        });
        resultBox.className = 'result-box success';
        resultBox.textContent = data.message + ' → systemctl restart llm-gateway';
        resultBox.classList.remove('hidden');
        document.getElementById('api-key-status').textContent = '已保存（需重启）✅';
        document.getElementById('api-key-status').className = 'badge badge-success';
        document.getElementById('api-key-input').value = '';
    } catch (err) {
        resultBox.className = 'result-box error';
        resultBox.textContent = '保存失败: ' + err.message;
        resultBox.classList.remove('hidden');
    } finally {
        btn.disabled = false;
        btn.textContent = '保存配置';
    }
}

// ===== Auth =====
async function logout() {
    try {
        await apiFetch('logout', { method: 'POST' });
    } catch (err) {
        // Ignore
    }
    window.location.href = 'login';
}

// ===== Modal Helpers =====
function showModal(id) {
    document.getElementById(id).classList.remove('hidden');
}

function closeModal(id) {
    document.getElementById(id).classList.add('hidden');
}

// ===== Utility Functions =====
function escapeHtml(str) {
    const div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
}

function escapeAttr(str) {
    return str.replace(/'/g, "\\'").replace(/"/g, '&quot;');
}

function formatDate(dateStr) {
    if (!dateStr) return '-';
    try {
        const d = new Date(dateStr);
        return d.toLocaleString('zh-CN');
    } catch {
        return dateStr;
    }
}

function formatDaysOfWeek(days) {
    if (days === '*') return '每天';
    const map = { '0': '日', '1': '一', '2': '二', '3': '三', '4': '四', '5': '五', '6': '六' };
    return days.split(',').map(d => '周' + map[d.trim()]).join(', ');
}

// ===== Share / Copy helpers =====
function copyShareAll() {
    const ta = document.getElementById('share-all-text');
    ta.select();
    navigator.clipboard.writeText(ta.value).then(() => {
        const btn = document.querySelector('#subkey-modal .btn-primary');
        btn.textContent = '✅ 已复制全部信息！';
        setTimeout(() => btn.textContent = '📋 一键复制全部', 2000);
    });
}

function copyToClipboard(elementId) {
    const el = document.getElementById(elementId);
    const text = el.textContent || el.value;
    navigator.clipboard.writeText(text).then(() => {
        // Find the button that triggered this
        const btn = el.parentElement.querySelector('button');
        if (btn) {
            btn.textContent = '✅';
            setTimeout(() => btn.textContent = '📋 复制 Key', 1500);
        }
    });
}

// ===== Close modals on background click =====
document.addEventListener('click', (e) => {
    if (e.target.classList.contains('modal')) {
        e.target.classList.add('hidden');
    }
});
