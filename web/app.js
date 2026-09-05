(() => {
  'use strict';

  const AUTO_REFRESH_MS = 5000;
  const MAX_RENDER_ROWS = 300;

  const state = {
    config: null,
    leases: [],
    status: null,
    online: null,
    includeStandby: false,
    sort: { key: 'id', dir: 1 },
    configDirty: false,
    refreshTimer: null,
    lastFocused: null,
    loggedIn: false,
  };

  const $ = (id) => document.getElementById(id);
  const durationToSeconds = (value) => Number(value || 0) / 1000000000;
  const secondsToDuration = (value) => Math.max(0, Number(value || 0)) * 1000000000;

  // ---------- 登录状态 ----------

  function setLoggedIn(loggedIn) {
    state.loggedIn = loggedIn;
    document.body.classList.toggle('is-logged-out', !loggedIn);
    const overlay = $('loginOverlay');
    if (overlay) overlay.hidden = loggedIn;
    if (!loggedIn) {
      const error = $('loginError');
      if (error) error.hidden = true;
      const password = $('loginPassword');
      if (password) password.value = '';
    }
  }

  function handleUnauthorized() {
    setLoggedIn(false);
  }

  async function login(username, password) {
    const response = await fetch('/v1/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password }),
    });
    if (!response.ok) {
      let message = `${response.status} ${response.statusText}`;
      try { message = (await response.json()).error || message; } catch (_) {}
      throw new Error(message);
    }
  }

  async function bootstrapAuth() {
    try {
      const response = await fetch('/v1/auth/session');
      const session = await response.json();
      setLoggedIn(Boolean(session.authenticated));
      if (session.authenticated) {
        await refresh();
        scheduleAutoRefresh();
      }
    } catch (_) {
      setLoggedIn(false);
      notify('无法连接管理 API，请确认服务正在运行。', 'error');
    }
  }

  // ---------- 基础请求 ----------

  async function request(path, options = {}) {
    const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
    const response = await fetch(path, { ...options, headers });
    if (response.status === 401) {
      handleUnauthorized();
      throw new Error('登录已失效，请重新登录。');
    }
    if (!response.ok) {
      let message = `${response.status} ${response.statusText}`;
      try { message = (await response.json()).error || message; } catch (_) {}
      throw new Error(message);
    }
    return response.status === 204 ? null : response.json();
  }

  // ---------- 通知与连接状态 ----------

  function notify(message, type = 'success') {
    const region = $('notificationRegion');
    if (!region) return;
    const item = document.createElement('div');
    item.className = `toast toast--${type}`;
    item.textContent = message;
    region.appendChild(item);
    setTimeout(() => item.remove(), 5000);
  }

  function setConnection(online) {
    if (state.online === online) return;
    state.online = online;
    const element = $('connectionState');
    if (!element) return;
    element.textContent = online ? '已连接' : '连接失败';
    element.classList.toggle('is-online', online);
    if (!online) notify('无法连接管理 API，将在下个周期自动重试。', 'error');
  }

  // ---------- 模态框 ----------

  function isModalOpen() { return !$('modalRoot').hidden; }

  function openModal({ title, body, actions }) {
    const root = $('modalRoot');
    state.lastFocused = document.activeElement;
    $('modalTitle').textContent = title;
    const bodyHost = $('modalBody');
    bodyHost.replaceChildren(...(Array.isArray(body) ? body : [body]));
    const actionsHost = $('modalActions');
    actionsHost.replaceChildren();
    for (const action of actions || []) {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = action.className || 'secondary-button';
      button.textContent = action.label;
      button.addEventListener('click', () => action.onClick?.(button));
      actionsHost.appendChild(button);
    }
    root.hidden = false;
    const focusable = bodyHost.querySelector('input, select, textarea');
    (focusable || root.querySelector('.modal-close')).focus();
  }

  function closeModal() {
    const root = $('modalRoot');
    if (root.hidden) return;
    root.hidden = true;
    $('modalBody').replaceChildren();
    $('modalActions').replaceChildren();
    state.lastFocused?.focus?.();
    state.lastFocused = null;
  }

  function confirmDialog({ title, message, confirmText = '确认', danger = true }) {
    return new Promise((resolve) => {
      let settled = false;
      const decide = (value) => { settled = true; closeModal(); resolve(value); };
      const body = document.createElement('p');
      body.className = 'confirm-text';
      body.textContent = message;
      openModal({
        title,
        body,
        actions: [
          { label: '取消', className: 'secondary-button', onClick: () => decide(false) },
          { label: confirmText, className: danger ? 'danger-button' : 'primary-button', onClick: () => decide(true) },
        ],
      });
      // 关闭（ESC / 背景点击）视为取消。
      rootOnceCancel(() => { if (!settled) resolve(false); });
    });
  }

  let cancelHook = null;
  function rootOnceCancel(hook) { cancelHook = hook; }
  function fireCancelHook() {
    const hook = cancelHook;
    cancelHook = null;
    hook?.();
  }

  function openCreateLeaseModal() {
    const idField = document.createElement('label');
    idField.className = 'field';
    const idLabel = document.createElement('span');
    idLabel.textContent = '租约 ID';
    const idInput = document.createElement('input');
    idInput.type = 'text';
    idInput.required = true;
    idInput.placeholder = '例如 client-a';
    idField.append(idLabel, idInput);

    const persistentLabel = document.createElement('label');
    persistentLabel.className = 'check-field';
    const persistentInput = document.createElement('input');
    persistentInput.type = 'checkbox';
    persistentInput.appendChild(document.createTextNode(''));
    persistentLabel.append(persistentInput, document.createTextNode('持久租约（不受空闲回收影响）'));

    const note = document.createElement('small');
    note.textContent = '备用池就绪时分配是即时的：返回该客户端专属的 SOCKS5 端口和出口 IPv6。';
    const error = document.createElement('p');
    error.className = 'modal-error';
    error.hidden = true;

    openModal({
      title: '新建代理租约',
      body: [idField, persistentLabel, note, error],
      actions: [
        { label: '取消', className: 'secondary-button', onClick: closeModal },
        {
          label: '创建',
          className: 'primary-button',
          onClick: async () => {
            const id = idInput.value.trim();
            if (!id) { error.textContent = '租约 ID 不能为空。'; error.hidden = false; idInput.focus(); return; }
            try {
              await request('/v1/leases', { method: 'POST', body: JSON.stringify({ id, persistent: persistentInput.checked }) });
              closeModal();
              notify(`已为 ${id} 分配代理。`);
              await refresh();
            } catch (err) { error.textContent = err.message; error.hidden = false; }
          },
        },
      ],
    });
    idInput.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') { event.preventDefault(); $('modalActions').lastElementChild?.click(); }
    });
  }

  // ---------- 格式化 ----------

  function formatUptime(totalSeconds) {
    const seconds = Math.max(0, Math.floor(totalSeconds || 0));
    const days = Math.floor(seconds / 86400);
    const hours = Math.floor((seconds % 86400) / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);
    if (days > 0) return `${days} 天 ${hours} 小时`;
    if (hours > 0) return `${hours} 小时 ${minutes} 分`;
    if (minutes > 0) return `${minutes} 分 ${seconds % 60} 秒`;
    return `${seconds} 秒`;
  }

  function formatRelative(iso) {
    if (!iso) return '-';
    const then = new Date(iso).getTime();
    if (Number.isNaN(then)) return '-';
    const diff = Math.max(0, Date.now() - then) / 1000;
    if (diff < 5) return '刚刚';
    if (diff < 60) return `${Math.floor(diff)} 秒前`;
    if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
    if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
    return `${Math.floor(diff / 86400)} 天前`;
  }

  function fullTime(iso) {
    const date = iso ? new Date(iso) : null;
    return date && !Number.isNaN(date.getTime()) ? date.toLocaleString() : '-';
  }

  const formatNumber = (value) => Number(value || 0).toLocaleString('zh-CN');

  async function copyText(text) {
    try {
      await navigator.clipboard.writeText(text);
      notify('已复制到剪贴板。');
    } catch (_) { notify('浏览器不支持剪贴板复制。', 'error'); }
  }

  function cell(row, value, className) {
    const element = row.insertCell();
    element.textContent = String(value);
    if (className) element.className = className;
    return element;
  }

  function actionButton(label, className, title, onClick) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = `button-small ${className}`;
    button.textContent = label;
    if (title) button.title = title;
    button.addEventListener('click', onClick);
    return button;
  }

  // ---------- 运行状态 ----------

  function renderStatus() {
    const status = state.status || {};
    const cfg = state.config || {};
    const ok = status.status === 'ok';
    $('serviceStatus').textContent = ok ? '正常' : '未知';
    $('serviceStatusHint').textContent = ok ? '管理 API 响应正常' : '请检查服务';
    $('serviceStatus').classList.toggle('metric-ok', ok);
    $('serviceStatus').classList.toggle('metric-bad', !ok);
    $('uptime').textContent = formatUptime(status.uptime_seconds);

    const leaseCount = status.lease_count ?? state.leases.length;
    $('activeLeases').textContent = formatNumber(leaseCount);
    $('persistentHint').textContent = `其中持久 ${status.persistent_count ?? 0} 个`;

    const standby = status.standby_count ?? 0;
    $('standbyLeases').textContent = `${formatNumber(standby)} / ${formatNumber(status.min_leases ?? 0)}`;

    const total = leaseCount + standby;
    const max = status.max_leases ?? 0;
    $('poolUsage').textContent = `${formatNumber(total)} / ${formatNumber(max)}`;
    const percent = max > 0 ? Math.min(100, Math.round((total / max) * 100)) : 0;
    $('poolUsageBar').style.width = `${percent}%`;
    $('poolUsageBar').classList.toggle('is-warning', percent >= 80 && percent < 95);
    $('poolUsageBar').classList.toggle('is-critical', percent >= 95);
    const progress = document.querySelector('.progress');
    progress?.setAttribute('aria-valuenow', String(percent));
    $('poolUsageHint').textContent = `客户端 ${leaseCount} + 备用 ${standby}，已用 ${percent}%`;

    $('requestCount').textContent = formatNumber(status.total_requests);

    const perIPv6 = (cfg.socks?.mode || status.socks_mode) === 'per_ipv6';
    if (perIPv6) {
      $('listenerCount').textContent = formatNumber(status.listener_count ?? 0);
      $('listenerHint').textContent = 'per_ipv6 模式动态监听';
    } else {
      $('listenerCount').textContent = '1';
      $('listenerHint').textContent = 'multiplex 单端口复用';
    }
    $('socksModeLabel').textContent = cfg.socks?.mode === 'per_ipv6' ? '一端口一 IPv6' : (cfg.socks?.mode === 'multiplex' ? '单端口复用' : (status.socks_mode || '-'));
    $('prefixHint').textContent = cfg.ipv6_prefix || status.ipv6_prefix || '-';

    renderPortTable();
    renderServiceInfo();
  }

  function portFromAddress(address) {
    const index = String(address || '').lastIndexOf(':');
    return index >= 0 ? Number(String(address).slice(index + 1)) : 0;
  }

  function renderPortTable() {
    const body = $('portTableBody');
    const cfg = state.config;
    const status = state.status || {};
    body.replaceChildren();
    if (!cfg) {
      cell(body.insertRow(), '正在加载端口状态', 'empty-state').colSpan = 6;
      return;
    }
    const perIPv6 = cfg.socks?.mode === 'per_ipv6';
    const rows = [];

    if (perIPv6) {
      const leasesByPort = new Map(state.leases.map((item) => [item.port, item]));
      const listeningPorts = new Set();
      for (const listener of status.listeners || []) {
        const port = portFromAddress(listener.address);
        listeningPorts.add(port);
        rows.push({ port, lease: listener.id, leaseInfo: state.leases.find((item) => item.id === listener.id), listening: true });
      }
      for (const port of cfg.socks?.always_on_ports || []) {
        if (listeningPorts.has(port)) continue;
        rows.push({ port, lease: `port-${port}`, leaseInfo: leasesByPort.get(port), listening: false });
      }
      rows.sort((a, b) => a.port - b.port);
    } else {
      rows.push({
        port: portFromAddress(cfg.socks?.listen_address),
        lease: '全部租约（复用）',
        leaseInfo: null,
        listening: status.status === 'ok',
        multiplex: true,
      });
    }

    if (!rows.length) {
      cell(body.insertRow(), perIPv6 ? '未配置常开端口；客户端租约会在认领后自动开启监听。' : '服务未运行', 'empty-state').colSpan = 6;
      return;
    }

    for (const row of rows) {
      const tr = body.insertRow();
      cell(tr, row.port || '-');
      cell(tr, row.lease);
      const ipv6 = row.leaseInfo?.ipv6 || (row.multiplex ? '按租约动态分配' : '-');
      const ipv6Cell = cell(tr, ipv6, 'mono');
      ipv6Cell.classList.add('ipv6-cell');
      const stateCell = cell(tr, row.listening ? '监听中' : '未监听');
      stateCell.innerHTML = '';
      const badge = document.createElement('span');
      badge.className = row.listening ? 'pill pill--ok' : 'pill pill--warn';
      badge.textContent = row.listening ? '监听中' : '未监听';
      stateCell.appendChild(badge);
      cell(tr, formatNumber(row.leaseInfo?.requests ?? (row.multiplex ? state.status?.total_requests : 0)));
      const actions = tr.insertCell();
      if (row.port && row.leaseInfo) {
        actions.appendChild(actionButton('测试', 'secondary-button', '网络可用性测试：经代理出网并比对出口 IPv6', () => {
          testProxy(row.leaseInfo.id);
        }));
        actions.appendChild(actionButton('复制地址', 'secondary-button', '复制该端口的 SOCKS5 连接地址', () => {
          copyText(`${socksHostValue(cfg)}:${row.port}`);
        }));
      }
    }
  }

  function renderServiceInfo() {
    const list = $('serviceInfoList');
    const cfg = state.config;
    const status = state.status || {};
    if (!cfg) { list.replaceChildren(); return; }
    const idle = durationToSeconds(cfg.idle_timeout);
    const rotateAfter = durationToSeconds(cfg.rotate_after);
    const entries = [
      ['IPv6 前缀', cfg.ipv6_prefix],
      ['SOCKS 模式', cfg.socks?.mode === 'per_ipv6' ? '一端口一 IPv6（per_ipv6）' : '单端口复用（multiplex）'],
      ['SOCKS 监听地址', cfg.socks?.listen_address],
      ['动态端口范围', `${cfg.socks?.port_start} - ${cfg.socks?.port_end}（共 ${(cfg.socks?.port_end ?? 0) - (cfg.socks?.port_start ?? 0) + 1} 个）`],
      ['常开端口', (cfg.socks?.always_on_ports || []).length ? (cfg.socks.always_on_ports.join(', ')) : '无'],
      ['空闲回收', idle > 0 ? `超过 ${formatDurationHuman(idle)} 且超出常驻保底时回收` : '关闭'],
      ['时间轮换', rotateAfter > 0 ? `每 ${formatDurationHuman(rotateAfter)}` : '关闭'],
      ['请求数轮换', cfg.rotate_requests > 0 ? `每 ${formatNumber(cfg.rotate_requests)} 次请求` : '关闭'],
      ['管理 API', cfg.admin?.listen_address],
      ['Web 登录账号', cfg.admin?.webui?.username || 'admin'],
      ['令牌保护', state.status?.token_required ? '已启用' : '未启用'],
    ];
    list.replaceChildren(...entries.map(([name, value]) => {
      const wrapper = document.createElement('div');
      wrapper.className = 'kv-item';
      const dt = document.createElement('dt');
      dt.textContent = name;
      const dd = document.createElement('dd');
      dd.textContent = String(value ?? '-');
      wrapper.append(dt, dd);
      return wrapper;
    }));
  }

  function formatDurationHuman(seconds) {
    if (seconds % 3600 === 0 && seconds >= 3600) return `${seconds / 3600} 小时`;
    if (seconds % 60 === 0 && seconds >= 60) return `${seconds / 60} 分钟`;
    return `${seconds} 秒`;
  }

  // ---------- 代理租约 ----------

  function sortedLeases(leases) {
    const { key, dir } = state.sort;
    const factor = (item) => {
      const value = item[key];
      if (key === 'created_at' || key === 'last_used_at') return new Date(value).getTime() || 0;
      if (typeof value === 'boolean') return value ? 1 : 0;
      if (typeof value === 'number') return value;
      return String(value ?? '');
    };
    return [...leases].sort((a, b) => {
      const left = factor(a);
      const right = factor(b);
      if (typeof left === 'string' || typeof right === 'string') {
        return String(left).localeCompare(String(right)) * dir;
      }
      return (left - right) * dir;
    });
  }

  function renderLeases() {
    const body = $('leaseTableBody');
    if (!body) return;
    const cfg = state.config;
    const filter = ($('leaseFilter')?.value || '').toLowerCase().trim();
    const filtered = state.leases.filter((item) =>
      !filter || `${item.id} ${item.ipv6} ${item.port || ''}`.toLowerCase().includes(filter));
    const sorted = sortedLeases(filtered);

    document.querySelectorAll('.sort-button').forEach((button) => {
      const active = button.dataset.sort === state.sort.key;
      button.classList.toggle('is-sorted', active);
      button.dataset.dir = active ? (state.sort.dir === 1 ? 'asc' : 'desc') : '';
    });

    body.replaceChildren();
    if (!sorted.length) {
      cell(body.insertRow(), filter ? '没有匹配的租约' : '暂无客户端租约；点击「新建代理」或在客户端申请后出现', 'empty-state').colSpan = 9;
      return;
    }

    const overflow = sorted.length > MAX_RENDER_ROWS;
    const footnote = $('leaseTableFootnote');
    footnote.hidden = !overflow;
    if (overflow) footnote.textContent = `共 ${sorted.length} 条，仅显示前 ${MAX_RENDER_ROWS} 条；可使用筛选缩小范围。`;

    for (const item of sorted.slice(0, MAX_RENDER_ROWS)) {
      const row = body.insertRow();
      if (item.role === 'standby') row.classList.add('standby-row');
      const idCell = cell(row, item.id, 'mono');
      idCell.classList.add('ipv6-cell');
      cell(row, item.port || '-');
      const ipv6Cell = cell(row, item.ipv6, 'mono');
      ipv6Cell.classList.add('ipv6-cell');

      const roleCell = row.insertCell();
      const roleBadge = document.createElement('span');
      roleBadge.className = item.role === 'standby' ? 'pill' : 'pill pill--ok';
      roleBadge.textContent = item.role === 'standby' ? '备用' : '客户端';
      roleCell.appendChild(roleBadge);

      if (item.role === 'standby') {
        cell(row, '-');
      } else {
        const persistentCell = row.insertCell();
        const checkbox = document.createElement('input');
        checkbox.type = 'checkbox';
        checkbox.checked = Boolean(item.persistent);
        checkbox.title = '持久租约免于空闲回收';
        checkbox.addEventListener('change', async () => {
          try {
            await request(`/v1/leases/${encodeURIComponent(item.id)}`, { method: 'PATCH', body: JSON.stringify({ persistent: checkbox.checked }) });
            notify(`已${checkbox.checked ? '开启' : '关闭'} ${item.id} 的持久保护。`);
            await refresh();
          } catch (error) { checkbox.checked = !checkbox.checked; notify(error.message, 'error'); }
        });
        persistentCell.appendChild(checkbox);
      }

      cell(row, formatNumber(item.requests));
      const created = cell(row, formatRelative(item.created_at));
      created.title = fullTime(item.created_at);
      const used = cell(row, formatRelative(item.last_used_at));
      used.title = fullTime(item.last_used_at);

      const actions = row.insertCell();
      actions.className = 'actions-cell';
      if (item.role !== 'standby') {
        actions.appendChild(actionButton('测试', 'secondary-button', '网络可用性测试：经代理出网并比对出口 IPv6', () => {
          testProxy(item.id);
        }));
      }
      if (item.port) {
        actions.appendChild(actionButton('复制', 'secondary-button', `复制 SOCKS5 地址 ${socksHostValue(cfg)}:${item.port}`, () => {
          copyText(`${socksHostValue(cfg)}:${item.port}`);
        }));
      }
      actions.appendChild(actionButton('换IP', 'secondary-button', '分配新的 IPv6 出口地址，端口不变', async () => {
        try {
          await request(`/v1/leases/${encodeURIComponent(item.id)}/rotate`, { method: 'POST', body: '{}' });
          notify(`已为 ${item.id} 更换 IPv6。`);
          await refresh();
        } catch (error) { notify(error.message, 'error'); }
      }));
      if (item.role !== 'standby' && !item.persistent) {
        actions.appendChild(actionButton('回收', 'secondary-button', '释放并立即重新获取：新端口 + 新 IPv6，租约 ID 不变', async () => {
          try {
            await request(`/v1/leases/${encodeURIComponent(item.id)}/recycle`, { method: 'POST', body: '{}' });
            notify(`已为 ${item.id} 回收并重新分配代理。`);
            await refresh();
          } catch (error) { notify(error.message, 'error'); }
        }));
      }
      actions.appendChild(actionButton('删除', 'danger-button', '销毁租约并回收端口与 IPv6', async () => {
        const confirmed = await confirmDialog({
          title: `删除租约 ${item.id}`,
          message: `将关闭 ${item.port ? `端口 ${item.port} 的` : ''}监听并回收 IPv6 ${item.ipv6 || ''}，客户端会立即断开。确定删除？`,
          confirmText: '删除',
        });
        if (!confirmed) return;
        try {
          await request(`/v1/leases/${encodeURIComponent(item.id)}`, { method: 'DELETE' });
          notify(`已删除租约 ${item.id}。`);
          await refresh();
        } catch (error) { notify(error.message, 'error'); }
      }));
    }
  }

  // ---------- 客户端接入 ----------

  function serverHost() {
    return window.location.hostname || '127.0.0.1';
  }

  function adminUrlValue(cfg) {
    const port = String(cfg?.admin?.listen_address || '').split(':').pop();
    return `http://${serverHost()}:${/^\d+$/.test(port) ? port : 10070}`;
  }

  function socksHostValue(cfg) {
    const listen = cfg?.socks?.listen_address || '';
    const idx = listen.lastIndexOf(':');
    const host = (idx > 0 ? listen.slice(0, idx) : listen).replace(/^\[|\]$/g, '');
    if (!host || host === '::' || host === '::1' || host === '0.0.0.0' || host === '127.0.0.1') return serverHost();
    return host;
  }

  function connectionRows() {
    const cfg = state.config;
    if (!cfg) return [];
    const adminPort = String(cfg.admin?.listen_address || '').split(':').pop();
    const port = /^\d+$/.test(adminPort) ? adminPort : '10070';
    const leaseIds = [...new Set(state.leases.filter((item) => item.role !== 'standby').map((item) => item.id).filter(Boolean))].join(', ');
    const rotateMinutes = Math.max(0, Number(cfg.rotate_after || 0) / 1000000000 / 60);
    return [
      { name: '池管理端 URL', value: adminUrlValue(cfg), hint: '客户端 -admin 参数 / 管理 API 根地址' },
      { name: '池 Token（可空）', value: cfg.admin?.token || '', hint: '未启用令牌则留空；客户端用 -token 或环境变量 IPV6_PROXY_POOL_TOKEN 提供' },
      { name: '租约 ID（空=自动）', value: leaseIds, hint: '现有租约标识，客户端填其中一个即可复用对应端口；留空自动创建' },
      { name: 'SOCKS5 地址', value: socksHostValue(cfg), hint: `代理端口范围 ${cfg.socks?.port_start ?? '-'}-${cfg.socks?.port_end ?? '-'}，每个租约一个端口` },
      { name: '换IP状态码', value: '', hint: '由客户端自行配置（如 403,429），服务端不涉及' },
      { name: '每 N 次请求换IP', value: String(cfg.rotate_requests ?? 0), hint: '服务端按请求数自动轮换 IPv6；0=关闭' },
      { name: '按时间换IP（分钟）', value: String(rotateMinutes), hint: '服务端按时间自动轮换 IPv6；0=关闭' },
      { name: 'IPv6 前缀 / 模式', value: `${cfg.ipv6_prefix || '-'} / ${cfg.socks?.mode || '-'}`, hint: '当前代理池的出口网段与工作模式' },
    ];
  }

  function renderConnection() {
    const body = $('connectionTableBody');
    if (!body) return;
    const rows = connectionRows();
    body.replaceChildren();
    if (!rows.length) {
      cell(body.insertRow(), '暂无连接信息', 'empty-state').colSpan = 4;
      return;
    }
    for (const item of rows) {
      const row = body.insertRow();
      cell(row, item.name);
      cell(row, item.value || '（留空）', 'connection-value');
      cell(row, item.hint, 'connection-hint');
      const actionCell = row.insertCell();
      if (item.value) {
        actionCell.appendChild(actionButton('复制', 'secondary-button', '', async () => {
          await copyText(item.value);
        }));
      }
    }
    renderCliExamples();
  }

  function cliExampleText() {
    const cfg = state.config;
    if (!cfg) return '';
    const admin = adminUrlValue(cfg);
    const token = cfg.admin?.token || '';
    const tokenPart = token ? ` -token ${token}` : '';
    const firstPort = state.leases.find((item) => item.role !== 'standby' && item.port)?.port;
    const lines = [
      '# 申请代理（返回 SOCKS5 端口和出口 IPv6）',
      `ipv6-proxy-pool client create -name client-a -admin ${admin}${tokenPart}`,
      '# 更换出口 IPv6（端口不变）',
      `ipv6-proxy-pool client rotate -name client-a -admin ${admin}${tokenPart}`,
      '# 释放并立即重新获取（同 ID，新端口 + 新 IPv6）',
      `ipv6-proxy-pool client recycle -name client-a -admin ${admin}${tokenPart}`,
      '# 销毁代理并回收端口 / IPv6',
      `ipv6-proxy-pool client delete -name client-a -admin ${admin}${tokenPart}`,
    ];
    if (firstPort) {
      lines.push('# 通过租约端口发起请求（示例查看当前出口 IP）', `curl --socks5-hostname ${socksHostValue(cfg)}:${firstPort} https://api64.ipify.org`);
    }
    return lines.join('\n');
  }

  function renderCliExamples() {
    const block = $('cliExamples');
    if (!block) return;
    block.textContent = cliExampleText() || '正在加载…';
  }

  async function copyAllConnection() {
    const lines = connectionRows().map((item) => `${item.name}: ${item.value || '（留空）'}`);
    await copyText(lines.join('\n'));
  }

  // 对某个租约发起网络可用性测试：经代理出网并比对出口 IPv6 与租约地址。
  async function testProxy(id, port) {
    try {
      const result = await request('/v1/proxies:test', {
        method: 'POST',
        body: JSON.stringify(port ? { port } : { id }),
      });
      if (!result.ok) {
        notify(`代理测试失败：${result.error}`, 'error');
        return;
      }
      const parts = [`延迟 ${result.latency_ms}ms`];
      if (result.exit_ipv6) {
        parts.push(`出口 ${result.exit_ipv6}`);
        parts.push(result.ipv6_match ? '与租约地址一致' : `与租约地址不一致（期望 ${result.expected_ipv6}）`);
      }
      notify(`代理可用：${parts.join('，')}`, result.ipv6_match === false ? 'error' : '');
    } catch (error) { notify(error.message, 'error'); }
  }

  // ---------- 配置 ----------

  function putValue(id, value) { const element = $(id); if (element) element.value = value ?? ''; }
  function getValue(id) { return $(id)?.value ?? ''; }

  function renderConfig() { renderConfigFrom(state.config); }

  function renderConfigFrom(cfg) {
    if (!cfg) return;
    putValue('ipv6Prefix', cfg.ipv6_prefix);
    putValue('minLeases', cfg.min_leases);
    putValue('maxLeases', cfg.max_leases);
    putValue('idleTimeout', durationToSeconds(cfg.idle_timeout));
    putValue('rotateAfter', durationToSeconds(cfg.rotate_after));
    putValue('rotateRequests', cfg.rotate_requests);
    putValue('socksListenAddress', cfg.socks.listen_address);
    putValue('portStart', cfg.socks.port_start);
    putValue('portEnd', cfg.socks.port_end);
    putValue('alwaysOnPorts', (cfg.socks.always_on_ports || []).join(', '));
    putValue('adminListenAddress', cfg.admin.listen_address);
    // 出于安全考虑不回显令牌；留空提交表示保持现有令牌不变。
    putValue('adminToken', '');
    // 用户名同样不回显默认值；留空提交表示保持现有用户名不变。
    putValue('webuiUsername', cfg.admin?.webui?.username || '');
    // 密码同样不回显；留空提交表示保持现有密码不变。
    putValue('webuiPassword', '');
    const mode = document.querySelector(`input[name="mode"][value="${cfg.socks.mode}"]`);
    if (mode) mode.checked = true;
    updateModeDependentFields();
  }

  function updateModeDependentFields() {
    const perIPv6 = document.querySelector('input[name="mode"]:checked')?.value === 'per_ipv6';
    const field = $('alwaysOnPortsField');
    if (!field) return;
    field.classList.toggle('is-disabled', !perIPv6);
    $('alwaysOnPorts').disabled = !perIPv6;
  }

  function parseAlwaysOnPorts(value) {
    const text = value.trim();
    if (!text) return [];
    const ports = text.split(',').map((item) => {
      const token = item.trim();
      if (!/^\d+$/.test(token)) {
        throw new Error(`常开端口 "${token}" 不是有效整数。`);
      }
      const port = Number(token);
      if (!Number.isInteger(port) || port < 1 || port > 65535) {
        throw new Error(`常开端口 ${token} 必须在 1 到 65535 之间。`);
      }
      return port;
    });
    return [...new Set(ports)].sort((a, b) => a - b);
  }

  function readConfig() {
    return {
      ipv6_prefix: getValue('ipv6Prefix').trim(),
      min_leases: Number(getValue('minLeases')),
      max_leases: Number(getValue('maxLeases')),
      idle_timeout: secondsToDuration(getValue('idleTimeout')),
      rotate_after: secondsToDuration(getValue('rotateAfter')),
      rotate_requests: Number(getValue('rotateRequests')),
      socks: {
        mode: document.querySelector('input[name="mode"]:checked')?.value || 'multiplex',
        listen_address: getValue('socksListenAddress').trim(),
        port_start: Number(getValue('portStart')),
        port_end: Number(getValue('portEnd')),
        always_on_ports: parseAlwaysOnPorts(getValue('alwaysOnPorts'))
      },
      admin: {
        listen_address: getValue('adminListenAddress').trim(),
        // 留空 = 保持现有令牌（表单不回显令牌）。
        token: getValue('adminToken').trim() || (state.config?.admin?.token || ''),
        webui: {
          username: getValue('webuiUsername').trim() || (state.config?.admin?.webui?.username || ''),
          // 留空 = 保持现有密码（表单不回显密码）。
          password: getValue('webuiPassword') || (state.config?.admin?.webui?.password || '')
        }
      }
    };
  }

  function setConfigDirty(dirty) {
    state.configDirty = dirty;
    $('configDirtyBadge').hidden = !dirty;
  }

  // ---------- 刷新与自动刷新 ----------

  async function refresh() {
    if (!state.loggedIn) return;
    try {
      const [status, leases, config] = await Promise.all([
        request('/v1/status'),
        request(`/v1/leases${state.includeStandby ? '?include_standby=true' : ''}`),
        request('/v1/config')
      ]);
      state.status = status;
      state.leases = leases;
      state.config = config;
      setConnection(true);
      renderStatus();
      renderConnection();
      renderLeases();
      if (!state.configDirty) renderConfig();
      const stamp = $('lastRefreshTime');
      if (stamp) stamp.textContent = `更新于 ${new Date().toLocaleTimeString()}`;
    } catch (error) {
      setConnection(false);
    }
  }

  function scheduleAutoRefresh() {
    if (state.refreshTimer) clearInterval(state.refreshTimer);
    state.refreshTimer = null;
    if (!$('autoRefreshToggle')?.checked) return;
    state.refreshTimer = setInterval(() => {
      if (document.hidden || isModalOpen()) return;
      refresh();
    }, AUTO_REFRESH_MS);
  }

  // ---------- 事件绑定 ----------

  // 页签切换 + hash 深链（#leases 直达对应页签）。
  function activateTab(name) {
    document.querySelectorAll('[data-tab]').forEach((item) => item.classList.toggle('is-active', item.dataset.tab === name));
    document.querySelectorAll('[data-panel]').forEach((panel) => {
      const active = panel.dataset.panel === name;
      panel.hidden = !active;
      panel.classList.toggle('is-active', active);
    });
  }

  function tabFromHash() {
    const name = window.location.hash.replace(/^#/, '');
    return document.querySelector(`[data-tab="${CSS.escape(name)}"]`) ? name : null;
  }

  document.querySelectorAll('[data-tab]').forEach((button) => button.addEventListener('click', () => {
    activateTab(button.dataset.tab);
    history.replaceState(null, '', `#${button.dataset.tab}`);
  }));
  window.addEventListener('hashchange', () => {
    const name = tabFromHash();
    if (name) activateTab(name);
  });
  const initialTab = tabFromHash();
  if (initialTab) activateTab(initialTab);

  $('refreshButton')?.addEventListener('click', () => refresh());
  $('leaseFilter')?.addEventListener('input', renderLeases);
  $('showStandbyToggle')?.addEventListener('change', async (event) => {
    state.includeStandby = event.target.checked;
    await refresh();
  });
  $('autoRefreshToggle')?.addEventListener('change', scheduleAutoRefresh);
  document.querySelectorAll('.sort-button').forEach((button) => button.addEventListener('click', () => {
    const key = button.dataset.sort;
    if (state.sort.key === key) {
      state.sort.dir = -state.sort.dir;
    } else {
      state.sort = { key, dir: 1 };
    }
    renderLeases();
  }));

  $('createLeaseButton')?.addEventListener('click', openCreateLeaseModal);
  $('releaseIdleButton')?.addEventListener('click', async () => {
    const confirmed = await confirmDialog({
      title: '释放空闲租约',
      message: '将回收超出常驻保底且已超过空闲超时的客户端租约；持久租约与常驻备用不受影响。确定继续？',
      confirmText: '释放空闲',
    });
    if (!confirmed) return;
    try {
      const result = await request('/v1/leases:release-idle', { method: 'POST', body: '{}' });
      notify(`已释放 ${result.released} 个空闲租约。`);
      await refresh();
    } catch (error) { notify(error.message, 'error'); }
  });
  $('copyConnectionButton')?.addEventListener('click', copyAllConnection);
  $('copyCliButton')?.addEventListener('click', async () => {
    const text = cliExampleText();
    if (text) await copyText(text);
  });

  $('configForm')?.addEventListener('input', () => setConfigDirty(true));
  $('configForm')?.addEventListener('change', (event) => {
    if (event.target?.name === 'mode') updateModeDependentFields();
    setConfigDirty(true);
  });
  $('resetButton')?.addEventListener('click', () => {
    setConfigDirty(false);
    $('restartBanner').hidden = true;
    $('saveMessage').textContent = '';
    renderConfig();
  });

  $('restoreDefaultsButton')?.addEventListener('click', async () => {
    try {
      const defaults = await request('/v1/config/defaults');
      renderConfigFrom(defaults);
      setConfigDirty(true);
      $('restartBanner').hidden = true;
      $('saveMessage').textContent = '已填入默认配置，确认后保存生效。';
      notify('已恢复默认配置，请确认后保存。');
    } catch (error) { notify(error.message, 'error'); }
  });

  $('tokenGenerateButton')?.addEventListener('click', () => {
    const bytes = new Uint8Array(24);
    crypto.getRandomValues(bytes);
    const token = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
    putValue('adminToken', token);
    setConfigDirty(true);
    const input = $('adminToken');
    input.focus();
    input.select();
    notify('已生成随机管理令牌，请立即复制保存——页面不会再次显示。');
  });

  $('configForm')?.addEventListener('submit', async (event) => {
    event.preventDefault();
    const button = $('saveButton');
    button.disabled = true;
    try {
      const result = await request('/v1/config', { method: 'PUT', body: JSON.stringify(readConfig()) });
      if (result.config) state.config = result.config;
      setConfigDirty(false);
      $('restartBanner').hidden = !result.restart_required;
      const message = result.restart_required ? '配置已保存，重启服务后生效。' : '配置已保存。';
      $('saveMessage').textContent = message;
      notify(message);
      renderConfig();
    } catch (error) {
      $('saveMessage').textContent = error.message;
      notify(error.message, 'error');
    } finally {
      button.disabled = false;
    }
  });

  // 模态框：ESC 关闭、背景/关闭按钮点击。
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && isModalOpen()) { fireCancelHook(); closeModal(); }
  });
  $('modalRoot')?.addEventListener('click', (event) => {
    if (event.target?.closest('[data-close-modal]')) { fireCancelHook(); closeModal(); }
  });

  // ---------- 登录 / 退出 ----------

  $('loginForm')?.addEventListener('submit', async (event) => {
    event.preventDefault();
    const button = $('loginSubmit');
    const error = $('loginError');
    button.disabled = true;
    error.hidden = true;
    try {
      await login($('loginUsername').value.trim(), $('loginPassword').value);
      setLoggedIn(true);
      notify('登录成功。');
      await refresh();
      scheduleAutoRefresh();
    } catch (err) {
      error.textContent = err.message;
      error.hidden = false;
    } finally {
      button.disabled = false;
    }
  });

  $('logoutButton')?.addEventListener('click', async () => {
    try { await fetch('/v1/auth/logout', { method: 'POST' }); } catch (_) {}
    setLoggedIn(false);
    if (state.refreshTimer) { clearInterval(state.refreshTimer); state.refreshTimer = null; }
  });

  bootstrapAuth();
})();
