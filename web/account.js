'use strict';

// Accounts: the user management page for administrators, and the console
// a signed-in user sees (their models, keys, logs and usage). Users never
// see providers, upstream models or routing. Loaded before app.js, whose
// helpers ($, api, t, openModal ...) these pages use at run time.

// ---------------------------------------------------------------- admin: users
async function pageUsers() {
  const [users, models, st] = await Promise.all([api('GET', '/users'), api('GET', '/models'), api('GET', '/settings')]);
  const limits = (u) => {
    const parts = [];
    if (u.rpm) parts.push(t('{n} 次/分', { n: fmtNum(u.rpm) }));
    if (u.tpm) parts.push(t('{n} tokens/分', { n: fmtNum(u.tpm) }));
    return parts.length ? parts.join('<br>') : `<span class="muted">${t('不限')}</span>`;
  };
  const spend = (u) => {
    const spent = fmtMoney(u.month_cost, st.currency);
    if (!u.monthly_budget) return `${spent} <span class="muted">/ ${t('不限')}</span>`;
    const over = u.month_cost >= u.monthly_budget;
    return `<span class="${over ? 'err-text' : ''}">${spent} / ${fmtMoney(u.monthly_budget, st.currency)}</span>`;
  };
  $('#page').innerHTML = `
    ${head(t('用户'), t('用户用用户名和密码登录控制台，只能看到可用的模型、自己的 API Key 和调用日志，看不到供应商和实际上游。用户的限额对其创建的所有 Key 合计生效。'), `<button class="btn primary" id="add-user">${t('+ 添加用户')}</button>`)}
    <div class="card">
      ${users.length ? `<div class="table-wrap"><table>
        <tr><th>${t('用户名')}</th><th>${t('角色')}</th><th>${t('可用模型')}</th><th>${t('本月费用 / 预算')}</th><th>${t('限流')}</th><th>${t('Key 数')}</th><th>${t('最近登录')}</th><th>${t('状态')}</th><th></th></tr>
        ${users.map((u) => `<tr>
          <td><b>${esc(u.username)}</b>${u.display_name ? `<div class="muted small">${esc(u.display_name)}</div>` : ''}</td>
          <td>${u.role === 'admin' ? `<span class="badge warn">${t('管理员')}</span>` : `<span class="badge">${t('普通用户')}</span>`}</td>
          <td class="small">${u.allowed_models.length ? u.allowed_models.map((x) => `<span class="badge">${esc(x)}</span>`).join(' ') : `<span class="muted">${t('全部')}</span>`}</td>
          <td class="small">${spend(u)}</td>
          <td class="small">${limits(u)}</td>
          <td class="small">${u.keys}${u.max_keys ? ` / ${u.max_keys}` : ''}</td>
          <td class="small">${fmtAgo(u.last_login_at)}</td>
          <td>${u.enabled ? `<span class="badge ok">${t('启用')}</span>` : `<span class="badge">${t('停用')}</span>`}</td>
          <td><div class="btns"><button class="btn sm" data-edit="${u.id}">${t('编辑')}</button><button class="btn sm danger" data-del="${u.id}">${t('删除')}</button></div></td>
        </tr>`).join('')}
      </table></div>` : `<div class="empty">${t('还没有用户。添加后把用户名和初始密码发给对方，对方登录后可以自己改密码。')}</div>`}
    </div>`;
  $('#add-user').onclick = () => userForm(null, models, st);
  $$('[data-edit]').forEach((b) => b.onclick = () => userForm(users.find((u) => u.id == b.dataset.edit), models, st));
  $$('[data-del]').forEach((b) => b.onclick = async () => {
    const u = users.find((x) => x.id == b.dataset.del);
    if (!(await confirmBox(t('删除用户 {name}？该用户创建的 {n} 个 Key 会一起删除，使用这些 Key 的客户端会立即无法访问。调用日志保留。', { name: u.username, n: u.keys })))) return;
    try {
      await api('DELETE', '/users/' + u.id);
      toast(t('已删除'), 'ok');
      route();
    } catch (e) { toast(e.message, 'err'); }
  });
}

function userForm(u, models, st) {
  const isNew = !u;
  u = u || { username: '', display_name: '', role: 'user', enabled: true, allowed_models: [], monthly_budget: 0, rpm: 0, tpm: 0, max_keys: 0, remark: '' };
  const sign = CURRENCY_SIGN[st.currency] || '';
  openModal({
    title: isNew ? t('添加用户') : t('编辑用户 {name}', { name: u.username }),
    body: `<div class="form">
      <div class="row2">
        <div class="field"><label>${t('用户名')}</label><input type="text" id="uf-name" value="${esc(u.username)}" autocomplete="off"><div class="help">${t('登录用，不区分大小写')}</div></div>
        <div class="field"><label>${t('显示名（可选）')}</label><input type="text" id="uf-display" value="${esc(u.display_name)}"></div>
      </div>
      <div class="row2">
        <div class="field"><label>${isNew ? t('初始密码') : t('重置密码（可选）')}</label><input type="password" id="uf-pw" autocomplete="new-password" placeholder="${esc(isNew ? t('至少 8 个字符') : t('留空表示不修改'))}"><div class="help">${isNew ? t('用户登录后可以自己修改') : t('重置后该用户在所有设备上需要重新登录')}</div></div>
        <div class="field"><label>${t('角色')}</label><select id="uf-role"><option value="user" ${u.role !== 'admin' ? 'selected' : ''}>${t('普通用户')}</option><option value="admin" ${u.role === 'admin' ? 'selected' : ''}>${t('管理员')}</option></select><div class="help">${t('管理员可以使用完整的控制台，和管理令牌一样')}</div></div>
      </div>
      <div class="row2">
        <div class="field"><label>${t('月预算（{sign}，可选）', { sign })}</label><input type="number" id="uf-budget" min="0" step="any" value="${u.monthly_budget || ''}" placeholder="${esc(t('不限'))}"><div class="help">${t('该用户所有 Key 本月费用合计达到后拒绝请求（402）')}</div></div>
        <div class="field"><label>${t('最多 Key 数')}</label><input type="number" id="uf-maxkeys" min="0" value="${u.max_keys || ''}" placeholder="${esc(t('不限'))}"></div>
      </div>
      <div class="row2">
        <div class="field"><label>${t('每分钟请求数 RPM')}</label><input type="number" id="uf-rpm" min="0" value="${u.rpm || ''}" placeholder="${esc(t('不限'))}"><div class="help">${t('该用户所有 Key 合计')}</div></div>
        <div class="field"><label>${t('每分钟 tokens TPM')}</label><input type="number" id="uf-tpm" min="0" value="${u.tpm || ''}" placeholder="${esc(t('不限'))}"><div class="help">${t('该用户所有 Key 合计')}</div></div>
      </div>
      <div class="field"><label>${t('可用模型（都不勾选 = 全部可用）')}</label>
        <div class="btns" id="uf-models">${models.map((m) => `<label class="check badge"><input type="checkbox" value="${esc(m.name)}" ${u.allowed_models.includes(m.name) ? 'checked' : ''}> ${esc(m.name)}</label>`).join('') || `<span class="muted small">${t('暂无模型')}</span>`}</div>
      </div>
      <div class="field"><label>${t('备注')}</label><input type="text" id="uf-remark" value="${esc(u.remark)}"></div>
      <label class="check"><input type="checkbox" id="uf-enabled" ${u.enabled ? 'checked' : ''}> ${t('启用')} <span class="muted small">${t('（停用后该用户立即退出登录，其 Key 也全部失效）')}</span></label>
    </div>`,
    foot: `<button class="btn" data-close>${t('取消')}</button><button class="btn primary" id="uf-save">${t('保存')}</button>`,
    onMount: (m) => {
      $('#uf-save', m).onclick = async () => {
        const body = {
          username: $('#uf-name', m).value.trim(),
          display_name: $('#uf-display', m).value.trim(),
          password: $('#uf-pw', m).value,
          role: $('#uf-role', m).value,
          monthly_budget: Number($('#uf-budget', m).value) || 0,
          max_keys: Math.floor(Number($('#uf-maxkeys', m).value) || 0),
          rpm: Math.floor(Number($('#uf-rpm', m).value) || 0),
          tpm: Math.floor(Number($('#uf-tpm', m).value) || 0),
          allowed_models: $$('#uf-models input:checked', m).map((c) => c.value),
          remark: $('#uf-remark', m).value.trim(),
          enabled: $('#uf-enabled', m).checked,
        };
        if (!body.username) return toast(t('请填写用户名'), 'err');
        if ((isNew || body.password) && body.password.length < 8) return toast(t('密码至少 8 个字符'), 'err');
        try {
          if (isNew) await api('POST', '/users', body);
          else await api('PUT', '/users/' + u.id, body);
          closeModal();
          toast(t('已保存'), 'ok');
          route();
        } catch (e) { toast(e.message, 'err'); }
      };
    },
  });
}

// ---------------------------------------------------------------- user console
function userLimitsText(me) {
  const u = me.user || {};
  const parts = [];
  if (u.monthly_budget) parts.push(t('月预算 {v}', { v: fmtMoney(u.monthly_budget, me.currency) }));
  if (u.rpm) parts.push(t('{n} 次/分', { n: fmtNum(u.rpm) }));
  if (u.tpm) parts.push(t('{n} tokens/分', { n: fmtNum(u.tpm) }));
  if (u.max_keys) parts.push(t('最多 {n} 个 Key', { n: u.max_keys }));
  return parts.length ? parts.join(t('，')) : t('不限');
}

let myRange = '24h';
async function pageMyOverview() {
  const [me, stats] = await Promise.all([api('GET', '/me'), api('GET', '/my/stats?range=' + myRange)]);
  ME = me;
  const u = me.user;
  const tot = stats.total;
  const ranges = [['1h', t('1 小时')], ['24h', t('24 小时')], ['7d', t('7 天')], ['30d', t('30 天')]];
  const max = Math.max(1, ...stats.timeline.map((r) => r.requests));
  const bars = stats.timeline.map((r) => {
    const h = (r.requests / max) * 100;
    const fh = r.requests ? (r.failed / r.requests) * h : 0;
    return `<div class="bar" style="height:${h}%" title="${esc(t('{key}：{n} 次，失败 {failed}', { key: r.key, n: r.requests, failed: r.failed }))}"><div class="ok" style="flex:${h - fh}"></div><div class="fail" style="flex:${fh}"></div></div>`;
  }).join('');
  const table = (rows, label) => rows.length ? `<div class="table-wrap"><table>
      <tr><th>${label}</th><th class="num">${t('请求')}</th><th class="num">${t('成功率')}</th><th class="num">${t('输入')}</th><th class="num">${t('输出')}</th><th class="num">${t('费用')}</th><th class="num">${t('平均耗时')}</th></tr>
      ${rows.map((r) => `<tr><td>${esc(r.key || '-')}</td><td class="num">${fmtNum(r.requests)}</td><td class="num">${pct(r.success, r.requests)}</td><td class="num">${fmtNum(r.input_tokens)}</td><td class="num">${fmtNum(r.output_tokens)}</td><td class="num">${r.cost ? fmtMoney(r.cost, stats.currency) : '-'}</td><td class="num">${fmtMs(r.avg_latency_ms)}</td></tr>`).join('')}
    </table></div>` : `<div class="empty">${t('暂无数据')}</div>`;
  const budget = u.monthly_budget ? `${fmtMoney(me.month_cost, me.currency)} / ${fmtMoney(u.monthly_budget, me.currency)}` : fmtMoney(me.month_cost, me.currency);
  $('#page').innerHTML = `
    ${head(t('概览'), t('你的调用量和费用；额度：{limits}', { limits: esc(userLimitsText(me)) }), `
      <select id="my-range">${ranges.map(([v, l]) => `<option value="${v}" ${v === myRange ? 'selected' : ''}>${l}</option>`).join('')}</select>
      <button class="btn" id="my-refresh">${t('刷新')}</button>`)}
    <div class="grid cols-4">
      <div class="card stat"><div class="label">${t('请求数')}</div><div class="value">${fmtNum(tot.requests)}</div><div class="hint">${t('失败 {n}', { n: fmtNum(tot.failed) })}</div></div>
      <div class="card stat"><div class="label">${t('成功率')}</div><div class="value">${pct(tot.success, tot.requests)}</div><div class="hint">${t('平均耗时 {ms}', { ms: fmtMs(tot.avg_latency_ms) })}</div></div>
      <div class="card stat"><div class="label">${t('Tokens（输入 / 输出）')}</div><div class="value">${fmtNum(tot.input_tokens)} / ${fmtNum(tot.output_tokens)}</div><div class="hint">${t('缓存命中 {n}', { n: fmtNum(tot.cached_tokens) })}</div></div>
      <div class="card stat"><div class="label">${t('本月费用')}</div><div class="value ${u.monthly_budget && me.month_cost >= u.monthly_budget ? 'err-text' : ''}">${budget}</div><div class="hint">${u.monthly_budget ? t('用完后请求会被拒绝，每月 1 日恢复') : t('没有设置月预算')}</div></div>
    </div>
    <div class="card">
      <div class="card-head">${t('请求趋势')} <span class="muted small">${t('蓝色：成功 · 红色：失败')}</span></div>
      <div class="card-body">${stats.timeline.length ? `<div class="bars">${bars}</div><div class="bars-axis"><span>${esc(stats.timeline[0].key)}</span><span>${esc(stats.timeline[stats.timeline.length - 1].key)}</span></div>` : `<div class="empty">${t('暂无数据')}</div>`}</div>
    </div>
    <div class="grid cols-2">
      <div class="card"><div class="card-head">${t('按模型')}</div>${table(stats.by_model, t('模型'))}</div>
      <div class="card"><div class="card-head">${t('按 API Key')}</div>${table(stats.by_key, 'Key')}</div>
    </div>`;
  $('#my-range').onchange = (e) => { myRange = e.target.value; route(); };
  $('#my-refresh').onclick = route;
}

async function pageMyModels() {
  const models = await api('GET', '/my/models');
  const origin = location.origin;
  $('#page').innerHTML = `
    ${head(t('可用模型'), t('请求里的 model 填下面的名称（或别名）'))}
    <div class="card"><div class="card-body form"><div class="kv">
      <div class="k">${t('OpenAI 兼容')}</div><div><code class="copy" data-copy="${esc(origin)}/v1">${esc(origin)}/v1</code></div>
      <div class="k">${t('Anthropic 兼容')}</div><div><code class="copy" data-copy="${esc(origin)}">${esc(origin)}</code></div>
    </div></div></div>
    <div class="card">
      ${models.length ? `<div class="table-wrap"><table>
        <tr><th>${t('模型')}</th><th>${t('别名')}</th><th>${t('标签')}</th><th>${t('说明')}</th></tr>
        ${models.map((m) => `<tr>
          <td><code class="copy" data-copy="${esc(m.name)}">${esc(m.name)}</code></td>
          <td class="small mono">${(m.aliases || []).map(esc).join('<br>') || '-'}</td>
          <td>${tagBadges(m.tags || [])}</td>
          <td class="small">${esc(m.description || '')}</td>
        </tr>`).join('')}
      </table></div>` : `<div class="empty">${t('管理员还没有给你开放任何模型')}</div>`}
    </div>`;
  $$('[data-copy]').forEach((el) => el.onclick = () => copyText(el.dataset.copy));
}

async function pageMyKeys() {
  const [keys, models, me] = await Promise.all([api('GET', '/my/keys'), api('GET', '/my/models'), api('GET', '/me')]);
  const u = me.user;
  const full = u.max_keys && keys.length >= u.max_keys;
  $('#page').innerHTML = `
    ${head('API Keys', t('Key 只在创建时显示一次，请立即保存；丢了就重新生成。你所有 Key 共用账号的额度：{limits}', { limits: esc(userLimitsText(me)) }),
      `<button class="btn primary" id="add-key" ${full ? `disabled title="${esc(t('已达到 Key 数上限'))}"` : ''}>${t('+ 创建 Key')}</button>`)}
    <div class="card">
      ${keys.length ? `<div class="table-wrap"><table>
        <tr><th>${t('名称')}</th><th>Key</th><th>${t('可用模型')}</th><th>${t('本月费用 / 预算')}</th><th>${t('到期')}</th><th>${t('最近使用')}</th><th>${t('状态')}</th><th></th></tr>
        ${keys.map((k) => {
          const expired = k.expires_at && k.expires_at < Date.now();
          return `<tr>
            <td>${esc(k.name || '-')}</td>
            <td class="mono small">${esc(k.hint)}</td>
            <td class="small">${k.allowed_models.length ? k.allowed_models.map((x) => `<span class="badge">${esc(x)}</span>`).join(' ') : `<span class="muted">${t('全部')}</span>`}</td>
            <td class="small">${fmtMoney(k.month_cost, me.currency)}${k.monthly_budget ? ` / ${fmtMoney(k.monthly_budget, me.currency)}` : ''}</td>
            <td class="small">${k.expires_at ? `<span class="${expired ? 'err-text' : ''}">${fmtTime(k.expires_at)}</span>` : `<span class="muted">${t('永不')}</span>`}</td>
            <td class="small">${fmtAgo(k.last_used_at)}</td>
            <td>${!k.enabled ? `<span class="badge">${t('停用')}</span>` : expired ? `<span class="badge err">${t('已过期')}</span>` : `<span class="badge ok">${t('启用')}</span>`}</td>
            <td><div class="btns">
              <button class="btn sm" data-edit="${k.id}">${t('编辑')}</button>
              <button class="btn sm" data-rotate="${k.id}">${t('重新生成')}</button>
              <button class="btn sm danger" data-del="${k.id}">${t('删除')}</button>
            </div></td></tr>`;
        }).join('')}
      </table></div>` : `<div class="empty">${t('还没有 API Key，点右上角创建一个')}</div>`}
    </div>`;
  if (!full) $('#add-key').onclick = () => myKeyForm(null, models, me);
  $$('[data-edit]').forEach((b) => b.onclick = () => myKeyForm(keys.find((k) => k.id == b.dataset.edit), models, me));
  $$('[data-rotate]').forEach((b) => b.onclick = async () => {
    const k = keys.find((x) => x.id == b.dataset.rotate);
    if (!(await confirmBox(t('重新生成 {name}？旧 Key 会立即失效，使用它的客户端需要更新配置。', { name: k.name || 'Key' })))) return;
    try {
      const r = await api('POST', `/my/keys/${k.id}/rotate`);
      route();
      showNewKey(t('Key 已重新生成'), r.key, true);
    } catch (e) { toast(e.message, 'err'); }
  });
  $$('[data-del]').forEach((b) => b.onclick = async () => {
    const k = keys.find((x) => x.id == b.dataset.del);
    if (!(await confirmBox(t('删除 Key {name}？使用它的客户端会立即无法访问。', { name: k.name || k.hint })))) return;
    await api('DELETE', '/my/keys/' + k.id);
    toast(t('已删除'), 'ok');
    route();
  });
}

function myKeyForm(k, models, me) {
  const isNew = !k;
  k = k || { name: '', enabled: true, allowed_models: [], expires_at: 0, monthly_budget: 0 };
  const sign = CURRENCY_SIGN[me.currency] || '';
  openModal({
    title: isNew ? t('创建 API Key') : t('编辑 API Key'),
    body: `<div class="form">
      <div class="field"><label>${t('名称')}</label><input type="text" id="mk-name" value="${esc(k.name)}" placeholder="${esc(t('如 笔记本-ClaudeCode'))}"></div>
      <div class="row2">
        <div class="field"><label>${t('到期时间（可选）')}</label><input type="datetime-local" id="mk-exp" value="${toLocalInput(k.expires_at)}"></div>
        <div class="field"><label>${t('月预算（{sign}，可选）', { sign })}</label><input type="number" id="mk-budget" min="0" step="any" value="${k.monthly_budget || ''}" placeholder="${esc(t('不限'))}"><div class="help">${t('只限这个 Key；账号的总额度另外生效')}</div></div>
      </div>
      <div class="field"><label>${t('可用模型（都不勾选 = 全部可用）')}</label>
        <div class="btns" id="mk-models">${models.map((m) => `<label class="check badge"><input type="checkbox" value="${esc(m.name)}" ${k.allowed_models.includes(m.name) ? 'checked' : ''}> ${esc(m.name)}</label>`).join('') || `<span class="muted small">${t('暂无模型')}</span>`}</div>
      </div>
      <label class="check"><input type="checkbox" id="mk-enabled" ${k.enabled ? 'checked' : ''}> ${t('启用')}</label>
    </div>`,
    foot: `<button class="btn" data-close>${t('取消')}</button><button class="btn primary" id="mk-save">${t('保存')}</button>`,
    onMount: (m) => {
      $('#mk-save', m).onclick = async () => {
        const exp = $('#mk-exp', m).value;
        const body = {
          name: $('#mk-name', m).value.trim(),
          enabled: $('#mk-enabled', m).checked,
          expires_at: exp ? new Date(exp).getTime() : 0,
          monthly_budget: Number($('#mk-budget', m).value) || 0,
          allowed_models: $$('#mk-models input:checked', m).map((c) => c.value),
        };
        try {
          if (isNew) {
            const created = await api('POST', '/my/keys', body);
            route();
            showNewKey(t('Key 已创建'), created.key, true);
          } else {
            await api('PUT', '/my/keys/' + k.id, body);
            closeModal();
            toast(t('已保存'), 'ok');
            route();
          }
        } catch (e) { toast(e.message, 'err'); }
      };
    },
  });
}

const myLogFilter = { model: '', key_id: '', status: '', offset: 0, limit: 50 };
async function pageMyLogs() {
  const qs = new URLSearchParams(myLogFilter);
  const [data, models, keys] = await Promise.all([api('GET', '/my/logs?' + qs), api('GET', '/my/models'), api('GET', '/my/keys')]);
  const items = data.items;
  $('#page').innerHTML = `
    ${head(t('请求日志'), t('你的 Key 发出的请求，共 {n} 条', { n: data.total }), `<button class="btn" id="log-refresh">${t('刷新')}</button>`)}
    <div class="card">
      <div class="card-head" style="font-weight:400"><div class="toolbar">
        <select id="ml-model"><option value="">${t('全部模型')}</option>${models.map((m) => `<option ${m.name === myLogFilter.model ? 'selected' : ''}>${esc(m.name)}</option>`).join('')}</select>
        <select id="ml-key"><option value="">${t('全部 Key')}</option>${keys.map((k) => `<option value="${k.id}" ${String(k.id) === myLogFilter.key_id ? 'selected' : ''}>${esc(k.name || k.hint)}</option>`).join('')}</select>
        <select id="ml-status"><option value="">${t('全部状态')}</option><option value="success" ${myLogFilter.status === 'success' ? 'selected' : ''}>${t('成功')}</option><option value="failed" ${myLogFilter.status === 'failed' ? 'selected' : ''}>${t('失败')}</option></select>
      </div></div>
      ${items.length ? `<div class="table-wrap"><table>
        <tr><th>${t('时间')}</th><th>Key</th><th>${t('模型')}</th><th>${t('协议')}</th><th>${t('状态')}</th><th class="num">${t('耗时')}</th><th class="num">${t('首字')}</th><th class="num">${t('输入/输出')}</th><th class="num">${t('费用')}</th></tr>
        ${items.map((l, i) => `<tr class="clickable" data-i="${i}">
          <td class="small">${fmtTime(l.created_at)}</td>
          <td class="small">${esc(l.key_name || '-')}</td>
          <td class="small">${esc(l.public_model || l.requested_model)}</td>
          <td class="small">${esc(l.inbound)}${l.stream ? ` <span class="badge">${t('流')}</span>` : ''}</td>
          <td>${l.success ? `<span class="badge ok">${t('成功')}</span>` : `<span class="badge err" title="${esc(l.error)}">${l.http_status && l.http_status !== 200 ? l.http_status : t('失败')}</span>`}</td>
          <td class="num small">${fmtMs(l.latency_ms)}</td>
          <td class="num small">${l.ttfb_ms ? fmtMs(l.ttfb_ms) : '-'}</td>
          <td class="num small">${fmtNum(l.input_tokens)} / ${fmtNum(l.output_tokens)}</td>
          <td class="num small">${l.cost ? fmtMoney(l.cost, data.currency) : '-'}</td>
        </tr>`).join('')}
      </table></div>
      <div class="pager"><span class="muted small">${myLogFilter.offset + 1} - ${myLogFilter.offset + items.length} / ${data.total}</span>
        <div class="btns"><button class="btn sm" id="lp-prev" ${myLogFilter.offset === 0 ? 'disabled' : ''}>${t('上一页')}</button><button class="btn sm" id="lp-next" ${myLogFilter.offset + items.length >= data.total ? 'disabled' : ''}>${t('下一页')}</button></div></div>` : `<div class="empty">${t('没有日志')}</div>`}
    </div>`;
  const upd = (k, v) => { myLogFilter[k] = v; myLogFilter.offset = 0; route(); };
  $('#ml-model').onchange = (e) => upd('model', e.target.value);
  $('#ml-key').onchange = (e) => upd('key_id', e.target.value);
  $('#ml-status').onchange = (e) => upd('status', e.target.value);
  $('#log-refresh').onclick = route;
  if ($('#lp-prev')) {
    $('#lp-prev').onclick = () => { myLogFilter.offset = Math.max(0, myLogFilter.offset - myLogFilter.limit); route(); };
    $('#lp-next').onclick = () => { myLogFilter.offset += myLogFilter.limit; route(); };
  }
  $$('tr.clickable').forEach((tr) => tr.onclick = () => {
    const l = items[Number(tr.dataset.i)];
    openModal({
      title: t('请求详情 #{id}', { id: l.id }),
      body: `<div class="form"><div class="kv">
        <div class="k">${t('时间')}</div><div>${fmtTime(l.created_at)}</div>
        <div class="k">${t('请求 ID')}</div><div class="mono small">${esc(l.request_id || '-')} <span class="muted">${t('（反馈问题时请提供）')}</span></div>
        <div class="k">API Key</div><div>${esc(l.key_name || '-')}</div>
        <div class="k">${t('模型')}</div><div>${esc(l.requested_model)}${l.public_model && l.public_model !== l.requested_model ? ' → ' + esc(l.public_model) : ''}</div>
        <div class="k">${t('协议')}</div><div>${esc(l.inbound)}${l.stream ? t('（流式）') : ''}</div>
        <div class="k">${t('结果')}</div><div>${l.success ? `<span class="badge ok">${t('成功')}</span>` : `<span class="badge err">${t('失败')}</span>`} HTTP ${l.http_status}</div>
        <div class="k">${t('耗时 / 首字')}</div><div>${fmtMs(l.latency_ms)} / ${l.ttfb_ms ? fmtMs(l.ttfb_ms) : '-'}</div>
        <div class="k">Tokens</div><div>${t('输入 {input}（缓存 {cached}） · 输出 {output}', { input: l.input_tokens, cached: l.cached_tokens, output: l.output_tokens })}</div>
        <div class="k">${t('费用')}</div><div>${l.cost ? fmtMoney(l.cost, data.currency) : '-'}</div>
        ${l.error ? `<div class="k">${t('错误')}</div><div class="err-text small">${esc(l.error)}</div>` : ''}
      </div></div>`,
    });
  });
}

async function pageMyAccount() {
  const [me, keys, models] = await Promise.all([api('GET', '/me'), api('GET', '/my/keys'), api('GET', '/my/models')]);
  const u = me.user;
  $('#page').innerHTML = `
    ${head(t('接入与账号'), '')}
    <div class="card">
      <div class="card-head">${t('客户端接入')}</div>
      <div class="card-body form"><div class="wizard" id="wz"></div></div>
    </div>
    <div class="card">
      <div class="card-head">${t('账号')}</div>
      <div class="card-body form">
        <div class="kv">
          <div class="k">${t('用户名')}</div><div>${esc(u.username)}${u.display_name ? ` <span class="muted">${esc(u.display_name)}</span>` : ''}</div>
          <div class="k">${t('额度')}</div><div>${esc(userLimitsText(me))}</div>
        </div>
        <div class="row3">
          <div class="field"><label>${t('当前密码')}</label><input type="password" id="pw-old" autocomplete="current-password"></div>
          <div class="field"><label>${t('新密码')}</label><input type="password" id="pw-new" autocomplete="new-password" placeholder="${esc(t('至少 8 个字符'))}"></div>
          <div class="field"><label>${t('再输一次')}</label><input type="password" id="pw-new2" autocomplete="new-password"></div>
        </div>
        <div><button class="btn primary" id="pw-save">${t('修改密码')}</button> <span class="muted small">${t('修改后其他设备上的登录会退出')}</span></div>
      </div>
    </div>`;
  setupWizard($('#wz'), location.origin, keys, models.map((m) => ({ ...m, enabled: true })));
  $('#pw-save').onclick = async () => {
    const oldPw = $('#pw-old').value, newPw = $('#pw-new').value;
    if (newPw.length < 8) return toast(t('密码至少 8 个字符'), 'err');
    if (newPw !== $('#pw-new2').value) return toast(t('两次输入的新密码不一样'), 'err');
    try {
      await api('POST', '/my/password', { old_password: oldPw, new_password: newPw });
      toast(t('密码已修改'), 'ok');
      route();
    } catch (e) { toast(e.message, 'err'); }
  };
}
