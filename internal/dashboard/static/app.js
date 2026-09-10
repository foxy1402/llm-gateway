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

const state = { providers: [], combos: [], proxies: [], settings: {}, currentRoute: 'overview', logRefresh: null, healthRefresh: null, logAutoRefresh: true, renderSeq: 0 };

const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => [...el.querySelectorAll(sel)];
const esc = (s) => String(s ?? '').replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
const fmtTime = (ts) => new Date(ts * 1000).toLocaleString();
const mask = (s) => (s && s.length > 8 ? s.slice(0, 4) + '••••••' + s.slice(-4) : '••••••');
// Log stat formatters: thousands separators; duration in seconds past 1s; TPS is
// completion tokens over the whole request (includes time-to-first-token for streams).
const fmtNum = (n) => n == null ? '—' : Number(n).toLocaleString();
const fmtDuration = (ms) => ms == null ? '—' : (ms >= 1000 ? (ms / 1000).toFixed(1) + 's' : ms + 'ms');
const fmtTps = (ct, ms) => (ct != null && ms > 0) ? (ct * 1000 / ms).toFixed(1) : '—';

function clearTimers() {
  if (state.logRefresh) clearInterval(state.logRefresh);
  if (state.healthRefresh) clearInterval(state.healthRefresh);
  state.logRefresh = state.healthRefresh = null;
  // Drop any open log detail panel when leaving the logs page.
  if (typeof closeExpandedDetail === 'function') closeExpandedDetail();
}

const routes = {
  overview: renderOverview,
  providers: renderProviders,
  combos: renderCombos,
  proxies: renderProxies,
  logs: renderLogs,
  settings: renderSettings,
  export: renderExport,
};

function router() {
  clearTimers();
  // Monotonic render generation: async render bodies captured before an await
  // check this after each await and bail when the route changed underneath them,
  // so a slow fetch can't scribble over the page the user already navigated to.
  state.renderSeq++;
  let h = (location.hash || '#overview').slice(1).split('?')[0];
  if (!routes[h]) h = 'overview';
  state.currentRoute = h;
  $$('#nav a').forEach(a => a.classList.toggle('active', a.dataset.route === h));
  routes[h]();
}
window.addEventListener('hashchange', router);

$('#logoutBtn').addEventListener('click', async () => {
  // Redirect only when the server actually killed the session — otherwise the
  // user lands on the login page with a live cookie and loops straight back in.
  try { await api.post('/logout', {}); location.href = '/dashboard/login'; }
  catch (e) { alert('Logout failed: ' + e.message); }
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
  const seq = state.renderSeq;
  const app = $('#app');
  app.innerHTML = '<div class="loading">Loading overview…</div>';
  try {
    const [ov, health, logs] = await Promise.all([
      api.get('/overview'), api.get('/health'), api.get('/logs?limit=10'),
    ]);
    if (seq !== state.renderSeq) return;
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
        <div class="card" id="healthCard"><h2>Provider health</h2>${healthTable(health)}</div>
        <div class="card"><h2>Recent requests</h2>${recentLogs(logs.items)}</div>
      </div>`;
    if (state.healthRefresh) clearInterval(state.healthRefresh);
    state.healthRefresh = setInterval(async () => {
      const card = $('#healthCard');
      if (!card) return;
      try { card.innerHTML = '<h2>Provider health</h2>' + healthTable(await api.get('/health')); } catch (_) {}
    }, 5000);
  } catch (e) { if (seq === state.renderSeq) app.innerHTML = errBox(e); }
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
  const seq = state.renderSeq;
  const app = $('#app');
  app.innerHTML = '<div class="loading">Loading providers…</div>';
  try {
    state.providers = await api.get('/providers');
    if (seq !== state.renderSeq) return;
    app.innerHTML = `
      <h1>Providers</h1>
      <div class="sub">Upstream LLM endpoints the gateway can route to.</div>
      <div class="toolbar">
        <div class="grow"></div>
        <button class="btn" onclick="showProviderForm()">+ Add provider</button>
      </div>
      <div id="providerForm"></div>
      <div class="card"><div id="providerTable">${providerTable()}</div></div>`;
  } catch (e) { if (seq === state.renderSeq) app.innerHTML = errBox(e); }
}

// Inline onclick attributes interpolate IDs through JSON.stringify so a quote
// inside the ID can't break out of the JS string (esc() alone skips ').
const jsq = (s) => esc(JSON.stringify(String(s ?? '')));

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
      <td><button class="btn sm ${p.enabled ? '' : 'ghost'}" onclick="toggleProvider(${jsq(p.id)})">${p.enabled ? 'On' : 'Off'}</button></td>
      <td style="white-space:nowrap;text-align:right">
        <button class="btn sm ghost" onclick="testProvider(event, ${jsq(p.id)})">Test</button>
        <button class="btn sm ghost" onclick="showProviderForm(${jsq(p.id)})">Edit</button>
        <button class="btn sm danger" onclick="deleteProvider(${jsq(p.id)})">Del</button>
      </td></tr>`).join('') + `</tbody></table>`;
}

function trunc(s, n) { return s && s.length > n ? s.slice(0, n - 1) + '…' : s; }

async function toggleProvider(id) {
  const p = state.providers.find(x => x.id === id);
  if (!p) return;
  const next = !p.enabled;
  try {
    await api.put('/providers/' + encodeURIComponent(id), { ...p, enabled: next });
    // Success: mutate local state so the re-render reflects reality without a
    // full refetch round trip.
    p.enabled = next;
  } catch (e) { alert('Toggle failed: ' + e.message); }
  $('#providerTable').innerHTML = providerTable();
}

async function deleteProvider(id) {
  if (!confirm('Delete provider "' + id + '"? Combos referencing it will drop that member.')) return;
  // Without the catch a failed delete rejected silently: no alert, and the row
  // stayed on screen, so the user believed the delete had worked.
  try {
    await api.del('/providers/' + encodeURIComponent(id));
  } catch (e) { alert('Delete failed: ' + e.message); return; }
  state.providers = state.providers.filter(x => x.id !== id);
  $('#providerTable').innerHTML = providerTable();
  cancelProviderForm();
}

async function testProvider(ev, id) {
  const btn = ev && ev.target;
  const label = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '…'; }
  try {
    const res = await api.post('/providers/' + encodeURIComponent(id) + '/test');
    alert(testResultMessage(res));
  } catch (e) { alert('Test error: ' + e.message); }
  finally { if (btn) { btn.disabled = false; btn.textContent = label; } }
}

// testResultMessage renders the INNER result. The outer /test call always answers
// 200 with the real outcome in the body, so the inner failure is only visible
// here — and provider_used names which member actually served a combo test.
function testResultMessage(res) {
  const via = res.provider_used ? ' via ' + res.provider_used : '';
  if (res.error) return 'Test failed (' + res.status + ')' + via + ': ' + res.error;
  return 'OK — status ' + res.status + via + ' in ' + res.latency_ms + 'ms';
}

function showProviderForm(id) {
  const p = id ? state.providers.find(x => x.id === id) : { id: '', display: '', base_url: '', auth_key: '', model: '', weight: 1, tags: [], enabled: true, responses_native: false, accounts: [], token_param_mode: '', proxy_rotate: false };
  const accounts = (p.accounts && p.accounts.length ? p.accounts : [{ label: 'default', auth_key: p.auth_key || '', weight: 1, enabled: true }]);
  $('#providerForm').innerHTML = `
    <div class="card"><h2>${id ? 'Edit' : 'Add'} provider</h2>
    <form onsubmit="return saveProvider(event, ${jsq(id || '')})">
      <div class="row">
        <div><label>ID *</label><input name="id" value="${esc(p.id)}" ${id ? 'readonly' : 'required'}></div>
        <div><label>Display name</label><input name="display" value="${esc(p.display)}"></div>
        <div><label>Weight</label><input name="weight" type="number" min="1" value="${p.weight}"></div>
      </div>
      <div class="row">
        <div style="flex:2"><label>Base URL *</label><input name="base_url" value="${esc(p.base_url)}" placeholder="full root incl. version, e.g. https://api.groq.com/openai/v1" required></div>
        <div style="flex:2">
          <label>Upstream model *</label>
          <div style="display:flex;gap:6px">
            <input name="model" value="${esc(p.model)}" placeholder="llama3-70b-8192" required list="modelOptions" style="flex:1">
            <datalist id="modelOptions"></datalist>
            <button class="btn ghost sm" type="button" id="fetchModelsBtn" title="Fetch available model IDs from the upstream's /models">Fetch</button>
          </div>
          <div id="fetchModelsMsg" class="ep-hint" style="margin-top:4px;display:none"></div>
        </div>
        <div style="flex:1">
          <label title="Some models hard-reject whichever of max_tokens/max_completion_tokens the client didn't send. Auto detects and self-heals on the fly (and remembers the answer); pin it here only if you want to skip that detection round trip entirely.">max_tokens field</label>
          <select name="token_param_mode">
            <option value="" ${!p.token_param_mode ? 'selected' : ''}>Auto (recommended)</option>
            <option value="max_tokens" ${p.token_param_mode === 'max_tokens' ? 'selected' : ''}>Force max_tokens</option>
            <option value="max_completion_tokens" ${p.token_param_mode === 'max_completion_tokens' ? 'selected' : ''}>Force max_completion_tokens</option>
          </select>
        </div>
      </div>
      <div>
        <label>Accounts (API keys) — rotation spreads requests across them; one burned key keeps the pool alive.
        Set a model on an account to pin that key to one upstream model (e.g. key1 → kimi, key2 → qwen on the same gateway); empty = provider/member model.</label>
        <div id="accountList"></div>
        <datalist id="acctModelPool"></datalist>
        <button type="button" class="btn sm ghost" id="addAccountBtn">+ Add account</button>
      </div>
      <div class="row" style="margin-top:6px">
        <div><label>Tags (comma)</label><input name="tags" value="${esc((p.tags || []).join(','))}"></div>
      </div>
      <div class="row">
        <label class="pill"><input type="checkbox" name="enabled" ${p.enabled ? 'checked' : ''} style="width:auto"> Enabled</label>
        <label class="pill"><input type="checkbox" name="responses_native" ${p.responses_native ? 'checked' : ''} style="width:auto"> Supports /v1/responses natively</label>
        <label class="pill" title="Route this provider's upstream calls through the proxy pool, advancing one proxy per attempt. Off by default — only needed when the upstream rate-limits per source IP rather than per key."><input type="checkbox" name="proxy_rotate" ${p.proxy_rotate ? 'checked' : ''} style="width:auto"> Rotate egress proxy</label>
      </div>
      <div class="form-actions"><button class="btn" type="submit">Save</button><button class="btn ghost" type="button" onclick="cancelProviderForm()">Cancel</button></div>
    </form></div>`;
  $('#fetchModelsBtn').addEventListener('click', fetchUpstreamModels);
  fillAcctModelPool(p.models || []);
  accounts.forEach(a => addAccountRow(a));
  $('#addAccountBtn').addEventListener('click', () => addAccountRow({ label: '', auth_key: '', model: '', weight: 1, enabled: true }));
  $('#providerForm').scrollIntoView({ behavior: 'smooth' });
}

// fillAcctModelPool populates the shared per-account model datalist — the same
// fetched pool that feeds the provider's own model <datalist>.
function fillAcctModelPool(models) {
  const dl = $('#acctModelPool');
  if (dl) dl.innerHTML = (models || []).map(m => `<option value="${esc(m)}"></option>`).join('');
}

// addAccountRow appends one editable account row (label + key + model + weight + remove).
function addAccountRow(a) {
  const wrap = $('#accountList');
  const row = document.createElement('div');
  row.className = 'acct-row';
  row.dataset.id = a.id || ''; // keep the ID so saves preserve keys (and combo pins)
  row.dataset.enabled = a.enabled === false ? 'false' : 'true';
  row.style.cssText = 'display:flex;gap:6px;align-items:center;margin:4px 0';
  row.innerHTML = `
    <input name="acct_label" value="${esc(a.label || '')}" placeholder="label" style="width:110px">
    <input name="acct_key" type="password" value="${esc(a.auth_key || '')}" placeholder="API key *" autocomplete="new-password" style="flex:1" required>
    <input name="acct_model" list="acctModelPool" value="${esc(a.model || '')}" placeholder="model (optional pin)" style="width:200px" title="Pin this key to one upstream model; empty = provider/member default">
    <select name="acct_token_param_mode" style="width:150px" title="Override the provider's max_tokens field setting for this key only; empty = inherit">
      <option value="" ${!a.token_param_mode ? 'selected' : ''}>max_tokens: inherit</option>
      <option value="max_tokens" ${a.token_param_mode === 'max_tokens' ? 'selected' : ''}>force max_tokens</option>
      <option value="max_completion_tokens" ${a.token_param_mode === 'max_completion_tokens' ? 'selected' : ''}>force max_completion_tokens</option>
    </select>
    <input name="acct_weight" type="number" min="1" value="${a.weight || 1}" title="weight" style="width:64px">
    <button type="button" class="btn sm danger" onclick="this.closest('.acct-row').remove()">×</button>`;
  wrap.appendChild(row);
}

// collectAccounts reads the account rows from the provider form into a payload
// list. Existing rows carry their id so the backend keeps stable IDs across
// saves — regenerating them would detach combo key pins.
function collectAccounts(form) {
  const out = [];
  form.querySelectorAll('.acct-row').forEach((row, i) => {
    const key = row.querySelector('[name=acct_key]').value.trim();
    if (!key) return;
    const acct = {
      label: row.querySelector('[name=acct_label]').value.trim() || 'acct-' + (i + 1),
      auth_key: key,
      model: row.querySelector('[name=acct_model]').value.trim() || '',
      token_param_mode: row.querySelector('[name=acct_token_param_mode]').value || '',
      weight: parseInt(row.querySelector('[name=acct_weight]').value) || 1,
      // Preserve the account's saved enabled state — hardcoding true silently
      // re-enables keys the user deliberately disabled.
      enabled: row.dataset.enabled !== 'false',
    };
    if (row.dataset.id) acct.id = row.dataset.id;
    out.push(acct);
  });
  return out;
}

// fetchUpstreamModels calls the dashboard API to list the provider's available
// model IDs and fills a <datalist> attached to the model input, turning it into
// a searchable dropdown while still allowing a free-text value.
async function fetchUpstreamModels() {
  const form = $('#providerForm form');
  const baseUrl = form.querySelector('[name=base_url]').value.trim();
  // Use the first entered account key as the fetch credential.
  const firstKey = form.querySelector('[name=acct_key]');
  const authKey = (firstKey && firstKey.value.trim()) || '';
  const btn = $('#fetchModelsBtn');
  const msg = $('#fetchModelsMsg');
  const list = $('#modelOptions');
  msg.style.display = 'none';
  list.innerHTML = '';
  if (!baseUrl) {
    msg.style.display = 'block';
    msg.textContent = 'Enter the Base URL first.';
    return;
  }
  btn.disabled = true;
  const orig = btn.textContent;
  btn.textContent = 'Fetching…';
  try {
    // Route through the shared api helper: correct /dashboard/api prefix,
    // 401 handling, and consistent error extraction.
    const data = await api.post('/models/list', { base_url: baseUrl, auth_key: authKey });
    if (!data.models || data.models.length === 0) {
      msg.style.display = 'block';
      msg.textContent = 'Upstream returned 0 models.';
    } else {
      list.innerHTML = data.models.map(m => `<option value="${esc(m)}"></option>`).join('');
      fillAcctModelPool(data.models);
      msg.style.display = 'block';
      msg.textContent = `Loaded ${data.models.length} models — click the field and pick one.`;
      const input = form.querySelector('[name=model]');
      input.focus();
      // Persist the fetched pool onto the saved provider so combo dropdowns can use it.
      const pid = form.querySelector('[name=id]').value.trim();
      if (pid && state.providers.some(x => x.id === pid)) {
        api.post('/providers/' + encodeURIComponent(pid) + '/models/fetch').then(() => {
          state.providers.find(x => x.id === pid).models = data.models;
        }).catch(() => {});
      }
    }
  } catch (e) {
    msg.style.display = 'block';
    msg.textContent = 'Fetch failed: ' + e.message;
  } finally {
    btn.disabled = false;
    btn.textContent = orig;
  }
}
function cancelProviderForm() { $('#providerForm').innerHTML = ''; }

async function saveProvider(e, id) {
  e.preventDefault();
  const accounts = collectAccounts(e.target);
  if (accounts.length === 0) { alert('Add at least one account with an API key.'); return false; }
  const f = new FormData(e.target);
  const payload = {
    id: f.get('id'), display: f.get('display'), base_url: f.get('base_url'),
    auth_key: accounts[0].auth_key, model: f.get('model'), weight: parseInt(f.get('weight')) || 1,
    tags: (f.get('tags') || '').split(',').map(s => s.trim()).filter(Boolean),
    enabled: f.get('enabled') === 'on', responses_native: f.get('responses_native') === 'on',
    token_param_mode: f.get('token_param_mode') || '',
    proxy_rotate: f.get('proxy_rotate') === 'on',
    accounts: accounts,
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
  const seq = state.renderSeq;
  const app = $('#app');
  app.innerHTML = '<div class="loading">Loading combos…</div>';
  try {
    [state.combos, state.providers] = await Promise.all([api.get('/combos'), api.get('/providers')]);
    if (seq !== state.renderSeq) return;
    app.innerHTML = `
      <h1>Combos</h1>
      <div class="sub">Virtual models that fan out across providers with rotation + fallback.</div>
      <div class="toolbar"><div class="grow"></div><button class="btn" onclick="showComboForm()">+ Add combo</button></div>
      <div id="comboForm"></div>
      <div class="card"><div id="comboTable">${comboTable()}</div></div>`;
  } catch (e) { if (seq === state.renderSeq) app.innerHTML = errBox(e); }
}

function comboTable() {
  const cs = state.combos;
  if (!cs.length) return '<div class="empty">No combos configured.</div>';
  return `<table><thead><tr><th>ID</th><th>Name</th><th>Rotation</th><th>Members</th><th>Enabled</th><th></th></tr></thead><tbody>` +
    cs.map(c => `<tr>
      <td class="mono">${esc(c.id)}</td>
      <td>${esc(c.display_name)}</td>
      <td><span class="tag">${esc(c.rotation)}</span></td>
      <td class="small">${(c.members || []).map(m => `<span class="chip">${esc(memberLabel(m))}</span>`).join('')}</td>
      <td><button class="btn sm ${c.enabled ? '' : 'ghost'}" onclick="toggleCombo(${jsq(c.id)})">${c.enabled ? 'On' : 'Off'}</button></td>
      <td style="white-space:nowrap;text-align:right">
        <button class="btn sm ghost" onclick="testCombo(event, ${jsq(c.id)})">Test</button>
        <button class="btn sm ghost" onclick="showComboForm(${jsq(c.id)})">Edit</button>
        <button class="btn sm danger" onclick="deleteCombo(${jsq(c.id)})">Del</button>
      </td></tr>`).join('') + `</tbody></table>`;
}

async function toggleCombo(id) {
  const c = state.combos.find(x => x.id === id);
  if (!c) return;
  const next = { ...c, enabled: !c.enabled, proxy_rotate: !!c.proxy_rotate };
  try {
    await api.put('/combos/' + encodeURIComponent(id), next);
    state.combos = await api.get('/combos');
  } catch (e) { alert('Toggle failed: ' + e.message); }
  $('#comboTable').innerHTML = comboTable();
}

async function deleteCombo(id) {
  if (!confirm('Delete combo "' + id + '"?')) return;
  try {
    await api.del('/combos/' + encodeURIComponent(id));
  } catch (e) { alert('Delete failed: ' + e.message); return; }
  state.combos = state.combos.filter(x => x.id !== id);
  $('#comboTable').innerHTML = comboTable();
  cancelComboForm();
}

async function testCombo(ev, id) {
  const btn = ev && ev.target;
  const label = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '…'; }
  try {
    const res = await api.post('/combos/' + encodeURIComponent(id) + '/test');
    alert(testResultMessage(res));
  } catch (e) { alert('Test error: ' + e.message); }
  finally { if (btn) { btn.disabled = false; btn.textContent = label; } }
}

// memberLabel renders a combo member as "provider[key] → model", tolerating both
// the structured {provider_id,account_id,model} shape and a legacy plain-string ID.
// Returns RAW text — the caller escapes once at interpolation time.
function memberLabel(m) {
  if (m && typeof m === 'object') {
    const acct = m.account_id ? '[' + accountShortLabel(m.provider_id, m.account_id) + ']' : '';
    const tp = m.token_param_mode ? ' (' + m.token_param_mode + ')' : '';
    return m.provider_id + acct + (m.model ? ' → ' + m.model : '') + tp;
  }
  return String(m);
}

// accountShortLabel turns an account ID ("vercel:abc123") into a readable tag
// ("abc123") using the provider's account list for a label when one exists.
function accountShortLabel(providerID, accountID) {
  const prov = state.providers.find(p => p.id === providerID);
  const acct = prov && (prov.accounts || []).find(a => a.id === accountID);
  if (acct && acct.label) return acct.label;
  const i = accountID.indexOf(':');
  return i >= 0 ? accountID.slice(i + 1) : accountID;
}

function showComboForm(id) {
  const c = id ? state.combos.find(x => x.id === id) : { id: '', display_name: '', rotation: 'round-robin', members: [], enabled: true, proxy_rotate: false };
  const avail = state.providers;
  $('#comboForm').innerHTML = `
    <div class="card"><h2>${id ? 'Edit' : 'Add'} combo</h2>
    <form onsubmit="return saveCombo(event, ${jsq(id || '')})">
      <div class="row">
        <div><label>ID *</label><input name="id" value="${esc(c.id)}" ${id ? 'readonly' : 'required'}></div>
        <div><label>Display name</label><input name="display_name" value="${esc(c.display_name)}"></div>
        <div><label>Rotation</label>
          <select name="rotation">
            ${['round-robin', 'weighted-round-robin', 'priority', 'random'].map(r => `<option ${c.rotation === r ? 'selected' : ''}>${r}</option>`).join('')}
          </select></div>
      </div>
      <label>Members (provider + key + model; drag to reorder — order matters for <span class="kbd">priority</span>)</label>
      <ul class="listMembers" id="memberList"></ul>
      <div class="toolbar">
        <select id="memberPicker">${avail.map(a => `<option>${esc(a.id)}</option>`).join('')}</select>
        <button type="button" class="btn sm ghost" onclick="addMember()">+ Add member</button>
      </div>
      <div class="row">
        <label class="pill"><input type="checkbox" name="enabled" ${c.enabled ? 'checked' : ''} style="width:auto"> Enabled</label>
        <label class="pill" title="Route every attempt this combo makes through the proxy pool, whatever the member provider's own setting is. Off by default — only needed when the upstream rate-limits per source IP rather than per key."><input type="checkbox" name="proxy_rotate" ${c.proxy_rotate ? 'checked' : ''} style="width:auto"> Rotate egress proxy</label>
      </div>
      <div class="form-actions"><button class="btn" type="submit">Save</button><button class="btn ghost" type="button" onclick="cancelComboForm()">Cancel</button></div>
    </form></div>`;
  // Render each existing member as a provider+account+model row.
  (c.members || []).forEach(m => addMemberRow(normProvider(m), normAccount(m), m.model || '', (m && m.token_param_mode) || ''));
  enableDrag();
}
function cancelComboForm() { $('#comboForm').innerHTML = ''; }

function normProvider(m) { return (m && typeof m === 'object') ? m.provider_id : String(m); }
function normAccount(m) { return (m && typeof m === 'object' && m.account_id) ? m.account_id : ''; }

// addMemberRow appends a draggable member row: provider name, a key dropdown fed
// by the provider's account pool (or "any key" = keep rotating), and a model
// select fed by the provider's fetched model pool (free text still allowed).
function addMemberRow(providerID, accountID, model, tokenParamMode) {
  const prov = state.providers.find(p => p.id === providerID);
  const li = document.createElement('li');
  li.draggable = true;
  li.dataset.provider = providerID;
  const dl = 'dl-' + Math.random().toString(36).slice(2, 8);
  const acctOpts = ['<option value="">⚡ any key (rotate)</option>']
    .concat((prov && prov.accounts ? prov.accounts : [])
      .map(a => `<option value="${esc(a.id)}" ${a.id === accountID ? 'selected' : ''}>${esc(a.label || a.id)}</option>`));
  li.innerHTML = `
    <span class="handle">☰</span>
    <select class="provs"><option value="${esc(providerID)}">${esc(providerID)}</option></select>
    <select class="acctSel" title="Key pinned to this member">${acctOpts.join('')}</select>
    <input class="modelSel" list="${dl}" placeholder="model (default: key/provider)" value="${esc(model || '')}">
    <datalist id="${dl}"></datalist>
    <select class="tokenParamSel" title="Override max_tokens field for this member only; empty = inherit account/provider setting">
      <option value="" ${!tokenParamMode ? 'selected' : ''}>max_tokens: inherit</option>
      <option value="max_tokens" ${tokenParamMode === 'max_tokens' ? 'selected' : ''}>force max_tokens</option>
      <option value="max_completion_tokens" ${tokenParamMode === 'max_completion_tokens' ? 'selected' : ''}>force max_completion_tokens</option>
    </select>
    <span class="spacer"></span>
    <button type="button" class="btn sm danger" onclick="this.closest('li').remove()">×</button>`;
  const dlEl = li.querySelector('datalist');
  (prov && prov.models ? prov.models : []).forEach(m => {
    const o = document.createElement('option');
    o.value = m;
    dlEl.appendChild(o);
  });
  $('#memberList').appendChild(li);
}

function addMember() {
  const sel = $('#memberPicker');
  if (!sel.value) return;
  addMemberRow(sel.value, '', '', '');
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
  const members = $$('#memberList li').map(li => ({
    provider_id: li.dataset.provider,
    account_id: (li.querySelector('.acctSel') || {}).value || '',
    model: (li.querySelector('.modelSel') || {}).value || '',
    token_param_mode: (li.querySelector('.tokenParamSel') || {}).value || '',
  })).filter(m => m.provider_id);
  const payload = {
    id: f.get('id'), display_name: f.get('display_name'), rotation: f.get('rotation'),
    members, enabled: f.get('enabled') === 'on',
    proxy_rotate: f.get('proxy_rotate') === 'on',
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

// ---------- Proxies ----------
async function renderProxies() {
  const seq = state.renderSeq;
  const app = $('#app');
  app.innerHTML = '<div class="loading">Loading proxies…</div>';
  try {
    state.proxies = await api.get('/proxies');
    if (seq !== state.renderSeq) return;
    app.innerHTML = `
      <h1>Egress proxies</h1>
      <div class="sub">Shared rotation pool for upstreams that rate-limit per source IP. Providers and combos opt in with their "Rotate egress proxy" toggle — everything else dials out directly.</div>
      <div class="toolbar"><div class="grow"></div><button class="btn" onclick="showProxyForm()">+ Add proxy</button></div>
      <div id="proxyForm"></div>
      <div class="card"><div id="proxyTable">${proxyTable()}</div></div>`;
  } catch (e) { if (seq === state.renderSeq) app.innerHTML = errBox(e); }
}

function proxyTable() {
  const ps = state.proxies || [];
  if (!ps.length) return '<div class="empty">No proxies configured — providers dial out directly.</div>';
  return `<table><thead><tr><th>Label</th><th>URL</th><th>Status</th><th>Enabled</th><th></th></tr></thead><tbody>` +
    ps.map(p => `<tr>
      <td>${esc(p.label || '(no label)')}</td>
      <td class="mono small">${esc(maskProxyURL(p.url))}</td>
      <td>${proxyStatusBadge(p)}</td>
      <td><button class="btn sm ${p.enabled ? '' : 'ghost'}" onclick="toggleProxy(${jsq(p.id)})">${p.enabled ? 'On' : 'Off'}</button></td>
      <td style="white-space:nowrap;text-align:right">
        <button class="btn sm ghost" onclick="showProxyForm(${jsq(p.id)})">Edit</button>
        <button class="btn sm danger" onclick="deleteProxy(${jsq(p.id)})">Del</button>
      </td></tr>`).join('') + `</tbody></table>`;
}

// proxyStatusBadge renders passive liveness for one pool entry: alive (a
// response arrived through it), dead + cooldown (transport failure, skipped by
// rotation until the cooldown elapses), or unknown (never tried since restart
// — not a verdict). No background probing exists, so the status only ever
// reflects real dispatched traffic.
function proxyStatusBadge(p) {
  const s = p.status || 'unknown';
  if (s === 'alive') return '<span class="badge ok">Alive</span>';
  if (s === 'dead') {
    const secs = p.cooldown_ms != null ? Math.max(0, Math.ceil(p.cooldown_ms / 1000)) : 0;
    return `<span class="badge err" title="Transport failure — skipped by rotation until the cooldown elapses. Failures: ${p.failures || 0}">Dead${secs ? ' · ' + secs + 's' : ''}</span>`;
  }
  return '<span class="badge" title="Never tried since restart — not a verdict">Unknown</span>';
}

// maskProxyURL shows the proxy URL without credentials — URLs carry
// user:pass in userinfo and the table must not leak them on screen. Parsed
// with the URL constructor (not a naive indexOf('@') split) because a
// non-percent-encoded '@' inside the password is itself legal userinfo — the
// LAST '@' before the host is the real delimiter, exactly like Go's
// url.Parse on the server side, and a first-'@' split would leak the tail of
// such a password plus the real host.
function maskProxyURL(raw) {
  const s = String(raw || '');
  try {
    const u = new URL(s);
    if (u.username || u.password) return u.protocol + '//' + '••••••@' + u.host + u.pathname + u.search;
    return s;
  } catch (_) {
    return s;
  }
}

function showProxyForm(id) {
  const p = id ? (state.proxies || []).find(x => x.id === id) : { id: '', label: '', url: '', enabled: true };
  $('#proxyForm').innerHTML = `
    <div class="card"><h2>${id ? 'Edit' : 'Add'} proxy</h2>
    <form onsubmit="return saveProxy(event, ${jsq(id || '')})">
      <div class="row">
        <div><label>Label</label><input name="label" value="${esc(p.label || '')}" placeholder="e.g. eu-resi-1"></div>
        <div style="flex:2"><label>URL *</label><input name="url" value="${esc(p.url || '')}" placeholder="socks5://user:pass@1.2.3.4:1080" required></div>
      </div>
      <div class="row">
        <label class="pill"><input type="checkbox" name="enabled" ${p.enabled ? 'checked' : ''} style="width:auto"> Enabled</label>
      </div>
      <div class="small muted" style="margin-top:6px">Scheme must be http, https, socks5 or socks5h, with an explicit port. URLs are masked in this table and never appear in logs (dashboard API responses do include the full URL, same as provider API keys, so this form can be edited).</div>
      <div class="form-actions"><button class="btn" type="submit">Save</button><button class="btn ghost" type="button" onclick="cancelProxyForm()">Cancel</button></div>
    </form></div>`;
  $('#proxyForm').scrollIntoView({ behavior: 'smooth' });
}
function cancelProxyForm() { $('#proxyForm').innerHTML = ''; }

async function saveProxy(e, id) {
  e.preventDefault();
  const f = new FormData(e.target);
  const payload = {
    label: (f.get('label') || '').toString().trim(),
    url: (f.get('url') || '').toString().trim(),
    enabled: f.get('enabled') === 'on',
  };
  try {
    if (id) await api.put('/proxies/' + encodeURIComponent(id), payload);
    else await api.post('/proxies', payload);
    state.proxies = await api.get('/proxies');
    cancelProxyForm();
    $('#proxyTable').innerHTML = proxyTable();
  } catch (err) { alert('Save failed: ' + err.message); }
  return false;
}

async function toggleProxy(id) {
  const p = (state.proxies || []).find(x => x.id === id);
  if (!p) return;
  try {
    await api.put('/proxies/' + encodeURIComponent(id), { label: p.label, url: p.url, enabled: !p.enabled });
    p.enabled = !p.enabled;
  } catch (e) { alert('Toggle failed: ' + e.message); }
  $('#proxyTable').innerHTML = proxyTable();
}

async function deleteProxy(id) {
  if (!confirm('Delete proxy "' + id + '"? Providers/combos using the pool keep working — they fall back to direct dial.')) return;
  try {
    await api.del('/proxies/' + encodeURIComponent(id));
  } catch (e) { alert('Delete failed: ' + e.message); return; }
  state.proxies = (state.proxies || []).filter(x => x.id !== id);
  $('#proxyTable').innerHTML = proxyTable();
  cancelProxyForm();
}

// ---------- Logs ----------
let logFilter = { limit: 50, offset: 0 };
async function renderLogs() {
  const seq = state.renderSeq;
  // Reset pagination per visit — the module-level filter otherwise leaks
  // across route changes (revisit mid-pagination → silent empty first page).
  logFilter = { limit: 50, offset: 0 };
  const app = $('#app');
  app.innerHTML = '<div class="loading">Loading logs…</div>';
  try {
    state.providers = await api.get('/providers');
    if (seq !== state.renderSeq) return;
    app.innerHTML = `
      <h1>Request Logs</h1>
      <div class="sub">Every proxied call, with filtering and hourly chart. Click a row for details.</div>
      <div class="card">
        <div class="row">
          <div><label>Provider</label><select id="f_provider"><option value="">All</option>${state.providers.map(p => `<option>${esc(p.id)}</option>`).join('')}</select></div>
          <div><label>Endpoint</label><select id="f_endpoint"><option value="">All</option><option>chat.completions</option><option>completions</option><option>responses</option><option>embeddings</option></select></div>
          <div><label>Errors only</label><select id="f_errors"><option value="0">No</option><option value="1">Yes</option></select></div>
          <div style="align-self:end;display:flex;gap:6px">
            <button class="btn sm ${state.logAutoRefresh ? '' : 'ghost'}" id="autoRefreshBtn" onclick="toggleLogAutoRefresh()">${state.logAutoRefresh ? '⏸ Pause' : '▶ Live'}</button>
            <button class="btn sm" onclick="applyLogFilter()">Apply</button>
            <button class="btn sm danger" onclick="clearLogs()">Clear Logs</button>
          </div>
        </div>
      </div>
      <div class="card"><h2>Requests per hour (24h)</h2><canvas id="logChart" width="1000" height="220"></canvas></div>
      <div class="card"><div id="logTable"></div><div class="pagination" id="logPager"></div></div>`;
    await loadLogs();
    await drawChart();
    if (seq !== state.renderSeq) return;
    // Auto-refresh every 5 seconds for real-time log updates (if enabled).
    // Clear before creating: a re-render must never stack intervals.
    if (state.logRefresh) clearInterval(state.logRefresh);
    if (state.logAutoRefresh) {
      state.logRefresh = setInterval(async () => {
        try { await loadLogs(); await drawChart(); } catch (_) {}
      }, 5000);
    }
  } catch (e) { if (seq === state.renderSeq) app.innerHTML = errBox(e); }
}

// A failed reload must not leave the pager advertising a page that was never
// loaded, and must say so instead of rejecting silently.
async function applyLogFilter() {
  const prev = logFilter.offset;
  logFilter.offset = 0;
  try { await loadLogs(); } catch (e) { logFilter.offset = prev; alert('Could not load logs: ' + e.message); }
}

function toggleLogAutoRefresh() {
  state.logAutoRefresh = !state.logAutoRefresh;
  const btn = $('#autoRefreshBtn');
  if (btn) {
    btn.textContent = state.logAutoRefresh ? '⏸ Pause' : '▶ Live';
    btn.className = 'btn sm ' + (state.logAutoRefresh ? '' : 'ghost');
  }
  // Never stack intervals: always clear the previous timer first.
  if (state.logRefresh) clearInterval(state.logRefresh);
  state.logRefresh = null;
  if (state.logAutoRefresh) {
    state.logRefresh = setInterval(async () => {
      try { await loadLogs(); await drawChart(); } catch (_) {}
    }, 5000);
  }
}

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
    `<table><thead><tr><th>Time</th><th>Model in</th><th>Provider</th><th>Endpoint</th><th>Status</th><th title="prompt / completion tokens">Tokens</th><th title="prompt tokens served from the upstream KV cache">Cache</th><th title="completion tokens per second (whole request)">TPS</th><th>Duration</th></tr></thead><tbody>` +
    items.map(l => `<tr class="log-row" data-id="${l.id}">
      <td class="small muted">${fmtTime(l.ts)}</td>
      <td class="mono small">${esc(l.model_in)}</td>
      <td class="mono small">${esc(l.provider_used)}</td>
      <td class="small muted">${esc(l.endpoint)}</td>
      <td><span class="badge ${l.status < 400 ? 'ok' : 'err'}">${l.status}</span>${l.error ? ' <span class="small pill-bad" title="' + esc(l.error) + '">!</span>' : ''}${l.heal_note ? ' <span class="small pill-warn" title="' + esc(l.heal_note) + '">heal</span>' : ''}</td>
      <td class="small">${fmtNum(l.prompt_tokens)}/${fmtNum(l.completion_tokens)}</td>
      <td class="small">${fmtNum(l.cached_tokens)}</td>
      <td class="small">${fmtTps(l.completion_tokens, l.latency_ms)}</td>
      <td class="small">${fmtDuration(l.latency_ms)}</td></tr>`).join('') + `</tbody></table>`;
  // Attach click handlers for expandable detail rows.
  $$('.log-row').forEach(row => {
    row.addEventListener('click', () => showLogDetail(row));
  });
  // Restore any open detail panel after a (re)render so auto-refresh keeps it visible.
  mountExpandedDetail($('#logTable').querySelector('tbody'));
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

async function gotoLogPage(p) {
  const prev = logFilter.offset;
  logFilter.offset = p * logFilter.limit;
  try { await loadLogs(); } catch (e) { logFilter.offset = prev; alert('Could not load page: ' + e.message); }
}

async function drawChart() {
  // Self-contained failure: a chart hiccup (or a missing canvas after the user
  // navigated away mid-await) must not nuke the whole logs view.
  const canvas = $('#logChart');
  if (!canvas) return;
  let data;
  try { data = await api.get('/logs/chart?hours=24'); } catch (_) { return; }
  if (!$('#logChart')) return;
  const ctx = canvas.getContext('2d');
  const W = canvas.width, H = canvas.height, pad = 30;
  ctx.clearRect(0, 0, W, H);

  // Bucket into 24 consecutive hours ending this hour.
  const nowHr = Math.floor(Date.now() / 1000 / 3600) * 3600;
  const buckets = Array.from({ length: 24 }, (_, i) => nowHr - (23 - i) * 3600);
  // Prototype-less map: a provider literally named "constructor"/"toString"
  // must not shadow builtins.
  const byProvider = Object.create(null);
  for (const row of data || []) {
    byProvider[row.provider] = byProvider[row.provider] || Object.create(null);
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

// Track the expanded log detail. The node is cached (not rebuilt) so auto-refresh
// re-rendering the table keeps the detail panel open — see mountExpandedDetail().
let expandedLogId = null;
let expandedDetailNode = null;

function closeExpandedDetail() {
  if (expandedDetailNode) expandedDetailNode.remove();
  const pinned = document.getElementById('logTable')?.querySelector('.log-detail-pinned');
  if (pinned) pinned.remove();
  expandedDetailNode = null;
  expandedLogId = null;
  $$('.log-row-expanded').forEach(r => r.classList.remove('log-row-expanded'));
}

// Re-insert the cached detail node after every (re)render so auto-refresh keeps
// it open. If the row is still on screen the panel stays anchored to it; if new
// entries pushed the row off the current page, the panel pins to the top of the
// log area instead of disappearing.
function mountExpandedDetail(tbody) {
  if (!expandedLogId) return;
  $$('.log-row-expanded').forEach(r => r.classList.remove('log-row-expanded'));
  const oldPinned = $('#logTable .log-detail-pinned');
  if (oldPinned) oldPinned.remove();
  const row = tbody && tbody.querySelector('.log-row[data-id="' + expandedLogId + '"]');
  if (row) {
    row.classList.add('log-row-expanded');
    if (expandedDetailNode) row.after(expandedDetailNode); // still fetching → highlight only
  } else if (expandedDetailNode) {
    // Row not on the current page: pin the panel at the top of the table area.
    const pinned = document.createElement('table');
    pinned.className = 'log-detail-pinned';
    const tb = document.createElement('tbody');
    tb.appendChild(expandedDetailNode);
    pinned.appendChild(tb);
    $('#logTable').insertBefore(pinned, $('#logTable').firstChild);
  }
}

async function showLogDetail(row) {
  const id = row.dataset.id;
  // If clicking the already-expanded row, collapse it.
  if (expandedLogId === id) { closeExpandedDetail(); return; }
  closeExpandedDetail();

  expandedLogId = id;
  row.classList.add('log-row-expanded');

  // Fetch full detail.
  let detail;
  try { detail = await api.get('/logs/' + id); } catch (e) { closeExpandedDetail(); return; }
  // User collapsed or picked another row while the fetch was in flight.
  if (expandedLogId !== id) return;

  const fmtJson = (s) => {
    if (!s) return '<span class="muted">empty</span>';
    try { return esc(JSON.stringify(JSON.parse(s), null, 2)); } catch (_) { return esc(s); }
  };

  const detailRow = document.createElement('tr');
  detailRow.className = 'log-detail-row';
  // Stat chips: duration/TPS derive from latency_ms + token counts; cache stats
  // come from the usage details blob and are only present when the upstream reports them.
  const stats = [`<div class="log-stat"><span class="log-stat-label">Duration</span><span class="log-stat-value">${fmtDuration(detail.latency_ms)}</span></div>`];
  if (detail.prompt_tokens != null || detail.completion_tokens != null)
    stats.push(`<div class="log-stat"><span class="log-stat-label">Tokens in/out</span><span class="log-stat-value">${fmtNum(detail.prompt_tokens)} / ${fmtNum(detail.completion_tokens)}</span></div>`);
  if (detail.completion_tokens != null && detail.latency_ms > 0)
    stats.push(`<div class="log-stat"><span class="log-stat-label">TPS</span><span class="log-stat-value">${fmtTps(detail.completion_tokens, detail.latency_ms)} tok/s</span></div>`);
  if (detail.cached_tokens != null && detail.cached_tokens > 0) {
    stats.push(`<div class="log-stat"><span class="log-stat-label">Cache read</span><span class="log-stat-value">${fmtNum(detail.cached_tokens)}</span></div>`);
    if (detail.prompt_tokens > 0) {
      // Clamp defensively: some upstreams have reported cached_tokens slightly
      // over prompt_tokens, which would otherwise render a negative "effective"
      // count or a >100% cache rate.
      const cappedCached = Math.min(detail.cached_tokens, detail.prompt_tokens);
      const eff = detail.prompt_tokens - cappedCached;
      const pct = Math.round(cappedCached / detail.prompt_tokens * 100);
      stats.push(`<div class="log-stat log-stat-wide"><span class="log-stat-label">Cache savings</span><span class="log-stat-value">${fmtNum(detail.prompt_tokens)} → ${fmtNum(eff)} effective (${pct}% cached)</span></div>`);
    }
  }
  detailRow.innerHTML = `<td colspan="9" class="log-detail">
    <div class="log-detail-header">
      <span class="small muted">Log #${detail.id} — ${fmtTime(detail.ts)}</span>
      <button class="btn sm ghost log-detail-close" title="Close detail">×</button>
    </div>
    <div class="log-detail-body">
      <div class="log-detail-stats">${stats.join('')}</div>
      <div class="log-detail-section">
        <div class="log-detail-label">Upstream URL</div>
        <pre class="log-detail-code">${esc(detail.upstream_url || '—')}</pre>
      </div>
      ${detail.heal_note ? `<div class="log-detail-section"><div class="log-detail-label" style="color:var(--warn,#e0a83a)">Smart heal</div><pre class="log-detail-code">${esc(detail.heal_note)}</pre></div>` : ''}
      <div class="log-detail-section">
        <div class="log-detail-label">Request Payload</div>
        <pre class="log-detail-code">${fmtJson(detail.request_payload)}</pre>
      </div>
      <div class="log-detail-section">
        <div class="log-detail-label">Response Snippet</div>
        <pre class="log-detail-code">${fmtJson(detail.response_snippet)}</pre>
      </div>
      ${detail.error ? `<div class="log-detail-section"><div class="log-detail-label" style="color:var(--bad)">Error</div><pre class="log-detail-code log-detail-error">${esc(detail.error)}</pre></div>` : ''}
    </div>
  </td>`;

  expandedDetailNode = detailRow;
  detailRow.querySelector('.log-detail-close').addEventListener('click', (e) => {
    e.stopPropagation();
    closeExpandedDetail();
  });
  // Use mountExpandedDetail to insert at the correct position — the original
  // row reference may be stale if auto-refresh re-rendered during the fetch.
  mountExpandedDetail($('#logTable').querySelector('tbody'));
}

async function clearLogs() {
  if (!confirm('Clear ALL request logs? This cannot be undone.')) return;
  closeExpandedDetail();
  try {
    const res = await api.post('/logs/clear', {});
    logFilter.offset = 0;
    await loadLogs();
    await drawChart();
    alert('Cleared ' + (res.deleted || 0) + ' log entries.');
  } catch (e) {
    alert('Failed to clear logs: ' + e.message);
  }
}

// ---------- Settings ----------
async function renderSettings() {
  const seq = state.renderSeq;
  const app = $('#app');
  app.innerHTML = '<div class="loading">Loading settings…</div>';
  try {
    state.settings = await api.get('/settings');
    if (seq !== state.renderSeq) return;
    const s = state.settings;
    app.innerHTML = `
      <h1>Settings</h1><div class="sub">Health, rotation, and retention knobs. Changes apply immediately.</div>
      <div class="grid cols-2">
        <div class="card"><h2>Health & rotation</h2>
          <form onsubmit="return saveSettings(event)">
            <label>Cooldown (seconds)</label><input name="health.cooldown" type="number" min="1" value="${esc(s['health.cooldown'] || '60')}">
            <label>Error codes triggering rotation (comma)</label><input name="health.error_codes" value="${esc(s['health.error_codes'] || '402,429,500,502,503,504')}">
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
  } catch (e) { if (seq === state.renderSeq) app.innerHTML = errBox(e); }
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
