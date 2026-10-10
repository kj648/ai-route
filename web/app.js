'use strict';

// ---------------------------------------------------------------- helpers
// UI strings are written in Chinese and wrapped in t() (see i18n.js), which
// returns the English text when the console language is English.
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => Array.from(el.querySelectorAll(s));
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const TOKEN_KEY = 'ai_route_admin_token';
let TOKEN = '';
// ME is the signed-in principal from /me: { role: 'admin' | 'user', user, ... }
let ME = null;
try { TOKEN = localStorage.getItem(TOKEN_KEY) || ''; } catch (e) { /* storage unavailable */ }

class ApiError extends Error {
  constructor(status, msg) { super(msg); this.status = status; }
}

async function api(method, path, body) {
  const opt = { method, headers: { Authorization: 'Bearer ' + TOKEN } };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const res = await fetch('/admin/api' + path, opt);
  let data = null;
  const text = await res.text();
  try { data = text ? JSON.parse(text) : null; } catch (e) { data = text; }
  if (res.status === 401) {
    TOKEN = '';
    ME = null;
    try { localStorage.removeItem(TOKEN_KEY); } catch (e) { /* ignore */ }
    renderLogin(t('登录已失效，请重新登录'));
    throw new ApiError(401, 'unauthorized');
  }
  if (!res.ok) throw new ApiError(res.status, (data && data.error) || res.statusText);
  return data;
}

function toast(msg, type = '') {
  const el = document.createElement('div');
  el.className = 'toast ' + type;
  el.textContent = msg;
  $('#toasts').appendChild(el);
  setTimeout(() => el.remove(), type === 'err' ? 6000 : 3000);
}

function fmtNum(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e4) return (n / 1e3).toFixed(1) + 'K';
  return n.toLocaleString();
}
function fmtMs(ms) {
  ms = Number(ms || 0);
  if (ms >= 60000) return (ms / 60000).toFixed(1) + 'm';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}
function fmtTime(ms) {
  if (!ms) return '-';
  const d = new Date(ms);
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getMonth() + 1}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
function fmtAgo(ms) {
  if (!ms) return t('从未');
  const s = (Date.now() - ms) / 1000;
  if (s < 60) return t('刚刚');
  if (s < 3600) return t('{n} 分钟前', { n: Math.floor(s / 60) });
  if (s < 86400) return t('{n} 小时前', { n: Math.floor(s / 3600) });
  return t('{n} 天前', { n: Math.floor(s / 86400) });
}
function fmtSecs(s) {
  if (s >= 3600) return (s / 3600).toFixed(1) + 'h';
  if (s >= 60) return Math.ceil(s / 60) + 'm';
  return s + 's';
}
const CURRENCY_SIGN = { CNY: '¥', USD: '$' };
function fmtMoney(v, cur) {
  if (!cur) return '-';
  const sign = CURRENCY_SIGN[cur] || '';
  if (!v) return sign + '0';
  return sign + (v < 0.01 ? v.toFixed(5) : v < 1 ? v.toFixed(4) : v.toFixed(2));
}
function pct(a, b) { return b ? ((a / b) * 100).toFixed(1) + '%' : '-'; }
const currencyLabel = (c) => (c === 'CNY' ? t('人民币 ¥') : t('美元 $'));

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast(t('已复制'), 'ok');
  } catch (e) {
    const ta = document.createElement('textarea');
    ta.value = text; document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); toast(t('已复制'), 'ok'); } catch (e2) { toast(t('复制失败'), 'err'); }
    ta.remove();
  }
}

// ---------------------------------------------------------------- modal
function openModal({ title, body, foot = '', wide = false, onMount }) {
  const root = $('#modal-root');
  root.innerHTML = `
    <div class="modal-bg">
      <div class="modal ${wide ? 'wide' : ''}" role="dialog">
        <div class="modal-head"><span>${esc(title)}</span><button class="x" data-close>&times;</button></div>
        <div class="modal-body">${body}</div>
        ${foot ? `<div class="modal-foot">${foot}</div>` : ''}
      </div>
    </div>`;
  const bg = $('.modal-bg', root);
  bg.addEventListener('mousedown', (e) => { if (e.target === bg) closeModal(); });
  $$('[data-close]', root).forEach((b) => b.addEventListener('click', closeModal));
  if (onMount) onMount($('.modal', root));
  const first = $('input:not([type=checkbox]),textarea,select', root);
  if (first) first.focus();
}
function closeModal() { $('#modal-root').innerHTML = ''; }
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeModal(); });

// confirmBox renders in its own layer so an open form modal is kept.
function confirmBox(msg) {
  return new Promise((resolve) => {
    const layer = document.createElement('div');
    layer.className = 'modal-bg confirm-layer';
    layer.innerHTML = `<div class="modal" role="alertdialog" style="max-width:460px">
      <div class="modal-head"><span>${t('确认')}</span></div>
      <div class="modal-body">${esc(msg)}</div>
      <div class="modal-foot"><button class="btn" data-no>${t('取消')}</button><button class="btn primary" data-yes>${t('确定')}</button></div>
    </div>`;
    const done = (v) => { layer.remove(); document.removeEventListener('keydown', onKey, true); resolve(v); };
    const onKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); done(false); } };
    document.addEventListener('keydown', onKey, true);
    $('[data-yes]', layer).onclick = () => done(true);
    $('[data-no]', layer).onclick = () => done(false);
    document.body.appendChild(layer);
    $('[data-yes]', layer).focus();
  });
}

// ---------------------------------------------------------------- presets
// The preset catalog lives in presets.js (PRESETS, PRESET_CATEGORIES, presetById).

// ---------------------------------------------------------------- shell
const ADMIN_PAGES = [
  ['dashboard', t('概览'), () => pageDashboard()],
  ['providers', t('供应商'), () => pageProviders()],
  ['models', t('模型映射'), () => pageModels()],
  ['keys', 'API Keys', () => pageKeys()],
  ['users', t('用户'), () => pageUsers()],
  ['logs', t('请求日志'), () => pageLogs()],
  ['settings', t('设置与接入'), () => pageSettings()],
];
const USER_PAGES = [
  ['overview', t('概览'), () => pageMyOverview()],
  ['models', t('可用模型'), () => pageMyModels()],
  ['keys', 'API Keys', () => pageMyKeys()],
  ['logs', t('请求日志'), () => pageMyLogs()],
  ['account', t('接入与账号'), () => pageMyAccount()],
];
const pagesFor = () => (ME && ME.role === 'admin' ? ADMIN_PAGES : USER_PAGES);

const LOGIN_MODE_KEY = 'ai_route_login_mode';
function renderLogin(msg) {
  closeModal();
  let mode = 'account';
  try { mode = localStorage.getItem(LOGIN_MODE_KEY) === 'token' ? 'token' : 'account'; } catch (e) { /* storage unavailable */ }
  $('#app').innerHTML = `
    <div class="login card">
      <div class="card-head">${t('AI Route 控制台')}${langSwitchHTML()}</div>
      <div class="card-body form">
        <div class="seg" id="login-mode"><button type="button" data-m="account">${t('账号登录')}</button><button type="button" data-m="token">${t('管理令牌')}</button></div>
        <div class="err-text small" id="login-msg">${msg ? esc(msg) : ''}</div>
        <div id="login-account">
          <div class="field"><label>${t('用户名')}</label><input type="text" id="login-user" autocomplete="username"></div>
          <div class="field"><label>${t('密码')}</label><input type="password" id="login-pw" autocomplete="current-password"></div>
        </div>
        <div id="login-token-box">
          <div class="field">
            <label>${t('管理令牌')}</label>
            <input type="password" id="login-token" placeholder="${esc(t('ADMIN_TOKEN，或首次启动时日志里打印的 admin-xxx'))}">
            <div class="help">${t('令牌只保存在本浏览器。')}</div>
          </div>
        </div>
        <button class="btn primary" id="login-btn">${t('登录')}</button>
      </div>
    </div>`;
  bindLangSwitch($('#app'));
  const setMode = (m) => {
    mode = m;
    try { localStorage.setItem(LOGIN_MODE_KEY, m); } catch (e) { /* ignore */ }
    $$('#login-mode button').forEach((b) => b.classList.toggle('on', b.dataset.m === m));
    $('#login-account').style.display = m === 'account' ? '' : 'none';
    $('#login-token-box').style.display = m === 'token' ? '' : 'none';
    (m === 'account' ? $('#login-user') : $('#login-token')).focus();
  };
  $$('#login-mode button').forEach((b) => b.onclick = () => setMode(b.dataset.m));
  const fail = (text) => { $('#login-msg').textContent = text; };
  const enter = (token) => {
    TOKEN = token;
    ME = null;
    try { localStorage.setItem(TOKEN_KEY, TOKEN); } catch (e) { /* ignore */ }
    renderShell();
  };
  const go = async () => {
    try {
      if (mode === 'account') {
        const res = await fetch('/admin/api/login', { method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ username: $('#login-user').value.trim(), password: $('#login-pw').value }) });
        if (res.status === 401) return fail(t('用户名或密码错误'));
        if (res.status === 429) return fail(t('失败次数太多，请稍后再试'));
        if (!res.ok) return fail(t('登录失败：HTTP {status}', { status: res.status }));
        enter((await res.json()).token);
      } else {
        const tok = $('#login-token').value.trim();
        const res = await fetch('/admin/api/me', { headers: { Authorization: 'Bearer ' + tok } });
        if (res.status === 401) return fail(t('管理令牌不对'));
        if (res.status === 429) return fail(t('失败次数太多，请稍后再试'));
        if (!res.ok) return fail(t('登录失败：HTTP {status}', { status: res.status }));
        enter(tok);
      }
    } catch (e) { fail(e.message); }
  };
  $('#login-btn').onclick = go;
  ['#login-user', '#login-pw', '#login-token'].forEach((s) => $(s).addEventListener('keydown', (e) => { if (e.key === 'Enter') go(); }));
  setMode(mode);
}

// the gateway's own User-Agent fingerprint (ai-route/<version>)
let PLATFORM_UA = 'ai-route';

let shellLoading = false;
async function renderShell() {
  if (shellLoading) return;
  shellLoading = true;
  try {
    ME = await api('GET', '/me');
  } catch (e) {
    if (e.status !== 401) $('#app').innerHTML = `<div class="empty err-text">${t('加载失败：{msg}', { msg: esc(e.message) })}</div>`;
    return;
  } finally { shellLoading = false; }
  const admin = ME.role === 'admin';
  if (admin) {
    api('GET', '/ping').then((r) => { if (r.user_agent) PLATFORM_UA = r.user_agent; }).catch(() => {});
  }
  const who = ME.user ? (ME.user.display_name || ME.user.username) : t('管理令牌');
  $('#app').innerHTML = `
    <div class="layout">
      <aside class="sidebar">
        <div class="brand"><span class="dot"></span>AI Route <span class="muted small" id="brand-ver">${ME.version ? 'v' + esc(ME.version) : ''}</span></div>
        <nav class="nav">${pagesFor().map(([id, name]) => `<a href="#/${id}" data-page="${id}">${name}</a>`).join('')}</nav>
        <div class="foot"><div class="muted small" style="margin-bottom:6px">${esc(who)}${admin && ME.user ? ` · ${t('管理员')}` : ''}</div>${langSwitchHTML()}<button class="btn sm" id="logout">${t('退出登录')}</button></div>
      </aside>
      <main class="main" id="page"></main>
    </div>`;
  bindLangSwitch($('#app'));
  $('#logout').onclick = async () => {
    try { await api('POST', '/logout'); } catch (e) { /* signed out anyway */ }
    try { localStorage.removeItem(TOKEN_KEY); } catch (e) { /* ignore */ }
    TOKEN = '';
    ME = null;
    renderLogin();
  };
  route();
}

let refreshTimer = null;
function route() {
  if (!TOKEN) return renderLogin();
  if (!ME || !$('#page')) return renderShell();
  clearInterval(refreshTimer);
  const pages = pagesFor();
  const id = (location.hash.replace(/^#\//, '') || pages[0][0]).split('?')[0];
  const page = pages.find((p) => p[0] === id) || pages[0];
  $$('.nav a').forEach((a) => a.classList.toggle('active', a.dataset.page === page[0]));
  $('#page').innerHTML = `<div class="empty">${t('加载中…')}</div>`;
  page[2]().catch((e) => {
    if (e.status !== 401) $('#page').innerHTML = `<div class="empty err-text">${t('加载失败：{msg}', { msg: esc(e.message) })}</div>`;
  });
}
window.addEventListener('hashchange', route);

function head(title, sub, actions = '') {
  return `<div class="page-head"><div><h1>${esc(title)}</h1>${sub ? `<div class="sub">${sub}</div>` : ''}</div><div class="btns">${actions}</div></div>`;
}

// health lookup from breaker status
function healthIndex(status) {
  const idx = { provider: {}, target: {} };
  for (const s of status || []) idx[s.kind][s.name] = s;
  return idx;
}
function targetHealth(tg, idx, providers) {
  const prefix = tg.split('/')[0];
  const p = providers.find((x) => x.prefix === prefix);
  if (!p) return { cls: 'missing', tip: t('套餐前缀不存在') };
  if (!p.enabled) return { cls: 'missing', tip: t('套餐已停用') };
  const ps = idx.provider[prefix];
  const ts = idx.target[tg];
  if (ps && ps.down) return { cls: 'cool', tip: t('健康检查失败，已移出调度：{err}', { err: ps.last_error }) };
  if (ps && ps.open) return { cls: 'cool', tip: t('套餐冷却中，剩余 {left}：{err}', { left: fmtSecs(ps.remaining_seconds), err: ps.last_error }) };
  if (ts && ts.open) return { cls: 'cool', tip: t('冷却中，剩余 {left}：{err}', { left: fmtSecs(ts.remaining_seconds), err: ts.last_error }) };
  if (ts && ts.failures > 0) return { cls: '', tip: t('连续失败 {n} 次：{err}', { n: ts.failures, err: ts.last_error }) };
  return { cls: '', tip: t('正常') };
}
// a target entry is "prefix/model" or a weighted same-priority group "a/x*3 | b/x"
function parseEntry(entry) {
  return entry.split('|').map((x) => x.trim()).filter(Boolean).map((part) => {
    const m = part.match(/^(.*\S)\s*\*\s*(\d+)$/);
    return m ? { t: m[1], w: Number(m[2]) } : { t: part, w: 1 };
  });
}
const entryTargets = (entry) => parseEntry(entry).map((x) => x.t);
// caller headers a provider requires: {{header.X}} with no "?" and no fallback
function requiredHeaders(p) {
  const out = [];
  for (const v of Object.values((p && p.headers) || {})) {
    for (const m of v.matchAll(/\{\{([^}]*)\}\}/g)) {
      const parts = m[1].split('??').map((x) => x.trim());
      const last = parts[parts.length - 1];
      if (last.startsWith('header.') && !last.endsWith('?')) out.push(last.slice(7));
    }
  }
  return out;
}
function chainHTML(targets, idx, providers) {
  if (!targets.length) return `<span class="muted small">${t('未配置')}</span>`;
  const one = (tg, w, group) => {
    const h = targetHealth(tg, idx, providers);
    return `<span class="target ${h.cls}" title="${esc(h.tip)}"><span class="s"></span>${esc(tg)}${group ? `<span class="weight">×${w}</span>` : ''}</span>`;
  };
  return `<div class="chain">${targets.map((entry, i) => {
    const members = parseEntry(entry);
    const inner = members.length > 1
      ? `<span class="group" title="${esc(t('同级按权重分流，同一会话固定走同一个'))}">${members.map((x) => one(x.t, x.w, true)).join('<span class="bar">|</span>')}</span>`
      : one(members[0].t, 1, false);
    return `${i ? '<span class="arrow">→</span>' : ''}${inner}`;
  }).join('')}</div>`;
}

// ---------------------------------------------------------------- dashboard
let dashRange = '24h';
let dashLoading = false; // a slow stats query must not pile up refreshes
async function pageDashboard() {
  const [stats, status, models, providers] = await (async () => { dashLoading = true; try { return await Promise.all([
    api('GET', '/stats?range=' + dashRange), api('GET', '/status'), api('GET', '/models'), api('GET', '/providers'),
  ]); } finally { dashLoading = false; } })();
  const tot = stats.total;
  const idx = healthIndex(status);
  const cooling = status.filter((s) => s.open);
  const ranges = [['1h', t('1 小时')], ['24h', t('24 小时')], ['7d', t('7 天')], ['30d', t('30 天')]];
  const max = Math.max(1, ...stats.timeline.map((r) => r.requests));
  const bars = stats.timeline.map((r) => {
    const h = (r.requests / max) * 100;
    const fh = r.requests ? (r.failed / r.requests) * h : 0;
    return `<div class="bar" style="height:${h}%" title="${esc(t('{key}：{n} 次，失败 {failed}，切换 {fallback}', { key: r.key, n: r.requests, failed: r.failed, fallback: r.fallback }))}"><div class="ok" style="flex:${h - fh}"></div><div class="fail" style="flex:${fh}"></div></div>`;
  }).join('');
  const rowTable = (rows, label) => rows.length ? `
    <div class="table-wrap"><table>
      <tr><th>${label}</th><th class="num">${t('请求')}</th><th class="num">${t('成功率')}</th><th class="num">${t('切换')}</th><th class="num">${t('输入')}</th><th class="num">${t('输出')}</th><th class="num">${t('费用')}</th><th class="num">${t('平均耗时')}</th><th class="num">${t('首字')}</th></tr>
      ${rows.map((r) => `<tr><td>${esc(r.key || '-')}</td><td class="num">${fmtNum(r.requests)}</td><td class="num">${pct(r.success, r.requests)}</td><td class="num">${fmtNum(r.fallback)}</td><td class="num">${fmtNum(r.input_tokens)}</td><td class="num">${fmtNum(r.output_tokens)}</td><td class="num" ${r.unpriced ? `title="${esc(t('{n} 次请求没有配置单价，未计入', { n: r.unpriced }))}"` : ''}>${r.unpriced && !r.cost ? '-' : fmtMoney(r.cost, stats.currency)}${r.unpriced && r.cost ? '*' : ''}</td><td class="num">${fmtMs(r.avg_latency_ms)}</td><td class="num">${r.avg_ttfb_ms ? fmtMs(r.avg_ttfb_ms) : '-'}</td></tr>`).join('')}
    </table></div>` : `<div class="empty">${t('暂无数据')}</div>`;

  $('#page').innerHTML = `
    ${head(t('概览'), t('请求量、成功率、候补切换与上游健康状态'), `
      <select id="dash-range">${ranges.map(([v, l]) => `<option value="${v}" ${v === dashRange ? 'selected' : ''}>${l}</option>`).join('')}</select>
      <button class="btn" id="dash-refresh">${t('刷新')}</button>`)}
    <div class="grid cols-5">
      <div class="card stat"><div class="label">${t('请求数')}</div><div class="value">${fmtNum(tot.requests)}</div><div class="hint">${t('失败 {n}', { n: fmtNum(tot.failed) })}</div></div>
      <div class="card stat"><div class="label">${t('成功率')}</div><div class="value">${pct(tot.success, tot.requests)}</div><div class="hint">${t('平均耗时 {ms}', { ms: fmtMs(tot.avg_latency_ms) })}</div></div>
      <div class="card stat"><div class="label">${t('发生候补切换')}</div><div class="value">${fmtNum(tot.fallback)}</div><div class="hint">${t('由非首选上游完成的请求')}</div></div>
      <div class="card stat"><div class="label">${t('Tokens（输入 / 输出）')}</div><div class="value">${fmtNum(tot.input_tokens)} / ${fmtNum(tot.output_tokens)}</div><div class="hint">${t('缓存命中 {n}', { n: fmtNum(tot.cached_tokens) })}</div></div>
      <div class="card stat"><div class="label">${t('费用')}</div><div class="value">${tot.cost ? fmtMoney(tot.cost, stats.currency) : '-'}</div><div class="hint">${tot.unpriced ? t('{n} 次未配单价，未计入', { n: fmtNum(tot.unpriced) }) : t('按上游实际费用或配置的单价')}</div></div>
    </div>
    <div class="card">
      <div class="card-head">${t('请求趋势')} <span class="muted small">${t('蓝色：成功 · 红色：失败')}</span></div>
      <div class="card-body">${stats.timeline.length ? `<div class="bars">${bars}</div><div class="bars-axis"><span>${esc(stats.timeline[0].key)}</span><span>${esc(stats.timeline[stats.timeline.length - 1].key)}</span></div>` : `<div class="empty">${t('暂无数据')}</div>`}</div>
    </div>
    <div class="card">
      <div class="card-head">${t('上游健康')} <span class="btns">${cooling.length ? `<span class="badge warn">${t('{n} 个冷却中', { n: cooling.length })}</span>` : `<span class="badge ok">${t('全部正常')}</span>`}<button class="btn sm" id="reset-all">${t('全部重置')}</button></span></div>
      <div class="card-body" style="padding:0">
        ${models.length ? `<div class="table-wrap"><table>
          <tr><th>${t('对外模型')}</th><th>${t('调度顺序（从左到右依次尝试）')}</th></tr>
          ${models.filter((m) => m.enabled).map((m) => `<tr><td><b>${esc(m.name)}</b></td><td>${chainHTML(m.targets, idx, providers)}</td></tr>`).join('')}
        </table></div>` : `<div class="empty">${t('还没有配置模型映射')}</div>`}
        ${cooling.length ? `<div class="table-wrap"><table>
          <tr><th>${t('冷却对象')}</th><th>${t('剩余')}</th><th>${t('最近错误')}</th><th></th></tr>
          ${cooling.map((s) => `<tr><td><span class="badge ${s.kind === 'provider' ? 'err' : 'warn'}">${s.kind === 'provider' ? t('整个套餐') : t('模型')}</span> ${esc(s.name)}</td><td>${s.down ? t('健康检查失败') : fmtSecs(s.remaining_seconds)}</td><td class="small err-text"><div class="ellipsis" title="${esc(s.last_error)}">${esc(s.last_error)}</div></td><td><button class="btn sm" data-reset="${esc(s.key)}">${t('恢复')}</button></td></tr>`).join('')}
        </table></div>` : ''}
      </div>
    </div>
    <div class="grid cols-2">
      <div class="card"><div class="card-head">${t('按对外模型')}</div>${rowTable(stats.by_model, t('模型'))}</div>
      <div class="card"><div class="card-head">${t('按实际上游')}</div>${rowTable(stats.by_target, t('上游'))}</div>
    </div>
    <div class="grid cols-2">
      <div class="card"><div class="card-head">${t('按套餐')}</div>${rowTable(stats.by_provider, t('套餐'))}</div>
      <div class="card"><div class="card-head">${t('按 API Key')}</div>${rowTable(stats.by_key, 'Key')}</div>
    </div>`;
  $('#dash-range').onchange = (e) => { dashRange = e.target.value; route(); };
  $('#dash-refresh').onclick = route;
  $('#reset-all').onclick = async () => { await api('POST', '/status/reset', { key: '' }); toast(t('已重置全部熔断状态'), 'ok'); route(); };
  $$('[data-reset]').forEach((b) => b.onclick = async () => { await api('POST', '/status/reset', { key: b.dataset.reset }); toast(t('已恢复'), 'ok'); route(); });
  // skip a tick while the previous refresh is still loading
  refreshTimer = setInterval(() => { if (!$('.modal-bg') && location.hash.startsWith('#/dashboard') && !dashLoading) route(); }, 30000);
}

// ---------------------------------------------------------------- providers
function parseLines(text, sep) {
  const out = {};
  for (const line of text.split('\n')) {
    const i = line.indexOf(sep);
    if (i <= 0) continue;
    const k = line.slice(0, i).trim();
    const v = line.slice(i + 1).trim();
    if (k) out[k] = v;
  }
  return out;
}
const toLines = (obj, sep) => Object.entries(obj || {}).map(([k, v]) => `${k}${sep} ${v}`).join('\n');
// unit prices: one line per model, "model(*) = input / cache / output" or
// "model = input / output" (cache billed at the input price), per 1M tokens
function parsePrices(text) {
  const prices = {};
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (!line) continue;
    const i = line.indexOf('=');
    const k = i > 0 ? line.slice(0, i).trim() : '';
    const nums = i > 0 ? line.slice(i + 1).split('/').map((x) => x.trim()) : [];
    if (!k || (nums.length !== 2 && nums.length !== 3) || nums.some((x) => x === '' || !(Number(x) >= 0))) {
      throw new Error(t('单价格式不对：{line}', { line }));
    }
    const [input, output] = [Number(nums[0]), Number(nums[nums.length - 1])];
    prices[k] = nums.length === 3 ? { input, cache: Number(nums[1]), output } : { input, output };
  }
  return prices;
}
const toPriceLines = (prices) => Object.entries(prices || {})
  .map(([k, v]) => `${k} = ${v.input} / ${v.cache != null ? v.cache + ' / ' : ''}${v.output}`).join('\n');
// request body rules: "model(*) [stream|nonstream, openai|anthropic|embeddings] = {json}"
// quotas: one line per period, "5h = 600" (requests) or "week = 0 / 50000000" (requests / tokens)
const QUOTA_PERIODS = ['5h', 'day', 'week', 'month'];
const QUOTA_LABEL = { '5h': t('最近 5 小时'), day: t('今天'), week: t('本周'), month: t('本月') };
function parseQuotas(text) {
  const out = [];
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (!line) continue;
    const [k, v] = line.split('=').map((x) => (x || '').trim());
    const [req, tok] = (v || '').split('/').map((x) => (x || '').trim().replace(/[_,]/g, ''));
    const period = (k || '').toLowerCase();
    if (!QUOTA_PERIODS.includes(period) || !/^\d+$/.test(req || '') || (tok && !/^\d+$/.test(tok))) {
      throw new Error(t('配额格式不对：{line}（应为 周期 = 请求数 / tokens，周期是 5h、day、week、month）', { line }));
    }
    out.push({ period, requests: Number(req), tokens: Number(tok || 0) });
  }
  return out;
}
const toQuotaLines = (qs) => (qs || []).map((q) => `${q.period} = ${q.requests || 0}${q.tokens ? ' / ' + q.tokens : ''}`).join('\n');
function quotaUsageHTML(us) {
  return us.map((u) => {
    const parts = [];
    if (u.requests) parts.push(t('{used} / {max} 次', { used: fmtNum(u.used_requests), max: fmtNum(u.requests) }));
    if (u.tokens) parts.push(t('{used} / {max} tokens', { used: fmtNum(u.used_tokens), max: fmtNum(u.tokens) }));
    return `<div>${t('{period}：{usage}', { period: esc(QUOTA_LABEL[u.period] || u.period), usage: parts.join(t('，')) })}${u.over ? ` <span class="badge warn">${t('已用完，排到其他候补后面')}</span>` : ''}</div>`;
  }).join('');
}

const RULE_TAGS = { stream: 'when', nonstream: 'when', openai: 'protocol', anthropic: 'protocol', responses: 'protocol', embeddings: 'protocol', rerank: 'protocol' };
function parseRules(text) {
  const rules = [];
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (!line) continue;
    const m = line.match(/^(\S+)\s*(?:\[([^\]]*)\])?\s*=\s*(\{.*\})$/);
    if (!m) throw new Error(t('参数规则格式不对：{line}', { line }));
    const rule = { model: m[1] };
    for (const tag of (m[2] || '').split(/[,\s]+/).filter(Boolean)) {
      if (!RULE_TAGS[tag]) throw new Error(t('参数规则的条件只能是 stream、nonstream、openai、anthropic、responses、embeddings、rerank：{line}', { line }));
      rule[RULE_TAGS[tag]] = tag;
    }
    try { rule.set = JSON.parse(m[3]); } catch (e) { throw new Error(t('参数规则的 JSON 不对：{line}', { line })); }
    if (!rule.set || Array.isArray(rule.set) || typeof rule.set !== 'object') throw new Error(t('参数规则必须是 JSON 对象：{line}', { line }));
    rules.push(rule);
  }
  return rules;
}
const toRuleLines = (rules) => (rules || []).map((r) => {
  const tags = [r.when, r.protocol].filter(Boolean);
  return `${r.model}${tags.length ? ` [${tags.join(', ')}]` : ''} = ${JSON.stringify(r.set)}`;
}).join('\n');
const usesProvider = (m, prefix) => m.targets.some((e) => entryTargets(e).some((tg) => tg.startsWith(prefix + '/')));

function compatOf(p) {
  if (p.openai_base_url && p.anthropic_base_url) return 'both';
  if (p.anthropic_base_url) return 'anthropic';
  return 'openai';
}
const COMPAT_LABEL = { openai: t('OpenAI 兼容'), anthropic: t('Anthropic 兼容'), both: 'OpenAI + Anthropic' };

// preset catalog entries keep their Chinese source text; translate on display
const catLabel = (cat) => (CATEGORY_LABEL[cat] ? t(CATEGORY_LABEL[cat]) : '');

// provider icon: the preset's brand logo, else a letter avatar tinted by name
function providerIcon(ps, name) {
  const ic = ps && BRAND_ICONS[ps.icon];
  if (ic) return `<span class="pi" aria-hidden="true"><svg viewBox="${ic[0]}" fill="currentColor" fill-rule="evenodd">${ic[1]}</svg></span>`;
  const s = String(name || '').trim() || '?';
  let h = 0;
  for (const c of s) h = (h * 31 + c.codePointAt(0)) % 360;
  return `<span class="pi letter" style="--h:${h}" aria-hidden="true">${esc([...s][0].toUpperCase())}</span>`;
}

function vendorBadge(p) {
  const ps = presetById(guessVendor(p));
  return ps ? `<span class="badge cat-${ps.category}" title="${esc(t(ps.name))}">${esc(catLabel(ps.category))}</span>` : `<span class="badge">${t('自定义')}</span>`;
}

async function pageProviders() {
  const [providers, status, models, rt] = await Promise.all([api('GET', '/providers'), api('GET', '/status'), api('GET', '/models'), api('GET', '/runtime')]);
  const idx = healthIndex(status);
  $('#page').innerHTML = `
    ${head(t('供应商'), t('编码套餐、官方 API、聚合平台都在这里配置。每个供应商有一个前缀，它的模型在映射里显示为 <code>前缀/模型名</code>'), `<button class="btn primary" id="add-provider">${t('+ 添加供应商')}</button>`)}
    ${providers.length ? providers.map((p) => {
      const ps = idx.provider[p.prefix];
      const used = models.filter((m) => usesProvider(m, p.prefix)).map((m) => m.name);
      let st = p.enabled ? `<span class="badge ok">${t('启用')}</span>` : `<span class="badge">${t('停用')}</span>`;
      if (p.enabled && (p.quota_usage || []).some((u) => u.over)) st = `<span class="badge warn">${t('配额用完')}</span>`;
      if (p.enabled && ps && ps.open) st = ps.down
        ? `<span class="badge err" title="${esc(ps.last_error)}">${t('健康检查失败')}</span>`
        : `<span class="badge warn" title="${esc(ps.last_error)}">${t('冷却中 {left}', { left: fmtSecs(ps.remaining_seconds) })}</span>`;
      const hc = rt.health[p.prefix];
      return `<div class="card">
        <div class="card-head">
          <div class="toolbar">${providerIcon(presetById(guessVendor(p)), p.name || p.prefix)} <code>${esc(p.prefix)}</code> <span>${esc(p.name || p.prefix)}</span> ${st}
            ${vendorBadge(p)} <span class="badge blue">${COMPAT_LABEL[compatOf(p)]}</span>
            ${p.ua_mode === 'override' ? `<span class="badge warn" title="${esc(p.user_agent)}">${t('固定 UA')}</span>` : ''}</div>
          <div class="btns">
            <button class="btn sm" data-test="${p.id}">${t('测试')}</button>
            <button class="btn sm" data-sync="${p.id}">${t('同步模型')}</button>
            <button class="btn sm" data-edit="${p.id}">${t('编辑')}</button>
            <button class="btn sm danger" data-del="${p.id}">${t('删除')}</button>
          </div>
        </div>
        <div class="card-body form">
          <div class="kv">
            ${p.openai_base_url ? `<div class="k">${t('OpenAI 地址')}</div><div class="mono small">${esc(p.openai_base_url)}</div>` : ''}
            ${p.anthropic_base_url ? `<div class="k">${t('Anthropic 地址')}</div><div class="mono small">${esc(p.anthropic_base_url)}</div>` : ''}
            <div class="k">API Key</div><div class="mono small">${p.has_api_key ? esc(p.api_key) : `<span class="err-text">${t('未设置')}</span>`}</div>
            <div class="k">User-Agent</div><div class="small">${p.ua_mode === 'platform' ? t('平台标识：{ua}', { ua: `<span class="mono">${esc(PLATFORM_UA)}</span>` }) : p.ua_mode === 'override' ? t('固定：{ua}', { ua: `<span class="mono">${esc(p.user_agent)}</span>` }) : p.user_agent ? t('透传客户端（缺省：{ua}）', { ua: `<span class="mono">${esc(p.user_agent)}</span>` }) : t('透传客户端')}</div>
            <div class="k">${t('被映射引用')}</div><div class="small">${used.length ? used.map(esc).join(t('，')) : `<span class="muted">${t('无')}</span>`}</div>
            ${p.max_concurrency ? `<div class="k">${t('并发')}</div><div class="small">${t('{n} / {max}（满了会溢出到下一个候补）', { n: rt.in_flight[p.prefix] || 0, max: p.max_concurrency })}</div>` : ''}
            ${p.health_check_seconds ? `<div class="k">${t('健康检查')}</div><div class="small">${t('每 {n} 秒', { n: p.health_check_seconds })}${hc && hc.last_check > 0 ? `${t('，最近 {ago}', { ago: fmtAgo(hc.last_check) })}${hc.failures ? `${t('，')}<span class="err-text">${t('连续失败 {n} 次', { n: hc.failures })}</span>` : `${t('，')}<span class="ok-text">${t('正常')}</span>`}` : ''}</div>` : ''}
            ${(p.quota_usage || []).length ? `<div class="k">${t('套餐配额')}</div><div class="small">${quotaUsageHTML(p.quota_usage)}</div>` : ''}
            ${p.first_token_timeout_seconds ? `<div class="k">${t('首包超时')}</div><div class="small">${t('{n} 秒', { n: p.first_token_timeout_seconds })}</div>` : ''}
            ${p.remark ? `<div class="k">${t('备注')}</div><div class="small">${esc(p.remark)}</div>` : ''}
          </div>
          <div>
            <div class="muted small" style="margin-bottom:6px">${t('模型（{n}）', { n: p.models.length })}</div>
            ${p.models.length ? `<div class="chips">${p.models.map((x) => `<span class="chip"><span class="pfx">${esc(p.prefix)}/</span>${esc(x)}</span>`).join('')}</div>` : `<div class="muted small">${t('还没有模型，点“同步模型”从上游拉取，或在编辑里手动添加')}</div>`}
          </div>
        </div>
      </div>`;
    }).join('') : `<div class="card"><div class="empty">${t('还没有供应商。点右上角“添加供应商”，选好预设后会自动预填地址')}</div></div>`}`;
  const find = (id) => providers.find((p) => p.id == id);
  $('#add-provider').onclick = () => providerForm(null, models, providers);
  $$('[data-edit]').forEach((b) => b.onclick = () => providerForm(find(b.dataset.edit), models, providers));
  $$('[data-test]').forEach((b) => b.onclick = () => providerTest(find(b.dataset.test)));
  $$('[data-sync]').forEach((b) => b.onclick = async () => {
    b.disabled = true; b.textContent = t('同步中…');
    try {
      const r = await api('POST', `/providers/${b.dataset.sync}/sync-models`);
      toast(r.added.length ? t('新增 {n} 个模型：{list}', { n: r.added.length, list: r.added.slice(0, 5).join(t('、')) + (r.added.length > 5 ? '…' : '') }) : t('没有新模型'), 'ok');
      route();
    } catch (e) {
      toast(t('拉取失败：{msg}（部分套餐不开放模型列表接口，可在编辑里手动添加）', { msg: e.message }), 'err');
      b.disabled = false; b.textContent = t('同步模型');
    }
  });
  $$('[data-del]').forEach((b) => b.onclick = async () => {
    const p = find(b.dataset.del);
    const used = models.filter((m) => usesProvider(m, p.prefix));
    const msg = used.length
      ? t('删除套餐 {prefix}？（{models} 的调度顺序里用到了它，删除后这些条目会被跳过）', { prefix: p.prefix, models: used.map((m) => m.name).join(t('、')) })
      : t('删除套餐 {prefix}？', { prefix: p.prefix });
    if (!(await confirmBox(msg))) return;
    await api('DELETE', '/providers/' + p.id);
    toast(t('已删除'), 'ok');
    route();
  });
}

// chip editor for a provider's model list
function modelChips(root, state) {
  const box = $('#pf-models', root);
  const prefix = $('#pf-prefix', root).textContent.trim() || t('前缀');
  box.innerHTML = state.models.length
    ? state.models.map((x, i) => `<span class="chip ${state.fresh.has(x) ? 'new' : ''}"><span class="pfx">${esc(prefix)}/</span>${esc(x)}<button type="button" data-rm="${i}" title="${esc(t('移除'))}">×</button></span>`).join('')
    : `<span class="muted small">${t('还没有模型')}</span>`;
  $('#pf-models-count', root).textContent = state.models.length;
  $$('[data-rm]', box).forEach((b) => b.onclick = () => { state.models.splice(Number(b.dataset.rm), 1); modelChips(root, state); });
}

function addModels(state, text) {
  let n = 0;
  for (const x of text.split(/[\s,，;；]+/)) {
    const v = x.trim();
    if (v && !state.models.includes(v)) { state.models.push(v); n++; }
  }
  return n;
}

const UA_LABEL = { passthrough: t('透传客户端'), platform: t('平台标识'), override: t('固定 UA') };

// Prefix rules (mirrors store.DerivePrefix): preset prefix, else the API
// host (api.deepseek.com -> deepseek), else the name, else "provider";
// a numeric suffix keeps it unique (kimi, kimi-2 ...).
const GENERIC_HOST_LABELS = new Set(['api', 'www', 'open', 'platform', 'coding', 'gateway', 'localhost', 'cn', 'com', 'ai', 'io']);
function sanitizePrefix(s) {
  return String(s || '').toLowerCase().trim().replace(/[^a-z0-9._-]+/g, '-').replace(/^[-._]+|[-._]+$/g, '');
}
function derivePrefix(vendor, openaiURL, anthropicURL, name) {
  const ps = presetById(vendor);
  if (ps) return ps.prefix;
  for (const raw of [openaiURL, anthropicURL]) {
    let host = '';
    try { host = new URL(raw).hostname.toLowerCase(); } catch (e) { continue; }
    if (!host || /^[\d.]+$/.test(host) || host.includes(':')) continue;
    let labels = host.split('.');
    if (labels.length > 1) labels = labels.slice(0, -1);
    for (const l of labels) {
      if (GENERIC_HOST_LABELS.has(l)) continue;
      const s = sanitizePrefix(l);
      if (s) return s;
    }
  }
  return sanitizePrefix(name) || 'provider';
}

// pick a prefix that is not taken yet: kimi, kimi-2, kimi-3 ...
function freePrefix(base, providers, selfId) {
  const taken = new Set(providers.filter((x) => x.id !== selfId).map((x) => x.prefix));
  if (!taken.has(base)) return base;
  for (let i = 2; ; i++) if (!taken.has(`${base}-${i}`)) return `${base}-${i}`;
}

function presetGrid(selected) {
  const cats = [['all', t('全部')], ...PRESET_CATEGORIES.map(([k, v]) => [k, t(v)])];
  return `
    <div class="toolbar" style="margin-bottom:8px">
      <div class="seg" id="pp-cat">${cats.map(([k, v]) => `<button type="button" data-cat="${k}" class="${k === 'all' ? 'on' : ''}">${esc(v)}</button>`).join('')}</div>
      <input type="text" id="pp-search" placeholder="${esc(t('搜索供应商'))}" style="flex:1;min-width:140px">
    </div>
    <div class="preset-grid" id="pp-grid">
      <button type="button" class="preset-card ${!selected ? 'on' : ''}" data-preset="">
        <span class="pi custom" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg></span>
        <span class="pt"><span class="pn">${t('自定义')}</span><span class="pc">${t('手动填写地址')}</span></span>
      </button>
      ${PRESETS.map((ps) => `<button type="button" class="preset-card ${selected === ps.id ? 'on' : ''}" data-preset="${esc(ps.id)}" data-cat="${ps.category}" data-q="${esc((ps.name + ' ' + t(ps.name) + ' ' + ps.id + ' ' + (ps.keywords || '')).toLowerCase())}">
        ${providerIcon(ps, t(ps.name))}
        <span class="pt"><span class="pn">${esc(t(ps.name))}</span><span class="pc cat-${ps.category}">${esc(catLabel(ps.category))}</span></span>
      </button>`).join('')}
    </div>`;
}

function presetInfoHTML(ps) {
  if (!ps) return `<span class="muted">${t('自定义供应商：手动填写地址、协议和模型。')}</span>`;
  const links = [
    ps.docs ? `<a href="${esc(ps.docs)}" target="_blank" rel="noopener">${t('官方文档 ↗')}</a>` : '',
    ps.keyUrl ? `<a href="${esc(ps.keyUrl)}" target="_blank" rel="noopener">${t('获取 API Key ↗')}</a>` : '',
  ].filter(Boolean).join(' · ');
  return `<b>${esc(t(ps.name))}</b> <span class="badge">${esc(catLabel(ps.category))}</span> ${links}
    ${ps.note ? `<div style="margin-top:4px">${esc(t(ps.note))}</div>` : ''}
    ${ps.ua && ps.ua.note ? `<div style="margin-top:4px"><b>${t('User-Agent：')}</b>${esc(t(ps.ua.note))}</div>` : ''}
    ${ps.verified === false ? `<div class="small" style="margin-top:4px">${t('⚠ 该预设地址未能从官方文档完全确认，保存前请点“测试”验证。')}</div>` : ''}`;
}

function providerForm(p, models, providers) {
  const isNew = !p;
  p = p || { prefix: '', name: '', vendor: '', openai_base_url: '', anthropic_base_url: '', api_key: '', headers: {}, model_protocols: {}, models: [], ua_mode: 'passthrough', user_agent: '', timeout_seconds: 300, prices: {}, currency: 'CNY', body_rules: [], max_concurrency: 0, first_token_timeout_seconds: 0, health_check_seconds: 0, health_check_url: '', quotas: [], enabled: true, remark: '' };
  const state = {
    vendor: guessVendor(p),
    models: [...(p.models || [])],
    fresh: new Set(),
    compat: isNew ? 'both' : compatOf(p),
    ua: p.ua_mode || 'passthrough',
  };
  const cur = presetById(state.vendor);
  openModal({
    title: isNew ? t('添加供应商') : t('编辑供应商 {prefix}', { prefix: p.prefix }),
    wide: true,
    body: `<div class="form">
      <div class="field">
        <label>${t('选择供应商')}</label>
        ${isNew ? '' : `<div class="toolbar" id="pp-current"><span>${t('当前：{name}', { name: `<b>${esc(cur ? t(cur.name) : t('自定义'))}</b>` })}</span><button type="button" class="btn sm" id="pp-toggle">${t('更换预设')}</button></div>`}
        <div id="pp-wrap" style="${isNew ? '' : 'display:none'}">${presetGrid(state.vendor)}</div>
        <div class="hint-box small" id="pp-info" style="margin-top:8px">${presetInfoHTML(cur)}</div>
      </div>
      <div class="row2">
        <div class="field"><label>${t('名称')}</label><input type="text" id="pf-name" value="${esc(p.name)}" placeholder="${esc(t('如 Kimi Code'))}"></div>
        <div class="field"><label>${t('前缀（自动生成）')}</label><div class="prefix-box"><code id="pf-prefix">${esc(p.prefix)}</code></div><div class="help">${isNew ? t('按预设或地址域名自动生成，重名时加数字后缀；模型在映射里显示为 前缀/模型名') : t('创建后固定不变；模型在映射里显示为 前缀/模型名')}</div></div>
      </div>
      <div class="field"><label>${t('兼容方案')}</label>
        <div class="seg" id="pf-compat">${['openai', 'anthropic', 'both'].map((c) => `<button type="button" data-c="${c}">${COMPAT_LABEL[c]}</button>`).join('')}</div>
        <div class="help">${t('客户端用哪种协议请求就优先走同协议地址，没有则自动转换')}</div>
      </div>
      <div class="field" data-for="openai"><label>${t('OpenAI 兼容地址')}</label><input type="text" id="pf-openai" value="${esc(p.openai_base_url)}" placeholder="${esc(t('https://xxx/v1（自动拼接 /chat/completions）'))}"></div>
      <div class="field" data-for="anthropic"><label>${t('Anthropic 兼容地址')}</label><input type="text" id="pf-anthropic" value="${esc(p.anthropic_base_url)}" placeholder="${esc(t('即 ANTHROPIC_BASE_URL（自动拼接 /v1/messages）'))}"></div>
      <div class="field"><label>API Key ${isNew ? '*' : ''} <span id="pf-keylink"></span></label><input type="password" id="pf-key" placeholder="${isNew ? '' : esc(t('留空表示不修改（当前 {key}）', { key: p.api_key }))}" autocomplete="new-password"></div>
      <div class="field">
        <label>${t('模型列表（{n}）', { n: '<span id="pf-models-count"></span>' })}</label>
        <div class="chips" id="pf-models"></div>
        <div class="toolbar" style="margin-top:8px">
          <input type="text" id="pf-model-input" placeholder="${esc(t('手动输入模型名，回车添加（可一次粘贴多个）'))}" style="flex:1">
          <button type="button" class="btn" id="pf-model-add">${t('添加')}</button>
          <button type="button" class="btn" id="pf-fetch">${t('从上游拉取')}</button>
          <button type="button" class="btn danger" id="pf-clear">${t('清空')}</button>
        </div>
        <div class="help" id="pf-fetch-msg">${t('填好地址和 Key 后会自动尝试拉取；拉不到（很多 coding plan 不开放 /models）就手动添加')}</div>
      </div>
      <div class="field"><label>User-Agent</label>
        <div class="toolbar">
          <div class="seg" id="pf-ua">${['passthrough', 'platform', 'override'].map((c) => `<button type="button" data-u="${c}">${UA_LABEL[c]}</button>`).join('')}</div>
          <input type="text" id="pf-ua-value" value="${esc(p.user_agent)}" style="flex:1;min-width:200px">
        </div>
        <div class="help" id="pf-ua-help"></div>
      </div>
      <details>
        <summary class="small muted">${t('高级设置')}</summary>
        <div class="form" style="margin-top:10px">
          <div class="row2">
            <div class="field"><label>${t('超时（秒）')}</label><input type="number" id="pf-timeout" value="${p.timeout_seconds}" min="5"><div class="help">${t('非流式为整个请求；流式为首包和事件间隔')}</div></div>
            <div class="field"><label>${t('备注')}</label><input type="text" id="pf-remark" value="${esc(p.remark)}"></div>
          </div>
          <div class="row2">
            <div class="field"><label>${t('模型协议规则')}</label><textarea id="pf-protos" placeholder="minimax-* = anthropic&#10;glm-* = openai&#10;gpt-* = responses">${esc(toLines(p.model_protocols, ' ='))}</textarea><div class="help">${t('每行 <code>模型(可用*) = openai|anthropic|responses</code>，某些模型只在一种端点提供时使用（<code>responses</code> 指 OpenAI 地址下的 /responses）')}</div></div>
            <div class="field"><label>${t('自定义请求头')}</label><textarea id="pf-headers" placeholder="X-Custom: value&#10;x-opencode-session: {{$session}}">${esc(toLines(p.headers, ':'))}</textarea>
              <div class="help">${t('每行 <code>Header: 值</code>，覆盖调用方的同名头；值留空表示删除该头。值里可以写：')}
                ${t('<code>{{header.X-Foo}}</code> 取调用方的头，<b>必传</b>（首选上游缺了直接报 400，候补上缺了就不发）；')}
                ${t('<code>{{header.X-Foo?}}</code> 可选；<code>{{header.X-Foo ?? $conversation}}</code> 没传就用平台生成的；')}
                ${t('内置变量 <code>$session</code>（同一会话稳定的 ses_…，按调用方的 X-Session-Id）、<code>$conversation</code>（按第一条用户消息）、<code>$uuid</code>（每次请求新的）、<code>$requestId</code>、<code>$timestamp</code>、<code>$keyName</code>、<code>$keyId</code>、<code>$model</code>')}</div></div>
          </div>
          <label class="check"><input type="checkbox" id="pf-responses" ${p.responses_api ? 'checked' : ''}> ${t('OpenAI 地址也支持 Responses API（/responses）')} <span class="muted small">${t('（勾选后，Codex 等 Responses 客户端的请求原样转发；不勾选就转换成 Chat Completions 发送。多数 OpenAI 兼容厂商不支持，别乱勾）')}</span></label>
          <label class="check"><input type="checkbox" id="pf-pass-headers" ${p.drop_client_headers ? '' : 'checked'}> ${t('透传调用方的请求头')} <span class="muted small">${t('（不会转发 Authorization、x-api-key、Cookie、Accept-Encoding、X-Forwarded-*、Origin 等凭证、逐跳和隐私相关的头）')}</span></label>
          <div class="field"><label>${t('自建模型（vLLM、SGLang、Ollama 等）')}</label>
            <div class="row3">
              <div><input type="number" id="pf-maxc" min="0" value="${p.max_concurrency || ''}" placeholder="${esc(t('最大并发：不限'))}"><div class="help">${t('同时在途的请求数上限，满了就溢出到下一个候补；都满时排队（见设置）')}</div></div>
              <div><input type="number" id="pf-ftt" min="0" value="${p.first_token_timeout_seconds || ''}" placeholder="${esc(t('首包超时（秒）：同超时'))}"><div class="help">${t('流式请求等第一个事件的时间，超过就切换；之后按上面的超时算')}</div></div>
              <div><input type="number" id="pf-hc" min="0" value="${p.health_check_seconds || ''}" placeholder="${esc(t('健康检查间隔（秒）：不检查'))}"><div class="help">${t('连续 2 次失败就移出调度，恢复后自动加回')}</div></div>
            </div>
            <input type="text" id="pf-hc-url" value="${esc(p.health_check_url || '')}" placeholder="${esc(t('健康检查地址（可选，默认 GET 模型列表接口，如 http://gpu-1:8000/v1/models）'))}" style="margin-top:8px">
            <div class="help">${t('检查请求会带上这个供应商的 Key 和自定义请求头，地址请填它自己的服务')}</div>
          </div>
          <div class="field"><label>${t('套餐配额')}</label>
            <textarea id="pf-quotas" placeholder="5h = 600&#10;week = 0 / 50000000">${esc(toQuotaLines(p.quotas))}</textarea>
            <div class="help">${t('每行 <code>周期 = 请求数 / tokens</code>，0 或省略表示不限。周期：<code>5h</code>（最近 5 小时，滚动）、<code>day</code>（今天）、<code>week</code>（本周，从周一算）、<code>month</code>（本月）。达到上限后，这个供应商在所有模型的调度顺序里排到其他候补后面，窗口有余量后自动回到原位；它不会被禁用，其他候补都失败时仍会兜底。计数来自请求日志，可能比上游自己的统计略有出入。')}</div></div>
          <div class="field"><label>${t('请求参数规则')}</label>
            <textarea id="pf-rules" placeholder='qwen3-* [nonstream] = {"enable_thinking": false}'>${esc(toRuleLines(p.body_rules))}</textarea>
            <div class="help">${t('每行 <code>模型(可用*) [条件] = JSON</code>，把 JSON 合并进发给上游的请求体（协议转换之后），值为 <code>null</code> 表示删除该字段。条件可选：<code>stream</code> / <code>nonstream</code>，<code>openai</code> / <code>anthropic</code> / <code>responses</code> / <code>embeddings</code> / <code>rerank</code>，多个用逗号分隔')}</div></div>
          <div class="field"><label>${t('单价（每百万 tokens，用于成本核算）')}
              <select id="pf-currency" style="margin-left:8px">${['CNY', 'USD'].map((c) => `<option value="${c}" ${(p.currency || 'CNY') === c ? 'selected' : ''}>${currencyLabel(c)}</option>`).join('')}</select></label>
            <textarea id="pf-prices" placeholder="glm-5.3 = 4 / 0.8 / 16&#10;deepseek-* = 2 / 8">${esc(toPriceLines(p.prices))}</textarea>
            <div class="help">${t('每行 <code>模型(可用*) = 输入 / 缓存命中 / 输出</code>，缓存价可省略（按输入价计）。没配单价的模型不计费用；包月套餐可以写 <code>* = 0 / 0</code>，表示不另外收费，概览里就不会算作“未配单价”。OpenRouter 会直接使用上游返回的实际费用，不需要配置。')}</div></div>
        </div>
      </details>
      <label class="check"><input type="checkbox" id="pf-enabled" ${p.enabled ? 'checked' : ''}> ${t('启用')}</label>
    </div>`,
    foot: `<button class="btn" data-close>${t('取消')}</button><button class="btn primary" id="pf-save">${t('保存')}</button>`,
    onMount: (m) => {
      const setCompat = (c) => {
        state.compat = c;
        $$('#pf-compat button', m).forEach((b) => b.classList.toggle('on', b.dataset.c === c));
        $('[data-for=openai]', m).style.display = c === 'anthropic' ? 'none' : '';
        $('[data-for=anthropic]', m).style.display = c === 'openai' ? 'none' : '';
      };
      const setUA = (u) => {
        state.ua = u;
        $$('#pf-ua button', m).forEach((b) => b.classList.toggle('on', b.dataset.u === u));
        const input = $('#pf-ua-value', m);
        input.style.display = u === 'platform' ? 'none' : '';
        input.placeholder = u === 'override' ? t('必填，所有请求都使用这个 UA') : t('可选，客户端没带 UA 时使用；不填则用平台标识 {ua}', { ua: PLATFORM_UA });
        const ps = presetById(state.vendor);
        $('#pf-ua-help', m).innerHTML = (u === 'override'
          ? t('所有发往该供应商的请求都改用这里填写的 User-Agent。')
          : u === 'platform'
            ? t('所有请求都使用本网关的标识 <code>{ua}</code>，适合自建模型、中转平台等需要识别来源的上游。限制客户端类型的套餐（如 Kimi Code）不能用这个。', { ua: esc(PLATFORM_UA) })
            : t('默认把调用方（Claude Code、Cursor 等）的真实 User-Agent 原样转发。限制客户端类型的套餐（如 Kimi Code）需要保持这个选项。'))
          + (ps && ps.ua && ps.ua.note ? ` <b>${esc(t(ps.ua.note))}</b>` : '');
      };
      const setKeyLink = () => {
        const ps = presetById(state.vendor);
        $('#pf-keylink', m).innerHTML = ps && ps.keyUrl ? `<a href="${esc(ps.keyUrl)}" target="_blank" rel="noopener" class="small">${t('获取 ↗')}</a>` : '';
      };
      $$('#pf-compat button', m).forEach((b) => b.onclick = () => setCompat(b.dataset.c));
      $$('#pf-ua button', m).forEach((b) => b.onclick = () => setUA(b.dataset.u));
      setCompat(state.compat);
      setUA(state.ua);
      setKeyLink();
      modelChips(m, state);
      // new providers: prefix follows the preset / URL / name as they change
      const refreshPrefix = () => {
        if (!isNew) return;
        const base = derivePrefix(state.vendor, $('#pf-openai', m).value.trim(), $('#pf-anthropic', m).value.trim(), $('#pf-name', m).value);
        $('#pf-prefix', m).textContent = freePrefix(base, providers, 0);
        modelChips(m, state);
      };
      ['#pf-openai', '#pf-anthropic', '#pf-name'].forEach((sel) => $(sel, m).addEventListener('input', refreshPrefix));
      refreshPrefix();

      const applyPreset = (id) => {
        state.vendor = id;
        const ps = presetById(id);
        $$('#pp-grid .preset-card', m).forEach((c) => c.classList.toggle('on', c.dataset.preset === id));
        $('#pp-info', m).innerHTML = presetInfoHTML(ps);
        setKeyLink();
        if (!ps) { setUA(state.ua); return; }
        if (isNew || !$('#pf-name', m).value) $('#pf-name', m).value = t(ps.name);
        refreshPrefix();
        $('#pf-openai', m).value = ps.openai || '';
        $('#pf-anthropic', m).value = ps.anthropic || '';
        $('#pf-protos', m).value = toLines(ps.protocols || {}, ' =');
        $('#pf-responses', m).checked = !!ps.responses;
        $('#pf-headers', m).value = toLines(ps.headers || {}, ':');
        if (isNew) $('#pf-currency', m).value = ps.currency || 'CNY';
        $('#pf-rules', m).value = toRuleLines(ps.rules || []);
        setCompat(ps.openai && ps.anthropic ? 'both' : ps.anthropic ? 'anthropic' : 'openai');
        if (ps.ua && ps.ua.mode) {
          $('#pf-ua-value', m).value = ps.ua.value || '';
          setUA(ps.ua.mode);
        } else {
          setUA(state.ua);
        }
        if (isNew) { state.models = []; state.fresh.clear(); }
        if (!state.models.length) addModels(state, (ps.models || []).join(' '));
        modelChips(m, state);
        if (!isNew) {
          $('#pp-wrap', m).style.display = 'none';
          $('#pp-current b', m).textContent = t(ps.name);
        }
      };
      const filterGrid = () => {
        const cat = ($('#pp-cat button.on', m) || {}).dataset?.cat || 'all';
        const q = $('#pp-search', m).value.trim().toLowerCase();
        $$('#pp-grid .preset-card', m).forEach((c) => {
          if (!c.dataset.preset) { c.style.display = q ? 'none' : ''; return; }
          const ok = (cat === 'all' || c.dataset.cat === cat) && (!q || c.dataset.q.includes(q));
          c.style.display = ok ? '' : 'none';
        });
      };
      $$('#pp-cat button', m).forEach((b) => b.onclick = () => {
        $$('#pp-cat button', m).forEach((x) => x.classList.toggle('on', x === b));
        filterGrid();
      });
      $('#pp-search', m).oninput = filterGrid;
      $$('#pp-grid .preset-card', m).forEach((c) => c.onclick = () => applyPreset(c.dataset.preset));
      if (!isNew) $('#pp-toggle', m).onclick = () => {
        const w = $('#pp-wrap', m);
        w.style.display = w.style.display === 'none' ? '' : 'none';
      };

      const addFromInput = () => {
        const n = addModels(state, $('#pf-model-input', m).value);
        $('#pf-model-input', m).value = '';
        if (n) modelChips(m, state);
      };
      $('#pf-model-add', m).onclick = addFromInput;
      $('#pf-model-input', m).addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); addFromInput(); } });
      $('#pf-clear', m).onclick = () => { state.models = []; state.fresh.clear(); modelChips(m, state); };

      const formBody = () => ({
        id: p.id || 0,
        openai_base_url: state.compat === 'anthropic' ? '' : $('#pf-openai', m).value.trim(),
        anthropic_base_url: state.compat === 'openai' ? '' : $('#pf-anthropic', m).value.trim(),
        api_key: $('#pf-key', m).value.trim(),
        headers: parseLines($('#pf-headers', m).value, ':'),
        drop_client_headers: !$('#pf-pass-headers', m).checked,
        responses_api: $('#pf-responses', m).checked,
        ua_mode: state.ua,
        user_agent: $('#pf-ua-value', m).value.trim(),
      });
      let fetching = false;
      const doFetch = async (auto) => {
        const body = formBody();
        if (fetching || (!body.openai_base_url && !body.anthropic_base_url) || (!body.api_key && isNew)) {
          if (!auto) toast(t('请先填写地址和 API Key'), 'err');
          return;
        }
        fetching = true;
        const msg = $('#pf-fetch-msg', m);
        msg.textContent = t('正在从上游拉取模型列表…');
        msg.className = 'help';
        try {
          const ids = await api('POST', '/providers/fetch-models', body);
          state.fresh = new Set(ids.filter((x) => !state.models.includes(x)));
          addModels(state, ids.join(' '));
          modelChips(m, state);
          msg.textContent = t('拉取到 {n} 个模型，新增 {added} 个（绿色标出）。不需要的可以点 × 移除。', { n: ids.length, added: state.fresh.size });
          msg.className = 'help ok-text';
        } catch (e) {
          msg.textContent = t('拉取失败：{msg}。可以手动添加模型名。', { msg: e.message });
          msg.className = 'help err-text';
        }
        fetching = false;
      };
      $('#pf-fetch', m).onclick = () => doFetch(false);
      // auto-fetch once the key is filled in
      $('#pf-key', m).addEventListener('change', () => doFetch(true));

      $('#pf-save', m).onclick = async () => {
        addFromInput();
        let prices, bodyRules, quotas;
        try {
          prices = parsePrices($('#pf-prices', m).value);
          bodyRules = parseRules($('#pf-rules', m).value);
          quotas = parseQuotas($('#pf-quotas', m).value);
        } catch (e) { return toast(e.message, 'err'); }
        const body = {
          ...formBody(),
          vendor: state.vendor,
          prefix: isNew ? $('#pf-prefix', m).textContent.trim() : p.prefix,
          name: $('#pf-name', m).value.trim(),
          timeout_seconds: Number($('#pf-timeout', m).value) || 300,
          remark: $('#pf-remark', m).value.trim(),
          model_protocols: parseLines($('#pf-protos', m).value, '='),
          models: state.models,
          prices,
          body_rules: bodyRules,
          quotas,
          max_concurrency: Math.floor(Number($('#pf-maxc', m).value) || 0),
          first_token_timeout_seconds: Math.floor(Number($('#pf-ftt', m).value) || 0),
          health_check_seconds: Math.floor(Number($('#pf-hc', m).value) || 0),
          health_check_url: $('#pf-hc-url', m).value.trim(),
          currency: $('#pf-currency', m).value,
          enabled: $('#pf-enabled', m).checked,
        };
        // drop protocol rules pointing at an endpoint that is no longer configured
        for (const [k, v] of Object.entries(body.model_protocols)) {
          if ((v === 'openai' && !body.openai_base_url) || (v === 'anthropic' && !body.anthropic_base_url)) delete body.model_protocols[k];
        }
        if (!body.prefix) return toast(t('无法生成前缀，请先选择供应商或填写地址'), 'err');
        if (/\$\{/.test(body.openai_base_url + body.anthropic_base_url)) return toast(t('地址里还有 ${...} 占位符，请替换成你自己的值'), 'err');
        if (body.ua_mode === 'override' && !body.user_agent) return toast(t('固定 UA 模式需要填写 User-Agent'), 'err');
        const presetURL = (presetById(state.vendor) || {}).openai;
        if (body.openai_base_url && body.openai_base_url !== presetURL && !/\/v\d+$|\/chat\/completions$/.test(body.openai_base_url)
          && !(await confirmBox(t('OpenAI 兼容地址一般以版本号结尾（如 /v1、/v3、/paas/v4），网关会在后面拼接 /chat/completions。当前地址 {url} 可能会 404，确定保存？', { url: body.openai_base_url })))) return;
        if (isNew && !body.api_key && !(await confirmBox(t('没有填写 API Key，确定保存？')))) return;
        try {
          if (isNew) await api('POST', '/providers', body);
          else await api('PUT', '/providers/' + p.id, body);
          closeModal();
          toast(t('已保存'), 'ok');
          route();
        } catch (e) { toast(e.message, 'err'); }
      };
    },
  });
}

function attemptResultHTML(a) {
  return `<td class="small ${a.error ? 'err-text' : 'ok-text'}">${a.error ? esc(a.error) : t('成功')}${a.cooling ? ` <span class="badge warn">${t('冷却中兜底')}</span>` : ''}${a.over_quota ? ` <span class="badge warn">${t('配额用完兜底')}</span>` : ''}${a.headers ? `<div class="muted mono">${Object.entries(a.headers).map(([k, v]) => `${esc(k)}: ${esc(v)}`).join('<br>')}</div>` : ''}</td>`;
}
const retryBadge = (a) => (a.retry ? ` <span class="badge">${t('重试 {n}', { n: a.retry })}</span>` : '');

function testResultHTML(r) {
  const atts = (r.attempts || []).map((a) => `<tr><td class="mono small">${esc(a.target)}${retryBadge(a)}</td><td>${esc(a.protocol)}</td><td>${a.http_status || '-'}</td><td>${fmtMs(a.latency_ms)}</td>${attemptResultHTML(a)}</tr>`).join('');
  return `<div class="form">
    <div class="kv">
      <div class="k">${t('结果')}</div><div>${r.ok ? `<span class="badge ok">${t('成功')}</span>` : `<span class="badge err">${t('失败')}</span>`} ${t('HTTP {status}，耗时 {ms}', { status: r.http_status, ms: fmtMs(r.latency_ms) })}</div>
      ${r.target ? `<div class="k">${t('实际上游')}</div><div class="mono">${esc(r.target)}</div>` : ''}
      ${r.reply ? `<div class="k">${t('回复')}</div><div>${esc(r.reply)}</div>` : ''}
    </div>
    ${atts ? `<div class="table-wrap"><table><tr><th>${t('尝试')}</th><th>${t('协议')}</th><th>${t('状态')}</th><th>${t('耗时')}</th><th>${t('结果')}</th></tr>${atts}</table></div>` : ''}
    ${(r.attempts || []).some((a) => a.http_status === 403) ? `<div class="hint-box small">${t('返回 403：部分编码套餐只允许特定客户端（按 User-Agent 识别），后台测试的 UA 是 ai-route-admin-test，可能被拒绝。请用实际客户端（如 Claude Code）经网关调用一次确认。')}</div>` : ''}
    <details><summary class="small muted">${t('原始响应')}</summary><pre class="box">${esc(r.raw)}</pre></details>
  </div>`;
}

function providerTest(p) {
  openModal({
    title: t('测试套餐 {prefix}', { prefix: p.prefix }),
    wide: true,
    body: `<div class="form">
      <div class="row2">
        <div class="field"><label>${t('模型')}</label><input type="text" id="pt-model" list="pt-models" value="${esc(p.models[0] || '')}"><datalist id="pt-models">${p.models.map((x) => `<option value="${esc(x)}">`).join('')}</datalist></div>
        <div class="field"><label>${t('协议')}</label><select id="pt-proto"><option value="">${t('自动')}</option>${p.openai_base_url ? '<option value="openai">OpenAI Chat</option><option value="responses">OpenAI Responses</option>' : ''}${p.anthropic_base_url ? '<option value="anthropic">Anthropic</option>' : ''}</select></div>
      </div>
      <label class="check"><input type="checkbox" id="pt-stream"> ${t('流式')}</label>
      <div class="muted small">${t('直连该套餐发一句简短的测试消息，不重试、不经过熔断，也不记日志。')}</div>
      <div id="pt-result"></div>
    </div>`,
    foot: `<button class="btn" data-close>${t('关闭')}</button><button class="btn primary" id="pt-go">${t('发送测试')}</button>`,
    onMount: (m) => {
      $('#pt-go', m).onclick = async () => {
        const btn = $('#pt-go', m);
        btn.disabled = true; btn.textContent = t('测试中…');
        $('#pt-result', m).innerHTML = '';
        try {
          const r = await api('POST', `/providers/${p.id}/test`, { model: $('#pt-model', m).value.trim(), protocol: $('#pt-proto', m).value, stream: $('#pt-stream', m).checked });
          $('#pt-result', m).innerHTML = testResultHTML(r);
        } catch (e) { $('#pt-result', m).innerHTML = `<div class="err-text">${esc(e.message)}</div>`; }
        btn.disabled = false; btn.textContent = t('发送测试');
      };
    },
  });
}

// ---------------------------------------------------------------- models
// tag labels and descriptions come from TAG_GROUPS (presets.js) in Chinese
const tagLabel = (tag) => t(tagInfo(tag).label);
const tagDesc = (tag) => t(tagInfo(tag).desc);

function tagBadges(tags) {
  return sortTags(tags || []).map((tag) => {
    const i = tagInfo(tag);
    return `<span class="tag tg-${i.group}" title="${esc(tagDesc(tag))}">${esc(tagLabel(tag))}</span>`;
  }).join('');
}

let modelTagFilter = '';
async function pageModels() {
  const [models, providers, status] = await Promise.all([api('GET', '/models'), api('GET', '/providers'), api('GET', '/status')]);
  const idx = healthIndex(status);
  const usedTags = sortTags([...new Set(models.flatMap((m) => m.tags))]);
  if (modelTagFilter && !usedTags.includes(modelTagFilter)) modelTagFilter = '';
  const shown = modelTagFilter ? models.filter((m) => m.tags.includes(modelTagFilter)) : models;
  $('#page').innerHTML = `
    ${head(t('模型映射'), t('给模型起一个对外名字，选好要映射的上游模型并排好顺序；前一个不可用时（重试后仍失败或在冷却中）自动切到下一个'), `<button class="btn primary" id="add-model">${t('+ 添加模型')}</button>`)}
    ${usedTags.length ? `<div class="toolbar tag-filter">
      <span class="muted small">${t('按能力筛选：')}</span>
      <button type="button" class="tag tg-all ${modelTagFilter ? '' : 'on'}" data-tf="">${t('全部')}</button>
      ${usedTags.map((tag) => { const i = tagInfo(tag); return `<button type="button" class="tag tg-${i.group} ${modelTagFilter === tag ? 'on' : ''}" data-tf="${esc(tag)}" title="${esc(tagDesc(tag))}">${esc(tagLabel(tag))}</button>`; }).join('')}
    </div>` : ''}
    <div class="card">
      ${shown.length ? `<div class="table-wrap"><table>
        <tr><th>${t('对外模型名')}</th><th>${t('调度顺序')}</th><th>${t('状态')}</th><th></th></tr>
        ${shown.map((m) => `<tr>
          <td><b class="copy" data-copy="${esc(m.name)}" title="${esc(t('点击复制'))}">${esc(m.name)}</b>
            ${m.tags.length ? `<div class="tags">${tagBadges(m.tags)}</div>` : ''}
            ${m.description ? `<div class="small muted">${esc(m.description)}</div>` : ''}
            ${m.aliases.length ? `<div class="small muted">${t('别名：{list}', { list: m.aliases.map(esc).join(t('，')) })}</div>` : ''}</td>
          <td>${chainHTML(m.targets, idx, providers)}</td>
          <td>${m.enabled ? `<span class="badge ok">${t('启用')}</span>` : `<span class="badge">${t('停用')}</span>`}</td>
          <td><div class="btns">
            <button class="btn sm" data-test="${m.id}">${t('测试')}</button>
            <button class="btn sm" data-edit="${m.id}">${t('编辑')}</button>
            <button class="btn sm" data-dup="${m.id}">${t('复制')}</button>
            <button class="btn sm danger" data-del="${m.id}">${t('删除')}</button>
          </div></td></tr>`).join('')}
      </table></div>` : `<div class="empty">${providers.length ? t('还没有模型映射，点右上角添加') : t('请先到“供应商”添加供应商，再来建模型映射')}</div>`}
    </div>
    <div class="hint-box">
      ${t('绿点：正常 · 黄点：冷却中（排到最后兜底）· 红点：前缀不存在或套餐已停用（跳过）。')}<br>
      ${t('别名支持通配符 <code>*</code>，例如设置别名 <code>claude-*haiku*</code>，Claude Code 的后台小模型请求就会落到这个映射上。')}
    </div>`;
  $('#add-model').onclick = () => modelForm(null, providers, models, idx);
  $$('[data-tf]').forEach((b) => b.onclick = () => { modelTagFilter = b.dataset.tf; route(); });
  $$('[data-copy]').forEach((el) => el.onclick = () => copyText(el.dataset.copy));
  $$('[data-edit]').forEach((b) => b.onclick = () => modelForm(models.find((m) => m.id == b.dataset.edit), providers, models, idx));
  $$('[data-dup]').forEach((b) => b.onclick = () => {
    const src = models.find((m) => m.id == b.dataset.dup);
    modelForm({ ...src, id: 0, name: src.name + '-copy', aliases: [] }, providers, models, idx, true);
  });
  $$('[data-test]').forEach((b) => b.onclick = () => modelTest(models.find((m) => m.id == b.dataset.test)));
  $$('[data-del]').forEach((b) => b.onclick = async () => {
    const m = models.find((x) => x.id == b.dataset.del);
    if (!(await confirmBox(t('删除模型映射 {name}？使用该模型名的客户端将无法调用。', { name: m.name })))) return;
    await api('DELETE', '/models/' + m.id);
    toast(t('已删除'), 'ok');
    route();
  });
}

function modelForm(m, providers, models, idx, asNew = false) {
  const isNew = !m || asNew;
  m = m || { name: '', aliases: [], targets: [], enabled: true, description: '' };
  // one row per target; tie = same priority as the row above (weighted group)
  const rows = m.targets.flatMap((e) => parseEntry(e).map((x, k) => ({ t: x.t, w: x.w, tie: k > 0 })));
  const rowIndex = (tg) => rows.findIndex((r) => r.t === tg);
  const toEntries = () => {
    const out = [];
    rows.forEach((r, i) => {
      const part = r.t + (r.w > 1 ? '*' + r.w : '');
      if (r.tie && i > 0) out[out.length - 1] += ' | ' + part; else out.push(part);
    });
    return out;
  };
  const tags = new Set(m.tags || []);
  let freshTags = new Set();

  const renderTags = (root) => {
    const custom = [...tags].filter((tag) => !TAG_INDEX[tag]);
    $('#mf-tags', root).innerHTML = TAG_GROUPS.map((g) => `
      <div class="tag-row"><span class="tag-group">${esc(t(g.label))}${g.single ? `<span class="muted">${t('（单选）')}</span>` : ''}</span>
        <div class="chips">${g.items.map(([id, label, desc]) => `<button type="button" class="tag tg-${g.id} ${tags.has(id) ? 'on' : ''} ${freshTags.has(id) ? 'fresh' : ''}" data-tag="${id}" title="${esc(t(desc))}">${esc(t(label))}</button>`).join('')}</div>
      </div>`).join('')
      + (custom.length ? `<div class="tag-row"><span class="tag-group">${t('自定义')}</span><div class="chips">${custom.map((tag) => `<button type="button" class="tag tg-custom on" data-tag="${esc(tag)}" title="${esc(t('点击移除'))}">${esc(tag)} ×</button>`).join('')}</div></div>` : '');
    $$('[data-tag]', root).forEach((b) => b.onclick = () => {
      const tag = b.dataset.tag;
      const info = tagInfo(tag);
      const group = TAG_GROUPS.find((g) => g.id === info.group);
      if (tags.has(tag)) tags.delete(tag);
      else {
        if (group && group.single) group.items.forEach(([id]) => tags.delete(id));
        tags.add(tag);
      }
      freshTags.delete(tag);
      renderTags(root);
    });
  };
  const all = providers.flatMap((p) => p.models.map((x) => `${p.prefix}/${x}`));

  const renderChain = (root) => {
    const box = $('#mf-chain', root);
    if (rows.length) rows[0].tie = false;
    let level = -1;
    box.innerHTML = rows.length ? rows.map((r, i) => {
      const h = targetHealth(r.t, idx, providers);
      if (!r.tie) level++;
      const inGroup = r.tie || (rows[i + 1] && rows[i + 1].tie);
      return `<div class="chain-row ${r.tie ? 'tied' : ''}">
        <span class="idx">${r.tie ? t('并列') : level === 0 ? t('首选') : t('候补 {n}', { n: level })}</span>
        <span class="target ${h.cls}" title="${esc(h.tip)}"><span class="s"></span>${esc(r.t)}</span>
        <span class="muted small">${h.cls === 'missing' ? esc(h.tip) : ''}${(() => {
          const req = requiredHeaders(providers.find((p) => p.prefix === r.t.split('/')[0]));
          if (!req.length) return '';
          return level === 0
            ? t('必传请求头：{list}，调用方没带会直接返回 400', { list: esc(req.join(t('、'))) })
            : t('必传请求头 {list} 只在首选生效，作为候补时缺了就不发', { list: esc(req.join(t('、'))) });
        })()}</span>
        <div class="btns">
          ${i > 0 ? `<label class="check small" title="${esc(t('和上一项同一优先级，按权重分流'))}"><input type="checkbox" data-act="tie" ${r.tie ? 'checked' : ''}> ${t('与上一项并列')}</label>` : ''}
          ${inGroup ? `<label class="small">${t('权重')} <input type="number" data-act="w" min="1" max="1000" value="${r.w}" style="width:64px"></label>` : ''}
          <button type="button" class="btn sm" data-act="up" ${i === 0 ? 'disabled' : ''} title="${esc(t('上移'))}">↑</button>
          <button type="button" class="btn sm" data-act="down" ${i === rows.length - 1 ? 'disabled' : ''} title="${esc(t('下移'))}">↓</button>
          <button type="button" class="btn sm danger" data-act="rm" title="${esc(t('移除'))}">✕</button>
        </div>
      </div>`;
    }).join('') : `<div class="muted small" style="padding:6px 0">${t('还没有选择模型，从下面点选或手动输入')}</div>`;
    $$('.chain-row', box).forEach((row, i) => {
      $$('[data-act]', row).forEach((b) => {
        const act = b.dataset.act;
        if (act === 'w') {
          b.onchange = () => { rows[i].w = Math.min(1000, Math.max(1, Math.floor(Number(b.value) || 1))); renderChain(root); };
          return;
        }
        if (act === 'tie') {
          b.onchange = () => { rows[i].tie = b.checked; renderChain(root); };
          return;
        }
        b.onclick = () => {
          if (act === 'rm') rows.splice(i, 1);
          if (act === 'up' && i > 0) [rows[i - 1], rows[i]] = [rows[i], rows[i - 1]];
          if (act === 'down' && i < rows.length - 1) [rows[i + 1], rows[i]] = [rows[i], rows[i + 1]];
          renderChain(root);
          renderPicker(root);
        };
      });
    });
  };

  const renderPicker = (root) => {
    const q = ($('#mf-filter', root).value || '').trim().toLowerCase();
    const groups = providers.map((p) => {
      const items = p.models.filter((x) => !q || `${p.prefix}/${x}`.toLowerCase().includes(q));
      if (!items.length) return '';
      return `<div class="pick-group">
        <div class="pick-head"><code>${esc(p.prefix)}</code> ${esc(p.name || '')}${p.enabled ? '' : ` <span class="badge">${t('停用')}</span>`}</div>
        <div class="chips">${items.map((x) => {
          const tg = `${p.prefix}/${x}`;
          const pos = rowIndex(tg);
          return `<button type="button" class="chip pick ${pos >= 0 ? 'on' : ''}" data-t="${esc(tg)}">${pos >= 0 ? `<span class="order">${pos + 1}</span>` : ''}${esc(x)}</button>`;
        }).join('')}</div>
      </div>`;
    }).join('');
    const empty = providers.filter((p) => !p.models.length).map((p) => p.prefix);
    $('#mf-picker', root).innerHTML = (groups || `<div class="muted small">${providers.some((p) => p.models.length) ? t('没有匹配的模型') : t('套餐里还没有模型')}</div>`)
      + (empty.length && !q ? `<div class="muted small">${t('{list} 还没有模型列表：可到“供应商”同步或添加，或在上方手动输入 <code>前缀/模型名</code>', { list: empty.map((x) => `<code>${esc(x)}</code>`).join(' ') })}</div>` : '');
    $$('[data-t]', root).forEach((b) => b.onclick = () => {
      const tg = b.dataset.t;
      const pos = rowIndex(tg);
      if (pos >= 0) rows.splice(pos, 1); else rows.push({ t: tg, w: 1, tie: false });
      renderChain(root);
      renderPicker(root);
    });
  };

  openModal({
    title: isNew ? t('添加模型映射') : t('编辑模型映射 {name}', { name: m.name }),
    wide: true,
    body: `<div class="form">
      <div class="row2">
        <div class="field"><label>${t('对外模型名 *')}</label><input type="text" id="mf-name" value="${esc(m.name)}" placeholder="${esc(t('如 dess、coder'))}"><div class="help">${t('客户端请求时填写的 model')}</div></div>
        <div class="field"><label>${t('说明')}</label><input type="text" id="mf-desc" value="${esc(m.description)}"></div>
      </div>
      <div class="field"><label>${t('别名（可选）')}</label><input type="text" id="mf-aliases" value="${esc(m.aliases.join(', '))}" placeholder="${esc(t('逗号分隔，支持 *，如 claude-sonnet-*'))}"><div class="help">${t('请求的 model 匹配别名时也走这个映射；精确名称优先于通配别名')}</div></div>
      <div class="field"><label>${t('能力标签')} <span class="muted small">${t('（让使用方一眼看出这个模型能做什么，也会出现在 /v1/models 里）')}</span></label>
        <div class="tag-picker" id="mf-tags"></div>
        <div class="toolbar" style="margin-top:8px">
          <input type="text" id="mf-tag-input" placeholder="${esc(t('自定义标签，回车添加'))}" style="flex:1;min-width:160px">
          <button type="button" class="btn" id="mf-suggest">${t('根据映射的模型推荐')}</button>
        </div>
        <div class="help" id="mf-suggest-msg"></div>
      </div>
      <div class="field"><label>${t('调度顺序 *（从上到下依次尝试）')}</label>
        <div id="mf-chain" class="chain-list"></div>
        <div class="help">${t('短暂错误（断连、5xx、上游过载）会先在同一个模型上重试，仍失败才切到下一个；额度用尽、Key 失效、超时等直接切换。重试次数在“设置”里调整。')}<br>${t('勾选“与上一项并列”把多个上游（比如同一家的多个 Key）放在同一优先级，按权重分流：同一个会话固定走同一个上游以保住提示词缓存，不同会话按权重分散；其中一个失败时先切到同级的其他上游。')}</div>
      </div>
      <div class="field"><label>${t('选择映射的模型（点击按顺序加入，再点一次移除）')}</label>
        <div class="toolbar" style="margin-bottom:8px">
          <input type="text" id="mf-filter" placeholder="${esc(t('搜索，或手动输入 前缀/模型名 后回车添加'))}" list="mf-all" style="flex:1">
          <datalist id="mf-all">${all.map((x) => `<option value="${esc(x)}">`).join('')}</datalist>
          <button type="button" class="btn" id="mf-add">${t('添加')}</button>
        </div>
        <div id="mf-picker" class="picker"></div>
      </div>
      <label class="check"><input type="checkbox" id="mf-enabled" ${m.enabled ? 'checked' : ''}> ${t('启用')}</label>
    </div>`,
    foot: `<button class="btn" data-close>${t('取消')}</button><button class="btn primary" id="mf-save">${t('保存')}</button>`,
    onMount: (root) => {
      renderChain(root);
      renderPicker(root);
      renderTags(root);
      $('#mf-tag-input', root).addEventListener('keydown', (e) => {
        if (e.key !== 'Enter') return;
        e.preventDefault();
        const v = e.target.value.trim();
        if (v) { tags.add(v); e.target.value = ''; renderTags(root); }
      });
      $('#mf-suggest', root).onclick = () => {
        const msg = $('#mf-suggest-msg', root);
        if (!rows.length) { msg.textContent = t('请先在下方选择映射的模型'); msg.className = 'help err-text'; return; }
        const s = suggestTags(rows.map((r) => r.t));
        freshTags = new Set(s.tags.filter((tag) => !tags.has(tag)));
        for (const tag of s.tags) {
          const group = TAG_GROUPS.find((g) => g.id === tagInfo(tag).group);
          if (group && group.single) group.items.forEach(([id]) => tags.delete(id));
          tags.add(tag);
        }
        renderTags(root);
        const partial = s.partial.map((p) => t('{tag}（仅 {targets}）', { tag: tagLabel(p.tag), targets: p.targets.join(t('、')) })).join(t('，'));
        msg.innerHTML = t('根据模型名推荐了 {n} 个新标签（虚线框标出），请确认后保存。推荐只取所有候补都具备的能力，上下文取最小值。', { n: freshTags.size })
          + (partial ? '<br>' + t('部分候补才有：{list}。切换到其他候补时这些能力可能缺失。', { list: esc(partial) }) : '');
        msg.className = 'help';
      };
      const addManual = () => {
        const v = $('#mf-filter', root).value.trim();
        if (!v) return;
        const i = v.indexOf('/');
        if (i <= 0 || i === v.length - 1) return toast(t('请输入 前缀/模型名，例如 kimi/k3'), 'err');
        if (!providers.some((p) => p.prefix === v.slice(0, i))) return toast(t('前缀 {prefix} 不存在，请先添加该套餐', { prefix: v.slice(0, i) }), 'err');
        if (rowIndex(v) < 0) rows.push({ t: v, w: 1, tie: false });
        $('#mf-filter', root).value = '';
        renderChain(root);
        renderPicker(root);
      };
      $('#mf-add', root).onclick = addManual;
      $('#mf-filter', root).addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); addManual(); } });
      $('#mf-filter', root).addEventListener('input', () => renderPicker(root));
      $('#mf-save', root).onclick = async () => {
        const body = {
          name: $('#mf-name', root).value.trim(),
          description: $('#mf-desc', root).value.trim(),
          aliases: $('#mf-aliases', root).value.split(/[,，\n]/).map((s) => s.trim()).filter(Boolean),
          tags: sortTags([...tags]),
          targets: toEntries(),
          enabled: $('#mf-enabled', root).checked,
        };
        if (!body.name) return toast(t('请填写对外模型名'), 'err');
        if (!body.targets.length) return toast(t('至少选择一个模型'), 'err');
        try {
          if (isNew) await api('POST', '/models', body);
          else await api('PUT', '/models/' + m.id, body);
          closeModal();
          toast(t('已保存'), 'ok');
          route();
        } catch (e) { toast(e.message, 'err'); }
      };
    },
  });
}

function modelTest(m) {
  openModal({
    title: t('测试模型 {name}', { name: m.name }),
    wide: true,
    body: `<div class="form">
      <div class="row2">
        <div class="field"><label>${t('以哪种协议调用')}</label><select id="mt-proto"><option value="openai">OpenAI /v1/chat/completions</option><option value="anthropic">Anthropic /v1/messages</option></select></div>
        <div class="field"><label>${t('测试内容')}</label><input type="text" id="mt-prompt" value="Reply with the single word: OK"></div>
      </div>
      <label class="check"><input type="checkbox" id="mt-stream"> ${t('流式')}</label>
      <div class="muted small">${t('走完整调度（含重试、熔断与切换），结果会计入请求日志和熔断状态。')}</div>
      <div id="mt-result"></div>
    </div>`,
    foot: `<button class="btn" data-close>${t('关闭')}</button><button class="btn primary" id="mt-go">${t('发送测试')}</button>`,
    onMount: (root) => {
      $('#mt-go', root).onclick = async () => {
        const btn = $('#mt-go', root);
        btn.disabled = true; btn.textContent = t('测试中…');
        $('#mt-result', root).innerHTML = '';
        try {
          const r = await api('POST', '/models/test', { model: m.name, protocol: $('#mt-proto', root).value, stream: $('#mt-stream', root).checked, prompt: $('#mt-prompt', root).value });
          $('#mt-result', root).innerHTML = testResultHTML(r);
        } catch (e) { $('#mt-result', root).innerHTML = `<div class="err-text">${esc(e.message)}</div>`; }
        btn.disabled = false; btn.textContent = t('发送测试');
      };
    },
  });
}

// ---------------------------------------------------------------- keys
function maskApiKey(k) { return k.length > 14 ? k.slice(0, 10) + '••••••' + k.slice(-4) : k; }
// keyLabel shows a key: masked, or its hint when only the hash is stored
const keyLabel = (k) => (k.key ? maskApiKey(k.key) : k.hint || '');

async function pageKeys() {
  const [keys, models, st] = await Promise.all([api('GET', '/keys'), api('GET', '/models'), api('GET', '/settings')]);
  const limitsHTML = (k) => {
    const parts = [];
    if (k.rpm) parts.push(t('{n} 次/分', { n: fmtNum(k.rpm) }));
    if (k.tpm) parts.push(t('{n} tokens/分', { n: fmtNum(k.tpm) }));
    if (k.max_concurrency) parts.push(t('最多 {n} 个并发', { n: k.max_concurrency }));
    return parts.length ? parts.join('<br>') : `<span class="muted">${t('不限')}</span>`;
  };
  const spendHTML = (k) => {
    const spent = fmtMoney(k.month_cost, st.currency);
    if (!k.monthly_budget) return `${spent} <span class="muted">/ ${t('不限')}</span>`;
    const over = k.month_cost >= k.monthly_budget;
    return `<span class="${over ? 'err-text' : ''}">${spent} / ${fmtMoney(k.monthly_budget, st.currency)}</span>${over ? ` <span class="badge err">${t('已超预算')}</span>` : ''}`;
  };
  $('#page').innerHTML = `
    ${head('API Keys', t('发给使用方的 Key；可单独停用、设置到期时间、限制可用模型、预算和请求频率'), `<button class="btn primary" id="add-key">${t('+ 创建 Key')}</button>`)}
    <div class="card">
      ${keys.length ? `<div class="table-wrap"><table>
        <tr><th>${t('名称')}</th><th>Key</th><th>${t('可用模型')}</th><th>${t('本月费用 / 预算')}</th><th>${t('限流')}</th><th>${t('到期')}</th><th>${t('最近使用')}</th><th>${t('状态')}</th><th></th></tr>
        ${keys.map((k) => {
          const expired = k.expires_at && k.expires_at < Date.now();
          return `<tr>
            <td>${esc(k.name || '-')}</td>
            <td class="mono small">${k.key ? `<span class="copy" data-copy="${esc(k.key)}" title="${esc(t('点击复制完整 Key'))}">${esc(maskApiKey(k.key))}</span>` : `<span title="${esc(t('用户自己创建的 Key 只存哈希，看不到完整值'))}">${esc(k.hint)}</span>`}${k.owner ? `<div class="muted">${t('用户：{name}', { name: esc(k.owner) })}</div>` : ''}</td>
            <td class="small">${k.allowed_models.length ? k.allowed_models.map((x) => `<span class="badge">${esc(x)}</span>`).join(' ') : `<span class="muted">${t('全部')}</span>`}</td>
            <td class="small">${spendHTML(k)}</td>
            <td class="small">${limitsHTML(k)}</td>
            <td class="small">${k.expires_at ? `<span class="${expired ? 'err-text' : ''}">${fmtTime(k.expires_at)}</span>` : `<span class="muted">${t('永不')}</span>`}</td>
            <td class="small">${fmtAgo(k.last_used_at)}</td>
            <td>${!k.enabled ? `<span class="badge">${t('停用')}</span>` : expired ? `<span class="badge err">${t('已过期')}</span>` : `<span class="badge ok">${t('启用')}</span>`}</td>
            <td><div class="btns">
              ${k.key ? `<button class="btn sm" data-copy="${esc(k.key)}">${t('复制')}</button>` : ''}
              <button class="btn sm" data-edit="${k.id}">${t('编辑')}</button>
              <button class="btn sm danger" data-del="${k.id}">${t('删除')}</button>
            </div></td></tr>`;
        }).join('')}
      </table></div>` : `<div class="empty">${t('还没有 API Key')}</div>`}
    </div>`;
  $('#add-key').onclick = () => keyForm(null, models, st);
  $$('[data-copy]').forEach((el) => el.onclick = () => copyText(el.dataset.copy));
  $$('[data-edit]').forEach((b) => b.onclick = () => keyForm(keys.find((k) => k.id == b.dataset.edit), models, st));
  $$('[data-del]').forEach((b) => b.onclick = async () => {
    const k = keys.find((x) => x.id == b.dataset.del);
    if (!(await confirmBox(t('删除 Key {name}？使用它的客户端会立即无法访问。', { name: k.name || keyLabel(k) })))) return;
    await api('DELETE', '/keys/' + k.id);
    toast(t('已删除'), 'ok');
    route();
  });
}

function toLocalInput(ms) {
  if (!ms) return '';
  const d = new Date(ms - new Date().getTimezoneOffset() * 60000);
  return d.toISOString().slice(0, 16);
}

function showNewKey(title, key, once) {
  openModal({
    title,
    body: `<div class="form"><div>${once ? t('请立即复制并妥善保存，关闭后无法再次查看：') : t('请复制并妥善保存：')}</div><pre class="box mono">${esc(key)}</pre></div>`,
    foot: `<button class="btn primary" id="kc-copy">${t('复制')}</button><button class="btn" data-close>${t('完成')}</button>`,
    onMount: (m2) => { $('#kc-copy', m2).onclick = () => copyText(key); },
  });
}

function keyForm(k, models, st) {
  const isNew = !k;
  k = k || { name: '', key: '', enabled: true, allowed_models: [], expires_at: 0, monthly_budget: 0, rpm: 0, tpm: 0, max_concurrency: 0 };
  const sign = CURRENCY_SIGN[st.currency] || '';
  openModal({
    title: isNew ? t('创建 API Key') : t('编辑 API Key'),
    body: `<div class="form">
      <div class="field"><label>${t('名称')}</label><input type="text" id="kf-name" value="${esc(k.name)}" placeholder="${esc(t('如 张三-ClaudeCode'))}"></div>
      <div class="field"><label>Key</label>${isNew
        ? `<div class="help">${t('保存后由平台自动生成（sk-route-…），不支持自定义')}</div>`
        : k.key ? `<div class="toolbar"><code class="mono">${esc(maskApiKey(k.key))}</code><button type="button" class="btn sm danger" id="kf-rotate">${t('重新生成')}</button></div><div class="help">${t('重新生成后旧 Key 立即失效')}</div>`
          : `<div class="toolbar"><code class="mono">${esc(k.hint)}</code></div><div class="help">${t('用户 {name} 自己创建的 Key，只有该用户能重新生成', { name: esc(k.owner) })}</div>`}</div>
      <div class="field"><label>${t('到期时间（可选）')}</label><input type="datetime-local" id="kf-exp" value="${toLocalInput(k.expires_at)}"></div>
      <div class="row3">
        <div class="field"><label>${t('月预算（{sign}，可选）', { sign })}</label><input type="number" id="kf-budget" min="0" step="any" value="${k.monthly_budget || ''}" placeholder="${esc(t('不限'))}"><div class="help">${t('本月费用达到后拒绝请求（402），每月 1 日恢复；按“设置与接入”里的统计货币计')}</div></div>
        <div class="field"><label>${t('每分钟请求数 RPM')}</label><input type="number" id="kf-rpm" min="0" value="${k.rpm || ''}" placeholder="${esc(t('不限'))}"><div class="help">${t('超过时返回 429，并带上 Retry-After')}</div></div>
        <div class="field"><label>${t('每分钟 tokens TPM')}</label><input type="number" id="kf-tpm" min="0" value="${k.tpm || ''}" placeholder="${esc(t('不限'))}"><div class="help">${t('按最近一分钟已完成请求的输入 + 输出计')}</div></div>
      </div>
      <div class="row3">
        <div class="field"><label>${t('并发上限')}</label><input type="number" id="kf-conc" min="0" value="${k.max_concurrency || ''}" placeholder="${esc(t('不限'))}"><div class="help">${t('同时在途的请求数，超过时返回 429。设了月预算或 TPM 时建议一起设：它们只在请求开始时检查，并发越高越可能超出')}</div></div>
      </div>
      <div class="field"><label>${t('可用模型（都不勾选 = 全部可用）')}</label>
        <div class="btns">${models.map((m) => `<label class="check badge"><input type="checkbox" value="${esc(m.name)}" ${k.allowed_models.includes(m.name) ? 'checked' : ''}> ${esc(m.name)}</label>`).join('') || `<span class="muted small">${t('暂无模型')}</span>`}</div>
      </div>
      <label class="check"><input type="checkbox" id="kf-enabled" ${k.enabled ? 'checked' : ''}> ${t('启用')}</label>
    </div>`,
    foot: `<button class="btn" data-close>${t('取消')}</button><button class="btn primary" id="kf-save">${t('保存')}</button>`,
    onMount: (root) => {
      if ($('#kf-rotate', root)) $('#kf-rotate', root).onclick = async () => {
        if (!(await confirmBox(t('重新生成 {name}？旧 Key 会立即失效，使用它的客户端需要更新配置。', { name: k.name || 'Key' })))) return;
        try {
          const r = await api('POST', `/keys/${k.id}/rotate`);
          route();
          showNewKey(t('Key 已重新生成'), r.key);
        } catch (e) { toast(e.message, 'err'); }
      };
      $('#kf-save', root).onclick = async () => {
        const exp = $('#kf-exp', root).value;
        const body = {
          name: $('#kf-name', root).value.trim(),
          enabled: $('#kf-enabled', root).checked,
          expires_at: exp ? new Date(exp).getTime() : 0,
          allowed_models: $$('.btns input[type=checkbox]:checked', root).map((c) => c.value),
          monthly_budget: Number($('#kf-budget', root).value) || 0,
          rpm: Math.floor(Number($('#kf-rpm', root).value) || 0),
          tpm: Math.floor(Number($('#kf-tpm', root).value) || 0),
          max_concurrency: Math.floor(Number($('#kf-conc', root).value) || 0),
        };
        if (body.monthly_budget < 0 || body.rpm < 0 || body.tpm < 0) return toast(t('预算和限流不能为负数'), 'err');
        try {
          if (isNew) {
            const created = await api('POST', '/keys', body);
            route();
            showNewKey(t('Key 已创建'), created.key);
          } else {
            await api('PUT', '/keys/' + k.id, body);
            closeModal();
            toast(t('已保存'), 'ok');
            route();
          }
        } catch (e) { toast(e.message, 'err'); }
      };
    },
  });
}

// ---------------------------------------------------------------- logs
const logFilter = { model: '', provider: '', status: '', fallback: false, session: '', offset: 0, limit: 50 };
let logsLoading = false;
async function pageLogs() {
  const qs = new URLSearchParams({ model: logFilter.model, provider: logFilter.provider, status: logFilter.status, fallback: logFilter.fallback ? '1' : '', session: logFilter.session, offset: logFilter.offset, limit: logFilter.limit });
  const [data, models, providers, caps] = await (async () => { logsLoading = true; try { return await Promise.all([
    api('GET', '/logs?' + qs), api('GET', '/models'), api('GET', '/providers'), api('GET', '/captures'),
  ]); } finally { logsLoading = false; } })();
  if (!location.hash.startsWith('#/logs')) return; // navigated away while loading
  const items = data.items;
  const capByReq = {};
  for (const c of caps.items) capByReq[c.request_id] = c.id;
  const rules = captureRuleViews(caps);
  $('#page').innerHTML = `
    ${head(t('请求日志'), t('共 {n} 条 · 点击行查看每次尝试的详情', { n: data.total }), `<button class="btn" id="log-capture">${t('抓取报文')}</button><button class="btn" id="log-refresh">${t('刷新')}</button>`)}
    ${rules.length ? `<div class="card"><div class="card-body form">${rules.map(captureRuleRow).join('')}</div></div>` : ''}
    <div class="card">
      <div class="card-head" style="font-weight:400">
        <div class="toolbar">
          <select id="lf-model"><option value="">${t('全部模型')}</option>${models.map((m) => `<option ${m.name === logFilter.model ? 'selected' : ''}>${esc(m.name)}</option>`).join('')}</select>
          <select id="lf-provider"><option value="">${t('全部套餐')}</option>${providers.map((p) => `<option ${p.prefix === logFilter.provider ? 'selected' : ''}>${esc(p.prefix)}</option>`).join('')}</select>
          <select id="lf-status"><option value="">${t('全部状态')}</option><option value="success" ${logFilter.status === 'success' ? 'selected' : ''}>${t('成功')}</option><option value="failed" ${logFilter.status === 'failed' ? 'selected' : ''}>${t('失败')}</option></select>
          <label class="check"><input type="checkbox" id="lf-fallback" ${logFilter.fallback ? 'checked' : ''}> ${t('只看发生切换的')}</label>
          ${logFilter.session ? `<span class="badge blue">${t('会话')} <span class="mono">${esc(logFilter.session)}</span></span><button class="btn sm" id="lf-session-clear">${t('清除')}</button>` : ''}
        </div>
      </div>
      ${items.length ? `<div class="table-wrap"><table>
        <tr><th>${t('时间')}</th><th>Key</th><th>${t('模型')}</th><th>${t('实际上游')}</th><th>${t('协议')}</th><th>${t('状态')}</th><th class="num">${t('耗时')}</th><th class="num">${t('首字')}</th><th class="num">${t('输入/输出')}</th><th class="num">${t('费用')}</th></tr>
        ${items.map((l, i) => `<tr class="clickable" data-i="${i}">
          <td class="small">${fmtTime(l.created_at)}</td>
          <td class="small">${esc(l.key_name || '-')}</td>
          <td class="small">${esc(l.public_model || l.requested_model)}${l.public_model && l.requested_model !== l.public_model ? `<div class="muted">${esc(l.requested_model)}</div>` : ''}</td>
          <td class="small mono">${l.provider ? esc(l.provider + '/' + l.upstream_model) : '-'}${l.fallback ? ` <span class="badge warn">${t('切换')}</span>` : ''}</td>
          <td class="small">${esc(l.inbound)}${l.upstream_protocol && l.upstream_protocol !== l.inbound ? ' → ' + esc(l.upstream_protocol) : ''}${l.stream ? ` <span class="badge">${t('流')}</span>` : ''}</td>
          <td>${l.success ? `<span class="badge ok">${t('成功')}</span>` : `<span class="badge err" title="${esc(l.error)}">${l.http_status && l.http_status !== 200 ? l.http_status : t('失败')}</span>`}${capByReq[l.request_id] ? ` <span class="badge blue">${t('报文')}</span>` : ''}</td>
          <td class="num small">${fmtMs(l.latency_ms)}</td>
          <td class="num small">${l.ttfb_ms ? fmtMs(l.ttfb_ms) : '-'}</td>
          <td class="num small">${fmtNum(l.input_tokens)} / ${fmtNum(l.output_tokens)}</td>
          <td class="num small">${fmtMoney(l.cost, l.currency)}</td>
        </tr>`).join('')}
      </table></div>
      <div class="pager"><span class="muted small">${logFilter.offset + 1} - ${logFilter.offset + items.length} / ${data.total}</span>
        <div class="btns"><button class="btn sm" id="lp-prev" ${logFilter.offset === 0 ? 'disabled' : ''}>${t('上一页')}</button><button class="btn sm" id="lp-next" ${logFilter.offset + items.length >= data.total ? 'disabled' : ''}>${t('下一页')}</button></div></div>` : `<div class="empty">${t('没有日志')}</div>`}
    </div>`;
  const upd = () => { logFilter.offset = 0; route(); };
  $('#lf-model').onchange = (e) => { logFilter.model = e.target.value; upd(); };
  $('#lf-provider').onchange = (e) => { logFilter.provider = e.target.value; upd(); };
  $('#lf-status').onchange = (e) => { logFilter.status = e.target.value; upd(); };
  $('#lf-fallback').onchange = (e) => { logFilter.fallback = e.target.checked; upd(); };
  if ($('#lf-session-clear')) $('#lf-session-clear').onclick = () => { logFilter.session = ''; upd(); };
  $('#log-refresh').onclick = route;
  $('#log-capture').onclick = () => captureForm(models, caps);
  $$('[data-stop-rule]').forEach((b) => b.onclick = async () => {
    await api('DELETE', '/captures/rules/' + b.dataset.stopRule);
    toast(b.dataset.ended ? t('已关闭') : t('已停止抓取'), 'ok');
    route();
  });
  if ($('#lp-prev')) {
    $('#lp-prev').onclick = () => { logFilter.offset = Math.max(0, logFilter.offset - logFilter.limit); route(); };
    $('#lp-next').onclick = () => { logFilter.offset += logFilter.limit; route(); };
  }
  $$('tr.clickable').forEach((tr) => tr.onclick = () => { const l = items[Number(tr.dataset.i)]; logDetail(l, capByReq[l.request_id]); });
  // while a capture is running, refresh in place (no loading placeholder, so
  // the scroll position stays); skip a tick while a dialog is open
  clearInterval(refreshTimer);
  if (rules.some((v) => v.state !== 'ended')) {
    refreshTimer = setInterval(() => { if (!$('.modal-bg') && location.hash.startsWith('#/logs') && !logsLoading) pageLogs().catch(() => {}); }, 5000);
  }
}

function logDetail(l, captureId) {
  const atts = (l.attempts || []).map((a, i) => `<tr><td>${i + 1}</td><td class="mono small">${esc(a.target)}${retryBadge(a)}</td><td>${esc(a.protocol)}</td><td>${a.http_status || '-'}</td><td>${fmtMs(a.latency_ms)}</td>${attemptResultHTML(a)}</tr>`).join('');
  openModal({
    title: t('请求详情 #{id}', { id: l.id }),
    wide: true,
    body: `<div class="form">
      <div class="kv">
        <div class="k">${t('时间')}</div><div>${fmtTime(l.created_at)}</div>
        ${l.request_id ? `<div class="k">${t('请求 ID')}</div><div class="mono small">${esc(l.request_id)} <span class="muted">${t('（响应头 X-Route-Request-Id）')}</span></div>` : ''}
        ${l.session_id ? `<div class="k">${t('会话')}</div><div class="mono small">${esc(l.session_id)} <button type="button" class="btn sm" id="ld-session">${t('只看这个会话')}</button></div>` : ''}
        <div class="k">API Key</div><div>${esc(l.key_name || '-')}</div>
        <div class="k">${t('客户端 IP')}</div><div>${esc(l.client_ip || '-')}</div>
        <div class="k">${t('请求模型')}</div><div>${esc(l.requested_model)}${l.public_model ? ' → ' + esc(l.public_model) : ''}</div>
        <div class="k">${t('实际上游')}</div><div class="mono">${l.provider ? esc(l.provider + '/' + l.upstream_model) : '-'}</div>
        <div class="k">${t('协议')}</div><div>${esc(l.inbound)} → ${esc(l.upstream_protocol || '-')}${l.stream ? t('（流式）') : ''}</div>
        <div class="k">${t('结果')}</div><div>${l.success ? `<span class="badge ok">${t('成功')}</span>` : `<span class="badge err">${t('失败')}</span>`} HTTP ${l.http_status}</div>
        <div class="k">${t('耗时 / 首字')}</div><div>${fmtMs(l.latency_ms)} / ${l.ttfb_ms ? fmtMs(l.ttfb_ms) : '-'}</div>
        <div class="k">Tokens</div><div>${t('输入 {input}（缓存 {cached}） · 输出 {output}', { input: l.input_tokens, cached: l.cached_tokens, output: l.output_tokens })}</div>
        <div class="k">${t('费用')}</div><div>${l.cost_source ? `${fmtMoney(l.cost, l.currency)} <span class="muted small">${l.cost_source === 'upstream' ? t('（上游返回的实际费用）') : t('（按配置的单价估算）')}</span>` : `<span class="muted">${t('未计费（没有配置该模型的单价）')}</span>`}</div>
        ${l.error ? `<div class="k">${t('错误')}</div><div class="err-text small">${esc(l.error)}</div>` : ''}
      </div>
      ${atts ? `<div class="table-wrap"><table><tr><th>#</th><th>${t('上游')}</th><th>${t('协议')}</th><th>${t('状态')}</th><th>${t('耗时')}</th><th>${t('结果')}</th></tr>${atts}</table></div>` : ''}
    </div>`,
    foot: captureId ? `<button class="btn" id="ld-capture">${t('查看报文')}</button>` : '',
    onMount: (m) => {
      if (captureId) $('#ld-capture', m).onclick = () => captureView(captureId);
      if ($('#ld-session', m)) $('#ld-session', m).onclick = () => { logFilter.session = l.session_id; logFilter.offset = 0; closeModal(); route(); };
    },
  });
}

// ---------------------------------------------------------------- request capture
function captureScope(r) {
  const key = r.key_id ? r.key_name || `#${r.key_id}` : t('全部 Key');
  return `${key} · ${r.model || t('全部模型')}`;
}

// how long after a rule's end the console still waits for its running
// requests (a capture is lost if the gateway restarts mid-request)
const CAPTURE_GRACE_MS = 30 * 60 * 1000;

// captureRuleViews pairs each rule with its progress. A request takes its
// slot when it starts but its capture is saved only when it ends, so a rule
// can be used up while its last requests are still running.
function captureRuleViews(caps, now = Date.now()) {
  const saved = {};
  for (const c of caps.items) saved[c.rule_id] = (saved[c.rule_id] || 0) + 1;
  return caps.rules.map((r) => {
    const done = saved[r.id] || 0;
    const running = Math.max(0, r.total - r.remaining - done);
    let state = 'ended';
    if (r.remaining > 0 && r.expires_at > now) state = 'active';
    else if (running > 0 && now < r.expires_at + CAPTURE_GRACE_MS) state = 'finishing';
    return { r, saved: done, running, state };
  });
}

function captureRuleRow({ r, saved, running, state }) {
  const left = fmtSecs(Math.round((r.expires_at - Date.now()) / 1000));
  const badge = { active: `<span class="badge warn">${t('抓取中')}</span>`, finishing: `<span class="badge warn">${t('收尾中')}</span>`, ended: `<span class="badge ok">${t('已结束')}</span>` }[state];
  const text = {
    active: t('已保存 {saved} · 进行中 {running} · 待抓 {left} 条，{time} 后结束', { saved, running, left: r.remaining, time: left }),
    finishing: t('名额已满，已保存 {saved} 条，还有 {running} 条请求未结束', { saved, running }),
    ended: t('已保存 {saved} / {total} 条', { saved, total: r.total }),
  }[state];
  const btn = state === 'ended' ? `<button class="btn sm" data-stop-rule="${r.id}" data-ended="1">${t('关闭')}</button>`
    : state === 'active' ? `<button class="btn sm" data-stop-rule="${r.id}">${t('停止')}</button>` : '';
  return `<div class="toolbar small">${badge} ${esc(captureScope(r))} · ${text} ${btn}</div>`;
}

async function captureForm(models, caps) {
  const keys = await api('GET', '/keys');
  openModal({
    title: t('抓取报文'),
    body: `<div class="form">
      <div class="hint-box small">${t('接下来匹配的请求会完整保存：客户端发来的请求、每次发给上游的请求和上游的响应、最后返回给客户端的内容。用来排查客户端兼容和协议转换问题。报文含完整的提示词，只有管理员能看，{h} 小时后自动删除{enc}。', { h: caps.retention_hours, enc: caps.encrypted ? t('，落库时用 SECRET_KEY 加密') : '' })}</div>
      <div class="row2">
        <div class="field"><label>API Key</label><select id="cp-key"><option value="0">${t('全部 Key')}</option>${keys.map((k) => `<option value="${k.id}">${esc(k.name || keyLabel(k))}</option>`).join('')}</select></div>
        <div class="field"><label>${t('模型')}</label><select id="cp-model"><option value="">${t('全部模型')}</option>${models.map((m) => `<option>${esc(m.name)}</option>`).join('')}</select></div>
      </div>
      <div class="row2">
        <div class="field"><label>${t('抓取条数')}</label><input type="number" id="cp-count" min="1" max="50" value="5"><div class="help">${t('最多 50 条，抓满自动停止')}</div></div>
        <div class="field"><label>${t('最长等待（分钟）')}</label><input type="number" id="cp-ttl" min="1" max="1440" value="60"><div class="help">${t('到时间没抓满也停止')}</div></div>
      </div>
      ${caps.items.length ? `<div class="toolbar small muted">${t('已保存 {n} 条报文', { n: caps.items.length })} <button type="button" class="btn sm danger" id="cp-clear">${t('全部删除')}</button></div>` : ''}
    </div>`,
    foot: `<button class="btn" data-close>${t('取消')}</button><button class="btn primary" id="cp-start">${t('开始抓取')}</button>`,
    onMount: (m) => {
      $('#cp-start', m).onclick = async () => {
        try {
          await api('POST', '/captures/rules', {
            key_id: Number($('#cp-key', m).value), model: $('#cp-model', m).value,
            count: Math.floor(Number($('#cp-count', m).value) || 0), ttl_minutes: Math.floor(Number($('#cp-ttl', m).value) || 0),
          });
          closeModal();
          toast(t('已开始抓取，匹配的请求会在日志里标出“报文”'), 'ok');
          route();
        } catch (e) { toast(e.message, 'err'); }
      };
      if ($('#cp-clear', m)) $('#cp-clear', m).onclick = async () => {
        if (!(await confirmBox(t('删除全部已抓取的报文？')))) return;
        await api('DELETE', '/captures');
        closeModal();
        toast(t('已删除'), 'ok');
        route();
      };
    },
  });
}

// pretty-prints JSON bodies; streams and other text are shown as is
function prettyBody(s) {
  if (!s) return '';
  try { return JSON.stringify(JSON.parse(s), null, 2); } catch (e) { return s; }
}

function bodyBlock(title, text, open, idx) {
  const body = prettyBody(text);
  return `<details ${open ? 'open' : ''}><summary class="small">${esc(title)} <span class="muted">${fmtNum(text ? text.length : 0)} B</span></summary>
    ${body ? `<div class="btns" style="margin:6px 0"><button type="button" class="btn sm" data-copy-body="${idx}">${t('复制')}</button></div><pre class="box">${esc(body)}</pre>` : `<div class="muted small">${t('（空）')}</div>`}</details>`;
}

async function captureView(id) {
  const c = await api('GET', '/captures/' + id);
  const bodies = [];
  const block = (title, text, open = false) => { bodies.push(prettyBody(text)); return bodyBlock(title, text, open, bodies.length - 1); };
  const html = c.unreadable
    ? `<div class="hint-box small err-text">${t('这条报文是用另一个 SECRET_KEY 加密的，当前实例打不开')}</div>`
    : `${c.truncated ? `<div class="hint-box small">${t('部分报文超过 1 MB，只保存了开头')}</div>` : ''}
      ${block(t('客户端请求'), c.client_request, true)}
      ${(c.attempts || []).map((a, i) => `<div class="card" style="margin:8px 0"><div class="card-body form">
        <div class="small"><b>${t('尝试 {n}', { n: i + 1 })}</b> <span class="mono">${esc(a.target)}</span> · ${esc(a.protocol)} · ${a.status ? 'HTTP ' + a.status : t('无响应')}</div>
        ${a.url ? `<div class="mono small muted">POST ${esc(a.url)}</div>` : ''}
        ${block(t('发给上游的请求'), a.request)}
        ${block(t('上游的响应'), a.response)}
      </div></div>`).join('')}
      ${block(t('返回给客户端'), c.client_response)}`;
  openModal({
    title: t('报文 {id}', { id: c.request_id || '#' + c.id }),
    wide: true,
    body: `<div class="form">
      <div class="kv">
        <div class="k">${t('时间')}</div><div>${fmtTime(c.created_at)}</div>
        <div class="k">API Key</div><div>${esc(c.key_name || '-')}</div>
        <div class="k">${t('模型')}</div><div>${esc(c.model)} · ${esc(c.inbound)}</div>
        <div class="k">${t('返回状态')}</div><div>HTTP ${c.status || '-'}</div>
      </div>
      ${html}
    </div>`,
    onMount: (m) => $$('[data-copy-body]', m).forEach((b) => b.onclick = () => copyText(bodies[Number(b.dataset.copyBody)])),
  });
}

// ---------------------------------------------------------------- settings
async function pageSettings() {
  const [st, models, alerts, keys, ping, cluster] = await Promise.all([api('GET', '/settings'), api('GET', '/models'), api('GET', '/alerts'), api('GET', '/keys'), api('GET', '/ping'), api('GET', '/cluster')]);
  if (ping.user_agent) PLATFORM_UA = ping.user_agent;
  const origin = location.origin;
  const alertLang = alerts.language === 'en' ? 'en' : 'zh';
  $('#page').innerHTML = `
    ${head(t('设置与接入'), '')}
    <div class="card">
      <div class="card-head">${t('客户端接入')}</div>
      <div class="card-body form">
        <div class="kv">
          <div class="k">${t('OpenAI 兼容')}</div><div><code class="copy" data-copy="${esc(origin)}/v1">${esc(origin)}/v1</code> <span class="muted small">${t('（POST /v1/chat/completions、POST /v1/responses、POST /v1/embeddings、POST /v1/rerank、GET /v1/models）')}</span></div>
          <div class="k">${t('Anthropic 兼容')}</div><div><code class="copy" data-copy="${esc(origin)}">${esc(origin)}</code> <span class="muted small">${t('（POST /v1/messages，即 ANTHROPIC_BASE_URL）')}</span></div>
          <div class="k">${t('鉴权')}</div><div>${t('<code>Authorization: Bearer sk-route-…</code> 或 <code>x-api-key: sk-route-…</code>')}</div>
        </div>
        <div class="wizard" id="wz"></div>
      </div>
    </div>
    <div class="card">
      <div class="card-head">${t('请求头说明')}</div>
      <div class="card-body form hdoc">${headerDocsHTML()}</div>
    </div>
    <div class="card">
      <div class="card-head">${t('重试、熔断与日志')}</div>
      <div class="card-body form">
        <div class="row2">
          <div class="field"><label>${t('同一模型重试次数')}</label><input type="number" id="st-retry" min="0" max="10" value="${st.max_retries}"><div class="help">${t('遇到断连、5xx、上游过载、短时限流时，先在同一个模型上重试几次再切换。0 = 不重试，直接切换')}</div></div>
          <div class="field"><label>${t('重试间隔（毫秒）')}</label><input type="number" id="st-backoff" min="1" value="${st.retry_backoff_ms}"><div class="help">${t('每次翻倍，例如 1000 → 1s、2s；上游返回的 Retry-After 更长时以它为准（最多等 10 秒，更长就直接切换）')}</div></div>
        </div>
        <div class="row2">
          <div class="field"><label>${t('连续失败几次后冷却')}</label><input type="number" id="st-th" min="1" value="${st.failure_threshold}"><div class="help">${t('按请求计：一次请求在该模型上重试完仍失败算 1 次。429（长时间）、401、402 会立即冷却整个套餐，404 立即冷却该模型；400、403、超时只切换不重试。')}</div></div>
          <div class="field"><label>${t('首次冷却时长（秒）')}</label><input type="number" id="st-cd" min="1" value="${st.cooldown_seconds}"><div class="help">${t('冷却到期后再次失败，时长翻倍')}</div></div>
        </div>
        <div class="row2">
          <div class="field"><label>${t('最长冷却（秒）')}</label><input type="number" id="st-max" min="1" value="${st.max_cooldown_seconds}"><div class="help">${t('上游返回的 Retry-After 更长时以它为准（最多 6 小时）')}</div></div>
          <div class="field"><label>${t('日志保留天数')}</label><input type="number" id="st-ret" min="1" value="${st.log_retention_days}"><div class="help">${t('当前数据库：{db}', { db: ping.database === 'postgres' ? 'PostgreSQL' : 'SQLite' })}</div></div>
        </div>
        <div class="row2">
          <div class="field"><label>${t('默认 max_tokens')}</label><input type="number" id="st-mt" min="1" value="${st.default_max_tokens}"><div class="help">${t('OpenAI 请求转 Anthropic 上游且没带 max_tokens 时使用')}</div></div>
          <div class="field"><label>${t('排队等待（秒）')}</label><input type="number" id="st-queue" min="0" value="${st.queue_timeout_seconds}"><div class="help">${t('设了最大并发的上游都满、其他候补也失败时，最多等这么久；0 = 不等，直接返回 429')}</div></div>
        </div>
        <div class="row2">
          <div class="field"><label>${t('统计货币')}</label><select id="st-cur">${['CNY', 'USD'].map((c) => `<option value="${c}" ${st.currency === c ? 'selected' : ''}>${currencyLabel(c)}</option>`).join('')}</select><div class="help">${t('概览里的费用统一换算成这种货币；日志里显示原始货币')}</div></div>
          <div class="field"><label>${t('美元兑人民币汇率')}</label><input type="number" id="st-rate" min="0" step="0.01" value="${st.usd_to_cny}"><div class="help">${t('换算时使用，修改后历史数据也按新汇率显示')}</div></div>
        </div>
        <div><button class="btn primary" id="st-save">${t('保存设置')}</button></div>
      </div>
    </div>
    ${cluster.enabled ? `<div class="card">
      <div class="card-head">${t('运行实例')} <span class="btns"><span class="badge ok">${t('{n} 个在线', { n: cluster.instances.filter((i) => i.alive).length })}</span></span></div>
      <div class="card-body"><div class="help">${t('连接同一个 PostgreSQL 的网关实例共享限流、并发、熔断冷却和配置；超过 20 秒没有心跳的实例不再占用并发名额。')}</div></div>
      <div class="table-wrap"><table>
        <tr><th>${t('实例')}</th><th>${t('版本')}</th><th>${t('启动时间')}</th><th>${t('最近心跳')}</th><th>${t('状态')}</th></tr>
        ${cluster.instances.map((i) => `<tr>
          <td class="mono">${esc(i.id)}${i.self ? ` <span class="badge">${t('当前')}</span>` : ''}</td>
          <td class="small">${esc(i.version)}</td>
          <td class="small">${fmtTime(i.started_at)}</td>
          <td class="small">${fmtAgo(i.seen_at)}</td>
          <td>${i.alive ? `<span class="badge ok">${t('在线')}</span>` : `<span class="badge">${t('离线')}</span>`}</td>
        </tr>`).join('')}
      </table></div>
    </div>` : ''}
    <div class="card">
      <div class="card-head">${t('告警通知')}</div>
      <div class="card-body form">
        <div class="muted small">${t('出问题时推送到飞书、钉钉、企业微信群机器人，或任意接收 JSON 的地址。同一件事在静默时间内只推送一次。')}</div>
        <div id="al-hooks"></div>
        <div><button type="button" class="btn sm" id="al-add">${t('+ 添加 Webhook')}</button></div>
        <div class="field"><label>${t('推送哪些事件')}</label>
          <label class="check"><input type="checkbox" id="al-auth" ${alerts.on_auth_failure ? 'checked' : ''}> ${t('上游返回 401 / 402（Key 失效、欠费），整个套餐被冷却')}</label>
          <label class="check"><input type="checkbox" id="al-all" ${alerts.on_all_failed ? 'checked' : ''}> ${t('某个对外模型的整条调度链全部失败（客户端收到错误）')}</label>
          <label class="check"><input type="checkbox" id="al-health" ${alerts.on_health_check ? 'checked' : ''}> ${t('开了健康检查的供应商连续检查失败被移出调度，以及恢复')}</label>
          <label class="check"><input type="checkbox" id="al-cool" ${alerts.on_long_cooldown ? 'checked' : ''}> ${t('套餐或模型一次冷却超过 {input} 分钟', { input: `<input type="number" id="al-cool-min" min="1" value="${alerts.long_cooldown_minutes}" style="width:70px">` })}</label>
        </div>
        <div class="row2">
          <div class="field"><label>${t('静默时间（分钟）')}</label><input type="number" id="al-silence" min="1" value="${alerts.silence_minutes}"><div class="help">${t('同一事件、同一对象在这段时间内只推送一次')}</div></div>
          <div class="field"><label>${t('推送语言')}</label><select id="al-lang"><option value="zh" ${alertLang === 'zh' ? 'selected' : ''}>中文</option><option value="en" ${alertLang === 'en' ? 'selected' : ''}>English</option></select><div class="help">${t('推送到群里的告警消息使用的语言')}</div></div>
        </div>
        <div><button class="btn primary" id="al-save">${t('保存告警设置')}</button></div>
      </div>
    </div>
    <div class="card">
      <div class="card-head">${t('备份 / 迁移')}</div>
      <div class="card-body form">
        <div class="muted small">${t('导出全部套餐（含上游 Key）、模型映射、API Key 和设置为 JSON。导入会<b>覆盖</b>现有配置（日志不受影响）。导出文件含密钥，请妥善保管。')}</div>
        <div class="small">${ping.encrypted
          ? `<span class="badge ok">${t('已加密')}</span> ${t('上游 Key 和告警 Webhook 在数据库里用 SECRET_KEY 加密保存；导出文件里也是密文，只能导入到设置了同一个 SECRET_KEY 的实例。')}`
          : `<span class="badge warn">${t('未加密')}</span> ${t('上游 Key 在数据库和导出文件里都是明文。设置环境变量 SECRET_KEY（至少 16 位随机字符）后重启，现有的 Key 会自动加密。')}`}</div>
        <div class="btns"><button class="btn" id="cfg-export">${t('导出配置')}</button><button class="btn" id="cfg-import">${t('导入配置')}</button><input type="file" id="cfg-file" accept=".json" style="display:none"></div>
      </div>
    </div>`;
  $$('[data-copy]').forEach((el) => el.onclick = () => copyText(el.dataset.copy));
  setupWizard($('#wz'), origin, keys, models);
  alertHooksEditor(alerts);
  $('#st-save').onclick = async () => {
    try {
      await api('PUT', '/settings', {
        max_retries: Number($('#st-retry').value), retry_backoff_ms: Number($('#st-backoff').value),
        failure_threshold: Number($('#st-th').value), cooldown_seconds: Number($('#st-cd').value),
        max_cooldown_seconds: Number($('#st-max').value), log_retention_days: Number($('#st-ret').value),
        default_max_tokens: Number($('#st-mt').value),
        currency: $('#st-cur').value, usd_to_cny: Number($('#st-rate').value),
        queue_timeout_seconds: Math.max(0, Math.floor(Number($('#st-queue').value) || 0)),
      });
      toast(t('已保存'), 'ok');
      route();
    } catch (e) { toast(e.message, 'err'); }
  };
  $('#cfg-export').onclick = async () => {
    const res = await fetch('/admin/api/export', { headers: { Authorization: 'Bearer ' + TOKEN } });
    if (!res.ok) return toast(t('导出失败'), 'err');
    const blob = await res.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'ai-route-config.json';
    a.click();
    URL.revokeObjectURL(a.href);
  };
  $('#cfg-import').onclick = () => $('#cfg-file').click();
  $('#cfg-file').onchange = async (e) => {
    const f = e.target.files[0];
    if (!f) return;
    try {
      const data = JSON.parse(await f.text());
      if (!(await confirmBox(t('导入将覆盖现有配置：{providers} 个套餐、{models} 个模型、{keys} 个 Key。继续？', { providers: (data.providers || []).length, models: (data.models || []).length, keys: (data.api_keys || []).length })))) return;
      await api('POST', '/import', data);
      toast(t('导入成功'), 'ok');
      route();
    } catch (err) { toast(t('导入失败：{msg}', { msg: err.message }), 'err'); }
    e.target.value = '';
  };
}

// ---------------------------------------------------------------- header docs
// What each header does on the way client -> gateway -> upstream and back.
// Vendor requirements link to the source they were checked against.
const HDR_DROPPED = [
  [t('Authorization、x-api-key'), t('调用方发给网关的 sk-route-…。转给上游会泄露平台 Key，网关会换成供应商自己的 Key')],
  [t('Cookie、Proxy-Authorization'), t('凭证类，不能带给第三方')],
  [t('Host、Content-Length、Content-Type'), t('网关重新组装请求体后自己设置')],
  ['Accept-Encoding', t('网关要解析上游返回的 JSON 和 SSE。网关自己声明 gzip 时，Go 会自动解压；如果转发调用方的值（浏览器还会带 br、zstd），收到的是压缩内容，解析会失败')],
  ['User-Agent', t('由供应商的 User-Agent 策略决定（见下文）')],
  [t('Connection 以及它列出的头、Keep-Alive、TE、Trailer、Transfer-Encoding、Upgrade、HTTP2-Settings、Proxy-*、Expect'), t('逐跳头，只对当前这一段连接有效。HTTP 规范（RFC 9110 §7.6.1）要求代理去掉，和 Go 标准库反向代理的处理一致')],
  [t('X-Forwarded-*、Forwarded、X-Real-IP、True-Client-IP、Via、CF-*'), t('反向代理和 CDN（如 Cloudflare）记录的用户真实 IP 和经过的代理，转出去会泄露用户信息')],
  [t('Origin、Referer、Sec-*'), t('浏览器自动加的头，表示请求来自网页。Anthropic 会拒绝带 Origin 的跨域请求，除非额外声明 anthropic-dangerous-direct-browser-access；网关是服务端调用，不需要它们')],
  [t('anthropic-*（上游不是 Anthropic 协议时）、openai-*（上游是 Anthropic 协议时）'), t('对面协议专属，协议转换后没有意义')],
  ['X-Session-Id', t('网关自己的会话头。上游需要会话 ID 时用 $session 发（按 API Key 做了哈希），不把调用方的原始值带给第三方')],
];
const HDR_SYNTAX = [
  ['abc', t('固定值'), t('平台写死，覆盖调用方的同名头')],
  [t('（留空）'), t('删除'), t('不发这个头，即使调用方带了')],
  ['{{header.X-Foo}}', t('调用方提供，必传'), t('首选上游缺了直接返回 400；候补上缺了就不发')],
  ['{{header.X-Foo?}}', t('调用方提供，可选'), t('没带就不发')],
  ['{{header.X-Foo ?? $conversation}}', t('调用方优先，平台兜底'), t('带了用调用方的，没带用平台生成的；也可以写 ?? "默认值"')],
  [t('{{$变量}}'), t('平台生成'), t('可以和文字拼接，如 ai-route-{{$requestId}}')],
];
const HDR_VARS = [
  ['$session', t('ses_ 开头，同一会话内不变'), t('按“API Key + 调用方的会话 ID”计算：X-Session-Id，没有就用 Claude Code、Codex、OpenCode 客户端自带的会话头。同一个会话 ID 换了 API Key 算两个会话。调用方都没带时等于 $conversation。供应商要会话 ID 时用它')],
  ['$conversation', t('ses_ 开头，同一对话内不变'), t('只按“API Key + 第一条用户消息”计算，不看调用方的会话头。没有用户消息的请求（向量、重排序）每次随机')],
  ['$uuid', t('每个请求一个新的 UUID'), t('请求 ID、幂等键。不要用作会话 ID')],
  ['$requestId', t('网关的请求 ID（req_…）'), t('同时出现在响应头 X-Route-Request-Id 和请求日志里，方便和上游对账')],
  ['$timestamp', t('当前 Unix 秒'), ''],
  ['$keyName / $keyId', t('调用方使用的 API Key 名称 / 编号'), t('名称里的中文等字符会做 URL 编码')],
  ['$model', t('实际请求的上游模型名'), ''],
];
const HDR_RESPONSE = [
  ['X-Route-Target', t('这次实际走的上游，前缀/模型名')],
  ['X-Route-Request-Id', t('网关的请求 ID，在“请求日志”里可以搜到')],
  ['Retry-After', t('被 RPM / TPM 限流或上游都满载时返回 429，告诉客户端多少秒后重试')],
];
// [vendor, requirement, how this gateway handles it, source]
const HDR_VENDORS = [
  ['OpenCode Go', t('每个会话带一个稳定的 x-opencode-session（官方说明用于路由和提示词缓存，没有规定格式）；客户端用自己的 User-Agent，不要用 SDK 或 HTTP 库的默认值。对 Claude Code、Codex 等客户端也能识别它们自带的会话头。GPT Luna、Grok、Muse Spark 只走 /responses'), t('预设：x-opencode-session: {{$session}}，UA 透传客户端；协议规则已预填（gpt-*、grok-*、muse-* = responses）'), 'https://opencode.ai/docs/go/'],
  ['Kimi Code', t('会员条款：篡改客户端标识（User-Agent）视为违规，可能暂停会员权益。接口只接受 Kimi CLI、Claude Code、Roo Code、Kilo Code 等编码工具，其他客户端会收到 403'), t('预设 UA 透传客户端，不要改成固定 UA 或平台标识'), 'https://www.kimi.com/help/kimi-code/membership-guide'],
  [t('Claude Code（作为调用方）'), t('发给网关的请求带 x-claude-code-session-id（当前会话的唯一 ID，v2.1.86 起），以及 anthropic-version、anthropic-beta'), t('默认透传；没有 X-Session-Id 时网关用它作为会话 ID（$session），比按消息计算更准（压缩上下文后也不变）'), 'https://code.claude.com/docs/en/llm-gateway-protocol'],
  [t('Anthropic 及兼容端点'), t('anthropic-version 必填（目前是 2023-06-01）；beta 功能用 anthropic-beta，多个用逗号分隔'), t('调用方带了就透传，没带补 2023-06-01；鉴权同时发 x-api-key 和 Authorization: Bearer'), 'https://platform.claude.com/docs/en/api/versioning'],
  ['OpenRouter', t('可选的应用标识：HTTP-Referer（应用网址，没有它不会生成应用页）、X-OpenRouter-Title（应用名，旧名 X-Title 仍兼容），用于在 OpenRouter 的排行和统计里显示你的应用'), t('需要的话在自定义请求头里写固定值；费用直接取响应里的 usage.cost'), 'https://openrouter.ai/docs/app-attribution'],
];

function headerDocsHTML() {
  const table = (head, rows) => `<div class="table-wrap"><table><tr>${head.map((h) => `<th>${h}</th>`).join('')}</tr>${rows.map((r) => `<tr>${r.map((c, i) => `<td class="${i === 0 ? 'mono small' : 'small'}">${c}</td>`).join('')}</tr>`).join('')}</table></div>`;
  const e = (rows) => rows.map((r) => r.map(esc));
  return `
    <div class="muted small">${t('请求头分三段经过网关：调用方 → 网关、网关 → 上游、网关 → 调用方。下面按这个顺序说明每段的规则。')}</div>
    <details open>
      <summary><b>${t('1. 调用方发给网关')}</b></summary>
      <div class="kv" style="margin-top:8px">
        <div class="k">${t('鉴权')}</div><div>${t('<code>Authorization: Bearer sk-route-…</code> 或 <code>x-api-key: sk-route-…</code>，两种都行。只用于网关鉴权，不会转给上游')}</div>
        <div class="k">${t('Anthropic 协议')}</div><div>${t('<code>anthropic-version</code>、<code>anthropic-beta</code> 照常带，上游也是 Anthropic 协议时透传')}</div>
        <div class="k">${t('会话')}</div><div>${t('<code>X-Session-Id: 任意字符串</code>，可选。值相同的请求属于同一个会话（一次对话、一个 Agent 任务）：网关让它们在同级上游里固定走同一个，换成各家要求的会话字段（比如 OpenCode Go 的 <code>x-opencode-session</code>）发给上游，并记在请求日志里，可以按会话筛选。调用方不用关心各家供应商的会话头。没带时依次认 Claude Code 的 <code>x-claude-code-session-id</code>、Codex 的 <code>session-id</code>、<code>x-opencode-session</code>，都没有就按第一条用户消息区分会话')}</div>
        <div class="k">${t('其他头')}</div><div>${t('默认原样转给上游（见第 2 段）。供应商要求的头由平台按下面的规则生成，调用方不用自己带')}</div>
      </div>
    </details>
    <details>
      <summary><b>${t('2. 网关转给上游：透传规则')}</b></summary>
      <div class="small" style="margin:8px 0">${t('调用方的请求头默认透传，下面这些除外。个别上游对多余请求头敏感时，可以在供应商的“高级设置”里关掉“透传调用方的请求头”。供应商配置的同名头总是覆盖调用方的值。')}</div>
      ${table([t('不透传的头'), t('原因')], e(HDR_DROPPED))}
    </details>
    <details>
      <summary><b>${t('3. 供应商自定义请求头：写法')}</b></summary>
      <div class="small" style="margin:8px 0">${t('在供应商的“高级设置 → 自定义请求头”里每行写 <code>Header: 值</code>。值可以是下面几种之一；变量名写错，或者引用 <code>header.Authorization</code> 这类凭证，保存时会直接报错。')}</div>
      ${table([t('值'), t('来源'), t('说明')], e(HDR_SYNTAX))}
      <div class="small" style="margin:8px 0">${t('<b>必传的判断按配置顺序</b>：模型调度顺序第一级上启用的供应商（并列组的每个成员都算）要求的头，调用方没带就直接返回 400，即使首选当前在冷却也一样，保证同一个客户端每次结果一致。只出现在候补位置的供应商，必传头缺了不报错，只是不发这个头。模型映射页会在每个上游旁边标出它的必传头。')}</div>
      ${table([t('内置变量'), t('值'), t('说明')], e(HDR_VARS))}
      <div class="small" style="margin-top:8px">${t('每次尝试实际发出的动态请求头可以在“请求日志 → 详情”里看到。')}</div>
    </details>
    <details>
      <summary><b>${t('4. User-Agent')}</b></summary>
      <div class="kv" style="margin-top:8px">
        <div class="k">${t('透传客户端（默认）')}</div><div>${t('转发调用方的 UA（如 <code>claude-cli/…</code>）；调用方没带时用供应商里填的值，再没有就用平台标识。限制客户端类型的套餐（如 Kimi Code）必须用这个')}</div>
        <div class="k">${t('平台标识')}</div><div>${t('总是发本网关的标识 <code>{ua}</code>，适合自建模型、中转平台这类需要识别来源的上游', { ua: esc(PLATFORM_UA) })}</div>
        <div class="k">${t('固定 UA')}</div><div>${t('总是发供应商里填的值，只在厂商明确要求某个固定 UA 时用')}</div>
      </div>
    </details>
    <details>
      <summary><b>${t('5. 网关返回给调用方')}</b></summary>
      ${table([t('响应头'), t('含义')], e(HDR_RESPONSE))}
    </details>
    <details>
      <summary><b>${t('6. 已知厂商要求')}</b></summary>
      <div class="small" style="margin:8px 0">${t('下面是已经对照官方文档核实过的要求；厂商可能调整，请以链接里的最新文档为准。')}</div>
      ${table([t('厂商'), t('要求'), t('本网关的处理'), t('来源')], HDR_VENDORS.map(([v, req, how, src]) => [esc(v), esc(req), esc(how), `<a href="${esc(src)}" target="_blank" rel="noopener">${t('文档')}</a>`]))}
    </details>`;
}

// ---------------------------------------------------------------- client setup wizard
const CLIENTS = [
  { id: 'claude-code', label: 'Claude Code', small: true },
  { id: 'codex', label: 'Codex CLI' },
  { id: 'opencode', label: 'OpenCode' },
  { id: 'cline', label: 'Cline / Roo Code / Kilo Code' },
  { id: 'cherry', label: 'Cherry Studio' },
  { id: 'openai-py', label: t('OpenAI SDK（Python）') },
  { id: 'anthropic-py', label: t('Anthropic SDK（Python）') },
  { id: 'curl', label: 'curl' },
];

// Snippet titles and GUI field names are translated; generated config
// file contents stay as they are (only the sample prompt follows the language).
function clientSnippets(client, o, key, model, small) {
  const v1 = o + '/v1';
  const hello = t('你好');
  switch (client) {
    case 'claude-code': return [
      { title: t('终端里临时使用（bash / zsh）'), lang: 'bash', text: `export ANTHROPIC_BASE_URL=${o}\nexport ANTHROPIC_AUTH_TOKEN=${key}\nexport ANTHROPIC_MODEL=${model}\nexport ANTHROPIC_DEFAULT_HAIKU_MODEL=${small}\nclaude` },
      { title: t('长期使用：写进 ~/.claude/settings.json'), lang: 'json', text: JSON.stringify({ env: { ANTHROPIC_BASE_URL: o, ANTHROPIC_AUTH_TOKEN: key, ANTHROPIC_MODEL: model, ANTHROPIC_DEFAULT_HAIKU_MODEL: small } }, null, 2) },
    ];
    case 'codex': return [
      { title: t('写进 ~/.codex/config.toml（Codex 只支持 Responses API，网关会按上游自动转换）'), lang: 'toml', text: `model = "${model}"\nmodel_provider = "ai-route"\n\n[model_providers.ai-route]\nname = "AI Route"\nbase_url = "${v1}"\nenv_key = "AI_ROUTE_API_KEY"\nwire_api = "responses"` },
      { title: t('然后在终端里设置 Key 并启动'), lang: 'bash', text: `export AI_ROUTE_API_KEY=${key}\ncodex` },
    ];
    case 'opencode': return [
      { title: t('写进项目根目录的 opencode.json（或 ~/.config/opencode/opencode.json），然后在 /models 里选 ai-route/{model}', { model }), lang: 'json', text: JSON.stringify({
        $schema: 'https://opencode.ai/config.json',
        provider: { 'ai-route': { npm: '@ai-sdk/openai-compatible', name: 'AI Route', options: { baseURL: v1, apiKey: key }, models: { [model]: { name: model } } } },
      }, null, 2) },
    ];
    case 'cline': return [
      { title: t('在插件设置里选择 API Provider：OpenAI Compatible，然后填写'), lang: 'text', text: t('Base URL：{url}\nAPI Key：{key}\nModel ID：{model}', { url: v1, key, model }) },
    ];
    case 'cherry': return [
      { title: t('设置 → 模型服务 → 添加，提供商类型选 OpenAI，然后填写'), lang: 'text', text: t('API 地址：{url}\nAPI 密钥：{key}\n模型：点“管理”从列表里添加 {model}（或手动添加）', { url: o, key, model }) },
    ];
    case 'openai-py': return [
      { title: 'pip install openai', lang: 'python', text: `from openai import OpenAI\n\nclient = OpenAI(base_url="${v1}", api_key="${key}")\nresp = client.chat.completions.create(\n    model="${model}",\n    messages=[{"role": "user", "content": "${hello}"}],\n)\nprint(resp.choices[0].message.content)` },
    ];
    case 'anthropic-py': return [
      { title: 'pip install anthropic', lang: 'python', text: `import anthropic\n\nclient = anthropic.Anthropic(base_url="${o}", api_key="${key}")\nmsg = client.messages.create(\n    model="${model}",\n    max_tokens=1024,\n    messages=[{"role": "user", "content": "${hello}"}],\n)\nprint(msg.content[0].text)` },
    ];
    default: return [
      { title: t('OpenAI 格式'), lang: 'bash', text: `curl ${v1}/chat/completions \\\n  -H "Authorization: Bearer ${key}" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model}","messages":[{"role":"user","content":"${hello}"}]}'` },
      { title: t('Anthropic 格式'), lang: 'bash', text: `curl ${o}/v1/messages \\\n  -H "x-api-key: ${key}" \\\n  -H "anthropic-version: 2023-06-01" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model":"${model}","max_tokens":1024,"messages":[{"role":"user","content":"${hello}"}]}'` },
    ];
  }
}

function setupWizard(box, origin, keys, models) {
  const enabledKeys = keys.filter((k) => k.enabled && !(k.expires_at && k.expires_at < Date.now()));
  const state = { client: 'claude-code', key: enabledKeys[0] ? enabledKeys[0].id : 0, model: '', small: '', reveal: false };
  const allowed = () => {
    const k = enabledKeys.find((x) => x.id == state.key);
    return models.filter((m) => m.enabled && (!k || !k.allowed_models.length || k.allowed_models.includes(m.name)));
  };
  const render = () => {
    const ms = allowed();
    if (!ms.some((m) => m.name === state.model)) state.model = ms[0] ? ms[0].name : '';
    if (!ms.some((m) => m.name === state.small)) state.small = (ms.find((m) => (m.tags || []).includes('fast')) || ms[0] || {}).name || '';
    const client = CLIENTS.find((c) => c.id === state.client);
    const k = enabledKeys.find((x) => x.id == state.key);
    // keys stored as hashes (users' own) cannot be filled in
    const keyValue = k && k.key ? k.key : 'sk-route-xxxx';
    const shown = state.reveal || !k || !k.key ? keyValue : maskApiKey(keyValue);
    const snippets = clientSnippets(state.client, origin, keyValue, state.model || t('模型名'), state.small || state.model || t('模型名'));
    box.innerHTML = `
      <div class="muted small" style="margin-bottom:8px">${t('选好客户端、Key 和模型，复制下面生成的配置即可。')}</div>
      <div class="seg" id="wz-client">${CLIENTS.map((c) => `<button type="button" data-c="${c.id}" class="${c.id === state.client ? 'on' : ''}">${esc(c.label)}</button>`).join('')}</div>
      <div class="row3" style="margin-top:10px">
        <div class="field"><label>API Key</label><select id="wz-key">${enabledKeys.length ? enabledKeys.map((x) => `<option value="${x.id}" ${x.id == state.key ? 'selected' : ''}>${esc(x.name || keyLabel(x))}</option>`).join('') : `<option value="0">${t('还没有可用的 Key，先到 API Keys 创建')}</option>`}</select></div>
        <div class="field"><label>${t('模型')}</label><select id="wz-model">${ms.map((m) => `<option ${m.name === state.model ? 'selected' : ''}>${esc(m.name)}</option>`).join('') || `<option value="">${t('没有可用模型')}</option>`}</select></div>
        ${client.small ? `<div class="field"><label>${t('后台小模型（Haiku 位）')}</label><select id="wz-small">${ms.map((m) => `<option ${m.name === state.small ? 'selected' : ''}>${esc(m.name)}</option>`).join('')}</select></div>` : '<div></div>'}
      </div>
      ${k && !k.key ? `<div class="hint-box small">${t('Key 只在创建时显示过一次：把下面的 sk-route-xxxx 换成你保存的 {hint}', { hint: esc(k.hint) })}</div>`
        : `<label class="check small"><input type="checkbox" id="wz-reveal" ${state.reveal ? 'checked' : ''}> ${t('预览里显示完整 Key（复制时总是完整的）')}</label>`}
      ${snippets.map((sn, i) => `
        <div class="snippet">
          <div class="toolbar"><span class="muted small">${esc(sn.title)}</span><button type="button" class="btn sm" data-copy-i="${i}" style="margin-left:auto">${t('复制')}</button></div>
          <pre class="box mono">${esc(sn.text.split(keyValue).join(shown))}</pre>
        </div>`).join('')}`;
    $$('#wz-client button', box).forEach((b) => b.onclick = () => { state.client = b.dataset.c; render(); });
    $('#wz-key', box).onchange = (e) => { state.key = e.target.value; render(); };
    $('#wz-model', box).onchange = (e) => { state.model = e.target.value; render(); };
    if ($('#wz-small', box)) $('#wz-small', box).onchange = (e) => { state.small = e.target.value; render(); };
    if ($('#wz-reveal', box)) $('#wz-reveal', box).onchange = (e) => { state.reveal = e.target.checked; render(); };
    $$('[data-copy-i]', box).forEach((b) => b.onclick = () => copyText(snippets[Number(b.dataset.copyI)].text));
  };
  render();
}

const HOOK_TYPES = {
  feishu: { label: t('飞书 / Lark'), help: t('群设置 → 群机器人 → 添加自定义机器人。开启“签名校验”时把密钥填在右边；用“自定义关键词”时关键词填 AI Route') },
  dingtalk: { label: t('钉钉'), help: t('群设置 → 机器人 → 自定义。安全设置选“加签”时填密钥（SEC 开头）；选“自定义关键词”时关键词填 AI Route') },
  wecom: { label: t('企业微信'), help: t('群聊 → 添加群机器人，复制 Webhook 地址，不需要密钥') },
  generic: { label: t('通用 JSON'), help: t('POST JSON：{event, subject, title, text, time}') },
};

function alertHooksEditor(alerts) {
  const hooks = (alerts.webhooks || []).map((h) => ({ ...h }));
  const box = $('#al-hooks');
  const render = () => {
    box.innerHTML = hooks.length ? hooks.map((h, i) => `
      <div class="hook-row" data-i="${i}">
        <div class="toolbar">
          <select data-k="type">${Object.entries(HOOK_TYPES).map(([v, ht]) => `<option value="${v}" ${h.type === v ? 'selected' : ''}>${ht.label}</option>`).join('')}</select>
          <input type="text" data-k="name" value="${esc(h.name || '')}" placeholder="${esc(t('名称（可选）'))}" style="width:140px">
          <label class="check"><input type="checkbox" data-k="enabled" ${h.enabled ? 'checked' : ''}> ${t('启用')}</label>
          <span class="btns" style="margin-left:auto"><button type="button" class="btn sm" data-test>${t('发送测试')}</button><button type="button" class="btn sm danger" data-del>${t('删除')}</button></span>
        </div>
        <div class="toolbar" style="margin-top:6px">
          <input type="text" data-k="url" value="${esc(h.url || '')}" placeholder="${esc(t('Webhook 地址'))}" style="flex:2;min-width:240px">
          ${h.type === 'feishu' || h.type === 'dingtalk' ? `<input type="password" data-k="secret" value="${esc(h.secret || '')}" placeholder="${esc(t('签名密钥（可选）'))}" style="flex:1;min-width:160px" autocomplete="new-password">` : ''}
        </div>
        <div class="help">${HOOK_TYPES[h.type] ? esc(HOOK_TYPES[h.type].help) : ''} <span data-result></span></div>
      </div>`).join('') : `<div class="muted small">${t('还没有 Webhook，添加后才会推送')}</div>`;
    $$('.hook-row', box).forEach((row) => {
      const h = hooks[Number(row.dataset.i)];
      $$('[data-k]', row).forEach((el) => {
        const k = el.dataset.k;
        el.onchange = el.oninput = () => {
          h[k] = el.type === 'checkbox' ? el.checked : el.value;
          if (k === 'type') render();
        };
      });
      $('[data-del]', row).onclick = () => { hooks.splice(Number(row.dataset.i), 1); render(); };
      $('[data-test]', row).onclick = async (e) => {
        const out = $('[data-result]', row);
        e.target.disabled = true;
        out.className = 'muted'; out.textContent = t('发送中…');
        try {
          const r = await api('POST', '/alerts/test', h);
          out.className = r.ok ? 'ok-text' : 'err-text';
          out.textContent = r.ok ? t('已发送，请到群里查看') : t('发送失败：{err}', { err: r.error });
        } catch (err) { out.className = 'err-text'; out.textContent = err.message; }
        e.target.disabled = false;
      };
    });
  };
  render();
  $('#al-add').onclick = () => { hooks.push({ type: 'feishu', name: '', url: '', secret: '', enabled: true }); render(); };
  $('#al-save').onclick = async () => {
    try {
      await api('PUT', '/alerts', {
        webhooks: hooks,
        on_auth_failure: $('#al-auth').checked, on_all_failed: $('#al-all').checked, on_long_cooldown: $('#al-cool').checked, on_health_check: $('#al-health').checked,
        long_cooldown_minutes: Number($('#al-cool-min').value), silence_minutes: Number($('#al-silence').value),
        language: $('#al-lang').value,
      });
      toast(t('已保存'), 'ok');
      route();
    } catch (e) { toast(e.message, 'err'); }
  };
}

// ---------------------------------------------------------------- boot
route();
