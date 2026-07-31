/* LLM Gateway dashboard — vanilla JS, hash-routed SPA. */

const api = {
  async req(method, url, body) {
    const opts = { method, headers: {} };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = typeof body === 'string' ? body : JSON.stringify(body);
    }
    const res = await fetch('/dashboard/api' + url, opts);
    if (res.status === 401) { location.hash = '#overview'; location.reload(); throw new Error('unauthorized'); }
    const ct = res.headers.get('content-type') || '';
    let data = null;
    if (ct.includes('application/json')) data = await res.json();
    else data = await res.text();
    if (!res.ok) throw new Error((data && data.error) || (res.status + ' ' + res.statusText));
    return data;
  },
  get: (u) => api.req('GET', u),
  post: (u, b) => api.req('POST', u, b),
  put: (u, b) => api.req('PUT', u, b),
  del: (u) => api.req('DELETE', u),
};

const state = { providers: [], combos: [], settings: {}, currentRoute: 'overview', logRefresh: null, healthRefresh: null };

const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => [...el.querySelectorAll(sel)];
const esc = (s) => String(s ?? '').replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
const fmtTime = (ts) => new Date(ts * 1000).toLocaleString();
const mask = (s) => (s && s.length > 8 ? s.slice(0, 4) + '••••••' + s.slice(-4) : '••••••');

function clearTimers() { if (state.logRefresh) clearInterval(state.logRefresh); if (state.healthRefresh) clearInterval(state.healthRefresh); state.logRefresh = state.healthRefresh = null; }

const routes = {
  overview: renderOverview,
  providers: renderProviders,
  combos: renderCombos,
  logs: renderLogs,
  settings: renderSettings,
  export: renderExport,
};

function router() {
  clearTimers();
  let h = (location.hash || '#overview').slice(1).split('?')[0];
  if (!routes[h]) h = 'overview';
  state.currentRoute = h;
  $$('#nav a').forEach(a => a.classList.toggle('active', a.dataset.route === h));
  routes[h]();
}
window.addEventListener('hashchange', router);

$('#logoutBtn').addEventListener('click', async () => {
  try { await api.post('/logout', {}); } catch (_) {}
  location.href = '/dashboard/login';
});

// --- API Endpoint popover ---
$('#endpointBtn').addEventListener('click', showEndpointPanel);

async function copyText(value, btn) {
  const orig = btn ? btn.textContent : null;
  const ok = await (async () => {
    try { await navigator.clipboard.writeText(value); return true; }
    catch (_) {
      const ta = document.createElement('textarea');
      ta.value = value; document.body.appendChild(ta); ta.select();
      const done = document.execCommand && document.execCommand('copy');
      document.body.removeChild(ta);
      return !!done;
    }
  })();
  if (btn) { btn.textContent = ok ? '✓ Copied' : 'Copy failed'; setTimeout(() => { btn.textContent = orig; }, 1200); }
}

async function showEndpointPanel() {
  const existing = $('#endpointPanel');
  if (existing) { existing.remove(); return; }

  const btn = $('#endpointBtn');
  const panel = document.createElement('div');
  panel.id = 'endpointPanel';
  panel.className = 'endpoint-panel';
  panel.innerHTML = '<div class="loading">Loading…</div>';
  document.body.appendChild(panel);
  const rect = btn.getBoundingClientRect();
  panel.style.top = (rect.bottom + window.scrollY + 8) + 'px';
  panel.style.right = (document.documentElement.clientWidth - rect.right - window.scrollX) + 'px';

  let info = null;
  try { info = await api.get('/endpoint'); } catch (e) { panel.innerHTML = '<div class="err">' + esc(e.message) + '</div>'; return; }

  panel.innerHTML = `
    <div class="ep-row">
      <div class="ep-label">Base URL</div>
      <div class="ep-value"><code id="epBase">${esc(info.base_url)}</code>
        <button class="btn sm" id="copyBase">Copy</button></div>
    </div>
    <div class="ep-row">
      <div class="ep-label">API Key</div>
      <div class="ep-value"><code id="epKey">${esc(info.api_key)}</code>
        <button class="btn sm" id="copyKey">Copy</button></div>
    </div>
    <div class="ep-hint">Use as <code>OPENAI_BASE_URL</code> / <code>OPENAI_API_KEY</code> in Cursor, OpenCode, etc.</div>
  `;
  $('#copyBase').addEventListener('click', (e) => copyText(info.base_url, e.currentTarget));
  $('#copyKey').addEventListener('click', (e) => copyText(info.api_key, e.currentTarget));

  const close = (ev) => {
    if (!panel.contains(ev.target) && ev.target !== btn) {
      panel.remove(); document.removeEventListener('click', close, true);
    }
  };
  setTimeout(() => document.addEventListener('click', close, true), 0);
}

// ---------- Overview ----------
async function renderOverview() {
  const app = $('#app');
  app.innerHTML = '<div class="loading">Loading overview…</div>';
  try {
    const [ov, health, logs] = await Promise.all([
      api.get('/overview'), api.get('/health'), api.get('/logs?limit=10'),
    ]);
    app.innerHTML = `
      <h1>Overview</h1>
      <div class="sub">Gateway status at a glance.</div>
      <div class="grid cols-4">
        ${stat('Providers', ov.providers_enabled + ' / ' + ov.providers_total)}
        ${stat('Combos', ov.combos_enabled + ' / ' + ov.combos_total)}
        ${stat('Requests today', ov.requests_today)}
        ${stat('Healthy providers', health.filter(h => h.available && h.enabled).length + ' / ' + health.filter(h => h.enabled).length)}
      </div>
      <div class="grid cols-2" style="margin-top:18px">
        <div class="card"><h2>Provider health</h2>${healthTable(health)}</div>
        <div class="card"><h2>Recent requests</h2>${recentLogs(logs.items)}</div>
      </div>`;
    state.healthRefresh = setInterval(async () => {
      try { $('.card h2').parentElement.querySelector('table').outerHTML = healthTable(await api.get('/health')); } catch (_) {}
    }, 5000);
  } catch (e) { app.innerHTML = errBox(e); }
}

function stat(label, value) {
  return `<div class="stat"><div class="num">${esc(value)}</div><div class="lbl">${esc(label)}</div></div>`;
}

function healthTable(health) {
  if (!health.length) return '<div class="empty">No providers yet.</div>';
  return `<table><thead><tr><th>Provider</th><th>Status</th><th>Fails</th><th>Cooldown</th></tr></thead><tbody>` +
    health.map(h => {
      let status, cls;
      if (!h.enabled) { status = 'Disabled'; cls = 'off'; }
      else if (h.available) { status = 'Healthy'; cls = 'ok'; }
      else { status = 'Cooldown'; cls = 'cool'; }
      const remain = h.available || h.cooldown_remaining_ms <= 0 ? '—' : Math.ceil(h.cooldown_remaining_ms / 1000) + 's';
      return `<tr><td class="mono">${esc(h.display || h.provider_id)}</td><td><span class="dot ${cls}"></span>${status}</td><td>${h.failures}</td><td>${remain}</td></tr>`;
    }).join('') + `</tbody></table>`;
}

function recentLogs(items) {
  if (!items || !items.length) return '<div class="empty">No requests yet.</div>';
  return `<table><thead><tr><th>Time</th><th>Model → Provider</th><th>Status</th><th>Latency</th></tr></thead><tbody>` +
    items.map(l => `<tr><td class="small muted">${new Date(l.ts * 1000).toLocaleTimeString()}</td>
      <td class="mono small">${esc(l.model_in)} → ${esc(l.provider_used)}</td>
      <td><span class="badge ${l.status < 400 ? 'ok' : 'err'}">${l.status}</span></td>
      <td class="small">${l.latency_ms}ms</td></tr>`).join('') + `</tbody></table>`;
}

// ---------- Providers ----------
async function renderProviders() {
  const app = $('#app');
  app.innerHTML = '<div class="loading">Loading providers…</div>';
  try {
    state.providers = await api.get('/providers');
    app.innerHTML = `
      <h1>Providers</h1>
      <div class="sub">Upstream LLM endpoints the gateway can route to.</div>
      <div class="toolbar">
        <div class="grow"></div>
        <button class="btn" onclick="showProviderForm()">+ Add provider</button>
      </div>
      <div id="providerForm"></div>
      <div class="card"><div id="providerTable">${providerTable()}</div></div>`;
  } catch (e) { app.innerHTML = errBox(e); }
}

function providerTable() {
  const ps = state.providers;
  if (!ps.length) return '<div class="empty">No providers configured.</div>';
  return `<table><thead><tr><th>ID</th><th>Name</th><th>Model</th><th>Base URL</th><th>Weight</th><th>Tags</th><th>Responses</th><th>Enabled</th><th></th></tr></thead><tbody>` +
    ps.map(p => `<tr>
      <td class="mono">${esc(p.id)}</td>
      <td>${esc(p.display)}</td>
      <td class="mono small">${esc(p.model)}</td>
      <td class="mono small muted" title="${esc(p.base_url)}">${esc(trunc(p.base_url, 32))}</td>
      <td>${p.weight}</td>
      <td>${(p.tags || []).map(t => `<span class="tag">${esc(t)}</span>`).join('')}</td>
      <td>${p.responses_native ? '<span class="pill-ok">native</span>' : '<span class="muted">translated</span>'}</td>
      <td><button class="btn sm ${p.enabled ? '' : 'ghost'}" onclick="toggleProvider('${esc(p.id)}')">${p.enabled ? 'On' : 'Off'}</button></td>
      <td style="white-space:nowrap;text-align:right">
        <button class="btn sm ghost" onclick="testProvider('${esc(p.id)}')">Test</button>
        <button class="btn sm ghost" onclick="showProviderForm('${esc(p.id)}')">Edit</button>
        <button class="btn sm danger" onclick="deleteProvider('${esc(p.id)}')">Del</button>
      </td></tr>`).join('') + `</tbody></table>`;
}

function trunc(s, n) { return s && s.length > n ? s.slice(0, n - 1) + '…' : s; }

async function toggleProvider(id) {
  const p = state.providers.find(x => x.id === id);
  if (!p) return;
  await api.put('/providers/' + encodeURIComponent(id), { ...p, enabled: !p.enabled });
  $('#providerTable').innerHTML = providerTable();
}

async function deleteProvider(id) {
  if (!confirm('Delete provider "' + id + '"? Combos referencing it will drop that member.')) return;
  await api.del('/providers/' + encodeURIComponent(id));
  state.providers = state.providers.filter(x => x.id !== id);
  $('#providerTable').innerHTML = providerTable();
  cancelProviderForm();
}

async function testProvider(id) {
  const btn = event.target; btn.disabled = true; btn.textContent = '…';
  try {
    const res = await api.post('/providers/' + encodeURIComponent(id) + '/test');
    alert(res.error ? 'Test failed (' + res.status + '): ' + res.error : 'OK — status ' + res.status + ' in ' + res.latency_ms + 'ms');
  } catch (e) { alert('Test error: ' + e.message); }
  finally { btn.disabled = false; btn.textContent = 'Test'; }
}

function showProviderForm(id) {
  const p = id ? state.providers.find(x => x.id === id) : { id: '', display: '', base_url: '', auth_key: '', model: '', weight: 1, tags: [], enabled: true, responses_native: false };
  $('#providerForm').innerHTML = `
    <div class="card"><h2>${id ? 'Edit' : 'Add'} provider</h2>
    <form onsubmit="return saveProvider(event, '${esc(id || '')}')">
      <div class="row">
        <div><label>ID *</label><input name="id" value="${esc(p.id)}" ${id ? 'readonly' : 'required'}></div>
        <div><label>Display name</label><input name="display" value="${esc(p.display)}"></div>
        <div><label>Weight</label><input name="weight" type="number" min="1" value="${p.weight}"></div>
      </div>
      <div class="row">
        <div style="flex:2"><label>Base URL *</label><input name="base_url" value="${esc(p.base_url)}" placeholder="https://api.groq.com/openai/v1" required></div>
        <div style="flex:2"><label>Upstream model *</label><input name="model" value="${esc(p.model)}" placeholder="llama3-70b-8192" required></div>
      </div>
      <div class="row">
        <div><label>Auth key</label><input name="auth_key" type="password" value="${esc(p.auth_key)}" autocomplete="new-password"></div>
        <div><label>Tags (comma)</label><input name="tags" value="${esc((p.tags || []).join(','))}"></div>
      </div>
      <div class="row">
        <label class="pill"><input type="checkbox" name="enabled" ${p.enabled ? 'checked' : ''} style="width:auto"> Enabled</label>
        <label class="pill"><input type="checkbox" name="responses_native" ${p.responses_native ? 'checked' : ''} style="width:auto"> Supports /v1/responses natively</label>
      </div>
      <div class="form-actions"><button class="btn" type="submit">Save</button><button class="btn ghost" type="button" onclick="cancelProviderForm()">Cancel</button></div>
    </form></div>`;
  $('#providerForm').scrollIntoView({ behavior: 'smooth' });
}
function cancelProviderForm() { $('#providerForm').innerHTML = ''; }

async function saveProvider(e, id) {
  e.preventDefault();
  const f = new FormData(e.target);
  const payload = {
    id: f.get('id'), display: f.get('display'), base_url: f.get('base_url'),
    auth_key: f.get('auth_key'), model: f.get('model'), weight: parseInt(f.get('weight')) || 1,
    tags: (f.get('tags') || '').split(',').map(s => s.trim()).filter(Boolean),
    enabled: f.get('enabled') === 'on', responses_native: f.get('responses_native') === 'on',
  };
  try {
    if (id) await api.put('/providers/' + encodeURIComponent(id), payload);
    else await api.post('/providers', payload);
    state.providers = await api.get('/providers');
    cancelProviderForm();
    $('#providerTable').innerHTML = providerTable();
  } catch (err) { alert('Save failed: ' + err.message); }
  return false;
}

// ---------- Combos ----------
async function renderCombos() {
  const app = $('#app');
  app.innerHTML = '<div class="loading">Loading combos…</div>';
  try {
    [state.combos, state.providers] = await Promise.all([api.get('/combos'), api.get('/providers')]);
    app.innerHTML = `
      <h1>Combos</h1>
      <div class="sub">Virtual models that fan out across providers with rotation + fallback.</div>
      <div class="toolbar"><div class="grow"></div><button class="btn" onclick="showComboForm()">+ Add combo</button></div>
      <div id="comboForm"></div>
      <div class="card"><div id="comboTable">${comboTable()}</div></div>`;
  } catch (e) { app.innerHTML = errBox(e); }
}

function comboTable() {
  const cs = state.combos;
  if (!cs.length) return '<div class="empty">No combos configured.</div>';
  return `<table><thead><tr><th>ID</th><th>Name</th><th>Rotation</th><th>Members</th><th>Enabled</th><th></th></tr></thead><tbody>` +
    cs.map(c => `<tr>
      <td class="mono">${esc(c.id)}</td>
      <td>${esc(c.display_name)}</td>
      <td><span class="tag">${esc(c.rotation)}</span></td>
      <td class="small">${(c.members || []).map(m => `<span class="chip">${esc(m)}</span>`).join('')}</td>
      <td><button class="btn sm ${c.enabled ? '' : 'ghost'}" onclick="toggleCombo('${esc(c.id)}')">${c.enabled ? 'On' : 'Off'}</button></td>
      <td style="white-space:nowrap;text-align:right">
        <button class="btn sm ghost" onclick="testCombo('${esc(c.id)}')">Test</button>
        <button class="btn sm ghost" onclick="showComboForm('${esc(c.id)}')">Edit</button>
        <button class="btn sm danger" onclick="deleteCombo('${esc(c.id)}')">Del</button>
      </td></tr>`).join('') + `</tbody></table>`;
}

async function toggleCombo(id) {
  const c = state.combos.find(x => x.id === id);
  await api.put('/combos/' + encodeURIComponent(id), { ...c, enabled: !c.enabled });
  state.combos = await api.get('/combos');
  $('#comboTable').innerHTML = comboTable();
}

async function deleteCombo(id) {
  if (!confirm('Delete combo "' + id + '"?')) return;
  await api.del('/combos/' + encodeURIComponent(id));
  state.combos = state.combos.filter(x => x.id !== id);
  $('#comboTable').innerHTML = comboTable();
  cancelComboForm();
}

async function testCombo(id) {
  const btn = event.target; btn.disabled = true; btn.textContent = '…';
  try {
    const res = await api.post('/combos/' + encodeURIComponent(id) + '/test');
    alert(res.error ? 'Test failed (' + res.status + '): ' + res.error : 'OK — status ' + res.status + ' in ' + res.latency_ms + 'ms');
  } catch (e) { alert('Test error: ' + e.message); }
  finally { btn.disabled = false; btn.textContent = 'Test'; }
}

function showComboForm(id) {
  const c = id ? state.combos.find(x => x.id === id) : { id: '', display_name: '', rotation: 'round-robin', members: [], enabled: true };
  const avail = state.providers.map(p => p.id);
  $('#comboForm').innerHTML = `
    <div class="card"><h2>${id ? 'Edit' : 'Add'} combo</h2>
    <form onsubmit="return saveCombo(event, '${esc(id || '')}')">
      <div class="row">
        <div><label>ID *</label><input name="id" value="${esc(c.id)}" ${id ? 'readonly' : 'required'}></div>
        <div><label>Display name</label><input name="display_name" value="${esc(c.display_name)}"></div>
        <div><label>Rotation</label>
          <select name="rotation">
            ${['round-robin', 'weighted-round-robin', 'priority', 'random'].map(r => `<option ${c.rotation === r ? 'selected' : ''}>${r}</option>`).join('')}
          </select></div>
      </div>
      <label>Members (drag to reorder — order matters for <span class="kbd">priority</span>)</label>
      <ul class="listMembers" id="memberList">
        ${(c.members || []).map(m => `<li draggable="true" data-id="${esc(m)}"><span class="handle">☰</span><span class="mono">${esc(m)}</span><span class="spacer"></span><button type="button" class="btn sm danger" onclick="this.closest('li').remove()">×</button></li>`).join('')}
      </ul>
      <div class="toolbar">
        <select id="memberPicker">${avail.filter(a => !(c.members || []).includes(a)).map(a => `<option>${esc(a)}</option>`).join('')}</select>
        <button type="button" class="btn sm ghost" onclick="addMember()">+ Add member</button>
      </div>
      <label class="pill"><input type="checkbox" name="enabled" ${c.enabled ? 'checked' : ''} style="width:auto"> Enabled</label>
      <div class="form-actions"><button class="btn" type="submit">Save</button><button class="btn ghost" type="button" onclick="cancelComboForm()">Cancel</button></div>
    </form></div>`;
  enableDrag();
}
function cancelComboForm() { $('#comboForm').innerHTML = ''; }

function addMember() {
  const sel = $('#memberPicker');
  if (!sel.value) return;
  const li = document.createElement('li');
  li.draggable = true; li.dataset.id = sel.value;
  li.innerHTML = `<span class="handle">☰</span><span class="mono">${esc(sel.value)}</span><span class="spacer"></span><button type="button" class="btn sm danger" onclick="this.closest('li').remove()">×</button>`;
  $('#memberList').appendChild(li);
  sel.querySelector('option[value="' + CSS.escape(sel.value) + '"]')?.remove();
  enableDrag();
}

function enableDrag() {
  const list = $('#memberList');
  if (!list) return;
  list.querySelectorAll('li').forEach(li => {
    li.ondragstart = (e) => { li.classList.add('dragging'); e.dataTransfer.effectAllowed = 'move'; };
    li.ondragend = () => li.classList.remove('dragging');
  });
  list.ondragover = (e) => {
    e.preventDefault();
    const dragging = $('.dragging', list);
    const after = [...list.querySelectorAll('li:not(.dragging)')].find(el => {
      const box = el.getBoundingClientRect();
      return e.clientY < box.top + box.height / 2;
    });
    if (after) list.insertBefore(dragging, after); else list.appendChild(dragging);
  };
}

async function saveCombo(e, id) {
  e.preventDefault();
  const f = new FormData(e.target);
  const members = $$('#memberList li').map(li => li.dataset.id);
  const payload = {
    id: f.get('id'), display_name: f.get('display_name'), rotation: f.get('rotation'),
    members, enabled: f.get('enabled') === 'on',
  };
  try {
    if (id) await api.put('/combos/' + encodeURIComponent(id), payload);
    else await api.post('/combos', payload);
    state.combos = await api.get('/combos');
    cancelComboForm();
    $('#comboTable').innerHTML = comboTable();
  } catch (err) { alert('Save failed: ' + err.message); }
  return false;
}

// ---------- Logs ----------
let logFilter = { limit: 50, offset: 0 };
async function renderLogs() {
  const app = $('#app');
  app.innerHTML = '<div class="loading">Loading logs…</div>';
  try {
    state.providers = await api.get('/providers');
    app.innerHTML = `
      <h1>Request Logs</h1>
      <div class="sub">Every proxied call, with filtering and hourly chart.</div>
      <div class="card">
        <div class="row">
          <div><label>Provider</label><select id="f_provider"><option value="">All</option>${state.providers.map(p => `<option>${esc(p.id)}</option>`).join('')}</select></div>
          <div><label>Endpoint</label><select id="f_endpoint"><option value="">All</option><option>chat.completions</option><option>completions</option><option>responses</option><option>embeddings</option></select></div>
          <div><label>Errors only</label><select id="f_errors"><option value="0">No</option><option value="1">Yes</option></select></div>
          <div style="align-self:end"><button class="btn sm" onclick="applyLogFilter()">Apply</button></div>
        </div>
      </div>
      <div class="card"><h2>Requests per hour (24h)</h2><canvas id="logChart" width="1000" height="220"></canvas></div>
      <div class="card"><div id="logTable"></div><div class="pagination" id="logPager"></div></div>`;
    await loadLogs();
    await drawChart();
  } catch (e) { app.innerHTML = errBox(e); }
}

async function applyLogFilter() { logFilter.offset = 0; await loadLogs(); }

async function loadLogs() {
  const params = new URLSearchParams({
    limit: logFilter.limit, offset: logFilter.offset,
    provider: $('#f_provider')?.value || '', endpoint: $('#f_endpoint')?.value || '',
    errors_only: $('#f_errors')?.value || '0',
  });
  const data = await api.get('/logs?' + params);
  renderLogTable(data);
}

function renderLogTable(data) {
  const items = data.items || [];
  $('#logTable').innerHTML = items.length === 0 ? '<div class="empty">No log entries match.</div>' :
    `<table><thead><tr><th>Time</th><th>Model in</th><th>Provider</th><th>Endpoint</th><th>Status</th><th>Tokens</th><th>Latency</th></tr></thead><tbody>` +
    items.map(l => `<tr>
      <td class="small muted">${fmtTime(l.ts)}</td>
      <td class="mono small">${esc(l.model_in)}</td>
      <td class="mono small">${esc(l.provider_used)}</td>
      <td class="small muted">${esc(l.endpoint)}</td>
      <td><span class="badge ${l.status < 400 ? 'ok' : 'err'}">${l.status}</span>${l.error ? ' <span class="small pill-bad" title="' + esc(l.error) + '">!</span>' : ''}</td>
      <td class="small">${l.prompt_tokens ?? '—'}/${l.completion_tokens ?? '—'}</td>
      <td class="small">${l.latency_ms}ms</td></tr>`).join('') + `</tbody></table>`;
  // Pagination.
  const pages = Math.ceil((data.total || 0) / data.limit);
  const cur = Math.floor(data.offset / data.limit);
  let pager = '';
  if (pages > 1) {
    if (cur > 0) pager += `<button class="btn sm ghost" onclick="gotoLogPage(${cur - 1})">‹ Prev</button>`;
    pager += `<span class="muted small">Page ${cur + 1} / ${pages}</span>`;
    if (cur < pages - 1) pager += `<button class="btn sm ghost" onclick="gotoLogPage(${cur + 1})">Next ›</button>`;
  }
  $('#logPager').innerHTML = pager;
}

async function gotoLogPage(p) { logFilter.offset = p * logFilter.limit; await loadLogs(); }

async function drawChart() {
  const data = await api.get('/logs/chart?hours=24');
  const canvas = $('#logChart');
  const ctx = canvas.getContext('2d');
  const W = canvas.width, H = canvas.height, pad = 30;
  ctx.clearRect(0, 0, W, H);

  // Bucket into 24 consecutive hours ending this hour.
  const nowHr = Math.floor(Date.now() / 1000 / 3600) * 3600;
  const buckets = Array.from({ length: 24 }, (_, i) => nowHr - (23 - i) * 3600);
  const byProvider = {};
  for (const row of data || []) {
    byProvider[row.provider] = byProvider[row.provider] || {};
    byProvider[row.provider][row.bucket] = row.count;
  }
  const providers = Object.keys(byProvider);
  const maxTotal = Math.max(1, ...buckets.map(b => providers.reduce((s, p) => s + (byProvider[p][b] || 0), 0)));

  const colors = ['#2563eb', '#16a34a', '#d97706', '#db2777', '#0891b2', '#7c3aed', '#dc2626', '#65a30d'];
  const barW = (W - pad * 2) / 24;

  // Axes.
  ctx.strokeStyle = getComputedStyle(document.body).getPropertyValue('--border');
  ctx.beginPath(); ctx.moveTo(pad, H - pad); ctx.lineTo(W - pad, H - pad); ctx.stroke();

  providers.forEach((p, pi) => {
    ctx.fillStyle = colors[pi % colors.length];
    buckets.forEach((b, bi) => {
      const v = byProvider[p][b] || 0;
      if (v === 0) return;
      // Stacked: offset by sum of previous providers.
      let below = 0;
      for (let j = 0; j < pi; j++) below += byProvider[providers[j]][b] || 0;
      const x = pad + bi * barW + 2;
      const h = (v / maxTotal) * (H - pad * 2);
      const y = H - pad - (below / maxTotal) * (H - pad * 2) - h;
      ctx.fillRect(x, y, barW - 4, h);
    });
  });

  // Legend.
  let lx = pad;
  ctx.font = '12px system-ui';
  providers.forEach((p, pi) => {
    ctx.fillStyle = colors[pi % colors.length];
    ctx.fillRect(lx, 6, 10, 10);
    ctx.fillStyle = getComputedStyle(document.body).getPropertyValue('--muted');
    ctx.fillText(p, lx + 14, 15);
    lx += 14 + ctx.measureText(p).width + 16;
  });
}

// ---------- Settings ----------
async function renderSettings() {
  const app = $('#app');
  app.innerHTML = '<div class="loading">Loading settings…</div>';
  try {
    state.settings = await api.get('/settings');
    const s = state.settings;
    app.innerHTML = `
      <h1>Settings</h1><div class="sub">Health, rotation, and retention knobs. Changes apply immediately.</div>
      <div class="grid cols-2">
        <div class="card"><h2>Health & rotation</h2>
          <form onsubmit="return saveSettings(event)">
            <label>Cooldown (seconds)</label><input name="health.cooldown" type="number" min="1" value="${esc(s['health.cooldown'] || '60')}">
            <label>Error codes triggering rotation (comma)</label><input name="health.error_codes" value="${esc(s['health.error_codes'] || '429,500,502,503,504')}">
            <div class="form-actions"><button class="btn" type="submit">Save</button></div>
          </form></div>
        <div class="card"><h2>Logging & retention</h2>
          <form onsubmit="return saveSettings(event)">
            <label>Keep logs (days)</label><input name="log.retention_days" type="number" min="1" value="${esc(s['log.retention_days'] || '30')}">
            <div class="form-actions"><button class="btn" type="submit">Save</button></div>
          </form></div>
      </div>
      <div class="card"><h2>Gateway API key</h2>
        <div class="mono">${esc(s['_gateway_api_key_masked'] || '')}</div>
        <div class="small muted" style="margin-top:6px">Set via <span class="kbd">GATEWAY_API_KEY</span> env var at startup; not editable here.</div>
      </div>`;
  } catch (e) { app.innerHTML = errBox(e); }
}

async function saveSettings(e) {
  e.preventDefault();
  const f = new FormData(e.target);
  const payload = {};
  for (const [k, v] of f.entries()) payload[k] = v;
  try { await api.put('/settings', payload); alert('Saved.'); }
  catch (err) { alert('Save failed: ' + err.message); }
  return false;
}

// ---------- Export / Import ----------
function renderExport() {
  $('#app').innerHTML = `
    <h1>Export / Import</h1><div class="sub">Backup or migrate providers, combos and settings as plain SQL.</div>
    <div class="grid cols-2">
      <div class="card"><h2>Export</h2>
        <p class="small muted">Downloads <span class="kbd">gateway-export-${new Date().toISOString().slice(0, 10)}.sql</span> with all providers, combos and settings.</p>
        <a class="btn" href="/dashboard/api/export" download>⬇ Download SQL export</a>
      </div>
      <div class="card"><h2>Import</h2>
        <div class="notice">⚠ Import replaces all providers, combos, and settings. This cannot be undone.</div>
        <input type="file" id="importFile" accept=".sql">
        <div class="form-actions"><button class="btn danger" onclick="doImport()">Import & replace</button></div>
        <div id="importResult" class="small" style="margin-top:8px"></div>
      </div>
    </div>`;
}

async function doImport() {
  const f = $('#importFile').files[0];
  if (!f) { $('#importResult').textContent = 'Choose a .sql file first.'; return; }
  if (!confirm('Import will REPLACE all providers, combos, and settings. Continue?')) return;
  const text = await f.text();
  try {
    const res = await api.req('POST', '/import', text);
    $('#importResult').innerHTML = '<span class="pill-ok">Imported successfully.</span>';
  } catch (e) {
    $('#importResult').innerHTML = '<span class="pill-bad">Import failed: ' + esc(e.message) + '</span>';
  }
}

function errBox(e) { return `<div class="card"><div class="pill-bad">Error: ${esc(e.message || e)}</div></div>`; }

router();
