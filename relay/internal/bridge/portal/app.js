// tunr Bridge portal — customer portal + vendor console.
// No framework, no build step. Every piece of server data is rendered with
// textContent (via h()); no HTML is ever built from strings.
'use strict';

const API = '/api/v1/bridge';
const LS_TOKEN = 'tunr.bridge.token';
const LS_EMAIL = 'tunr.bridge.email';
const LS_ORG = 'tunr.bridge.org';
// LS_ASKED: which address this browser asked a sign-in link for, and when.
const LS_ASKED = 'tunr.bridge.asked';

const store = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* private mode */ } },
  del(k) { try { localStorage.removeItem(k); } catch { /* ignore */ } },
};

const state = { token: store.get(LS_TOKEN), email: store.get(LS_EMAIL), orgs: [], org: null, pending: 0 };
let pollTimer = null;

// ── tiny DOM helper ─────────────────────────────────────────────────────────

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'value') el.value = v;
    // CSSOM, not the style attribute: the CSP blocks style attributes but
    // not this, and only literal strings from this file ever reach it.
    else if (k === 'style') el.style.cssText = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid === null || kid === undefined || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

function toast(msg, bad) {
  const t = h('div', { class: 'toast' + (bad ? ' bad' : '') }, msg);
  document.getElementById('toasts').append(t);
  setTimeout(() => t.remove(), bad ? 6000 : 3200);
}

function confirmBox(title, body, okLabel, danger) {
  return new Promise((resolve) => {
    const d = h('dialog', {},
      h('h2', { style: 'margin-top:0' }, title),
      h('p', { class: 'muted' }, body),
      h('div', { class: 'row', style: 'justify-content:flex-end;margin-top:16px' },
        h('button', { onclick: () => { d.close(); resolve(false); } }, 'Cancel'),
        h('button', { class: danger ? 'danger solid' : 'primary', onclick: () => { d.close(); resolve(true); } }, okLabel)));
    d.addEventListener('close', () => d.remove());
    document.body.append(d);
    d.showModal();
  });
}

function copyBlock(text, secret) {
  const btn = h('button', {
    onclick: async () => {
      try { await navigator.clipboard.writeText(text); btn.textContent = 'Copied'; setTimeout(() => { btn.textContent = 'Copy'; }, 1500); }
      catch { toast('Copy failed — select the text instead', true); }
    },
  }, 'Copy');
  return h('div', { class: 'code' + (secret ? ' secret' : '') }, text, btn);
}

// ── time & labels ───────────────────────────────────────────────────────────

function dur(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return s + 's';
  const m = Math.round(s / 60);
  if (m < 60) return m + ' min';
  const hh = Math.floor(m / 60), mm = m % 60;
  return hh + 'h' + (mm ? ' ' + mm + 'm' : '');
}
const ago = (t) => (t ? dur(Date.now() - new Date(t)) + ' ago' : '—');
const until = (t) => (t ? (new Date(t) > Date.now() ? 'in ' + dur(new Date(t) - Date.now()) : 'ended') : '—');
const when = (t) => (t ? new Date(t).toLocaleString() : '—');

function modeBadge(mode) {
  return mode === 'ro'
    ? h('span', { class: 'badge ok' }, 'read-only')
    : h('span', { class: 'badge warn' }, 'read-write');
}
function statusBadge(s) {
  const cls = { approved: 'ok', pending: 'warn', denied: 'bad', revoked: 'bad', expired: '' }[s] ?? '';
  return h('span', { class: 'badge ' + cls }, s);
}

// ── API ─────────────────────────────────────────────────────────────────────

async function api(method, path, body, raw) {
  const res = await fetch(API + path, {
    method,
    headers: Object.assign({ Authorization: 'Bearer ' + state.token }, body ? { 'Content-Type': 'application/json' } : {}),
    body: body ? JSON.stringify(body) : undefined,
    cache: 'no-store',
  });
  if (res.status === 401) { logout(); throw new Error('Signed out'); }
  if (raw) { if (!res.ok) throw new Error('HTTP ' + res.status); return res; }
  const text = await res.text();
  const data = text ? JSON.parse(text) : {};
  if (!res.ok) throw new Error(data.message || data.error || 'HTTP ' + res.status);
  return data;
}
const orgPath = (p) => '/orgs/' + encodeURIComponent(state.org.slug) + p;

async function act(fn, okMsg) {
  try { const r = await fn(); if (okMsg) toast(okMsg); return r; }
  catch (e) { toast(e.message, true); return null; }
}

function logout() {
  store.del(LS_TOKEN); store.del(LS_EMAIL);
  state.token = null; state.email = null;
  location.hash = '#/login';
}

// ── routing ─────────────────────────────────────────────────────────────────

const isCustomer = () => state.org && (state.org.kind === 'customer' || state.org.kind === 'both');
const isVendor = () => state.org && (state.org.kind === 'vendor' || state.org.kind === 'both');
const isAdmin = () => state.org && (state.org.role === 'owner' || state.org.role === 'admin');
const isApprover = () => isAdmin() || (state.org && state.org.role === 'approver');

const routes = {
  approvals: { title: 'Approvals', side: 'customer', render: viewApprovals, poll: true },
  sites: { title: 'Sites', side: 'customer', render: viewSites, poll: true },
  'sites/new': { title: 'Add a site', side: 'customer', render: viewNewSite },
  vendors: { title: 'Vendors', side: 'customer', render: viewVendors },
  activity: { title: 'Activity', side: 'customer', render: viewActivity },
  agents: { title: 'Agents', side: 'vendor', render: viewAgents },
  catalog: { title: 'Customer resources', side: 'vendor', render: viewCatalog },
  'my-grants': { title: 'My access requests', side: 'vendor', render: viewMyGrants, poll: true },
  members: { title: 'Members', side: 'org', render: viewMembers },
  settings: { title: 'Settings', side: 'org', render: viewSettings },
  'orgs/new': { title: 'New organization', side: 'org', render: viewNewOrg },
};

function parseHash() {
  const raw = location.hash.replace(/^#\/?/, '');
  const [path, query] = raw.split('?');
  return { path: path || '', params: new URLSearchParams(query || '') };
}

async function route() {
  clearInterval(pollTimer);
  const { path, params } = parseHash();
  if (path === 'verify') return verify(params);
  if (!state.token) return viewLogin();
  if (!state.orgs.length) {
    const ok = await act(loadOrgs);
    if (ok === null) return;
  }
  let r = routes[path];
  if (!r || (r.side === 'customer' && !isCustomer()) || (r.side === 'vendor' && !isVendor())) {
    location.hash = '#/' + (isCustomer() ? 'approvals' : 'agents');
    return;
  }
  const main = h('main', { class: 'main' });
  render(shell(path, main));
  const draw = async () => {
    try {
      const view = await r.render(params);
      main.replaceChildren(h('div', { class: 'head' }, h('div', {}, h('h1', {}, r.title), h('p', { class: 'muted' }, subtitle(path)))), view);
    } catch (e) { main.replaceChildren(h('div', { class: 'empty' }, e.message)); }
  };
  await draw();
  if (r.poll) pollTimer = setInterval(() => { if (document.visibilityState === 'visible' && !document.querySelector('dialog[open]')) draw(); }, 5000);
}

function subtitle(path) {
  return ({
    approvals: 'Agents ask here. Nothing reaches your systems until you approve, and you can cut access at any time.',
    sites: 'Each site runs one connector inside your network. It only connects out; no inbound ports.',
    vendors: 'Vendors you allow to see your resources and ask for access.',
    activity: 'Every session and query, per grant. Blocked statements are highlighted.',
    agents: 'Agent identities for your product. Customers approve these, one by one.',
    catalog: 'Resources of customers who linked your organization.',
    'my-grants': 'Your agents’ requests and their status.',
    members: 'People in this organization and what they may do.',
    settings: 'Organization policy and outgoing webhooks.',
  })[path] || '';
}

function render(node) { document.getElementById('root').replaceChildren(node); }

async function loadOrgs() {
  const { orgs } = await api('GET', '/orgs');
  state.orgs = orgs || [];
  const want = store.get(LS_ORG);
  state.org = state.orgs.find((o) => o.slug === want) || state.orgs.find((o) => !o.personal) || state.orgs[0] || null;
  if (state.org) await refreshPending();
  return true;
}

async function refreshPending() {
  if (!isCustomer()) { state.pending = 0; return; }
  try {
    const { grants } = await api('GET', orgPath('/grants?side=customer&status=pending'));
    state.pending = (grants || []).length;
  } catch { state.pending = 0; }
}

function shell(active, main) {
  const link = (path, label, count) => h('a', { href: '#/' + path, class: active === path ? 'active' : '' },
    label, count ? h('span', { class: 'count' }, count) : null);
  const sel = h('select', {
    'aria-label': 'Organization',
    onchange: (e) => {
      if (e.target.value === '__new') { location.hash = '#/orgs/new'; return; }
      state.org = state.orgs.find((o) => o.slug === e.target.value);
      store.set(LS_ORG, state.org.slug);
      refreshPending().then(() => { location.hash = '#/' + (isCustomer() ? 'approvals' : 'agents'); route(); });
    },
  }, state.orgs.map((o) => h('option', { value: o.slug, selected: state.org && o.slug === state.org.slug },
    (o.personal ? 'Personal' : o.name) + ' · ' + o.kind)), h('option', { value: '__new' }, '+ New organization'));
  return h('div', { class: 'app' },
    h('aside', { class: 'side' },
      h('div', { class: 'brand' }, h('span', { class: 'dot' }), 'tunr', h('small', {}, 'Bridge')),
      sel,
      h('nav', { class: 'nav' },
        isCustomer() ? [h('div', { class: 'nav-title' }, 'Your data'),
          link('approvals', 'Approvals', state.pending), link('sites', 'Sites'), link('vendors', 'Vendors'), link('activity', 'Activity')] : null,
        isVendor() ? [h('div', { class: 'nav-title' }, 'Your agents'),
          link('agents', 'Agents'), link('catalog', 'Customer resources'), link('my-grants', 'Access requests')] : null,
        h('div', { class: 'nav-title' }, 'Organization'), link('members', 'Members'), link('settings', 'Settings')),
      h('div', { class: 'foot small muted' }, state.email || '', h('br'),
        h('button', { class: 'ghost', style: 'padding:4px 0', onclick: logout }, 'Sign out'))),
    main);
}

// ── login ───────────────────────────────────────────────────────────────────

function viewLogin() {
  const email = h('input', { type: 'email', placeholder: 'you@company.com', autocomplete: 'email', required: true });
  const out = h('div');
  const send = async () => {
    out.replaceChildren();
    try {
      const res = await fetch(API + '/portal/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ email: email.value }) });
      const data = await res.json();
      if (!res.ok) throw new Error(data.message || 'Could not send the link');
      store.set(LS_ASKED, JSON.stringify({ email: email.value.trim().toLowerCase(), at: Date.now() }));
      out.append(h('div', { class: 'callout ok' }, 'Check your inbox for a sign-in link.'));
      if (data.dev_link) out.append(h('p', { class: 'small muted' }, 'Development mode — no mail provider configured:'), h('a', { href: data.dev_link }, 'Open the sign-in link'));
    } catch (e) { out.append(h('div', { class: 'callout' }, e.message)); }
  };
  render(h('div', { class: 'center' }, h('div', { class: 'card login' },
    h('div', { class: 'brand' }, h('span', { class: 'dot' }), 'tunr', h('small', {}, 'Bridge')),
    h('h1', {}, 'Sign in'),
    h('p', { class: 'muted' }, 'Approve and watch AI agents that use your company’s data — or manage your own agents.'),
    h('label', {}, 'Work e-mail'), email,
    h('button', { class: 'primary', onclick: send }, 'Send sign-in link'), out)));
  email.addEventListener('keydown', (e) => { if (e.key === 'Enter') send(); });
  email.focus();
}

// verify signs in at once only in the browser that asked for this address's
// link in the last 15 minutes. Anywhere else (another device, or a link
// someone else sent) it first shows which account the link signs in to.
function verify(params) {
  const email = (params.get('email') || '').toLowerCase();
  let asked = null;
  try { asked = JSON.parse(store.get(LS_ASKED) || 'null'); } catch { asked = null; }
  if (asked && asked.email === email && Date.now() - asked.at < 15 * 60 * 1000) return doVerify(params);
  render(h('div', { class: 'center' }, h('div', { class: 'card login' },
    h('h1', {}, 'Sign in to tunr Bridge'),
    h('p', { class: 'muted' }, 'This link signs you in as ', h('b', {}, email || '(no address)'),
      '. Continue only if that is your address and you asked for this link.'),
    h('button', { class: 'primary', onclick: () => doVerify(params) }, 'Sign in as ' + email),
    h('p', { class: 'small' }, h('a', { href: '#/login' }, 'Not you? Go to the sign-in page')))));
}

async function doVerify(params) {
  store.del(LS_ASKED);
  render(h('div', { class: 'center' }, h('div', { class: 'card login' }, h('p', {}, 'Signing you in…'))));
  try {
    const res = await fetch(API + '/portal/verify', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: params.get('email') || '', token: params.get('token') || '' }),
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.message || 'Sign-in failed');
    state.token = data.token; state.email = data.email;
    store.set(LS_TOKEN, data.token); store.set(LS_EMAIL, data.email);
    const next = (params.get('next') || '').replace(/[^a-z/-]/g, '');
    history.replaceState(null, '', location.pathname + '#/' + next); // drop the token from the URL
    state.orgs = [];
    route();
  } catch (e) {
    render(h('div', { class: 'center' }, h('div', { class: 'card login' },
      h('h1', {}, 'Sign-in link not valid'), h('p', { class: 'muted' }, e.message),
      h('a', { href: '#/login' }, 'Request a new link'))));
  }
}

// ── customer: approvals ─────────────────────────────────────────────────────

async function viewApprovals() {
  const { grants = [] } = await api('GET', orgPath('/grants?side=customer'));
  await refreshPending();
  const pending = grants.filter((g) => g.status === 'pending');
  const active = grants.filter((g) => g.status === 'approved');
  const history = grants.filter((g) => !['pending', 'approved'].includes(g.status)).slice(0, 50);

  const decide = (g, verb) => act(async () => {
    if (verb === 'deny' && !(await confirmBox('Deny this request?', g.agent_name + ' will not get access to ' + g.address + '.', 'Deny', true))) return;
    const r = await api('POST', orgPath('/grants/' + g.id + '/' + verb));
    toast(verb === 'approve' ? (r.status === 'approved' ? 'Approved' : 'Approval recorded — a second approver is needed') : 'Denied');
    route();
  });
  const revoke = (g) => act(async () => {
    if (!(await confirmBox('Revoke access?', 'Open connections from ' + g.agent_name + ' to ' + g.address + ' are cut immediately.', 'Revoke', true))) return;
    await api('POST', orgPath('/grants/' + g.id + '/revoke'));
    toast('Revoked — connections cut'); route();
  });

  const card = (g) => h('div', { class: 'card pending' },
    h('div', { class: 'spread' },
      h('div', {},
        h('div', { style: 'font-weight:700;font-size:15px' }, g.agent_name, h('span', { class: 'muted', style: 'font-weight:500' }, ' from ', g.vendor_slug)),
        h('div', { class: 'row', style: 'margin-top:6px' }, 'wants ', modeBadge(g.mode), ' access to ', h('code', {}, g.address),
          h('span', { class: 'badge' }, g.resource_kind), ' for ', h('b', {}, dur(g.requested_ttl_secs * 1000)))),
      h('div', { class: 'small muted' }, 'asked ', ago(g.created_at))),
    h('div', { class: 'reason' }, g.reason || '(no reason given)'),
    g.mode === 'rw' ? h('p', { class: 'small', style: 'color:var(--warn)' },
      g.first_approval_recorded ? '1 of 2 approvals recorded — a second, different approver must approve.' :
        'Read-write access needs two different approvers.') : null,
    isApprover() ? h('div', { class: 'row' },
      h('button', { class: 'primary', onclick: () => decide(g, 'approve') }, g.first_approval_recorded ? 'Add second approval' : 'Approve'),
      h('button', { onclick: () => decide(g, 'deny') }, 'Deny'))
      : h('p', { class: 'small muted' }, 'Only owners, admins and approvers can decide.'));

  return h('div', {},
    pending.length ? pending.map(card) : h('div', { class: 'empty' }, 'No pending requests. New ones appear here automatically.'),
    h('h2', {}, 'Active access'),
    active.length ? h('div', { class: 'tablewrap' }, h('table', {},
      h('tr', {}, h('th', {}, 'Agent'), h('th', {}, 'Resource'), h('th', {}, 'Mode'), h('th', {}, 'Ends'), h('th', {}, '')),
      active.map((g) => h('tr', {},
        h('td', {}, g.agent_name, h('div', { class: 'small muted' }, g.vendor_slug)),
        h('td', {}, h('code', {}, g.address)), h('td', {}, modeBadge(g.mode)), h('td', {}, until(g.expires_at)),
        h('td', { style: 'text-align:right' }, isApprover() ? h('button', { class: 'danger', onclick: () => revoke(g) }, 'Revoke') : null)))))
      : h('div', { class: 'empty' }, 'No agent has access right now.'),
    history.length ? [h('h2', {}, 'History'), h('div', { class: 'tablewrap' }, h('table', {},
      h('tr', {}, h('th', {}, 'Agent'), h('th', {}, 'Resource'), h('th', {}, 'Status'), h('th', {}, 'Requested')),
      history.map((g) => h('tr', {}, h('td', {}, g.agent_name, ' · ', h('span', { class: 'muted' }, g.vendor_slug)),
        h('td', {}, h('code', {}, g.address)), h('td', {}, statusBadge(g.status)), h('td', { class: 'small muted' }, when(g.created_at))))))] : null);
}

// ── customer: sites ─────────────────────────────────────────────────────────

async function viewSites() {
  const [{ sites = [] }, { resources = [] }] = await Promise.all([api('GET', orgPath('/sites')), api('GET', orgPath('/resources'))]);
  const kill = (s) => act(async () => {
    if (!(await confirmBox('Cut all access to ' + s.name + '?',
      'Every approved grant on this site is revoked and every open connection is closed now. Agents must ask again.', 'Cut access', true))) return;
    const r = await api('POST', orgPath('/sites/' + s.id + '/kill'));
    toast('Cut: ' + r.grants_revoked + ' grants revoked, ' + r.streams_closed + ' connections closed in ' + r.cut_ms + ' ms');
    route();
  });
  const rotate = (s) => act(async () => {
    if (!(await confirmBox('Rotate the site token?', 'The running connector is disconnected and must be set up again with the new token.', 'Rotate', true))) return;
    const r = await api('POST', orgPath('/sites/' + s.id + '/rotate-token'));
    const d = h('dialog', {}, h('h2', { style: 'margin-top:0' }, 'New connector command'),
      h('div', { class: 'callout' }, 'Shown once. Run the command on the connector machine and paste the token when asked.'),
      copyBlock(r.connect_command + ' --setup-only'), h('span', { class: 'small muted' }, 'Token:'), copyBlock(r.token, true),
      h('div', { class: 'row', style: 'justify-content:flex-end' }, h('button', { class: 'primary', onclick: () => d.close() }, 'Done')));
    d.addEventListener('close', () => { d.remove(); route(); });
    document.body.append(d); d.showModal();
  });
  const edit = (r) => {
    const desc = h('input', { value: r.description || '', maxlength: 500, style: 'width:100%' });
    const lim = r.limits || {};
    const rows = h('input', { type: 'number', min: 0, value: lim.max_rows || '', placeholder: 'unlimited' });
    const qps = h('input', { type: 'number', min: 0, value: lim.qps || '', placeholder: 'unlimited' });
    const d = h('dialog', {}, h('h2', { style: 'margin-top:0' }, r.name),
      h('label', {}, 'Description (vendors see this)'), desc,
      h('div', { class: 'form' }, h('div', {}, h('label', {}, 'Max rows per query'), rows), h('div', {}, h('label', {}, 'Queries per second'), qps)),
      h('div', { class: 'row', style: 'justify-content:flex-end;margin-top:16px' },
        h('button', { onclick: () => d.close() }, 'Cancel'),
        h('button', { class: 'primary', onclick: () => act(async () => {
          const limits = {};
          if (+rows.value > 0) limits.max_rows = +rows.value;
          if (+qps.value > 0) limits.qps = +qps.value;
          await api('PATCH', orgPath('/resources/' + r.id), { description: desc.value, limits });
          d.close(); route();
        }, 'Saved') }, 'Save')));
    d.addEventListener('close', () => d.remove());
    document.body.append(d); d.showModal();
  };

  return h('div', {},
    isAdmin() ? h('div', { style: 'margin-bottom:14px' }, h('button', { class: 'primary', onclick: () => { location.hash = '#/sites/new'; } }, '+ Add a site')) : null,
    sites.length ? sites.map((s) => {
      const res = resources.filter((r) => r.site_id === s.id);
      return h('div', { class: 'card' },
        h('div', { class: 'spread' },
          h('div', {}, h('div', { style: 'font-weight:700;font-size:15px' }, s.name),
            h('div', { class: 'row small', style: 'margin-top:4px' }, h('span', { class: 'status ' + s.status }, s.status),
              h('span', { class: 'muted' }, 'last seen ' + ago(s.last_seen_at)),
              s.connector_version ? h('span', { class: 'muted' }, 'connector ' + s.connector_version) : null),
            s.tls_fingerprint ? h('div', { class: 'small muted', style: 'margin-top:4px' }, 'End-to-end TLS · sha256 ',
              h('code', {}, s.tls_fingerprint.slice(0, 16) + '…' + s.tls_fingerprint.slice(-8))) : null),
          isAdmin() ? h('div', { class: 'row' }, h('button', { class: 'danger solid', onclick: () => kill(s) }, 'Cut all access'),
            h('button', { onclick: () => rotate(s) }, 'Rotate token')) : null),
        res.length ? h('div', { class: 'tablewrap', style: 'margin-top:12px' }, h('table', {},
          h('tr', {}, h('th', {}, 'Resource'), h('th', {}, 'Kind'), h('th', {}, 'Status'), h('th', {}, 'Limits'), h('th', {}, '')),
          res.map((r) => h('tr', {},
            h('td', {}, h('code', {}, state.org.slug + '/' + r.name), r.description ? h('div', { class: 'small muted' }, r.description) : null),
            h('td', {}, r.kind), h('td', {}, h('span', { class: 'badge ' + (r.status === 'active' ? 'ok' : '') }, r.status)),
            h('td', { class: 'small muted' }, Object.entries(r.limits || {}).map(([k, v]) => k + '=' + v).join(', ') || '—'),
            h('td', { style: 'text-align:right' }, isAdmin() ? h('button', { onclick: () => edit(r) }, 'Edit') : null)))))
          : h('p', { class: 'muted small', style: 'margin-top:10px' }, 'No resources yet — add them on the connector machine with tunr connect resource add.'));
    }) : h('div', { class: 'empty' }, 'No sites yet. Add one to connect a network location.'));
}

async function viewNewSite() {
  const name = h('input', { placeholder: 'istanbul-office', pattern: '[a-z0-9-]{3,32}' });
  const box = h('div');
  const create = () => act(async () => {
    const r = await api('POST', orgPath('/sites'), { name: name.value.trim() });
    const base = r.connect_command.replace(/^tunr connect /, '');
    box.replaceChildren(
      h('div', { class: 'callout' }, 'The token below is shown only once. Keep this page open until the connector is online.'),
      h('ol', { class: 'steps' },
        h('li', {}, h('b', {}, 'Install tunr on a machine inside the network'), ' (it only needs outbound HTTPS):',
          copyBlock('curl -fsSL https://tunr.sh/install.sh | sh')),
        h('li', {}, h('b', {}, 'Connect it to this site.'), ' Linux server (runs as a service):',
          copyBlock('sudo tunr connect ' + base + ' --secret-store file --setup-only'),
          h('span', { class: 'small muted' }, 'macOS or a desktop:'), copyBlock('tunr connect ' + base + ' --setup-only'),
          h('span', { class: 'small muted' }, 'Paste this token when asked:'), copyBlock(r.token, true)),
        h('li', {}, h('b', {}, 'Name what agents may reach.'), ' The address never leaves this machine:',
          copyBlock('sudo tunr connect resource add erp-db --kind postgres --target 10.0.0.5:5432 --database logo')),
        h('li', {}, h('b', {}, 'Create a read-only database role'), ' and give the SQL to your DBA:',
          copyBlock('sudo tunr connect pg-role --database logo --tables invoices,stock --resource erp-db > pg-role.sql')),
        h('li', {}, h('b', {}, 'Start it as a service:'), copyBlock('sudo tunr connect install'))),
      h('div', { class: 'card', id: 'wait' }, h('span', { class: 'status offline' }, 'Waiting for the connector…')));
    const site = r.site;
    const tick = setInterval(async () => {
      if (!document.getElementById('wait')) { clearInterval(tick); return; }
      try {
        const s = await api('GET', orgPath('/sites/' + site.id));
        if (s.status === 'online') {
          clearInterval(tick);
          document.getElementById('wait').replaceChildren(h('span', { class: 'status online' }, 'Connected'),
            h('p', { class: 'muted', style: 'margin-top:8px' }, 'Next: allow a vendor under Vendors.'),
            h('button', { class: 'primary', onclick: () => { location.hash = '#/sites'; } }, 'Go to sites'));
        }
      } catch { /* keep waiting */ }
    }, 3000);
  });
  return h('div', {}, h('div', { class: 'card' },
    h('label', {}, 'Site name'), h('div', { class: 'form' }, name, h('button', { class: 'primary', onclick: create }, 'Create site')),
    h('p', { class: 'small muted', style: 'margin-top:6px' }, 'Lowercase letters, digits and hyphens — e.g. the office or data-centre name.')), box);
}

// ── customer: vendors ───────────────────────────────────────────────────────

async function viewVendors() {
  const { links = [] } = await api('GET', orgPath('/links'));
  const mine = links.filter((l) => l.customer.id === state.org.id);
  const slug = h('input', { placeholder: 'vendor-slug' });
  const add = () => act(async () => { await api('POST', orgPath('/links'), { vendor_slug: slug.value.trim() }); route(); }, 'Vendor allowed');
  const unlink = (l) => act(async () => {
    if (!(await confirmBox('Remove ' + l.vendor.name + '?', 'They can no longer see your resources or ask for access. Existing grants are not revoked automatically — revoke them under Approvals.', 'Remove', true))) return;
    await api('DELETE', orgPath('/links/' + l.id)); route();
  }, 'Vendor removed');
  return h('div', {},
    isAdmin() ? h('div', { class: 'card' }, h('label', {}, 'Allow a vendor (their organization slug)'),
      h('div', { class: 'form' }, slug, h('button', { class: 'primary', onclick: add }, 'Allow')),
      h('p', { class: 'small muted', style: 'margin-top:6px' }, 'Allowing a vendor only lets them see resource names and ask. Every access still needs your approval.')) : null,
    mine.length ? h('div', { class: 'tablewrap' }, h('table', {},
      h('tr', {}, h('th', {}, 'Vendor'), h('th', {}, 'Status'), h('th', {}, 'Since'), h('th', {}, '')),
      mine.map((l) => h('tr', {}, h('td', {}, l.vendor.name, h('div', { class: 'small muted' }, l.vendor.slug)),
        h('td', {}, h('span', { class: 'badge ' + (l.status === 'active' ? 'ok' : 'bad') }, l.status)),
        h('td', { class: 'small muted' }, when(l.created_at)),
        h('td', { style: 'text-align:right' }, isAdmin() && l.status === 'active' ? h('button', { class: 'danger', onclick: () => unlink(l) }, 'Remove') : null)))))
      : h('div', { class: 'empty' }, 'No vendors allowed yet.'));
}

// ── customer: activity ──────────────────────────────────────────────────────

async function viewActivity() {
  const { grants = [] } = await api('GET', orgPath('/grants?side=customer'));
  const used = grants.filter((g) => g.status !== 'pending' && g.status !== 'denied');
  const detail = h('div');
  const showGrant = async (g) => {
    detail.replaceChildren(h('p', { class: 'muted' }, 'Loading…'));
    const { sessions = [] } = await api('GET', orgPath('/grants/' + g.id + '/sessions'));
    const blocks = await Promise.all(sessions.slice(0, 20).map(async (s) => {
      const { queries = [] } = await api('GET', orgPath('/sessions/' + s.id + '/queries'));
      return h('div', { class: 'card' },
        h('div', { class: 'spread small' }, h('span', {}, h('b', {}, when(s.opened_at)), ' · ', s.closed_at ? dur(new Date(s.closed_at) - new Date(s.opened_at)) : 'open'),
          h('span', { class: 'muted' }, (s.close_reason || 'active') + ' · ' + s.query_count + ' queries · ' + (s.bytes_down / 1024).toFixed(1) + ' KiB out')),
        queries.length ? h('div', { class: 'tablewrap', style: 'margin-top:10px' }, h('table', { class: 'fixed' },
          h('tr', {}, h('th', {}, 'Time'), h('th', {}, 'Command'), h('th', {}, 'Tables'), h('th', {}, 'Rows'), h('th', {}, 'ms')),
          queries.map((q) => [h('tr', { class: q.blocked ? 'blocked' : '' },
            h('td', { class: 'small' }, new Date(q.created_at).toLocaleTimeString()),
            h('td', {}, q.blocked ? h('span', { class: 'badge bad' }, 'blocked ' + q.command.replace(/^blocked:/, '')) : h('code', {}, q.command)),
            h('td', {}, (q.tables || []).join(', ') || '—'), h('td', {}, q.row_count ?? '—'), h('td', {}, q.duration_ms ?? '—')),
          q.query_text ? h('tr', {}, h('td', { colspan: 5 }, h('code', { class: 'small' }, q.query_text))) : null])))
          : h('p', { class: 'small muted', style: 'margin-top:8px' }, g.resource_kind === 'postgres'
            ? 'No queries in this session.' : 'Raw TCP resources record sessions and bytes, not individual queries.'));
    }));
    detail.replaceChildren(h('h2', {}, g.agent_name + ' → ' + g.address), ...(blocks.length ? blocks : [h('div', { class: 'empty' }, 'No sessions yet.')]));
  };
  const exportCSV = () => act(async () => {
    const res = await api('GET', orgPath('/audit/export?format=csv'), null, true);
    const url = URL.createObjectURL(await res.blob());
    const a = h('a', { href: url, download: 'tunr-audit-' + state.org.slug + '.csv' });
    document.body.append(a); a.click(); a.remove(); URL.revokeObjectURL(url);
  });
  if (used.length) showGrant(used[0]);
  return h('div', {},
    isAdmin() ? h('div', { style: 'margin-bottom:14px' }, h('button', { onclick: exportCSV }, 'Export audit log (CSV)')) : null,
    used.length ? h('p', { class: 'small muted' }, 'Select a grant to see its sessions and queries.') : null,
    used.length ? h('div', { class: 'tablewrap' }, h('table', {},
      h('tr', {}, h('th', {}, 'Agent'), h('th', {}, 'Resource'), h('th', {}, 'Mode'), h('th', {}, 'Status'), h('th', {}, 'Approved')),
      used.map((g) => h('tr', { class: 'click', onclick: () => showGrant(g) },
        h('td', {}, g.agent_name, h('div', { class: 'small muted' }, g.vendor_slug)), h('td', {}, h('code', {}, g.address)),
        h('td', {}, modeBadge(g.mode)), h('td', {}, statusBadge(g.status)), h('td', { class: 'small muted' }, when(g.approved_at))))))
      : h('div', { class: 'empty' }, 'No access has been used yet.'),
    detail);
}

// ── vendor: agents ──────────────────────────────────────────────────────────

async function viewAgents() {
  const { agents = [] } = await api('GET', orgPath('/agents'));
  const name = h('input', { placeholder: 'invoice-reconciler' });
  const create = () => act(async () => { await api('POST', orgPath('/agents'), { name: name.value.trim() }); route(); }, 'Agent created');
  const issue = (a) => act(async () => {
    const ttl = h('select', {}, ['1h', '8h', '24h'].map((v) => h('option', { value: v, selected: v === '24h' }, v)));
    const out = h('div');
    const d = h('dialog', {}, h('h2', { style: 'margin-top:0' }, 'Token for ' + a.name),
      h('label', {}, 'Lifetime'), ttl, out,
      h('div', { class: 'row', style: 'justify-content:flex-end;margin-top:14px' },
        h('button', { onclick: () => d.close() }, 'Close'),
        h('button', { class: 'primary', onclick: () => act(async () => {
          const r = await api('POST', orgPath('/agents/' + a.id + '/tokens'), { ttl: ttl.value });
          const relay = location.origin;
          out.replaceChildren(h('div', { class: 'callout' }, 'Shown once — store it in the agent’s secret manager.'),
            copyBlock('export TUNR_AGENT_TOKEN=' + r.token + '\nexport TUNR_RELAY_URL=' + relay, true),
            h('p', { class: 'small muted' }, 'MCP (Claude Code, Cursor):'),
            copyBlock(JSON.stringify({ mcpServers: { tunr: { command: 'tunr', args: ['mcp'], env: { TUNR_AGENT_TOKEN: r.token, TUNR_RELAY_URL: relay } } } }, null, 2), true));
        }) }, 'Issue token')));
    d.addEventListener('close', () => { d.remove(); route(); });
    document.body.append(d); d.showModal();
  });
  const toggle = (a) => act(async () => {
    await api('PATCH', orgPath('/agents/' + a.id), { status: a.status === 'active' ? 'disabled' : 'active' }); route();
  });
  const tokens = async (a, cell) => {
    const { tokens: ts = [] } = await api('GET', orgPath('/agents/' + a.id + '/tokens'));
    const live = ts.filter((t) => !t.revoked_at && new Date(t.expires_at) > Date.now());
    cell.replaceChildren(...(live.length ? live.map((t) => h('div', { class: 'row small' }, h('code', {}, t.id), h('span', { class: 'muted' }, 'expires ' + until(t.expires_at)),
      isAdmin() ? h('button', { class: 'ghost', onclick: () => act(async () => {
        await api('DELETE', orgPath('/agents/' + a.id + '/tokens/' + t.id)); route();
      }, 'Token revoked') }, 'revoke') : null)) : [h('span', { class: 'small muted' }, 'no live tokens')]));
  };
  const rows = agents.map((a) => {
    const cell = h('td', {}, '…');
    tokens(a, cell);
    return h('tr', {}, h('td', {}, h('b', {}, a.name), h('div', { class: 'small muted mono' }, a.id)),
      h('td', {}, h('span', { class: 'badge ' + (a.status === 'active' ? 'ok' : 'bad') }, a.status)), cell,
      h('td', { style: 'text-align:right;white-space:nowrap' }, isAdmin() ? [
        a.status === 'active' ? h('button', { class: 'primary', onclick: () => issue(a) }, 'Issue token') : null, ' ',
        h('button', { onclick: () => toggle(a) }, a.status === 'active' ? 'Disable' : 'Enable')] : null));
  });
  return h('div', {},
    isAdmin() ? h('div', { class: 'card' }, h('label', {}, 'New agent'), h('div', { class: 'form' }, name, h('button', { class: 'primary', onclick: create }, 'Create')),
      h('p', { class: 'small muted', style: 'margin-top:6px' }, 'One identity per agent or product feature. Customers see this name when they approve.')) : null,
    agents.length ? h('div', { class: 'tablewrap' }, h('table', {},
      h('tr', {}, h('th', {}, 'Agent'), h('th', {}, 'Status'), h('th', {}, 'Tokens'), h('th', {}, '')), rows))
      : h('div', { class: 'empty' }, 'No agents yet.'));
}

async function viewCatalog() {
  const { resources = [] } = await api('GET', orgPath('/catalog'));
  return resources.length ? h('div', { class: 'tablewrap' }, h('table', {},
    h('tr', {}, h('th', {}, 'Resource'), h('th', {}, 'Kind'), h('th', {}, 'Description')),
    resources.map((r) => h('tr', {}, h('td', {}, h('code', {}, r.address)), h('td', {}, r.kind), h('td', { class: 'muted' }, r.description || '—')))))
    : h('div', { class: 'empty' }, 'No customer has linked your organization yet. Give them your slug: ', h('code', {}, state.org.slug));
}

async function viewMyGrants() {
  const { grants = [] } = await api('GET', orgPath('/grants?side=vendor'));
  return grants.length ? h('div', { class: 'tablewrap' }, h('table', {},
    h('tr', {}, h('th', {}, 'Agent'), h('th', {}, 'Resource'), h('th', {}, 'Mode'), h('th', {}, 'Status'), h('th', {}, 'Ends'), h('th', {}, 'Reason')),
    grants.map((g) => h('tr', {}, h('td', {}, g.agent_name), h('td', {}, h('code', {}, g.address)), h('td', {}, modeBadge(g.mode)),
      h('td', {}, statusBadge(g.status), g.status === 'pending' && g.first_approval_recorded ? h('div', { class: 'small muted' }, '1 of 2 approvals') : null),
      h('td', {}, g.status === 'approved' ? until(g.expires_at) : '—'), h('td', { class: 'small muted' }, g.reason)))))
    : h('div', { class: 'empty' }, 'No requests yet. Agents ask with tunr access request or the MCP tool tunr_request_access.');
}

// ── org: members, settings, new org ─────────────────────────────────────────

async function viewMembers() {
  const { members = [] } = await api('GET', orgPath('/members'));
  const email = h('input', { type: 'email', placeholder: 'colleague@company.com' });
  const role = h('select', {}, ['approver', 'member', 'admin', 'owner'].map((r) => h('option', { value: r }, r)));
  const add = () => act(async () => { await api('POST', orgPath('/members'), { email: email.value.trim(), role: role.value }); route(); }, 'Member added');
  return h('div', {},
    state.org.personal ? h('div', { class: 'callout' }, 'Personal organizations have one member. Create a team organization to invite colleagues.') :
      isAdmin() ? h('div', { class: 'card' }, h('div', { class: 'form' }, h('div', {}, h('label', {}, 'E-mail'), email), h('div', {}, h('label', {}, 'Role'), role),
        h('button', { class: 'primary', onclick: add }, 'Add')),
        h('p', { class: 'small muted', style: 'margin-top:6px' }, 'Approvers can approve and revoke access. Read-write access needs two different approvers.')) : null,
    h('div', { class: 'tablewrap' }, h('table', {}, h('tr', {}, h('th', {}, 'E-mail'), h('th', {}, 'Role'), h('th', {}, 'Since')),
      members.map((m) => h('tr', {}, h('td', {}, m.email), h('td', {}, h('span', { class: 'badge' }, m.role)), h('td', { class: 'small muted' }, when(m.created_at)))))));
}

async function viewSettings() {
  const st = await api('GET', orgPath('/settings'));
  const parts = [h('div', { class: 'card' },
    h('div', { class: 'spread' }, h('div', {}, h('b', {}, 'Organization'), h('div', { class: 'small muted' }, state.org.name + ' · slug ', h('code', {}, state.org.slug), ' · ' + state.org.kind + ' · your role: ' + state.org.role))))];
  if (isCustomer()) {
    const logText = h('input', { type: 'checkbox', checked: !!st.log_query_text, disabled: !isAdmin() });
    const current = ({ '1h0m0s': '1h', '4h0m0s': '4h', '8h0m0s': '8h', '24h0m0s': '24h' })[st.max_grant_ttl] || '8h';
    const maxTTL = h('select', { disabled: !isAdmin() }, ['1h', '4h', '8h', '24h'].map((v) =>
      h('option', { value: v, selected: v === current }, v)));
    const save = () => act(async () => { await api('PATCH', orgPath('/settings'), { log_query_text: logText.checked, max_grant_ttl: maxTTL.value }); }, 'Settings saved');
    parts.push(h('div', { class: 'card' }, h('b', {}, 'Access policy'),
      h('label', { class: 'row', style: 'font-weight:500' }, logText, 'Keep the full text of every query (otherwise only command, tables, rows and timing)'),
      h('label', {}, 'Longest access an agent may ask for'), maxTTL,
      isAdmin() ? h('div', { style: 'margin-top:12px' }, h('button', { class: 'primary', onclick: save }, 'Save')) : null));
  }
  if (isAdmin()) {
    const { webhooks = [] } = await api('GET', orgPath('/webhooks'));
    const url = h('input', { type: 'url', placeholder: 'https://hooks.example.com/tunr', style: 'min-width:280px' });
    const out = h('div');
    const add = () => act(async () => {
      const r = await api('POST', orgPath('/webhooks'), { url: url.value.trim() });
      out.replaceChildren(h('div', { class: 'callout' }, 'Signing secret (shown once). Verify X-Tunr-Signature: t=…,v1=HMAC-SHA256(secret, "t.body").'), copyBlock(r.secret, true));
    }, 'Webhook added');
    parts.push(h('div', { class: 'card' }, h('b', {}, 'Webhooks'), h('p', { class: 'small muted' }, 'Grant requests, approvals, revocations and expiries are POSTed here.'),
      h('div', { class: 'form' }, url, h('button', { class: 'primary', onclick: add }, 'Add')), out,
      webhooks.map((w) => h('div', { class: 'row small', style: 'margin-top:8px' }, h('code', {}, w.url),
        h('button', { class: 'ghost', onclick: () => act(async () => { await api('DELETE', orgPath('/webhooks/' + w.id)); route(); }, 'Webhook removed') }, 'remove')))));
  }
  return h('div', {}, parts);
}

function viewNewOrg() {
  const name = h('input', { placeholder: 'Acme Muhasebe' });
  const slug = h('input', { placeholder: 'acme', pattern: '[a-z0-9-]{3,32}' });
  name.addEventListener('input', () => { if (!slug.dataset.touched) slug.value = name.value.toLowerCase().normalize('NFKD').replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '').slice(0, 32); });
  slug.addEventListener('input', () => { slug.dataset.touched = '1'; });
  const kind = h('select', {}, h('option', { value: 'customer' }, 'Customer — our data, AI vendors ask for access'),
    h('option', { value: 'vendor' }, 'Vendor — we build agents that use customer data'), h('option', { value: 'both' }, 'Both'));
  const create = () => act(async () => {
    const o = await api('POST', '/orgs', { name: name.value.trim(), slug: slug.value.trim(), kind: kind.value });
    store.set(LS_ORG, o.slug); state.orgs = []; location.hash = '#/'; route();
  }, 'Organization created');
  return h('div', { class: 'card', style: 'max-width:520px' },
    h('label', {}, 'Name'), name, h('label', {}, 'Slug (part of resource addresses, e.g. acme/erp-db)'), slug,
    h('label', {}, 'Type'), kind, h('div', { style: 'margin-top:14px' }, h('button', { class: 'primary', onclick: create }, 'Create')));
}

window.addEventListener('hashchange', route);
route();
