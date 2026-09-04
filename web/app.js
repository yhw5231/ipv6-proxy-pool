(() => {
  'use strict';

  const state = { config: null, leases: [], status: null };
  const $ = (id) => document.getElementById(id);
  const durationToSeconds = (value) => Number(value || 0) / 1000000000;
  const secondsToDuration = (value) => Math.max(0, Number(value || 0)) * 1000000000;
  const getToken = () => localStorage.getItem('ipv6ProxyToken') || '';
  const setToken = (value) => localStorage.setItem('ipv6ProxyToken', value);

  async function request(path, options = {}) {
    const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
    const token = getToken();
    if (token) headers.Authorization = `Bearer ${token}`;
    const response = await fetch(path, { ...options, headers });
    if (response.status === 401 && !options._retried) {
      const input = window.prompt('需要管理令牌（config.json 中 admin.token）：');
      if (!input) throw new Error('401 未授权：缺少管理令牌');
      setToken(input.trim());
      return request(path, { ...options, _retried: true });
    }
    if (!response.ok) {
      let message = `${response.status} ${response.statusText}`;
      try { message = (await response.json()).error || message; } catch (_) {}
      throw new Error(message);
    }
    return response.status === 204 ? null : response.json();
  }

  function notify(message, error = false) {
    const region = $('notificationRegion');
    if (!region) return;
    const item = document.createElement('div');
    item.textContent = message;
    if (!error) item.style.borderColor = '#22c55e';
    region.appendChild(item);
    setTimeout(() => item.remove(), 5000);
  }

  function setConnection(online) {
    const element = $('connectionState');
    if (!element) return;
    element.textContent = online ? '已连接' : '连接失败';
    element.classList.toggle('is-online', online);
  }

  function renderStatus() {
    const status = state.status || {};
    if ($('serviceStatus')) $('serviceStatus').textContent = status.status === 'ok' ? '正常' : '未知';
    if ($('activeLeases')) $('activeLeases').textContent = String(status.lease_count ?? state.leases.length);
    if ($('standbyLeases')) $('standbyLeases').textContent =
      `${status.standby_count ?? '-'} / ${status.min_leases ?? '-'}`;
    const total = (status.lease_count ?? state.leases.length) + (status.standby_count ?? 0);
    if ($('poolUsage')) $('poolUsage').textContent = `${total} / ${status.max_leases ?? '-'}`;
    if ($('requestCount')) $('requestCount').textContent = '-';
    if ($('lastRotation')) $('lastRotation').textContent = '-';
    if ($('uptime')) $('uptime').textContent = '-';
  }

  function renderLeases() {
    const body = $('leaseTableBody');
    if (!body) return;
    const filter = ($('leaseFilter')?.value || '').toLowerCase();
    const leases = state.leases.filter((item) => `${item.id} ${item.ipv6} ${item.port || ''}`.toLowerCase().includes(filter));
    body.replaceChildren();
    if (!leases.length) {
      const row = body.insertRow();
      const cell = row.insertCell();
      cell.colSpan = 8;
      cell.className = 'empty-state';
      cell.textContent = '暂无租约';
      return;
    }
    for (const lease of leases) {
      const row = body.insertRow();
      [lease.id, lease.port || '-', lease.ipv6,
        new Date(lease.created_at).toLocaleString(),
        new Date(lease.last_used_at).toLocaleString(),
        lease.requests ?? 0].forEach((value) => {
        const cell = row.insertCell();
        cell.textContent = String(value);
      });
      const persistentCell = row.insertCell();
      const checkbox = document.createElement('input');
      checkbox.type = 'checkbox';
      checkbox.checked = Boolean(lease.persistent);
      checkbox.addEventListener('change', async () => {
        try {
          await request(`/v1/leases/${encodeURIComponent(lease.id)}`, { method: 'PATCH', body: JSON.stringify({ persistent: checkbox.checked }) });
          await refresh();
        } catch (error) { checkbox.checked = !checkbox.checked; notify(error.message, true); }
      });
      persistentCell.appendChild(checkbox);
      const actions = row.insertCell();
      const rotate = document.createElement('button');
      rotate.type = 'button';
      rotate.className = 'secondary-button';
      rotate.textContent = '换IP';
      rotate.title = '为租约分配新的 IPv6 出口地址（端口不变）';
      rotate.addEventListener('click', async () => {
        try {
          await request(`/v1/leases/${encodeURIComponent(lease.id)}/rotate`, { method: 'POST', body: '{}' });
          notify(`已为 ${lease.id} 更换 IPv6。`);
          await refresh();
        } catch (error) { notify(error.message, true); }
      });
      actions.appendChild(rotate);
      const recycle = document.createElement('button');
      recycle.type = 'button';
      recycle.className = 'secondary-button';
      recycle.textContent = '回收换新';
      recycle.title = '释放当前代理并立即重新获取：新端口 + 新 IPv6，客户端标识不变';
      recycle.addEventListener('click', async () => {
        try {
          await request(`/v1/leases/${encodeURIComponent(lease.id)}/recycle`, { method: 'POST', body: '{}' });
          notify(`已为 ${lease.id} 回收并重新分配代理。`);
          await refresh();
        } catch (error) { notify(error.message, true); }
      });
      actions.appendChild(recycle);
      const remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'danger-button';
      remove.textContent = '删除';
      remove.addEventListener('click', async () => {
        try {
          await request(`/v1/leases/${encodeURIComponent(lease.id)}`, { method: 'DELETE' });
          await refresh();
        } catch (error) { notify(error.message, true); }
      });
      actions.appendChild(remove);
    }
  }

  function putValue(id, value) { if ($(id)) $(id).value = value ?? ''; }
  function getValue(id) { return $(id)?.value ?? ''; }

  function serverHost() {
    return window.location.hostname || '127.0.0.1';
  }

  function adminUrlValue(cfg) {
    const port = String(cfg?.admin?.listen_address || '').split(':').pop();
    return `http://${serverHost()}:${/^\d+$/.test(port) ? port : 8080}`;
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
    const port = /^\d+$/.test(adminPort) ? adminPort : '8080';
    const leaseIds = [...new Set(state.leases.map((item) => item.id).filter(Boolean))].join(', ');
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
      const row = body.insertRow();
      const cell = row.insertCell();
      cell.colSpan = 4;
      cell.className = 'empty-state';
      cell.textContent = '暂无连接信息';
      return;
    }
    for (const item of rows) {
      const row = body.insertRow();
      const nameCell = row.insertCell();
      nameCell.textContent = item.name;
      const valueCell = row.insertCell();
      valueCell.className = 'connection-value';
      valueCell.textContent = item.value || '（留空）';
      const hintCell = row.insertCell();
      hintCell.className = 'connection-hint';
      hintCell.textContent = item.hint;
      const actionCell = row.insertCell();
      if (item.value) {
        const copy = document.createElement('button');
        copy.type = 'button';
        copy.className = 'secondary-button';
        copy.textContent = '复制';
        copy.addEventListener('click', async () => {
          try {
            await navigator.clipboard.writeText(item.value);
            copy.textContent = '已复制';
            setTimeout(() => { copy.textContent = '复制'; }, 1500);
          } catch (_) { notify('浏览器不支持剪贴板复制', true); }
        });
        actionCell.appendChild(copy);
      }
    }
  }

  async function copyAllConnection() {
    const lines = connectionRows().map((item) => `${item.name}: ${item.value || '（留空）'}`);
    try {
      await navigator.clipboard.writeText(lines.join('\n'));
      notify('连接信息已复制到剪贴板。');
    } catch (_) { notify('浏览器不支持剪贴板复制', true); }
  }

  function renderConfig() {
    const cfg = state.config;
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
    putValue('adminToken', cfg.admin.token || '');
    const mode = document.querySelector(`input[name="mode"][value="${cfg.socks.mode}"]`);
    if (mode) mode.checked = true;
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
        token: getValue('adminToken').trim() || (state.config?.admin?.token || '')
      }
    };
  }

  async function refresh() {
    try {
      const [status, leases, config] = await Promise.all([
        request('/v1/status'), request('/v1/leases'), request('/v1/config')
      ]);
      state.status = status;
      state.leases = leases;
      state.config = config;
      setConnection(true);
      renderStatus();
      renderConnection();
      renderLeases();
      renderConfig();
    } catch (error) {
      setConnection(false);
      notify(error.message, true);
    }
  }

  document.querySelectorAll('[data-tab]').forEach((button) => button.addEventListener('click', () => {
    document.querySelectorAll('[data-tab]').forEach((item) => item.classList.toggle('is-active', item === button));
    document.querySelectorAll('[data-panel]').forEach((panel) => {
      const active = panel.dataset.panel === button.dataset.tab;
      panel.hidden = !active;
      panel.classList.toggle('is-active', active);
    });
  }));

  $('refreshButton')?.addEventListener('click', refresh);
  $('leaseFilter')?.addEventListener('input', renderLeases);
  $('resetButton')?.addEventListener('click', renderConfig);
  $('copyConnectionButton')?.addEventListener('click', copyAllConnection);
  $('configForm')?.addEventListener('submit', async (event) => {
    event.preventDefault();
    try {
      const result = await request('/v1/config', { method: 'PUT', body: JSON.stringify(readConfig()) });
      const message = result.restart_required ? '配置已保存，需要重启服务后生效。' : '配置已保存。';
      if ($('saveMessage')) $('saveMessage').textContent = message;
      notify(message);
    } catch (error) { notify(error.message, true); }
  });

  document.addEventListener('click', async (event) => {
    if (event.target?.id === 'releaseIdleButton') {
      try {
        const result = await request('/v1/leases:release-idle', { method: 'POST', body: '{}' });
        notify(`已释放 ${result.released} 个空闲租约。`);
        await refresh();
      } catch (error) { notify(error.message, true); }
    }
    if (event.target?.id === 'createLeaseButton') {
      const id = window.prompt('请输入租约 ID');
      if (!id) return;
      try {
        await request('/v1/leases', { method: 'POST', body: JSON.stringify({ id: id.trim(), persistent: false }) });
        await refresh();
      } catch (error) { notify(error.message, true); }
    }
  });

  refresh();
})();

