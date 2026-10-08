'use strict';

// ---------------------------------------------------------------- helpers
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => Array.from(el.querySelectorAll(s));
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const TOKEN_KEY = 'ai_route_admin_token';
let TOKEN = '';
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
    renderLogin('登录已失效，请重新输入管理令牌');
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
  if (!ms) return '从未';
  const s = (Date.now() - ms) / 1000;
  if (s < 60) return '刚刚';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
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

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast('已复制', 'ok');
  } catch (e) {
    const ta = document.createElement('textarea');
    ta.value = text; document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); toast('已复制', 'ok'); } catch (e2) { toast('复制失败', 'err'); }
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
      <div class="modal-head"><span>确认</span></div>
      <div class="modal-body">${esc(msg)}</div>
      <div class="modal-foot"><button class="btn" data-no>取消</button><button class="btn primary" data-yes>确定</button></div>
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
const PAGES = [
  ['dashboard', '概览'],
  ['providers', '供应商'],
  ['models', '模型映射'],
  ['keys', 'API Keys'],
  ['logs', '请求日志'],
  ['settings', '设置与接入'],
];

function renderLogin(msg) {
  closeModal();
  $('#app').innerHTML = `
    <div class="login card">
      <div class="card-head">AI Route 控制台</div>
      <div class="card-body form">
        ${msg ? `<div class="err-text small">${esc(msg)}</div>` : ''}
        <div class="field">
          <label>管理令牌</label>
          <input type="password" id="login-token" placeholder="ADMIN_TOKEN，或首次启动时日志里打印的 admin-xxx">
          <div class="help">令牌只保存在本浏览器。</div>
        </div>
        <button class="btn primary" id="login-btn">进入</button>
      </div>
    </div>`;
  const go = async () => {
    TOKEN = $('#login-token').value.trim();
    try {
      await api('GET', '/ping');
      try { localStorage.setItem(TOKEN_KEY, TOKEN); } catch (e) { /* ignore */ }
      renderShell();
    } catch (e) { /* api() re-renders login on 401 */ }
  };
  $('#login-btn').onclick = go;
  $('#login-token').addEventListener('keydown', (e) => { if (e.key === 'Enter') go(); });
  $('#login-token').focus();
}

function renderShell() {
  $('#app').innerHTML = `
    <div class="layout">
      <aside class="sidebar">
        <div class="brand"><span class="dot"></span>AI Route</div>
        <nav class="nav">${PAGES.map(([id, name]) => `<a href="#/${id}" data-page="${id}">${name}</a>`).join('')}</nav>
        <div class="foot"><button class="btn sm" id="logout">退出登录</button></div>
      </aside>
      <main class="main" id="page"></main>
    </div>`;
  $('#logout').onclick = () => {
    try { localStorage.removeItem(TOKEN_KEY); } catch (e) { /* ignore */ }
    TOKEN = '';
    renderLogin();
  };
  route();
}

let refreshTimer = null;
function route() {
  if (!TOKEN) return renderLogin();
  if (!$('#page')) return renderShell();
  clearInterval(refreshTimer);
  const id = (location.hash.replace(/^#\//, '') || 'dashboard').split('?')[0];
  $$('.nav a').forEach((a) => a.classList.toggle('active', a.dataset.page === id));
  const fn = { dashboard: pageDashboard, providers: pageProviders, models: pageModels, keys: pageKeys, logs: pageLogs, settings: pageSettings }[id] || pageDashboard;
  $('#page').innerHTML = '<div class="empty">加载中…</div>';
  fn().catch((e) => {
    if (e.status !== 401) $('#page').innerHTML = `<div class="empty err-text">加载失败：${esc(e.message)}</div>`;
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
function targetHealth(t, idx, providers) {
  const prefix = t.split('/')[0];
  const p = providers.find((x) => x.prefix === prefix);
  if (!p) return { cls: 'missing', tip: '套餐前缀不存在' };
  if (!p.enabled) return { cls: 'missing', tip: '套餐已停用' };
  const ps = idx.provider[prefix];
  const ts = idx.target[t];
  if (ps && ps.open) return { cls: 'cool', tip: `套餐冷却中，剩余 ${fmtSecs(ps.remaining_seconds)}：${ps.last_error}` };
  if (ts && ts.open) return { cls: 'cool', tip: `冷却中，剩余 ${fmtSecs(ts.remaining_seconds)}：${ts.last_error}` };
  if (ts && ts.failures > 0) return { cls: '', tip: `连续失败 ${ts.failures} 次：${ts.last_error}` };
  return { cls: '', tip: '正常' };
}
function chainHTML(targets, idx, providers) {
  if (!targets.length) return '<span class="muted small">未配置</span>';
  return `<div class="chain">${targets.map((t, i) => {
    const h = targetHealth(t, idx, providers);
    return `${i ? '<span class="arrow">→</span>' : ''}<span class="target ${h.cls}" title="${esc(h.tip)}"><span class="s"></span>${esc(t)}</span>`;
  }).join('')}</div>`;
}

// ---------------------------------------------------------------- dashboard
let dashRange = '24h';
async function pageDashboard() {
  const [stats, status, models, providers] = await Promise.all([
    api('GET', '/stats?range=' + dashRange), api('GET', '/status'), api('GET', '/models'), api('GET', '/providers'),
  ]);
  const t = stats.total;
  const idx = healthIndex(status);
  const cooling = status.filter((s) => s.open);
  const ranges = [['1h', '1 小时'], ['24h', '24 小时'], ['7d', '7 天'], ['30d', '30 天']];
  const max = Math.max(1, ...stats.timeline.map((r) => r.requests));
  const bars = stats.timeline.map((r) => {
    const h = (r.requests / max) * 100;
    const fh = r.requests ? (r.failed / r.requests) * h : 0;
    return `<div class="bar" style="height:${h}%" title="${esc(r.key)}：${r.requests} 次，失败 ${r.failed}，切换 ${r.fallback}"><div class="ok" style="flex:${h - fh}"></div><div class="fail" style="flex:${fh}"></div></div>`;
  }).join('');
  const rowTable = (rows, label) => rows.length ? `
    <div class="table-wrap"><table>
      <tr><th>${label}</th><th class="num">请求</th><th class="num">成功率</th><th class="num">切换</th><th class="num">输入</th><th class="num">输出</th><th class="num">费用</th><th class="num">平均耗时</th><th class="num">首字</th></tr>
      ${rows.map((r) => `<tr><td>${esc(r.key || '-')}</td><td class="num">${fmtNum(r.requests)}</td><td class="num">${pct(r.success, r.requests)}</td><td class="num">${fmtNum(r.fallback)}</td><td class="num">${fmtNum(r.input_tokens)}</td><td class="num">${fmtNum(r.output_tokens)}</td><td class="num" ${r.unpriced ? `title="${r.unpriced} 次请求没有配置单价，未计入"` : ''}>${r.unpriced && !r.cost ? '-' : fmtMoney(r.cost, stats.currency)}${r.unpriced && r.cost ? '*' : ''}</td><td class="num">${fmtMs(r.avg_latency_ms)}</td><td class="num">${r.avg_ttfb_ms ? fmtMs(r.avg_ttfb_ms) : '-'}</td></tr>`).join('')}
    </table></div>` : '<div class="empty">暂无数据</div>';

  $('#page').innerHTML = `
    ${head('概览', '请求量、成功率、候补切换与上游健康状态', `
      <select id="dash-range">${ranges.map(([v, l]) => `<option value="${v}" ${v === dashRange ? 'selected' : ''}>${l}</option>`).join('')}</select>
      <button class="btn" id="dash-refresh">刷新</button>`)}
    <div class="grid cols-5">
      <div class="card stat"><div class="label">请求数</div><div class="value">${fmtNum(t.requests)}</div><div class="hint">失败 ${fmtNum(t.failed)}</div></div>
      <div class="card stat"><div class="label">成功率</div><div class="value">${pct(t.success, t.requests)}</div><div class="hint">平均耗时 ${fmtMs(t.avg_latency_ms)}</div></div>
      <div class="card stat"><div class="label">发生候补切换</div><div class="value">${fmtNum(t.fallback)}</div><div class="hint">由非首选上游完成的请求</div></div>
      <div class="card stat"><div class="label">Tokens（输入 / 输出）</div><div class="value">${fmtNum(t.input_tokens)} / ${fmtNum(t.output_tokens)}</div><div class="hint">缓存命中 ${fmtNum(t.cached_tokens)}</div></div>
      <div class="card stat"><div class="label">费用</div><div class="value">${t.cost ? fmtMoney(t.cost, stats.currency) : '-'}</div><div class="hint">${t.unpriced ? `${fmtNum(t.unpriced)} 次未配单价，未计入` : '按上游实际费用或配置的单价'}</div></div>
    </div>
    <div class="card">
      <div class="card-head">请求趋势 <span class="muted small">蓝色：成功 · 红色：失败</span></div>
      <div class="card-body">${stats.timeline.length ? `<div class="bars">${bars}</div><div class="bars-axis"><span>${esc(stats.timeline[0].key)}</span><span>${esc(stats.timeline[stats.timeline.length - 1].key)}</span></div>` : '<div class="empty">暂无数据</div>'}</div>
    </div>
    <div class="card">
      <div class="card-head">上游健康 <span class="btns">${cooling.length ? `<span class="badge warn">${cooling.length} 个冷却中</span>` : '<span class="badge ok">全部正常</span>'}<button class="btn sm" id="reset-all">全部重置</button></span></div>
      <div class="card-body" style="padding:0">
        ${models.length ? `<div class="table-wrap"><table>
          <tr><th>对外模型</th><th>调度顺序（从左到右依次尝试）</th></tr>
          ${models.filter((m) => m.enabled).map((m) => `<tr><td><b>${esc(m.name)}</b></td><td>${chainHTML(m.targets, idx, providers)}</td></tr>`).join('')}
        </table></div>` : '<div class="empty">还没有配置模型映射</div>'}
        ${cooling.length ? `<div class="table-wrap"><table>
          <tr><th>冷却对象</th><th>剩余</th><th>最近错误</th><th></th></tr>
          ${cooling.map((s) => `<tr><td><span class="badge ${s.kind === 'provider' ? 'err' : 'warn'}">${s.kind === 'provider' ? '整个套餐' : '模型'}</span> ${esc(s.name)}</td><td>${fmtSecs(s.remaining_seconds)}</td><td class="small err-text"><div class="ellipsis" title="${esc(s.last_error)}">${esc(s.last_error)}</div></td><td><button class="btn sm" data-reset="${esc(s.key)}">恢复</button></td></tr>`).join('')}
        </table></div>` : ''}
      </div>
    </div>
    <div class="grid cols-2">
      <div class="card"><div class="card-head">按对外模型</div>${rowTable(stats.by_model, '模型')}</div>
      <div class="card"><div class="card-head">按实际上游</div>${rowTable(stats.by_target, '上游')}</div>
    </div>
    <div class="grid cols-2">
      <div class="card"><div class="card-head">按套餐</div>${rowTable(stats.by_provider, '套餐')}</div>
      <div class="card"><div class="card-head">按 API Key</div>${rowTable(stats.by_key, 'Key')}</div>
    </div>`;
  $('#dash-range').onchange = (e) => { dashRange = e.target.value; route(); };
  $('#dash-refresh').onclick = route;
  $('#reset-all').onclick = async () => { await api('POST', '/status/reset', { key: '' }); toast('已重置全部熔断状态', 'ok'); route(); };
  $$('[data-reset]').forEach((b) => b.onclick = async () => { await api('POST', '/status/reset', { key: b.dataset.reset }); toast('已恢复', 'ok'); route(); });
  refreshTimer = setInterval(() => { if (!$('.modal-bg') && location.hash.startsWith('#/dashboard')) route(); }, 30000);
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
      throw new Error('单价格式不对：' + line);
    }
    const [input, output] = [Number(nums[0]), Number(nums[nums.length - 1])];
    prices[k] = nums.length === 3 ? { input, cache: Number(nums[1]), output } : { input, output };
  }
  return prices;
}
const toPriceLines = (prices) => Object.entries(prices || {})
  .map(([k, v]) => `${k} = ${v.input} / ${v.cache != null ? v.cache + ' / ' : ''}${v.output}`).join('\n');
const usesProvider = (m, prefix) => m.targets.some((t) => t.startsWith(prefix + '/'));

function compatOf(p) {
  if (p.openai_base_url && p.anthropic_base_url) return 'both';
  if (p.anthropic_base_url) return 'anthropic';
  return 'openai';
}
const COMPAT_LABEL = { openai: 'OpenAI 兼容', anthropic: 'Anthropic 兼容', both: 'OpenAI + Anthropic' };

function vendorBadge(p) {
  const ps = presetById(guessVendor(p));
  return ps ? `<span class="badge cat-${ps.category}" title="${esc(ps.name)}">${esc(CATEGORY_LABEL[ps.category])}</span>` : '<span class="badge">自定义</span>';
}

async function pageProviders() {
  const [providers, status, models] = await Promise.all([api('GET', '/providers'), api('GET', '/status'), api('GET', '/models')]);
  const idx = healthIndex(status);
  $('#page').innerHTML = `
    ${head('供应商', '编码套餐、官方 API、聚合平台都在这里配置。每个供应商有一个前缀，它的模型在映射里显示为 <code>前缀/模型名</code>', '<button class="btn primary" id="add-provider">+ 添加供应商</button>')}
    ${providers.length ? providers.map((p) => {
      const ps = idx.provider[p.prefix];
      const used = models.filter((m) => usesProvider(m, p.prefix)).map((m) => m.name);
      let st = p.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge">停用</span>';
      if (p.enabled && ps && ps.open) st = `<span class="badge warn" title="${esc(ps.last_error)}">冷却中 ${fmtSecs(ps.remaining_seconds)}</span>`;
      return `<div class="card">
        <div class="card-head">
          <div class="toolbar"><code>${esc(p.prefix)}</code> <span>${esc(p.name || p.prefix)}</span> ${st}
            ${vendorBadge(p)} <span class="badge blue">${COMPAT_LABEL[compatOf(p)]}</span>
            ${p.ua_mode === 'override' ? `<span class="badge warn" title="${esc(p.user_agent)}">固定 UA</span>` : ''}</div>
          <div class="btns">
            <button class="btn sm" data-test="${p.id}">测试</button>
            <button class="btn sm" data-sync="${p.id}">同步模型</button>
            <button class="btn sm" data-edit="${p.id}">编辑</button>
            <button class="btn sm danger" data-del="${p.id}">删除</button>
          </div>
        </div>
        <div class="card-body form">
          <div class="kv">
            ${p.openai_base_url ? `<div class="k">OpenAI 地址</div><div class="mono small">${esc(p.openai_base_url)}</div>` : ''}
            ${p.anthropic_base_url ? `<div class="k">Anthropic 地址</div><div class="mono small">${esc(p.anthropic_base_url)}</div>` : ''}
            <div class="k">API Key</div><div class="mono small">${p.has_api_key ? esc(p.api_key) : '<span class="err-text">未设置</span>'}</div>
            <div class="k">User-Agent</div><div class="small">${p.ua_mode === 'override' ? `固定：<span class="mono">${esc(p.user_agent)}</span>` : `透传客户端${p.user_agent ? `（缺省：<span class="mono">${esc(p.user_agent)}</span>）` : ''}`}</div>
            <div class="k">被映射引用</div><div class="small">${used.length ? used.map(esc).join('，') : '<span class="muted">无</span>'}</div>
            ${p.remark ? `<div class="k">备注</div><div class="small">${esc(p.remark)}</div>` : ''}
          </div>
          <div>
            <div class="muted small" style="margin-bottom:6px">模型（${p.models.length}）</div>
            ${p.models.length ? `<div class="chips">${p.models.map((x) => `<span class="chip"><span class="pfx">${esc(p.prefix)}/</span>${esc(x)}</span>`).join('')}</div>` : '<div class="muted small">还没有模型，点“同步模型”从上游拉取，或在编辑里手动添加</div>'}
          </div>
        </div>
      </div>`;
    }).join('') : '<div class="card"><div class="empty">还没有套餐。点右上角“添加套餐”，选好供应商后会自动预填地址</div></div>'}`;
  const find = (id) => providers.find((p) => p.id == id);
  $('#add-provider').onclick = () => providerForm(null, models, providers);
  $$('[data-edit]').forEach((b) => b.onclick = () => providerForm(find(b.dataset.edit), models, providers));
  $$('[data-test]').forEach((b) => b.onclick = () => providerTest(find(b.dataset.test)));
  $$('[data-sync]').forEach((b) => b.onclick = async () => {
    b.disabled = true; b.textContent = '同步中…';
    try {
      const r = await api('POST', `/providers/${b.dataset.sync}/sync-models`);
      toast(r.added.length ? `新增 ${r.added.length} 个模型：${r.added.slice(0, 5).join('、')}${r.added.length > 5 ? '…' : ''}` : '没有新模型', 'ok');
      route();
    } catch (e) {
      toast('拉取失败：' + e.message + '（部分套餐不开放模型列表接口，可在编辑里手动添加）', 'err');
      b.disabled = false; b.textContent = '同步模型';
    }
  });
  $$('[data-del]').forEach((b) => b.onclick = async () => {
    const p = find(b.dataset.del);
    const used = models.filter((m) => usesProvider(m, p.prefix));
    const warn = used.length ? `（${used.map((m) => m.name).join('、')} 的调度顺序里用到了它，删除后这些条目会被跳过）` : '';
    if (!(await confirmBox(`删除套餐 ${p.prefix}？${warn}`))) return;
    await api('DELETE', '/providers/' + p.id);
    toast('已删除', 'ok');
    route();
  });
}

// chip editor for a provider's model list
function modelChips(root, state) {
  const box = $('#pf-models', root);
  const prefix = $('#pf-prefix', root).textContent.trim() || '前缀';
  box.innerHTML = state.models.length
    ? state.models.map((x, i) => `<span class="chip ${state.fresh.has(x) ? 'new' : ''}"><span class="pfx">${esc(prefix)}/</span>${esc(x)}<button type="button" data-rm="${i}" title="移除">×</button></span>`).join('')
    : '<span class="muted small">还没有模型</span>';
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

const UA_LABEL = { passthrough: '透传客户端', override: '固定 UA' };

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
  return `
    <div class="toolbar" style="margin-bottom:8px">
      <div class="seg" id="pp-cat">${[['all', '全部'], ...PRESET_CATEGORIES].map(([k, v]) => `<button type="button" data-cat="${k}" class="${k === 'all' ? 'on' : ''}">${v}</button>`).join('')}</div>
      <input type="text" id="pp-search" placeholder="搜索供应商" style="flex:1;min-width:140px">
    </div>
    <div class="preset-grid" id="pp-grid">
      <button type="button" class="preset-card ${!selected ? 'on' : ''}" data-preset="">
        <span class="pn">自定义</span><span class="pc">手动填写地址</span>
      </button>
      ${PRESETS.map((ps) => `<button type="button" class="preset-card ${selected === ps.id ? 'on' : ''}" data-preset="${esc(ps.id)}" data-cat="${ps.category}" data-q="${esc((ps.name + ' ' + ps.id + ' ' + (ps.keywords || '')).toLowerCase())}">
        <span class="pn">${esc(ps.name)}</span><span class="pc cat-${ps.category}">${esc(CATEGORY_LABEL[ps.category] || '')}</span>
      </button>`).join('')}
    </div>`;
}

function presetInfoHTML(ps) {
  if (!ps) return '<span class="muted">自定义供应商：手动填写地址、协议和模型。</span>';
  const links = [
    ps.docs ? `<a href="${esc(ps.docs)}" target="_blank" rel="noopener">官方文档 ↗</a>` : '',
    ps.keyUrl ? `<a href="${esc(ps.keyUrl)}" target="_blank" rel="noopener">获取 API Key ↗</a>` : '',
  ].filter(Boolean).join(' · ');
  return `<b>${esc(ps.name)}</b> <span class="badge">${esc(CATEGORY_LABEL[ps.category] || '')}</span> ${links}
    ${ps.note ? `<div style="margin-top:4px">${esc(ps.note)}</div>` : ''}
    ${ps.ua && ps.ua.note ? `<div style="margin-top:4px"><b>User-Agent：</b>${esc(ps.ua.note)}</div>` : ''}
    ${ps.verified === false ? '<div class="small" style="margin-top:4px">⚠ 该预设地址未能从官方文档完全确认，保存前请点“测试”验证。</div>' : ''}`;
}

function providerForm(p, models, providers) {
  const isNew = !p;
  p = p || { prefix: '', name: '', vendor: '', openai_base_url: '', anthropic_base_url: '', api_key: '', headers: {}, model_protocols: {}, models: [], ua_mode: 'passthrough', user_agent: '', timeout_seconds: 300, prices: {}, currency: 'CNY', enabled: true, remark: '' };
  const state = {
    vendor: guessVendor(p),
    models: [...(p.models || [])],
    fresh: new Set(),
    compat: isNew ? 'both' : compatOf(p),
    ua: p.ua_mode || 'passthrough',
  };
  const cur = presetById(state.vendor);
  openModal({
    title: isNew ? '添加供应商' : '编辑供应商 ' + p.prefix,
    wide: true,
    body: `<div class="form">
      <div class="field">
        <label>选择供应商</label>
        ${isNew ? '' : `<div class="toolbar" id="pp-current"><span>当前：<b>${esc(cur ? cur.name : '自定义')}</b></span><button type="button" class="btn sm" id="pp-toggle">更换预设</button></div>`}
        <div id="pp-wrap" style="${isNew ? '' : 'display:none'}">${presetGrid(state.vendor)}</div>
        <div class="hint-box small" id="pp-info" style="margin-top:8px">${presetInfoHTML(cur)}</div>
      </div>
      <div class="row2">
        <div class="field"><label>名称</label><input type="text" id="pf-name" value="${esc(p.name)}" placeholder="如 Kimi Code"></div>
        <div class="field"><label>前缀（自动生成）</label><div class="prefix-box"><code id="pf-prefix">${esc(p.prefix)}</code></div><div class="help">${isNew ? '按预设或地址域名自动生成，重名时加数字后缀；' : '创建后固定不变；'}模型在映射里显示为 前缀/模型名</div></div>
      </div>
      <div class="field"><label>兼容方案</label>
        <div class="seg" id="pf-compat">${['openai', 'anthropic', 'both'].map((c) => `<button type="button" data-c="${c}">${COMPAT_LABEL[c]}</button>`).join('')}</div>
        <div class="help">客户端用哪种协议请求就优先走同协议地址，没有则自动转换</div>
      </div>
      <div class="field" data-for="openai"><label>OpenAI 兼容地址</label><input type="text" id="pf-openai" value="${esc(p.openai_base_url)}" placeholder="https://xxx/v1（自动拼接 /chat/completions）"></div>
      <div class="field" data-for="anthropic"><label>Anthropic 兼容地址</label><input type="text" id="pf-anthropic" value="${esc(p.anthropic_base_url)}" placeholder="即 ANTHROPIC_BASE_URL（自动拼接 /v1/messages）"></div>
      <div class="field"><label>API Key ${isNew ? '*' : ''} <span id="pf-keylink"></span></label><input type="password" id="pf-key" placeholder="${isNew ? '' : '留空表示不修改（当前 ' + esc(p.api_key) + '）'}" autocomplete="new-password"></div>
      <div class="field">
        <label>模型列表（<span id="pf-models-count"></span>）</label>
        <div class="chips" id="pf-models"></div>
        <div class="toolbar" style="margin-top:8px">
          <input type="text" id="pf-model-input" placeholder="手动输入模型名，回车添加（可一次粘贴多个）" style="flex:1">
          <button type="button" class="btn" id="pf-model-add">添加</button>
          <button type="button" class="btn" id="pf-fetch">从上游拉取</button>
          <button type="button" class="btn danger" id="pf-clear">清空</button>
        </div>
        <div class="help" id="pf-fetch-msg">填好地址和 Key 后会自动尝试拉取；拉不到（很多 coding plan 不开放 /models）就手动添加</div>
      </div>
      <div class="field"><label>User-Agent</label>
        <div class="toolbar">
          <div class="seg" id="pf-ua">${['passthrough', 'override'].map((c) => `<button type="button" data-u="${c}">${UA_LABEL[c]}</button>`).join('')}</div>
          <input type="text" id="pf-ua-value" value="${esc(p.user_agent)}" style="flex:1;min-width:200px">
        </div>
        <div class="help" id="pf-ua-help"></div>
      </div>
      <details>
        <summary class="small muted">高级设置</summary>
        <div class="form" style="margin-top:10px">
          <div class="row2">
            <div class="field"><label>超时（秒）</label><input type="number" id="pf-timeout" value="${p.timeout_seconds}" min="5"><div class="help">非流式为整个请求；流式为首包和事件间隔</div></div>
            <div class="field"><label>备注</label><input type="text" id="pf-remark" value="${esc(p.remark)}"></div>
          </div>
          <div class="row2">
            <div class="field"><label>模型协议规则</label><textarea id="pf-protos" placeholder="minimax-* = anthropic&#10;glm-* = openai">${esc(toLines(p.model_protocols, ' ='))}</textarea><div class="help">每行 <code>模型(可用*) = openai|anthropic</code>，某些模型只在一种端点提供时使用</div></div>
            <div class="field"><label>自定义请求头</label><textarea id="pf-headers" placeholder="X-Custom: value">${esc(toLines(p.headers, ':'))}</textarea><div class="help">每行 <code>Header: 值</code>，值留空表示删除该头</div></div>
          </div>
          <div class="field"><label>单价（每百万 tokens，用于成本核算）
              <select id="pf-currency" style="margin-left:8px">${['CNY', 'USD'].map((c) => `<option value="${c}" ${(p.currency || 'CNY') === c ? 'selected' : ''}>${c === 'CNY' ? '人民币 ¥' : '美元 $'}</option>`).join('')}</select></label>
            <textarea id="pf-prices" placeholder="glm-5.3 = 4 / 0.8 / 16&#10;deepseek-* = 2 / 8">${esc(toPriceLines(p.prices))}</textarea>
            <div class="help">每行 <code>模型(可用*) = 输入 / 缓存命中 / 输出</code>，缓存价可省略（按输入价计）。没配单价的模型不计费用；包月套餐可以写 <code>* = 0 / 0</code>，表示不另外收费，概览里就不会算作“未配单价”。OpenRouter 会直接使用上游返回的实际费用，不需要配置。</div></div>
        </div>
      </details>
      <label class="check"><input type="checkbox" id="pf-enabled" ${p.enabled ? 'checked' : ''}> 启用</label>
    </div>`,
    foot: `<button class="btn" data-close>取消</button><button class="btn primary" id="pf-save">保存</button>`,
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
        input.placeholder = u === 'override' ? '必填，所有请求都使用这个 UA' : '可选，客户端没带 UA 时使用';
        const ps = presetById(state.vendor);
        $('#pf-ua-help', m).innerHTML = (u === 'override'
          ? '所有发往该供应商的请求都改用这里填写的 User-Agent。'
          : '默认把调用方（Claude Code、Cursor 等）的真实 User-Agent 原样转发。限制客户端类型的套餐（如 Kimi Code）需要保持这个选项。')
          + (ps && ps.ua && ps.ua.note ? ` <b>${esc(ps.ua.note)}</b>` : '');
      };
      const setKeyLink = () => {
        const ps = presetById(state.vendor);
        $('#pf-keylink', m).innerHTML = ps && ps.keyUrl ? `<a href="${esc(ps.keyUrl)}" target="_blank" rel="noopener" class="small">获取 ↗</a>` : '';
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
        if (isNew || !$('#pf-name', m).value) $('#pf-name', m).value = ps.name;
        refreshPrefix();
        $('#pf-openai', m).value = ps.openai || '';
        $('#pf-anthropic', m).value = ps.anthropic || '';
        $('#pf-protos', m).value = toLines(ps.protocols || {}, ' =');
        $('#pf-headers', m).value = toLines(ps.headers || {}, ':');
        if (isNew) $('#pf-currency', m).value = ps.currency || 'CNY';
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
          $('#pp-current b', m).textContent = ps.name;
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
        ua_mode: state.ua,
        user_agent: $('#pf-ua-value', m).value.trim(),
      });
      let fetching = false;
      const doFetch = async (auto) => {
        const body = formBody();
        if (fetching || (!body.openai_base_url && !body.anthropic_base_url) || (!body.api_key && isNew)) {
          if (!auto) toast('请先填写地址和 API Key', 'err');
          return;
        }
        fetching = true;
        const msg = $('#pf-fetch-msg', m);
        msg.textContent = '正在从上游拉取模型列表…';
        msg.className = 'help';
        try {
          const ids = await api('POST', '/providers/fetch-models', body);
          state.fresh = new Set(ids.filter((x) => !state.models.includes(x)));
          addModels(state, ids.join(' '));
          modelChips(m, state);
          msg.textContent = `拉取到 ${ids.length} 个模型，新增 ${state.fresh.size} 个（绿色标出）。不需要的可以点 × 移除。`;
          msg.className = 'help ok-text';
        } catch (e) {
          msg.textContent = `拉取失败：${e.message}。可以手动添加模型名。`;
          msg.className = 'help err-text';
        }
        fetching = false;
      };
      $('#pf-fetch', m).onclick = () => doFetch(false);
      // auto-fetch once the key is filled in
      $('#pf-key', m).addEventListener('change', () => doFetch(true));

      $('#pf-save', m).onclick = async () => {
        addFromInput();
        let prices;
        try { prices = parsePrices($('#pf-prices', m).value); } catch (e) { return toast(e.message, 'err'); }
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
          currency: $('#pf-currency', m).value,
          enabled: $('#pf-enabled', m).checked,
        };
        // drop protocol rules pointing at an endpoint that is no longer configured
        for (const [k, v] of Object.entries(body.model_protocols)) {
          if ((v === 'openai' && !body.openai_base_url) || (v === 'anthropic' && !body.anthropic_base_url)) delete body.model_protocols[k];
        }
        if (!body.prefix) return toast('无法生成前缀，请先选择供应商或填写地址', 'err');
        if (/\$\{/.test(body.openai_base_url + body.anthropic_base_url)) return toast('地址里还有 ${...} 占位符，请替换成你自己的值', 'err');
        if (body.ua_mode === 'override' && !body.user_agent) return toast('固定 UA 模式需要填写 User-Agent', 'err');
        const presetURL = (presetById(state.vendor) || {}).openai;
        if (body.openai_base_url && body.openai_base_url !== presetURL && !/\/v\d+$|\/chat\/completions$/.test(body.openai_base_url)
          && !(await confirmBox(`OpenAI 兼容地址一般以版本号结尾（如 /v1、/v3、/paas/v4），网关会在后面拼接 /chat/completions。当前地址 ${body.openai_base_url} 可能会 404，确定保存？`))) return;
        if (isNew && !body.api_key && !(await confirmBox('没有填写 API Key，确定保存？'))) return;
        try {
          if (isNew) await api('POST', '/providers', body);
          else await api('PUT', '/providers/' + p.id, body);
          closeModal();
          toast('已保存', 'ok');
          route();
        } catch (e) { toast(e.message, 'err'); }
      };
    },
  });
}

function testResultHTML(r) {
  const atts = (r.attempts || []).map((a) => `<tr><td class="mono small">${esc(a.target)}${a.retry ? ` <span class="badge">重试 ${a.retry}</span>` : ''}</td><td>${esc(a.protocol)}</td><td>${a.http_status || '-'}</td><td>${fmtMs(a.latency_ms)}</td><td class="small ${a.error ? 'err-text' : 'ok-text'}">${a.error ? esc(a.error) : '成功'}${a.cooling ? ' <span class="badge warn">冷却中兜底</span>' : ''}</td></tr>`).join('');
  return `<div class="form">
    <div class="kv">
      <div class="k">结果</div><div>${r.ok ? '<span class="badge ok">成功</span>' : '<span class="badge err">失败</span>'} HTTP ${r.http_status}，耗时 ${fmtMs(r.latency_ms)}</div>
      ${r.target ? `<div class="k">实际上游</div><div class="mono">${esc(r.target)}</div>` : ''}
      ${r.reply ? `<div class="k">回复</div><div>${esc(r.reply)}</div>` : ''}
    </div>
    ${atts ? `<div class="table-wrap"><table><tr><th>尝试</th><th>协议</th><th>状态</th><th>耗时</th><th>结果</th></tr>${atts}</table></div>` : ''}
    ${(r.attempts || []).some((a) => a.http_status === 403) ? '<div class="hint-box small">返回 403：部分编码套餐只允许特定客户端（按 User-Agent 识别），后台测试的 UA 是 ai-route-admin-test，可能被拒绝。请用实际客户端（如 Claude Code）经网关调用一次确认。</div>' : ''}
    <details><summary class="small muted">原始响应</summary><pre class="box">${esc(r.raw)}</pre></details>
  </div>`;
}

function providerTest(p) {
  openModal({
    title: '测试套餐 ' + p.prefix,
    wide: true,
    body: `<div class="form">
      <div class="row2">
        <div class="field"><label>模型</label><input type="text" id="pt-model" list="pt-models" value="${esc(p.models[0] || '')}"><datalist id="pt-models">${p.models.map((x) => `<option value="${esc(x)}">`).join('')}</datalist></div>
        <div class="field"><label>协议</label><select id="pt-proto"><option value="">自动</option>${p.openai_base_url ? '<option value="openai">OpenAI</option>' : ''}${p.anthropic_base_url ? '<option value="anthropic">Anthropic</option>' : ''}</select></div>
      </div>
      <label class="check"><input type="checkbox" id="pt-stream"> 流式</label>
      <div class="muted small">直连该套餐发一句简短的测试消息，不重试、不经过熔断，也不记日志。</div>
      <div id="pt-result"></div>
    </div>`,
    foot: `<button class="btn" data-close>关闭</button><button class="btn primary" id="pt-go">发送测试</button>`,
    onMount: (m) => {
      $('#pt-go', m).onclick = async () => {
        const btn = $('#pt-go', m);
        btn.disabled = true; btn.textContent = '测试中…';
        $('#pt-result', m).innerHTML = '';
        try {
          const r = await api('POST', `/providers/${p.id}/test`, { model: $('#pt-model', m).value.trim(), protocol: $('#pt-proto', m).value, stream: $('#pt-stream', m).checked });
          $('#pt-result', m).innerHTML = testResultHTML(r);
        } catch (e) { $('#pt-result', m).innerHTML = `<div class="err-text">${esc(e.message)}</div>`; }
        btn.disabled = false; btn.textContent = '发送测试';
      };
    },
  });
}

// ---------------------------------------------------------------- models
function tagBadges(tags) {
  return sortTags(tags || []).map((t) => {
    const i = tagInfo(t);
    return `<span class="tag tg-${i.group}" title="${esc(i.desc)}">${esc(i.label)}</span>`;
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
    ${head('模型映射', '给模型起一个对外名字，选好要映射的上游模型并排好顺序；前一个不可用时（重试后仍失败或在冷却中）自动切到下一个', '<button class="btn primary" id="add-model">+ 添加模型</button>')}
    ${usedTags.length ? `<div class="toolbar tag-filter">
      <span class="muted small">按能力筛选：</span>
      <button type="button" class="tag tg-all ${modelTagFilter ? '' : 'on'}" data-tf="">全部</button>
      ${usedTags.map((t) => { const i = tagInfo(t); return `<button type="button" class="tag tg-${i.group} ${modelTagFilter === t ? 'on' : ''}" data-tf="${esc(t)}" title="${esc(i.desc)}">${esc(i.label)}</button>`; }).join('')}
    </div>` : ''}
    <div class="card">
      ${shown.length ? `<div class="table-wrap"><table>
        <tr><th>对外模型名</th><th>调度顺序</th><th>状态</th><th></th></tr>
        ${shown.map((m) => `<tr>
          <td><b class="copy" data-copy="${esc(m.name)}" title="点击复制">${esc(m.name)}</b>
            ${m.tags.length ? `<div class="tags">${tagBadges(m.tags)}</div>` : ''}
            ${m.description ? `<div class="small muted">${esc(m.description)}</div>` : ''}
            ${m.aliases.length ? `<div class="small muted">别名：${m.aliases.map(esc).join('，')}</div>` : ''}</td>
          <td>${chainHTML(m.targets, idx, providers)}</td>
          <td>${m.enabled ? '<span class="badge ok">启用</span>' : '<span class="badge">停用</span>'}</td>
          <td><div class="btns">
            <button class="btn sm" data-test="${m.id}">测试</button>
            <button class="btn sm" data-edit="${m.id}">编辑</button>
            <button class="btn sm" data-dup="${m.id}">复制</button>
            <button class="btn sm danger" data-del="${m.id}">删除</button>
          </div></td></tr>`).join('')}
      </table></div>` : `<div class="empty">${providers.length ? '还没有模型映射，点右上角添加' : '请先到“供应商套餐”添加套餐，再来建模型映射'}</div>`}
    </div>
    <div class="hint-box">
      绿点：正常 · 黄点：冷却中（排到最后兜底）· 红点：前缀不存在或套餐已停用（跳过）。<br>
      别名支持通配符 <code>*</code>，例如设置别名 <code>claude-*haiku*</code>，Claude Code 的后台小模型请求就会落到这个映射上。
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
    if (!(await confirmBox(`删除模型映射 ${m.name}？使用该模型名的客户端将无法调用。`))) return;
    await api('DELETE', '/models/' + m.id);
    toast('已删除', 'ok');
    route();
  });
}

function modelForm(m, providers, models, idx, asNew = false) {
  const isNew = !m || asNew;
  m = m || { name: '', aliases: [], targets: [], enabled: true, description: '' };
  const targets = [...m.targets];
  const tags = new Set(m.tags || []);
  let freshTags = new Set();

  const renderTags = (root) => {
    const custom = [...tags].filter((t) => !TAG_INDEX[t]);
    $('#mf-tags', root).innerHTML = TAG_GROUPS.map((g) => `
      <div class="tag-row"><span class="tag-group">${g.label}${g.single ? '<span class="muted">（单选）</span>' : ''}</span>
        <div class="chips">${g.items.map(([id, label, desc]) => `<button type="button" class="tag tg-${g.id} ${tags.has(id) ? 'on' : ''} ${freshTags.has(id) ? 'fresh' : ''}" data-tag="${id}" title="${esc(desc)}">${esc(label)}</button>`).join('')}</div>
      </div>`).join('')
      + (custom.length ? `<div class="tag-row"><span class="tag-group">自定义</span><div class="chips">${custom.map((t) => `<button type="button" class="tag tg-custom on" data-tag="${esc(t)}" title="点击移除">${esc(t)} ×</button>`).join('')}</div></div>` : '');
    $$('[data-tag]', root).forEach((b) => b.onclick = () => {
      const t = b.dataset.tag;
      const info = tagInfo(t);
      const group = TAG_GROUPS.find((g) => g.id === info.group);
      if (tags.has(t)) tags.delete(t);
      else {
        if (group && group.single) group.items.forEach(([id]) => tags.delete(id));
        tags.add(t);
      }
      freshTags.delete(t);
      renderTags(root);
    });
  };
  const all = providers.flatMap((p) => p.models.map((x) => `${p.prefix}/${x}`));

  const renderChain = (root) => {
    const box = $('#mf-chain', root);
    box.innerHTML = targets.length ? targets.map((t, i) => {
      const h = targetHealth(t, idx, providers);
      return `<div class="chain-row">
        <span class="idx">${i === 0 ? '首选' : '候补 ' + i}</span>
        <span class="target ${h.cls}" title="${esc(h.tip)}"><span class="s"></span>${esc(t)}</span>
        <span class="muted small">${h.cls === 'missing' ? esc(h.tip) : ''}</span>
        <div class="btns">
          <button type="button" class="btn sm" data-act="up" ${i === 0 ? 'disabled' : ''} title="上移">↑</button>
          <button type="button" class="btn sm" data-act="down" ${i === targets.length - 1 ? 'disabled' : ''} title="下移">↓</button>
          <button type="button" class="btn sm danger" data-act="rm" title="移除">✕</button>
        </div>
      </div>`;
    }).join('') : '<div class="muted small" style="padding:6px 0">还没有选择模型，从下面点选或手动输入</div>';
    $$('.chain-row', box).forEach((row, i) => {
      $$('[data-act]', row).forEach((b) => b.onclick = () => {
        const act = b.dataset.act;
        if (act === 'rm') targets.splice(i, 1);
        if (act === 'up' && i > 0) [targets[i - 1], targets[i]] = [targets[i], targets[i - 1]];
        if (act === 'down' && i < targets.length - 1) [targets[i + 1], targets[i]] = [targets[i], targets[i + 1]];
        renderChain(root);
        renderPicker(root);
      });
    });
  };

  const renderPicker = (root) => {
    const q = ($('#mf-filter', root).value || '').trim().toLowerCase();
    const groups = providers.map((p) => {
      const items = p.models.filter((x) => !q || `${p.prefix}/${x}`.toLowerCase().includes(q));
      if (!items.length) return '';
      return `<div class="pick-group">
        <div class="pick-head"><code>${esc(p.prefix)}</code> ${esc(p.name || '')}${p.enabled ? '' : ' <span class="badge">停用</span>'}</div>
        <div class="chips">${items.map((x) => {
          const t = `${p.prefix}/${x}`;
          const pos = targets.indexOf(t);
          return `<button type="button" class="chip pick ${pos >= 0 ? 'on' : ''}" data-t="${esc(t)}">${pos >= 0 ? `<span class="order">${pos + 1}</span>` : ''}${esc(x)}</button>`;
        }).join('')}</div>
      </div>`;
    }).join('');
    const empty = providers.filter((p) => !p.models.length).map((p) => p.prefix);
    $('#mf-picker', root).innerHTML = (groups || `<div class="muted small">${providers.some((p) => p.models.length) ? '没有匹配的模型' : '套餐里还没有模型'}</div>`)
      + (empty.length && !q ? `<div class="muted small">${empty.map((x) => `<code>${esc(x)}</code>`).join(' ')} 还没有模型列表：可到“供应商套餐”同步或添加，或在上方手动输入 <code>前缀/模型名</code></div>` : '');
    $$('[data-t]', root).forEach((b) => b.onclick = () => {
      const t = b.dataset.t;
      const pos = targets.indexOf(t);
      if (pos >= 0) targets.splice(pos, 1); else targets.push(t);
      renderChain(root);
      renderPicker(root);
    });
  };

  openModal({
    title: isNew ? '添加模型映射' : '编辑模型映射 ' + m.name,
    wide: true,
    body: `<div class="form">
      <div class="row2">
        <div class="field"><label>对外模型名 *</label><input type="text" id="mf-name" value="${esc(m.name)}" placeholder="如 dess、coder"><div class="help">客户端请求时填写的 model</div></div>
        <div class="field"><label>说明</label><input type="text" id="mf-desc" value="${esc(m.description)}"></div>
      </div>
      <div class="field"><label>别名（可选）</label><input type="text" id="mf-aliases" value="${esc(m.aliases.join(', '))}" placeholder="逗号分隔，支持 *，如 claude-sonnet-*"><div class="help">请求的 model 匹配别名时也走这个映射；精确名称优先于通配别名</div></div>
      <div class="field"><label>能力标签 <span class="muted small">（让使用方一眼看出这个模型能做什么，也会出现在 /v1/models 里）</span></label>
        <div class="tag-picker" id="mf-tags"></div>
        <div class="toolbar" style="margin-top:8px">
          <input type="text" id="mf-tag-input" placeholder="自定义标签，回车添加" style="flex:1;min-width:160px">
          <button type="button" class="btn" id="mf-suggest">根据映射的模型推荐</button>
        </div>
        <div class="help" id="mf-suggest-msg"></div>
      </div>
      <div class="field"><label>调度顺序 *（从上到下依次尝试）</label>
        <div id="mf-chain" class="chain-list"></div>
        <div class="help">短暂错误（断连、5xx、上游过载）会先在同一个模型上重试，仍失败才切到下一个；额度用尽、Key 失效、超时等直接切换。重试次数在“设置”里调整。</div>
      </div>
      <div class="field"><label>选择映射的模型（点击按顺序加入，再点一次移除）</label>
        <div class="toolbar" style="margin-bottom:8px">
          <input type="text" id="mf-filter" placeholder="搜索，或手动输入 前缀/模型名 后回车添加" list="mf-all" style="flex:1">
          <datalist id="mf-all">${all.map((x) => `<option value="${esc(x)}">`).join('')}</datalist>
          <button type="button" class="btn" id="mf-add">添加</button>
        </div>
        <div id="mf-picker" class="picker"></div>
      </div>
      <label class="check"><input type="checkbox" id="mf-enabled" ${m.enabled ? 'checked' : ''}> 启用</label>
    </div>`,
    foot: `<button class="btn" data-close>取消</button><button class="btn primary" id="mf-save">保存</button>`,
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
        if (!targets.length) { msg.textContent = '请先在下方选择映射的模型'; msg.className = 'help err-text'; return; }
        const s = suggestTags(targets);
        freshTags = new Set(s.tags.filter((t) => !tags.has(t)));
        for (const t of s.tags) {
          const group = TAG_GROUPS.find((g) => g.id === tagInfo(t).group);
          if (group && group.single) group.items.forEach(([id]) => tags.delete(id));
          tags.add(t);
        }
        renderTags(root);
        const partial = s.partial.map((p) => `${tagInfo(p.tag).label}（仅 ${p.targets.join('、')}）`).join('，');
        msg.innerHTML = `根据模型名推荐了 ${freshTags.size} 个新标签（虚线框标出），请确认后保存。推荐只取所有候补都具备的能力，上下文取最小值。`
          + (partial ? `<br>部分候补才有：${esc(partial)}。切换到其他候补时这些能力可能缺失。` : '');
        msg.className = 'help';
      };
      const addManual = () => {
        const v = $('#mf-filter', root).value.trim();
        if (!v) return;
        const i = v.indexOf('/');
        if (i <= 0 || i === v.length - 1) return toast('请输入 前缀/模型名，例如 kimi/k3', 'err');
        if (!providers.some((p) => p.prefix === v.slice(0, i))) return toast(`前缀 ${v.slice(0, i)} 不存在，请先添加该套餐`, 'err');
        if (!targets.includes(v)) targets.push(v);
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
          targets,
          enabled: $('#mf-enabled', root).checked,
        };
        if (!body.name) return toast('请填写对外模型名', 'err');
        if (!body.targets.length) return toast('至少选择一个模型', 'err');
        try {
          if (isNew) await api('POST', '/models', body);
          else await api('PUT', '/models/' + m.id, body);
          closeModal();
          toast('已保存', 'ok');
          route();
        } catch (e) { toast(e.message, 'err'); }
      };
    },
  });
}

function modelTest(m) {
  openModal({
    title: '测试模型 ' + m.name,
    wide: true,
    body: `<div class="form">
      <div class="row2">
        <div class="field"><label>以哪种协议调用</label><select id="mt-proto"><option value="openai">OpenAI /v1/chat/completions</option><option value="anthropic">Anthropic /v1/messages</option></select></div>
        <div class="field"><label>测试内容</label><input type="text" id="mt-prompt" value="Reply with the single word: OK"></div>
      </div>
      <label class="check"><input type="checkbox" id="mt-stream"> 流式</label>
      <div class="muted small">走完整调度（含重试、熔断与切换），结果会计入请求日志和熔断状态。</div>
      <div id="mt-result"></div>
    </div>`,
    foot: `<button class="btn" data-close>关闭</button><button class="btn primary" id="mt-go">发送测试</button>`,
    onMount: (root) => {
      $('#mt-go', root).onclick = async () => {
        const btn = $('#mt-go', root);
        btn.disabled = true; btn.textContent = '测试中…';
        $('#mt-result', root).innerHTML = '';
        try {
          const r = await api('POST', '/models/test', { model: m.name, protocol: $('#mt-proto', root).value, stream: $('#mt-stream', root).checked, prompt: $('#mt-prompt', root).value });
          $('#mt-result', root).innerHTML = testResultHTML(r);
        } catch (e) { $('#mt-result', root).innerHTML = `<div class="err-text">${esc(e.message)}</div>`; }
        btn.disabled = false; btn.textContent = '发送测试';
      };
    },
  });
}

// ---------------------------------------------------------------- keys
function maskApiKey(k) { return k.length > 14 ? k.slice(0, 10) + '••••••' + k.slice(-4) : k; }

async function pageKeys() {
  const [keys, models] = await Promise.all([api('GET', '/keys'), api('GET', '/models')]);
  $('#page').innerHTML = `
    ${head('API Keys', '发给使用方的 Key；可单独停用、设置到期时间、限制可用模型', '<button class="btn primary" id="add-key">+ 创建 Key</button>')}
    <div class="card">
      ${keys.length ? `<div class="table-wrap"><table>
        <tr><th>名称</th><th>Key</th><th>可用模型</th><th>到期</th><th>最近使用</th><th>状态</th><th></th></tr>
        ${keys.map((k) => {
          const expired = k.expires_at && k.expires_at < Date.now();
          return `<tr>
            <td>${esc(k.name || '-')}</td>
            <td class="mono small"><span class="copy" data-copy="${esc(k.key)}" title="点击复制完整 Key">${esc(maskApiKey(k.key))}</span></td>
            <td class="small">${k.allowed_models.length ? k.allowed_models.map((x) => `<span class="badge">${esc(x)}</span>`).join(' ') : '<span class="muted">全部</span>'}</td>
            <td class="small">${k.expires_at ? `<span class="${expired ? 'err-text' : ''}">${fmtTime(k.expires_at)}</span>` : '<span class="muted">永不</span>'}</td>
            <td class="small">${fmtAgo(k.last_used_at)}</td>
            <td>${!k.enabled ? '<span class="badge">停用</span>' : expired ? '<span class="badge err">已过期</span>' : '<span class="badge ok">启用</span>'}</td>
            <td><div class="btns">
              <button class="btn sm" data-copy="${esc(k.key)}">复制</button>
              <button class="btn sm" data-edit="${k.id}">编辑</button>
              <button class="btn sm danger" data-del="${k.id}">删除</button>
            </div></td></tr>`;
        }).join('')}
      </table></div>` : '<div class="empty">还没有 API Key</div>'}
    </div>`;
  $('#add-key').onclick = () => keyForm(null, models);
  $$('[data-copy]').forEach((el) => el.onclick = () => copyText(el.dataset.copy));
  $$('[data-edit]').forEach((b) => b.onclick = () => keyForm(keys.find((k) => k.id == b.dataset.edit), models));
  $$('[data-del]').forEach((b) => b.onclick = async () => {
    const k = keys.find((x) => x.id == b.dataset.del);
    if (!(await confirmBox(`删除 Key ${k.name || maskApiKey(k.key)}？使用它的客户端会立即无法访问。`))) return;
    await api('DELETE', '/keys/' + k.id);
    toast('已删除', 'ok');
    route();
  });
}

function toLocalInput(ms) {
  if (!ms) return '';
  const d = new Date(ms - new Date().getTimezoneOffset() * 60000);
  return d.toISOString().slice(0, 16);
}

function showNewKey(title, key) {
  openModal({
    title,
    body: `<div class="form"><div>请复制并妥善保存：</div><pre class="box mono">${esc(key)}</pre></div>`,
    foot: `<button class="btn primary" id="kc-copy">复制</button><button class="btn" data-close>完成</button>`,
    onMount: (m2) => { $('#kc-copy', m2).onclick = () => copyText(key); },
  });
}

function keyForm(k, models) {
  const isNew = !k;
  k = k || { name: '', key: '', enabled: true, allowed_models: [], expires_at: 0 };
  openModal({
    title: isNew ? '创建 API Key' : '编辑 API Key',
    body: `<div class="form">
      <div class="field"><label>名称</label><input type="text" id="kf-name" value="${esc(k.name)}" placeholder="如 张三-ClaudeCode"></div>
      <div class="field"><label>Key</label>${isNew
        ? '<div class="help">保存后由平台自动生成（sk-route-…），不支持自定义</div>'
        : `<div class="toolbar"><code class="mono">${esc(maskApiKey(k.key))}</code><button type="button" class="btn sm danger" id="kf-rotate">重新生成</button></div><div class="help">重新生成后旧 Key 立即失效</div>`}</div>
      <div class="field"><label>到期时间（可选）</label><input type="datetime-local" id="kf-exp" value="${toLocalInput(k.expires_at)}"></div>
      <div class="field"><label>可用模型（都不勾选 = 全部可用）</label>
        <div class="btns">${models.map((m) => `<label class="check badge"><input type="checkbox" value="${esc(m.name)}" ${k.allowed_models.includes(m.name) ? 'checked' : ''}> ${esc(m.name)}</label>`).join('') || '<span class="muted small">暂无模型</span>'}</div>
      </div>
      <label class="check"><input type="checkbox" id="kf-enabled" ${k.enabled ? 'checked' : ''}> 启用</label>
    </div>`,
    foot: `<button class="btn" data-close>取消</button><button class="btn primary" id="kf-save">保存</button>`,
    onMount: (root) => {
      if (!isNew) $('#kf-rotate', root).onclick = async () => {
        if (!(await confirmBox(`重新生成 ${k.name || 'Key'}？旧 Key 会立即失效，使用它的客户端需要更新配置。`))) return;
        try {
          const r = await api('POST', `/keys/${k.id}/rotate`);
          route();
          showNewKey('Key 已重新生成', r.key);
        } catch (e) { toast(e.message, 'err'); }
      };
      $('#kf-save', root).onclick = async () => {
        const exp = $('#kf-exp', root).value;
        const body = {
          name: $('#kf-name', root).value.trim(),
          enabled: $('#kf-enabled', root).checked,
          expires_at: exp ? new Date(exp).getTime() : 0,
          allowed_models: $$('.btns input[type=checkbox]:checked', root).map((c) => c.value),
        };
        try {
          if (isNew) {
            const created = await api('POST', '/keys', body);
            route();
            showNewKey('Key 已创建', created.key);
          } else {
            await api('PUT', '/keys/' + k.id, body);
            closeModal();
            toast('已保存', 'ok');
            route();
          }
        } catch (e) { toast(e.message, 'err'); }
      };
    },
  });
}

// ---------------------------------------------------------------- logs
const logFilter = { model: '', provider: '', status: '', fallback: false, offset: 0, limit: 50 };
async function pageLogs() {
  const qs = new URLSearchParams({ model: logFilter.model, provider: logFilter.provider, status: logFilter.status, fallback: logFilter.fallback ? '1' : '', offset: logFilter.offset, limit: logFilter.limit });
  const [data, models, providers] = await Promise.all([api('GET', '/logs?' + qs), api('GET', '/models'), api('GET', '/providers')]);
  const items = data.items;
  $('#page').innerHTML = `
    ${head('请求日志', `共 ${data.total} 条 · 点击行查看每次尝试的详情`, `<button class="btn" id="log-refresh">刷新</button>`)}
    <div class="card">
      <div class="card-head" style="font-weight:400">
        <div class="toolbar">
          <select id="lf-model"><option value="">全部模型</option>${models.map((m) => `<option ${m.name === logFilter.model ? 'selected' : ''}>${esc(m.name)}</option>`).join('')}</select>
          <select id="lf-provider"><option value="">全部套餐</option>${providers.map((p) => `<option ${p.prefix === logFilter.provider ? 'selected' : ''}>${esc(p.prefix)}</option>`).join('')}</select>
          <select id="lf-status"><option value="">全部状态</option><option value="success" ${logFilter.status === 'success' ? 'selected' : ''}>成功</option><option value="failed" ${logFilter.status === 'failed' ? 'selected' : ''}>失败</option></select>
          <label class="check"><input type="checkbox" id="lf-fallback" ${logFilter.fallback ? 'checked' : ''}> 只看发生切换的</label>
        </div>
      </div>
      ${items.length ? `<div class="table-wrap"><table>
        <tr><th>时间</th><th>Key</th><th>模型</th><th>实际上游</th><th>协议</th><th>状态</th><th class="num">耗时</th><th class="num">首字</th><th class="num">输入/输出</th><th class="num">费用</th></tr>
        ${items.map((l, i) => `<tr class="clickable" data-i="${i}">
          <td class="small">${fmtTime(l.created_at)}</td>
          <td class="small">${esc(l.key_name || '-')}</td>
          <td class="small">${esc(l.public_model || l.requested_model)}${l.public_model && l.requested_model !== l.public_model ? `<div class="muted">${esc(l.requested_model)}</div>` : ''}</td>
          <td class="small mono">${l.provider ? esc(l.provider + '/' + l.upstream_model) : '-'}${l.fallback ? ' <span class="badge warn">切换</span>' : ''}</td>
          <td class="small">${esc(l.inbound)}${l.upstream_protocol && l.upstream_protocol !== l.inbound ? ' → ' + esc(l.upstream_protocol) : ''}${l.stream ? ' <span class="badge">流</span>' : ''}</td>
          <td>${l.success ? '<span class="badge ok">成功</span>' : `<span class="badge err" title="${esc(l.error)}">${l.http_status || '失败'}</span>`}</td>
          <td class="num small">${fmtMs(l.latency_ms)}</td>
          <td class="num small">${l.ttfb_ms ? fmtMs(l.ttfb_ms) : '-'}</td>
          <td class="num small">${fmtNum(l.input_tokens)} / ${fmtNum(l.output_tokens)}</td>
          <td class="num small">${fmtMoney(l.cost, l.currency)}</td>
        </tr>`).join('')}
      </table></div>
      <div class="pager"><span class="muted small">${logFilter.offset + 1} - ${logFilter.offset + items.length} / ${data.total}</span>
        <div class="btns"><button class="btn sm" id="lp-prev" ${logFilter.offset === 0 ? 'disabled' : ''}>上一页</button><button class="btn sm" id="lp-next" ${logFilter.offset + items.length >= data.total ? 'disabled' : ''}>下一页</button></div></div>` : '<div class="empty">没有日志</div>'}
    </div>`;
  const upd = () => { logFilter.offset = 0; route(); };
  $('#lf-model').onchange = (e) => { logFilter.model = e.target.value; upd(); };
  $('#lf-provider').onchange = (e) => { logFilter.provider = e.target.value; upd(); };
  $('#lf-status').onchange = (e) => { logFilter.status = e.target.value; upd(); };
  $('#lf-fallback').onchange = (e) => { logFilter.fallback = e.target.checked; upd(); };
  $('#log-refresh').onclick = route;
  if ($('#lp-prev')) {
    $('#lp-prev').onclick = () => { logFilter.offset = Math.max(0, logFilter.offset - logFilter.limit); route(); };
    $('#lp-next').onclick = () => { logFilter.offset += logFilter.limit; route(); };
  }
  $$('tr.clickable').forEach((tr) => tr.onclick = () => logDetail(items[Number(tr.dataset.i)]));
}

function logDetail(l) {
  const atts = (l.attempts || []).map((a, i) => `<tr><td>${i + 1}</td><td class="mono small">${esc(a.target)}${a.retry ? ` <span class="badge">重试 ${a.retry}</span>` : ''}</td><td>${esc(a.protocol)}</td><td>${a.http_status || '-'}</td><td>${fmtMs(a.latency_ms)}</td><td class="small ${a.error ? 'err-text' : 'ok-text'}">${a.error ? esc(a.error) : '成功'}${a.cooling ? ' <span class="badge warn">冷却中兜底</span>' : ''}</td></tr>`).join('');
  openModal({
    title: '请求详情 #' + l.id,
    wide: true,
    body: `<div class="form">
      <div class="kv">
        <div class="k">时间</div><div>${fmtTime(l.created_at)}</div>
        <div class="k">API Key</div><div>${esc(l.key_name || '-')}</div>
        <div class="k">客户端 IP</div><div>${esc(l.client_ip || '-')}</div>
        <div class="k">请求模型</div><div>${esc(l.requested_model)}${l.public_model ? ' → ' + esc(l.public_model) : ''}</div>
        <div class="k">实际上游</div><div class="mono">${l.provider ? esc(l.provider + '/' + l.upstream_model) : '-'}</div>
        <div class="k">协议</div><div>${esc(l.inbound)} → ${esc(l.upstream_protocol || '-')}${l.stream ? '（流式）' : ''}</div>
        <div class="k">结果</div><div>${l.success ? '<span class="badge ok">成功</span>' : '<span class="badge err">失败</span>'} HTTP ${l.http_status}</div>
        <div class="k">耗时 / 首字</div><div>${fmtMs(l.latency_ms)} / ${l.ttfb_ms ? fmtMs(l.ttfb_ms) : '-'}</div>
        <div class="k">Tokens</div><div>输入 ${l.input_tokens}（缓存 ${l.cached_tokens}） · 输出 ${l.output_tokens}</div>
        <div class="k">费用</div><div>${l.cost_source ? `${fmtMoney(l.cost, l.currency)} <span class="muted small">（${l.cost_source === 'upstream' ? '上游返回的实际费用' : '按配置的单价估算'}）</span>` : '<span class="muted">未计费（没有配置该模型的单价）</span>'}</div>
        ${l.error ? `<div class="k">错误</div><div class="err-text small">${esc(l.error)}</div>` : ''}
      </div>
      ${atts ? `<div class="table-wrap"><table><tr><th>#</th><th>上游</th><th>协议</th><th>状态</th><th>耗时</th><th>结果</th></tr>${atts}</table></div>` : ''}
    </div>`,
  });
}

// ---------------------------------------------------------------- settings
async function pageSettings() {
  const [st, models] = await Promise.all([api('GET', '/settings'), api('GET', '/models')]);
  const origin = location.origin;
  const sample = models.find((m) => m.enabled);
  const mn = sample ? sample.name : '你的模型名';
  $('#page').innerHTML = `
    ${head('设置与接入', '')}
    <div class="card">
      <div class="card-head">客户端接入</div>
      <div class="card-body form">
        <div class="kv">
          <div class="k">OpenAI 兼容</div><div><code class="copy" data-copy="${esc(origin)}/v1">${esc(origin)}/v1</code> <span class="muted small">（POST /v1/chat/completions、POST /v1/embeddings、GET /v1/models）</span></div>
          <div class="k">Anthropic 兼容</div><div><code class="copy" data-copy="${esc(origin)}">${esc(origin)}</code> <span class="muted small">（POST /v1/messages，即 ANTHROPIC_BASE_URL）</span></div>
          <div class="k">鉴权</div><div><code>Authorization: Bearer sk-route-…</code> 或 <code>x-api-key: sk-route-…</code></div>
        </div>
        <div>
          <div class="muted small" style="margin-bottom:4px">Claude Code 示例</div>
<pre class="box mono">export ANTHROPIC_BASE_URL=${esc(origin)}
export ANTHROPIC_AUTH_TOKEN=sk-route-xxxx
export ANTHROPIC_MODEL=${esc(mn)}
export ANTHROPIC_DEFAULT_HAIKU_MODEL=${esc(mn)}
claude</pre>
        </div>
        <div>
          <div class="muted small" style="margin-bottom:4px">curl 示例</div>
<pre class="box mono">curl ${esc(origin)}/v1/chat/completions \\
  -H "Authorization: Bearer sk-route-xxxx" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"${esc(mn)}","messages":[{"role":"user","content":"你好"}]}'</pre>
        </div>
      </div>
    </div>
    <div class="card">
      <div class="card-head">重试、熔断与日志</div>
      <div class="card-body form">
        <div class="row2">
          <div class="field"><label>同一模型重试次数</label><input type="number" id="st-retry" min="0" max="10" value="${st.max_retries}"><div class="help">遇到断连、5xx、上游过载、短时限流时，先在同一个模型上重试几次再切换。0 = 不重试，直接切换</div></div>
          <div class="field"><label>重试间隔（毫秒）</label><input type="number" id="st-backoff" min="1" value="${st.retry_backoff_ms}"><div class="help">每次翻倍，例如 1000 → 1s、2s；上游返回的 Retry-After 更长时以它为准（最多等 10 秒，更长就直接切换）</div></div>
        </div>
        <div class="row2">
          <div class="field"><label>连续失败几次后冷却</label><input type="number" id="st-th" min="1" value="${st.failure_threshold}"><div class="help">按请求计：一次请求在该模型上重试完仍失败算 1 次。429（长时间）、401、402 会立即冷却整个套餐，404 立即冷却该模型；400、403、超时只切换不重试。</div></div>
          <div class="field"><label>首次冷却时长（秒）</label><input type="number" id="st-cd" min="1" value="${st.cooldown_seconds}"><div class="help">冷却到期后再次失败，时长翻倍</div></div>
        </div>
        <div class="row2">
          <div class="field"><label>最长冷却（秒）</label><input type="number" id="st-max" min="1" value="${st.max_cooldown_seconds}"><div class="help">上游返回的 Retry-After 更长时以它为准（最多 6 小时）</div></div>
          <div class="field"><label>日志保留天数</label><input type="number" id="st-ret" min="1" value="${st.log_retention_days}"></div>
        </div>
        <div class="row2">
          <div class="field"><label>默认 max_tokens</label><input type="number" id="st-mt" min="1" value="${st.default_max_tokens}"><div class="help">OpenAI 请求转 Anthropic 上游且没带 max_tokens 时使用</div></div>
        </div>
        <div class="row2">
          <div class="field"><label>统计货币</label><select id="st-cur">${['CNY', 'USD'].map((c) => `<option value="${c}" ${st.currency === c ? 'selected' : ''}>${c === 'CNY' ? '人民币 ¥' : '美元 $'}</option>`).join('')}</select><div class="help">概览里的费用统一换算成这种货币；日志里显示原始货币</div></div>
          <div class="field"><label>美元兑人民币汇率</label><input type="number" id="st-rate" min="0" step="0.01" value="${st.usd_to_cny}"><div class="help">换算时使用，修改后历史数据也按新汇率显示</div></div>
        </div>
        <div><button class="btn primary" id="st-save">保存设置</button></div>
      </div>
    </div>
    <div class="card">
      <div class="card-head">备份 / 迁移</div>
      <div class="card-body form">
        <div class="muted small">导出全部套餐（含上游 Key）、模型映射、API Key 和设置为 JSON。导入会<b>覆盖</b>现有配置（日志不受影响）。导出文件含密钥，请妥善保管。</div>
        <div class="btns"><button class="btn" id="cfg-export">导出配置</button><button class="btn" id="cfg-import">导入配置</button><input type="file" id="cfg-file" accept=".json" style="display:none"></div>
      </div>
    </div>`;
  $$('[data-copy]').forEach((el) => el.onclick = () => copyText(el.dataset.copy));
  $('#st-save').onclick = async () => {
    try {
      await api('PUT', '/settings', {
        max_retries: Number($('#st-retry').value), retry_backoff_ms: Number($('#st-backoff').value),
        failure_threshold: Number($('#st-th').value), cooldown_seconds: Number($('#st-cd').value),
        max_cooldown_seconds: Number($('#st-max').value), log_retention_days: Number($('#st-ret').value),
        default_max_tokens: Number($('#st-mt').value),
        currency: $('#st-cur').value, usd_to_cny: Number($('#st-rate').value),
      });
      toast('已保存', 'ok');
      route();
    } catch (e) { toast(e.message, 'err'); }
  };
  $('#cfg-export').onclick = async () => {
    const res = await fetch('/admin/api/export', { headers: { Authorization: 'Bearer ' + TOKEN } });
    if (!res.ok) return toast('导出失败', 'err');
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
      if (!(await confirmBox(`导入将覆盖现有配置：${(data.providers || []).length} 个套餐、${(data.models || []).length} 个模型、${(data.api_keys || []).length} 个 Key。继续？`))) return;
      await api('POST', '/import', data);
      toast('导入成功', 'ok');
      route();
    } catch (err) { toast('导入失败：' + err.message, 'err'); }
    e.target.value = '';
  };
}

// ---------------------------------------------------------------- boot
route();
