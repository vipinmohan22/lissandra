'use strict';

// Lissandra admin UI. No framework and no build step: the DOM is built with the
// h() helper, which only ever inserts text nodes for content, so log data
// can never be interpreted as HTML.

// ---------- DOM helpers ----------

function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = Boolean(v);
    else if (k === 'selected') el.selected = Boolean(v);
    else if (k === 'disabled') el.disabled = Boolean(v);
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  append(el, ...children);
  return el;
}

function append(el, ...children) {
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

const SVGNS = 'http://www.w3.org/2000/svg';
function s(tag, attrs, ...children) {
  const el = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, String(v));
  for (const c of children) el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  return el;
}

function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); return el; }

const SEVERITIES = ['debug', 'info', 'notice', 'warning', 'error', 'critical'];
const sevRank = (sv) => SEVERITIES.indexOf(sv);

function sevBadge(sv) { return h('span', { class: `badge sev sev-${SEVERITIES.includes(sv) ? sv : 'debug'}` }, sv || '?'); }
function tags(list) { return (list || []).map((t) => h('span', { class: 'tag' }, t)); }

function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  return isNaN(d) ? '—' : d.toLocaleString();
}

function fmtDate(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  return isNaN(d) ? '—' : d.toLocaleDateString();
}

function ago(iso) {
  if (!iso) return 'never';
  const sec = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
  if (sec < 5) return 'just now';
  if (sec < 60) return `${sec}s ago`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ago`;
  if (sec < 86400) return `${Math.floor(sec / 3600)}h ago`;
  return `${Math.floor(sec / 86400)}d ago`;
}

function timeCell(iso) { return h('span', { title: fmtTime(iso), class: 'nowrap' }, ago(iso)); }

function num(n) { return Number(n || 0).toLocaleString(); }

// plural counts: plural(1, 'event') is "1 event", plural(2, 'event') "2 events".
function plural(n, word) { return `${num(n)} ${word}${Number(n || 0) === 1 ? '' : 's'}`; }

function toast(msg, kind) {
  const t = h('div', { class: `toast ${kind || ''}` }, msg);
  document.getElementById('toasts').append(t);
  setTimeout(() => t.remove(), kind === 'error' ? 7000 : 3500);
}

async function copyText(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
    } else {
      // Plain-HTTP deployments have no Clipboard API.
      const ta = h('textarea', { readonly: true }, text);
      document.body.append(ta);
      ta.select();
      const ok = document.execCommand('copy');
      ta.remove();
      if (!ok) throw new Error('copy failed');
    }
    toast('Copied to clipboard');
  } catch {
    toast('Copy failed — select the text manually', 'error');
  }
}

function codeBox(text) {
  return h('div', { class: 'code-box' },
    h('button', { type: 'button', class: 'small copy', onclick: () => copyText(text) }, 'Copy'),
    h('pre', null, text));
}

// field labels a form control. A single input, select or textarea is tied
// to its label and help; composite controls are passed as they are.
let fieldSeq = 0;
function field(label, input, help) {
  const lab = h('label', null, label);
  const hint = help ? h('div', { class: 'help' }, help) : null;
  if (input instanceof HTMLInputElement || input instanceof HTMLSelectElement || input instanceof HTMLTextAreaElement) {
    if (!input.id) input.id = `f-${++fieldSeq}`;
    lab.htmlFor = input.id;
    if (hint) {
      hint.id = `${input.id}-help`;
      addDescribedBy(input, hint.id);
    }
  }
  return h('div', { class: 'field' }, lab, input, hint);
}

function addDescribedBy(el, id) {
  const ids = (el.getAttribute('aria-describedby') || '').split(' ').filter(Boolean);
  if (!ids.includes(id)) el.setAttribute('aria-describedby', [...ids, id].join(' '));
}

// fieldError shows text next to input (in box), marks the input invalid
// and focuses it.
function fieldError(box, input, text) {
  const id = `${input.id || `f-${++fieldSeq}`}-err`;
  clear(box).append(h('div', { class: 'callout bad', role: 'alert', id }, text));
  input.setAttribute('aria-invalid', 'true');
  addDescribedBy(input, id);
  input.focus();
}

function randomHex(bytes) {
  const b = new Uint8Array(bytes);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

function checkbox(label, checked, onchange) {
  const input = h('input', { type: 'checkbox', checked, onchange: onchange ? (e) => onchange(e.target.checked) : null });
  return { el: h('label', { class: 'check' }, input, label), input };
}

function select(options, value, props) {
  const el = h('select', props || {});
  for (const o of options) {
    const [v, label] = Array.isArray(o) ? o : [o, o];
    el.append(h('option', { value: v, selected: v === value }, label));
  }
  return el;
}

// Secret inputs stay readable when they hold only ${env:NAME} or
// ${file:/path} references, which are not themselves secret.
function secretInput(value, props) {
  const isRef = (v) => /^(\$\{(env|file):[^}]+\})+$/.test(v);
  const input = h('input', { ...props, type: isRef(value) ? 'text' : 'password', value, autocomplete: 'off' });
  input.addEventListener('input', () => { input.type = isRef(input.value) || input.value === '' ? 'text' : 'password'; });
  return input;
}

function splitList(text) {
  return String(text || '').split(',').map((x) => x.trim()).filter(Boolean);
}

function headersToText(obj) {
  return Object.entries(obj || {}).map(([k, v]) => `${k}: ${v}`).join('\n');
}

function textToHeaders(text) {
  const out = {};
  for (const line of String(text || '').split('\n')) {
    const i = line.indexOf(':');
    if (i > 0) out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return out;
}

// ---------- API ----------

// The admin token is kept for the tab. Storage can be blocked (private
// windows, site data policies): the session then lasts while the page is
// open.
let token = '';
try { token = sessionStorage.getItem('lissandra.token') || ''; } catch { /* storage blocked */ }

function setToken(t) {
  token = t;
  try {
    if (t) sessionStorage.setItem('lissandra.token', t); else sessionStorage.removeItem('lissandra.token');
  } catch { /* storage blocked: kept in memory only */ }
}

// tokenChange is set while this tab changes the admin token. It always
// resolves: a request refused meanwhile waits for it, then retries with
// the new token, so this tab stays signed in.
let tokenChange = null;

const TOKEN_REJECTED = 'That token is no longer accepted. Lissandra may have restarted with a new generated token, or the admin token was changed. Sign in again.';
const TOKEN_CHANGED_ELSEWHERE = 'The admin token was changed in another session. Sign in with the new one.';

class AuthError extends Error {}
// InputError is a form value that can't be saved: shown, but not logged.
class InputError extends Error {}
// SetupStopped is a setup request refused because setup was finished or
// skipped in another session. It has been reported.
class SetupStopped extends Error {}

// unauthorized handles a 401 to a request sent with the token sent. A 401
// comes from the token check before any handler runs, so retrying once with
// a token this tab changed to meanwhile is safe even for a POST.
async function unauthorized(sent, retried, retry) {
  if (tokenChange) await tokenChange;
  if (!retried && token !== sent) return retry();
  showLogin(sent ? TOKEN_REJECTED : undefined);
  throw new AuthError('Sign in required');
}

async function api(method, path, body, retried = false) {
  const sent = token;
  const opts = { method, headers: { Authorization: `Bearer ${sent}` } };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401) return unauthorized(sent, retried, () => api(method, path, body, true));
  if (res.status === 204) return null;
  const data = await res.json().catch(() => null);
  if (!res.ok) {
    const err = new Error((data && data.error) || `${res.status} ${res.statusText}`);
    err.status = res.status;
    err.code = (data && data.code) || '';
    throw err;
  }
  return data;
}

const GET = (p) => api('GET', p);

// download fetches path with the admin token and saves the response under
// the name the server gives it (else fallbackName). With emptyNote, an
// empty response is not saved: the note is shown instead.
async function download(path, fallbackName, emptyNote, retried = false) {
  const sent = token;
  const res = await fetch(path, { headers: { Authorization: `Bearer ${sent}` } });
  if (res.status === 401) return unauthorized(sent, retried, () => download(path, fallbackName, emptyNote, true));
  if (!res.ok) {
    const data = await res.json().catch(() => null);
    throw new Error(`Download failed: ${(data && data.error) || `${res.status} ${res.statusText}`}`);
  }
  const blob = await res.blob();
  if (!blob.size && emptyNote) {
    toast(emptyNote);
    return;
  }
  const m = /filename="([^"/\\]+)"/.exec(res.headers.get('Content-Disposition') || '');
  saveBlob(blob, m ? m[1] : fallbackName);
}

function reportError(err) {
  if (err instanceof AuthError || err instanceof SetupStopped) return;
  if (!(err instanceof InputError)) console.error(err);
  toast(err.message || String(err), 'error');
}

// busy runs fn with btn disabled, reporting a failure. Disabling the
// button dropped its focus: it gets it back unless fn moved it.
async function busy(btn, fn) {
  btn.disabled = true;
  try { await fn(); } catch (err) { reportError(err); } finally {
    btn.disabled = false;
    if (!document.activeElement || document.activeElement === document.body) btn.focus();
  }
}

// Shared lookups, refreshed when a page needs them.
const cache = {};
async function load(...keys) {
  const paths = {
    info: '/api/info', sources: '/api/sources', notifiers: '/api/notifiers', settings: '/api/settings',
    types: '/api/notifier-types', rules: '/api/rules', routes: '/api/routes', groups: '/api/alert-groups',
    auth: '/api/auth', onboarding: '/api/onboarding',
  };
  await Promise.all(keys.map(async (k) => { cache[k] = await GET(paths[k]); }));
}

function sourceName(id) {
  const src = (cache.sources || []).find((x) => x.id === id);
  return src ? src.name : id;
}

function groupName(id) {
  const g = (cache.groups || []).find((x) => x.id === id);
  return g ? g.name : id;
}

// categoryList returns the configured categories as {name, description}, in
// order. Older configs stored plain names.
function categoryList() {
  return ((cache.settings && cache.settings.categories) || [])
    .map((c) => (typeof c === 'string' ? { name: c, description: '' } : { name: c.name || '', description: c.description || '' }))
    .filter((c) => c.name);
}

// categories returns the configured category names, in order.
function categories() {
  return categoryList().map((c) => c.name);
}

function engineSettings() {
  return (cache.settings && cache.settings.engine) || {};
}

// ---------- Laya engine helpers ----------

// Checkpoints laya-serve ships. Anything else is a fine-tune named in the
// engine's LAYA_EXTRA_MODELS.
const ENGINE_MODELS = [
  ['english', 'english — English logs, calibrated'],
  ['multilingual', 'multilingual — other languages, faster on CPU'],
  ['typed-decisions', 'typed-decisions'],
];
const CUSTOM_MODEL = '\u0000custom';

// modelPicker is a checkpoint select with a "custom…" choice that reveals a
// text input. Stored values it doesn't know show up as custom, so they
// survive a save. first is an optional leading [value, label] option.
function modelPicker(value, first) {
  value = value || '';
  const known = [...(first ? [first[0]] : []), ...ENGINE_MODELS.map((m) => m[0])];
  const sel = select([...(first ? [first] : []), ...ENGINE_MODELS, [CUSTOM_MODEL, 'custom…']], '', { 'aria-label': 'Checkpoint' });
  const custom = h('input', { placeholder: 'checkpoint name from the engine\'s LAYA_EXTRA_MODELS', 'aria-label': 'Custom checkpoint name', autocomplete: 'off', spellcheck: 'false', maxlength: 64 });
  const toggle = () => { custom.hidden = sel.value !== CUSTOM_MODEL; };
  const set = (v) => {
    v = v || '';
    const isCustom = !known.includes(v);
    sel.value = isCustom ? CUSTOM_MODEL : v;
    custom.value = isCustom ? v : '';
    toggle();
  };
  sel.addEventListener('change', () => { toggle(); if (!custom.hidden) custom.focus(); });
  set(value);
  return {
    el: h('div', { class: 'model-pick' }, sel, custom),
    set,
    value: () => {
      if (sel.value !== CUSTOM_MODEL) return sel.value;
      const v = custom.value.trim();
      if (!v) throw new InputError('Enter the custom checkpoint name, or pick a checkpoint from the list.');
      return v;
    },
  };
}

function pct(p) {
  const n = Number(p || 0) * 100;
  if (n > 0 && n < 1) return '<1%';
  if (n > 99 && n < 100) return '>99%';
  return `${Math.round(n)}%`;
}

// topText lists label probabilities, most likely first: "db 82% · net 9%".
function topText(probs, n = 3) {
  return Object.entries(probs || {}).sort((a, b) => b[1] - a[1]).slice(0, n).map(([k, p]) => `${k} ${pct(p)}`).join(' · ');
}

// probBars draws the n most likely labels as bars.
function probBars(probs, n, chosen) {
  const entries = Object.entries(probs || {}).sort((a, b) => b[1] - a[1]).slice(0, n);
  return h('div', { class: 'probs' }, entries.map(([k, p]) => {
    const fill = h('div', { class: 'fill' });
    fill.style.width = `${Math.max(0, Math.min(100, p * 100))}%`;
    return h('div', { class: `hbar ${k === chosen ? 'chosen' : ''}` },
      h('span', { class: 'name', title: k }, k), h('div', { class: 'track' }, fill), h('span', { class: 'n' }, pct(p)));
  }));
}

// engineAnswer renders a POST /api/engine/classify reply: what the engine
// chose, how sure it was, and what the rules alone decide today (for a
// stored event too: the server runs the rules again, without the engine).
function engineAnswer(r) {
  if (!r.ok) return h('div', { class: 'callout bad mt' }, r.error || 'The engine did not answer.');
  const res = r.result;
  const rules = r.rules || {};
  const es = engineSettings();
  const min = Number(es.min_confidence ?? 0.6);
  const sure = res.category_confidence >= min;
  const rulesCat = rules.category || 'uncategorized';
  const ruleCat = rulesCat !== 'uncategorized';
  const asked = res.category_asked !== false;
  // Would a live event like this be sent to the engine at all?
  const belowMin = es.min_severity && rules.severity && sevRank(rules.severity) < sevRank(es.min_severity);
  const onlyUncat = es.only_uncategorized !== false;
  let notSent = '';
  if (es.enabled === false) notSent = 'The engine is switched off, so live events are not sent to it.';
  else if (rules.dropped) notSent = `A rule drops live events like this (${(rules.matched_rules || []).slice(-1)[0] || 'a drop rule'}), so the engine is never asked.`;
  else if (belowMin) notSent = `Live events at ${rules.severity} are not sent to the engine (minimum severity: ${es.min_severity}), so its answer would not be used.`;
  else if (ruleCat && onlyUncat) notSent = `A rule sets the category (${rulesCat}) and only uncategorized events are sent to the engine, so its answer would not be used.`;
  let outcome;
  if (notSent) {
    outcome = notSent;
  } else if (!asked) {
    outcome = 'No categories are configured, so only the severity was asked.';
  } else if (ruleCat) {
    outcome = `A rule sets the category (${rulesCat}), and rules win: the engine's category would only be recorded.`;
  } else if (res.category === 'uncategorized') {
    outcome = 'The engine found no category that fits; the event stays uncategorized.';
  } else if (!sure) {
    outcome = `Below the ${pct(min)} confidence threshold: the event would stay uncategorized.`;
  } else {
    outcome = `The engine's category (${res.category}) would be used.`;
  }
  // The engine is asked for a severity only when the rules guessed it from
  // keywords; a severity the record states (or a rule sets) is never replaced.
  let severity = null;
  if (res.severity) {
    let use;
    if (notSent) use = 'not used: a live event like this is not sent to the engine';
    else if (res.severity_confidence < min) use = `below ${pct(min)}: the severity guessed from keywords (${rules.severity}) stands`;
    else if (res.severity === rules.severity) use = 'agrees with the severity guessed from keywords';
    else use = `would replace ${rules.severity}, which was only guessed from keywords`;
    severity = h('div', null, sevBadge(res.severity), ` · ${pct(res.severity_confidence)} sure · ${use}`);
  } else if (es.set_severity) {
    severity = h('div', { class: 'muted' }, 'Not asked: the record states its level or a rule sets it, and a stated severity is never replaced.');
  }
  const matched = rules.matched_rules && rules.matched_rules.length ? ` · matched: ${rules.matched_rules.join(', ')}` : ' · no rule matched';
  const kv = h('div', { class: 'kv' },
    asked ? [h('div', null, 'Category'), h('div', null, h('strong', null, res.category), ` · ${pct(res.category_confidence)} sure `,
      h('span', { class: `badge ${sure ? 'ok' : 'warn'}` }, sure ? 'above threshold' : `below ${pct(min)}`))] : null,
    severity ? [h('div', null, 'Severity'), severity] : null,
    h('div', null, 'Checkpoint'), h('div', null, res.model || '—'),
    h('div', null, 'Latency'), h('div', null, `${num(res.latency_ms)} ms`, res.inference_ms ? ` (${num(res.inference_ms)} ms computing)` : ''),
    res.truncated ? [h('div', null, 'Truncated'), h('div', null, 'Yes: the event was cut to fit the model, which reads only the beginning.')] : null,
    h('div', null, 'Rules alone'), h('div', null, rulesCat, ' · ', sevBadge(rules.severity), matched),
    h('div', null, 'Outcome'), h('div', null, outcome));
  return h('div', { class: 'engine-answer mt' }, kv,
    asked && res.probabilities && Object.keys(res.probabilities).length ? [h('div', { class: 'muted small mt' }, 'Most likely categories'), probBars(res.probabilities, 6, res.category)] : null);
}

// unloadedCheckpoint returns the checkpoint a test request for sourceId
// would use when the engine has said it hasn't loaded it, else ''. Asking
// anyway would make laya-serve load it (about 2 GB more memory), which
// Lissandra itself never does, so the UI doesn't ask.
async function unloadedCheckpoint(sourceId) {
  const e = (await GET('/api/stats')).engine || {};
  if (!e.loaded) return '';
  const src = sourceId ? (cache.sources || []).find((x) => x.id === sourceId) : null;
  const m = (src && src.engine_model) || e.model;
  return m && !e.loaded.includes(m) ? m : '';
}

function notLoadedNote(m) {
  return h('div', { class: 'callout warn mt' }, `Not asked: the engine hasn't loaded ${m}, and asking would make it load it (about 2 GB more memory). Add ${m} to the engine's LAYA_MODELS, or pick a loaded checkpoint.`);
}

// engineThreshold is the confidence an event's engine answer needed to be
// used: min_confidence when it answered (stored with the answer).
function engineThreshold(e) {
  return typeof e.threshold === 'number' ? e.threshold : Number(engineSettings().min_confidence ?? 0.6);
}

// severityNote says where a stored event's severity came from when the
// record did not state one.
function severityNote(ev) {
  if (!ev.severity_guessed) return '';
  const e = ev.engine;
  if (e && e.severity && e.severity === ev.severity && e.severity_confidence >= engineThreshold(e)) {
    return `judged by the Laya engine (${pct(e.severity_confidence)} sure): the record states no level`;
  }
  return 'guessed from keywords: the record states no level and no rule set one';
}

// engineVerdict says whether an event's engine answer was used.
function engineVerdict(ev) {
  const e = ev.engine;
  const min = engineThreshold(e);
  if (e.category_applied) return { cls: 'ok', text: e.severity_applied ? 'applied (category and severity)' : 'applied' };
  const below = e.category && e.category !== 'uncategorized' && e.confidence < min;
  if (e.severity_applied) {
    // Only the severity was taken.
    return { cls: 'ok', text: below ? `severity applied; category below the ${pct(min)} confidence threshold` : 'severity applied' };
  }
  if (!e.category) return { cls: '', text: 'not applied' }; // only the severity was asked
  // Rules win whatever the engine said.
  if (ev.category !== 'uncategorized') return { cls: '', text: 'rules decided' };
  if (below) return { cls: 'warn', text: `not applied (below the ${pct(min)} confidence threshold)` };
  if (e.category === 'uncategorized') return { cls: '', text: 'not applied (no category fits)' };
  return { cls: '', text: 'not applied' };
}

// ---------- Modal ----------

function openModal({ title, body, actions, wide, left, onClose }) {
  const prevFocus = document.activeElement;
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    back.remove();
    document.removeEventListener('keydown', onKey);
    if (prevFocus && prevFocus.focus) prevFocus.focus();
    if (onClose) onClose();
  };
  const onKey = (e) => {
    const open = document.querySelectorAll('.modal-back');
    if (e.key === 'Escape' && open[open.length - 1] === back) close();
  };
  const foot = h('div', { class: 'modal-foot' });
  if (left) foot.append(h('div', { class: 'left' }, left));
  for (const a of actions || []) {
    foot.append(h('button', {
      type: 'button',
      class: a.primary ? 'primary' : a.danger ? 'danger' : '',
      'data-autofocus': a.autofocus,
      onclick: async (e) => {
        const btn = e.currentTarget;
        btn.disabled = true;
        try {
          const keepOpen = await a.onClick(close);
          if (keepOpen === false) return;
        } catch (err) {
          reportError(err);
        } finally {
          btn.disabled = false;
        }
      },
    }, a.label));
  }
  const modal = h('div', { class: `modal ${wide ? 'wide' : ''}`, role: 'dialog', 'aria-modal': 'true', 'aria-label': title },
    h('div', { class: 'modal-head' }, h('h2', null, title), h('button', { type: 'button', class: 'x', 'aria-label': 'Close', onclick: close }, '×')),
    h('div', { class: 'modal-body' }, body),
    foot);
  const back = h('div', { class: 'modal-back', onmousedown: (e) => { if (e.target === back) close(); } }, modal);
  document.body.append(back);
  document.addEventListener('keydown', onKey);
  // Focus goes into the dialog: its first field, else the action marked
  // autofocus (Cancel, in a confirmation), else Close — never an action
  // Enter would run by accident — without scrolling a long one to its end.
  const first = modal.querySelector('input, select, textarea');
  if (first) first.focus();
  else (modal.querySelector('.modal-foot button[data-autofocus]') || modal.querySelector('button.x')).focus({ preventScroll: true });
  return { close };
}

function confirmDialog(message, label = 'Delete') {
  return new Promise((resolve) => {
    openModal({
      title: 'Are you sure?',
      body: h('p', null, message),
      onClose: () => resolve(false),
      actions: [
        { label: 'Cancel', autofocus: true, onClick: (close) => close() },
        { label, danger: true, onClick: (close) => { resolve(true); close(); } },
      ],
    });
  });
}

// ---------- Login ----------

// showLogin asks for the admin token. message says why a session ended.
// The help needs no request: nothing is asked of Lissandra before sign-in.
function showLogin(message) {
  if (document.querySelector('.login-back')) return;
  const input = h('input', { type: 'password', autocomplete: 'current-password', placeholder: 'Admin token' });
  const err = h('div', { class: 'help', role: 'alert' });
  const form = h('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      const t = input.value.trim();
      token = t;
      try {
        await GET('/api/info');
        setToken(t);
        back.remove();
        boot();
      } catch (ex) {
        err.textContent = ex instanceof AuthError ? 'That token was not accepted.' : ex.message;
      }
    },
  },
  brandMark(),
  message ? h('div', { class: 'callout warn', role: 'alert' }, message) : null,
  field('Admin token', input, 'LISSANDRA_ADMIN_TOKEN (in Nomad, from the nomad/jobs/lissandra variable), or the permanent token set in Lissandra.'),
  err,
  h('button', { type: 'submit', class: 'primary' }, 'Sign in'),
  h('div', { class: 'login-help small muted' },
    h('p', null, 'First start without LISSANDRA_ADMIN_TOKEN? Lissandra printed a token for this run in its log: look for ', h('code', null, 'admin_token'),
      ' (for example ', h('code', null, 'docker logs <container> 2>&1 | grep admin_token'), ').'),
    h('p', null, 'Lost the token? Set LISSANDRA_ADMIN_TOKEN, or delete admin-token.json in Lissandra\'s data directory, then restart Lissandra.')));
  const back = h('div', { class: 'login-back' }, h('div', { class: 'card login' }, form));
  document.body.append(back);
  input.focus();
}

function brandMark() {
  return h('div', { class: 'brand' }, h('img', { src: 'favicon.svg', alt: '', width: 26, height: 26 }), h('span', null, 'Lissandra'));
}

// signOut forgets the token, and the setup wizard's unsaved edits (they
// can hold recipients and tokens).
function signOut() {
  setToken('');
  clearSetupDrafts();
  showLogin();
}

// ---------- Router ----------

const pages = {};
let cleanup = null;
// routeGen numbers renders: one that finishes after the user moved on is
// dropped, with its poll.
let routeGen = 0;

function parseHash() {
  const raw = location.hash.replace(/^#\/?/, '') || 'dashboard';
  const [path, query] = raw.split('?');
  const [page, ...rest] = path.split('/');
  return { page, arg: rest.join('/'), params: new URLSearchParams(query || '') };
}

async function route() {
  if (cleanup) { cleanup(); cleanup = null; }
  const gen = ++routeGen;
  const { page, arg, params } = parseHash();
  const render = pages[page] || pages.dashboard;
  // The setup wizard is a full page. The class stays on while it moves
  // between its steps.
  document.body.classList.toggle('setup-mode', page === 'setup');
  document.title = 'Lissandra';
  document.querySelectorAll('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.page === page));
  // Each render gets its own element, so a slow page that finishes late
  // never adds to the next one.
  const view = h('div');
  clear(document.getElementById('main')).append(view);
  try {
    const c = (await render(view, arg, params)) || null;
    if (gen !== routeGen) { if (c) c(); return; }
    cleanup = c;
  } catch (err) {
    if (gen !== routeGen) return;
    // A page that failed leaves the sidebar as the way out.
    document.body.classList.remove('setup-mode');
    reportError(err);
  }
}

function every(ms, fn) {
  const id = setInterval(() => { if (!document.hidden) fn().catch(reportError); }, ms);
  return () => clearInterval(id);
}

function pageHead(title, subtitle, ...actions) {
  return h('div', { class: 'page-head' },
    h('div', null, h('h1', null, title), subtitle ? h('p', null, subtitle) : null),
    h('div', { class: 'row' }, actions));
}

// ---------- Dashboard ----------

pages.dashboard = async (main) => {
  await load('sources', 'settings', 'info');
  const body = h('div');
  append(main, pageHead('Dashboard', 'Last 60 minutes of classified log activity.'), setupBanner(cache.info), tokenWarning(cache.info), body);
  const refresh = async () => {
    const st = await GET('/api/stats');
    renderDashboard(body, st);
  };
  await refresh();
  return every(10000, refresh);
};

// setupBanner offers to finish setup while it is pending or deferred.
function setupBanner(info) {
  const ob = info.onboarding;
  if (!ob || (ob.status !== 'pending' && ob.status !== 'deferred')) return null;
  const next = SETUP_PAGES.find((p) => p.id === ob.resume);
  const left = ob.remaining ? `${plural(ob.remaining, 'step')} left` : 'every step is done';
  const el = h('div', { class: 'callout banner' },
    h('span', null, `Finish setting up Lissandra: ${left}.${next && next.id !== 'welcome' ? ` Next: ${next.label}.` : ''}`),
    h('span', { class: 'row' },
      h('button', {
        type: 'button', class: 'small primary',
        onclick: (e) => busy(e.currentTarget, async () => {
          const st = await api('POST', '/api/onboarding/start');
          location.hash = `#/setup/${st.resume}`;
        }),
      }, 'Resume setup'),
      h('button', {
        type: 'button', class: 'small',
        onclick: (e) => busy(e.currentTarget, async () => {
          if (!(await confirmDialog('Dismiss setup? Lissandra keeps its current configuration. You can run it again from Settings.', 'Dismiss'))) return;
          await setupApi('POST', '/api/onboarding/dismiss');
          el.remove();
        }),
      }, 'Dismiss')));
  return el;
}

// tokenWarning flags a per-run admin token whatever the setup status, so
// existing and seeded installs see it without being sent through setup.
function tokenWarning(info) {
  if (!info.auth || info.auth.source !== 'generated') return null;
  try {
    if (sessionStorage.getItem('lissandra.tokenWarnDismissed')) return null;
  } catch { /* storage blocked */ }
  const el = h('div', { class: 'callout warn banner' },
    h('span', null, 'Your admin token changes every time Lissandra restarts. ', h('a', { href: '#/settings' }, 'Set a permanent token'), ', or set LISSANDRA_ADMIN_TOKEN.'),
    h('button', {
      type: 'button', class: 'small', 'aria-label': 'Dismiss this warning for this tab',
      onclick: () => {
        try { sessionStorage.setItem('lissandra.tokenWarnDismissed', '1'); } catch { /* not remembered */ }
        el.remove();
      },
    }, 'Dismiss'));
  return el;
}

function renderDashboard(root, st) {
  const ev = st.events;
  const bySev = ev.by_severity || {};
  const bad = (bySev.error || 0) + (bySev.critical || 0);
  const pulls = (cache.sources || []).filter((x) => x.type === 'pull' && x.enabled);
  const healthy = pulls.filter((x) => { const ps = st.sources[x.id]; return ps && !ps.last_error && ps.last_success; }).length;
  const n = st.notifications;

  const kpi = (label, value, sub, alert) => h('div', { class: `card kpi ${alert ? 'alert' : ''}` },
    h('div', { class: 'label' }, label), h('div', { class: 'value' }, value), h('div', { class: 'sub' }, sub));

  const kpis = h('div', { class: 'kpis' },
    kpi('Events (1h)', num(ev.last_hour), `${num(ev.total_since_start)} since start`),
    kpi('Errors + critical (1h)', num(bad), `${num(bySev.critical || 0)} critical`, bad > 0),
    kpi('Notifications', num(n.sent), `${num(n.failed)} failed · ${n.dropped ? `${num(n.dropped)} dropped (queue full) · ` : ''}${num(n.throttled)} throttled`, n.failed > 0 || n.dropped > 0),
    kpi('Pull sources', pulls.length ? `${healthy}/${pulls.length}` : '—', pulls.length ? 'healthy' : 'none configured', healthy < pulls.length),
    st.engine && st.engine.enabled
      ? engineKpi(st.engine, st.pipeline, kpi)
      : kpi('Pipeline queue', num(st.pipeline.queue_depth), `${num(st.pipeline.dropped)} dropped by rules`),
    st.learning && st.learning.enabled ? learningKpi(st.learning, kpi) : null);
  updateReviewBadge(st.learning ? st.learning.review : 0);

  const timeline = h('div', { class: 'card' }, h('h2', null, 'Events per minute'), timelineChart(ev.timeline));

  const patterns = h('div', { class: 'card flush mt' },
    h('div', { class: 'list-item' }, h('div', { class: 'main' },
      h('h2', null, 'Top issues'),
      h('div', { class: 'meta' }, 'Grouped by fingerprint (same message pattern from the same source). Errors first.'))));
  if (!ev.top_patterns || !ev.top_patterns.length) {
    patterns.append(h('div', { class: 'empty' }, 'No events in the last hour. Send some logs to a source to get started.'));
  } else {
    const tbl = h('table', null, h('tr', null, h('th', null, 'Severity'), h('th', null, 'Pattern'), h('th', null, 'Source'), h('th', null, 'Count'), h('th', null, 'Last seen')));
    for (const p of ev.top_patterns) {
      tbl.append(h('tr', { class: 'clickable', onclick: () => { location.hash = `#/events?fingerprint=${encodeURIComponent(p.fingerprint)}`; } },
        h('td', null, sevBadge(p.severity)),
        h('td', { class: 'msg', title: p.sample }, p.sample),
        h('td', { class: 'nowrap' }, p.source_name, h('div', { class: 'muted small' }, p.category)),
        h('td', null, num(p.count)),
        h('td', null, timeCell(p.last_seen))));
    }
    patterns.append(tbl);
  }

  const bars = (title, obj, link) => {
    const entries = Object.entries(obj || {}).sort((a, b) => b[1] - a[1]).slice(0, 10);
    const max = Math.max(1, ...entries.map((e) => e[1]));
    const card = h('div', { class: 'card' }, h('h2', null, title));
    if (!entries.length) card.append(h('div', { class: 'muted' }, 'Nothing yet.'));
    for (const [k, v] of entries) {
      const fill = h('div', { class: 'fill' });
      fill.style.width = `${(v / max) * 100}%`;
      card.append(h('div', { class: 'hbar' }, link ? h('a', { class: 'name', href: link(k) }, k) : h('span', { class: 'name' }, k), h('div', { class: 'track' }, fill), h('span', { class: 'n' }, num(v))));
    }
    return card;
  };

  clear(root);
  append(root, kpis, timeline,
    h('div', { class: 'grid cols-2 mt' },
      bars('By category', ev.by_category, (k) => `#/events?category=${encodeURIComponent(k)}`),
      bars('By source', ev.by_source)),
    patterns);
}

// engineKpi summarizes the Laya engine: flagged when it is not configured,
// paused after errors, failed in the last 10 minutes, or has setup warnings
// (a checkpoint in use is not loaded or paused, categories it can't tell
// apart). The tooltip has the details.
function engineKpi(e, pipeline, kpi) {
  if (!e.configured) {
    return kpi('Laya engine', 'off', e.config_error || 'not configured: set the URL in Settings', true);
  }
  const paused = e.paused_until && new Date(e.paused_until) > new Date();
  // An error counts until the engine answers again (or for 10 minutes).
  const recovered = e.last_success_at && e.last_error_at && new Date(e.last_success_at) > new Date(e.last_error_at);
  const recent = e.last_error && e.last_error_at && !recovered && Date.now() - new Date(e.last_error_at).getTime() < 10 * 60 * 1000;
  const warnings = e.warnings || [];
  const parts = [`${num(e.cache_hits)} cached`, `${num(e.applied)} applied`, `${num(e.avg_latency_ms)} ms avg`];
  if (warnings.length) parts.unshift(warnings.length === 1 ? '1 warning' : `${warnings.length} warnings`);
  if (paused) parts.unshift(`paused until ${new Date(e.paused_until).toLocaleTimeString()}`);
  else if (recent) parts.unshift(`error ${ago(e.last_error_at)}`);
  const card = kpi('Laya engine calls', num(e.calls_total), parts.join(' · '), paused || recent || warnings.length > 0);
  const inUse = e.models_in_use || [e.model];
  const notes = [
    `Checkpoints in use: ${inUse.join(', ')}${e.loaded ? ` · loaded on the engine: ${e.loaded.join(', ') || 'none'}` : ''}`,
    `Capacity: ${plural(e.capacity, 'request')} at a time · ${num(e.in_flight)} in flight · ${num((pipeline || {}).engine_queue_depth)} queued`,
    `Average: ${num(e.avg_inference_ms)} ms computing, ${num(e.avg_latency_ms)} ms round trip`,
    `${plural(e.errors, 'error')} · ${num(e.timed_out)} timed out · ${num(e.skipped)} went on without the engine (${num((pipeline || {}).engine_shed)} not queued: they would have timed out)`,
  ];
  if (paused || recent) notes.push(`Last error: ${e.last_error}`);
  for (const w of warnings) notes.push(`Warning: ${w}`);
  card.title = notes.join('\n');
  return card;
}

// learningKpi links to the learned rules awaiting review.
function learningKpi(l, kpi) {
  const full = l.review >= l.max_review;
  const card = kpi('Rules to review', num(l.review),
    full ? `limit reached: nothing new is learned until some are reviewed` : `${num(l.learned)} learned since start`, full);
  const link = h('a', { class: 'kpi-link', href: '#/rules?status=review' }, card);
  link.title = `Rules Lissandra learned from the engine's answers (at ${pct(l.min_confidence)} confidence or more). They classify and alert already; approve, edit or reject them.`;
  return link;
}

// timelineChart draws stacked severity bars. labels sets the axis text:
// {left, mid, right, unit}; the default suits the live 60-minute view.
function timelineChart(points, labels = { left: '60 min ago', mid: '30 min', right: 'now', unit: 'min' }) {
  const W = 600, H = 160;
  const max = Math.max(1, ...points.map((p) => p.total));
  const bw = W / points.length;
  const svg = s('svg', { class: 'chart', viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: 'none', role: 'img', 'aria-label': 'Events per minute by severity' });
  for (const frac of [0.25, 0.5, 0.75, 1]) {
    svg.append(s('line', { class: 'grid-line', x1: 0, x2: W, y1: H - frac * H, y2: H - frac * H, 'vector-effect': 'non-scaling-stroke' }));
  }
  points.forEach((p, i) => {
    let y = H;
    const g = s('g', null);
    const lines = [(labels.tip ? labels.tip(p.time) : new Date(p.time).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })) + ` — ${p.total} events`];
    for (const sv of SEVERITIES) {
      const c = (p.by_severity || {})[sv] || 0;
      if (!c) continue;
      const bh = (c / max) * H;
      y -= bh;
      g.append(s('rect', { class: `bar-${sv}`, x: i * bw + 1, y, width: Math.max(bw - 2, 1), height: bh }));
      lines.push(`${sv}: ${c}`);
    }
    g.append(s('rect', { x: i * bw, y: 0, width: bw, height: H, fill: 'transparent' }));
    g.append(s('title', null, lines.join('\n')));
    svg.append(g);
  });
  return h('div', null,
    h('div', { class: 'row small muted' }, h('span', null, `max ${max}/${labels.unit}`)),
    svg,
    h('div', { class: 'row small muted' }, h('span', null, labels.left), h('span', { class: 'spacer' }), h('span', null, labels.mid), h('span', { class: 'spacer' }), h('span', null, labels.right)),
    h('div', { class: 'legend' }, SEVERITIES.map((sv) => h('span', null, h('i', { class: sv }), sv))));
}

// ---------- Events ----------

const eventFilter = { min_severity: '', category: '', source: '', q: '', fingerprint: '', tag: '' };

pages.events = async (main, arg, params) => {
  await load('sources', 'settings');
  for (const k of Object.keys(eventFilter)) if (params.has(k)) eventFilter[k] = params.get(k);
  if ([...params.keys()].length) history.replaceState(null, '', '#/events');

  let live = true;
  let lastID = '';
  const tableWrap = h('div', { class: 'card flush' });
  const more = h('button', { type: 'button', onclick: () => fetchEvents(true).catch(reportError) }, 'Load older');

  const sevSel = select([['', 'Any severity'], ...SEVERITIES.map((x) => [x, `≥ ${x}`])], eventFilter.min_severity);
  const catSel = select([['', 'Any category'], ...[...new Set([...categories(), 'uncategorized', eventFilter.category].filter(Boolean))].map((x) => [x, x])], eventFilter.category);
  const srcSel = select([['', 'Any source'], ...cache.sources.map((x) => [x.id, x.name])], eventFilter.source);
  const q = h('input', { type: 'search', placeholder: 'Search messages…', value: eventFilter.q });
  const liveBox = checkbox('Live', live, (v) => { live = v; });
  const fpNote = h('span');

  const apply = () => {
    Object.assign(eventFilter, { min_severity: sevSel.value, category: catSel.value, source: srcSel.value, q: q.value.trim() });
    fetchEvents(false).catch(reportError);
  };
  [sevSel, catSel, srcSel].forEach((el) => el.addEventListener('change', apply));
  let qTimer;
  q.addEventListener('input', () => { clearTimeout(qTimer); qTimer = setTimeout(apply, 300); });

  const renderFpNote = () => {
    clear(fpNote);
    for (const key of ['fingerprint', 'tag']) {
      if (eventFilter[key]) {
        fpNote.append(h('span', { class: 'badge' }, `${key}: ${eventFilter[key]} `,
          h('button', { type: 'button', class: 'link', onclick: () => { eventFilter[key] = ''; renderFpNote(); apply(); } }, '×')));
      }
    }
  };
  renderFpNote();

  append(main,
    pageHead('Events', 'Classified log events held in memory (newest first).'),
    h('div', { class: 'row' }, sevSel, catSel, srcSel, q, fpNote, h('span', { class: 'spacer' }), liveBox.el),
    h('div', { class: 'mt' }),
    tableWrap,
    h('div', { class: 'row mt' }, more));

  async function fetchEvents(older) {
    const p = new URLSearchParams({ limit: '100' });
    for (const [k, v] of Object.entries(eventFilter)) if (v) p.set(k, v);
    if (older && lastID) p.set('before', lastID);
    const list = await GET(`/api/events?${p}`);
    if (!older) clear(tableWrap);
    let tbl = tableWrap.querySelector('table');
    if (!tbl) {
      tbl = h('table', null, h('tr', null, h('th', null, 'Time'), h('th', null, 'Severity'), h('th', null, 'Category'), h('th', null, 'Source'), h('th', null, 'Message')));
      tableWrap.append(tbl);
    }
    if (!older && !list.length) {
      clear(tableWrap).append(h('div', { class: 'empty' }, 'No matching events.'));
    }
    for (const ev of list) {
      tbl.append(h('tr', { class: 'clickable', onclick: () => showEvent({ id: ev.id }) },
        h('td', null, timeCell(ev.received_at)),
        h('td', null, sevBadge(ev.severity)),
        h('td', { class: 'nowrap' }, ev.category, ev.classifier === 'laya' ? h('span', { class: 'muted small', title: 'Classified by the Laya engine' }, ' ✦') : null),
        h('td', { class: 'nowrap' }, ev.source_name),
        h('td', { class: 'msg', title: ev.message }, ev.message, ' ', tags(ev.tags))));
    }
    if (list.length) lastID = list[list.length - 1].id;
    more.disabled = list.length < 100;
  }

  await fetchEvents(false);
  if (arg) showEvent({ id: arg });
  return every(5000, async () => { if (live && !document.querySelector('.modal-back')) await fetchEvents(false); });
};

// showEvent opens the detail dialog for a live event ({id}) or an event
// from an analyzed file ({importId, id}). Paths are always built here with
// every segment encoded: ids can come from the URL hash, and a caller-chosen
// path would let a crafted link send the admin token elsewhere.
// opts.onFingerprint replaces the default "similar live events" link.
async function showEvent(target, opts = {}) {
  const path = target.importId
    ? `/api/imports/${encodeURIComponent(target.importId)}/events/${encodeURIComponent(target.id)}`
    : `/api/events/${encodeURIComponent(target.id)}`;
  let data;
  try {
    data = await GET(path);
  } catch (err) {
    reportError(err);
    return;
  }
  await load('settings');
  const ev = data.event;
  const engineOut = h('div');
  const kv = h('div', { class: 'kv' });
  const row = (k, v) => { if (v !== null && v !== undefined && v !== '' && !(Array.isArray(v) && !v.length)) kv.append(h('div', null, k), h('div', null, v)); };
  const sevNote = severityNote(ev);
  row('Severity', h('div', null, sevBadge(ev.severity), sevNote ? h('span', { class: 'muted small' }, ` ${sevNote}`) : null));
  row('Category', ev.category);
  row('Classified by', ev.classifier === 'laya' ? 'the Laya engine' : ev.classifier);
  row('Source', ev.source_name);
  row('Level field', ev.level);
  row('Log timestamp', ev.time_source === 'none' ? '— (no timestamp in this record or before it)' : `${fmtTime(ev.timestamp)}${ev.time_source === 'inferred' ? ' (taken from the previous record)' : ''}`);
  row(ev.import_id ? 'Analyzed' : 'Received', fmtTime(ev.received_at));
  row('File', ev.file ? `${ev.file}${ev.line ? `:${ev.line}` : ''}` : '');
  row('Tags', tags(ev.tags));
  row('Matched rules', (ev.matched_rules || []).join(', '));
  row('Alert routes', (data.routes || []).join(', ') || 'none');
  if (ev.engine) {
    const e = ev.engine;
    const v = engineVerdict(ev);
    row('Engine', h('div', null,
      [e.category ? `${e.category} ${pct(e.confidence)}` : null, e.severity ? `severity ${e.severity} ${pct(e.severity_confidence)}` : null, e.model].filter(Boolean).join(' · '), ' ',
      h('span', { class: `badge ${v.cls}` }, v.text),
      h('div', { class: 'muted small' }, [`Threshold when it answered: ${pct(engineThreshold(e))}`, e.top && Object.keys(e.top).length ? `Top: ${topText(e.top)}` : null].filter(Boolean).join(' · ')),
      e.truncated ? h('div', { class: 'muted small' }, 'The message was cut to fit the model, which reads only its beginning.') : null));
  }
  row('Fingerprint', opts.onFingerprint
    ? h('button', { type: 'button', class: 'link', onclick: () => { modal.close(); opts.onFingerprint(ev.fingerprint); } }, ev.fingerprint)
    : h('a', { href: `#/events?fingerprint=${encodeURIComponent(ev.fingerprint)}`, onclick: () => modal.close() }, ev.fingerprint));

  const body = h('div', null,
    h('div', { class: 'code-box' }, h('pre', null, ev.message)),
    h('h3', null, 'Classification'), kv,
    ev.fields && Object.keys(ev.fields).length ? [h('h3', null, 'Fields'), codeBox(JSON.stringify(ev.fields, null, 2))] : null,
    engineOut);
  const actions = [];
  // Only live events: the API looks up event_id among live events.
  if (engineSettings().enabled && !ev.import_id) {
    actions.push({
      label: 'Ask the Laya engine',
      onClick: async () => {
        clear(engineOut).append(h('h3', null, 'Laya engine'), h('p', { class: 'muted' }, 'Asking the engine…'));
        try {
          const missing = await unloadedCheckpoint(ev.source_id);
          if (missing) {
            clear(engineOut).append(h('h3', null, 'Laya engine'), notLoadedNote(missing));
            return false;
          }
          const r = await api('POST', '/api/engine/classify', { event_id: ev.id });
          clear(engineOut).append(h('h3', null, 'Laya engine (asked now)'),
            h('p', { class: 'small muted' }, 'With today\'s settings and rules; nothing is stored.'), engineAnswer(r));
        } catch (err) {
          clear(engineOut);
          throw err;
        }
        return false;
      },
    });
  }
  actions.push({ label: 'Create rule from this…', onClick: (close) => { close(); editRule(null, ev); } });
  actions.push({ label: 'Close', primary: true, onClick: (close) => close() });
  const modal = openModal({ title: 'Event', body, actions, wide: true });
}

// ---------- Sources ----------

pages.sources = async (main) => {
  await load('sources', 'info', 'settings', 'groups');
  const list = h('div', { class: 'card flush' });
  append(main, pageHead('Sources', 'Where logs come from: Lissandra can receive them on an ingest endpoint (push) or poll an API (pull).',
    h('button', { type: 'button', class: 'primary', onclick: () => editSource(null) }, 'Add source')), list);
  const refresh = async () => {
    const [sources, stats] = await Promise.all([GET('/api/sources'), GET('/api/stats')]);
    cache.sources = sources;
    renderSources(list, sources, stats.sources || {});
  };
  await refresh();
  return every(10000, refresh);
};

function renderSources(root, sources, status) {
  clear(root);
  if (!sources.length) root.append(h('div', { class: 'empty' }, 'No sources yet.'));
  for (const src of sources) {
    const meta = [];
    if (src.type === 'push') {
      meta.push(h('div', { class: 'meta mono' }, `POST ${cache.info.public_url}/ingest/${src.id}`));
    } else {
      const st = status[src.id] || {};
      meta.push(h('div', { class: 'meta mono' }, `${src.pull.method} ${src.pull.url}`));
      meta.push(h('div', { class: 'meta' }, `every ${src.pull.interval_seconds}s · last poll ${ago(st.last_poll)} · ${num(st.last_count)} records last time · ${num(st.total)} total`));
      if (st.last_error) meta.push(h('div', { class: 'meta' }, h('span', { class: 'badge bad' }, 'error'), ' ', st.last_error));
    }
    if (src.description) meta.unshift(h('div', { class: 'meta' }, src.description));
    root.append(h('div', { class: `list-item ${src.enabled ? '' : 'disabled-item'}` },
      h('div', { class: 'main' },
        h('div', { class: 'title' }, src.name, h('span', { class: 'badge' }, src.type), src.enabled ? null : h('span', { class: 'badge' }, 'disabled'),
          src.engine_model ? h('span', { class: 'badge', title: 'Laya engine checkpoint for this source' }, `checkpoint: ${src.engine_model}`) : null, tags(src.tags)),
        meta,
        h('div', { class: 'meta' }, 'Alerts → ', (src.alert_group_ids || []).length
          ? src.alert_group_ids.map(groupName).join(', ')
          : h('span', { class: 'muted' }, 'no alert group (alert routes only)'))),
      h('div', { class: 'actions' },
        src.type === 'push' ? h('button', { type: 'button', class: 'small', onclick: () => showIngest(src) }, 'Setup instructions') : null,
        h('button', { type: 'button', class: 'small', onclick: () => { location.hash = `#/files?source=${encodeURIComponent(src.id)}`; } }, 'Analyze a file'),
        h('button', { type: 'button', class: 'small', onclick: () => editSource(src) }, 'Edit'),
        h('button', {
          type: 'button', class: 'small danger',
          onclick: async () => {
            if (!(await confirmDialog(`Delete source "${src.name}"? Senders using it will get 404s.`))) return;
            await api('DELETE', `/api/sources/${src.id}`).catch(reportError);
            route();
          },
        }, 'Delete'))));
  }
}

// showIngest shows how to send logs to a push source. onRotated, when
// given, is called with the source after a token rotation instead of
// re-rendering the page.
function showIngest(src, { onRotated } = {}) {
  const base = cache.info.public_url;
  const url = `${base}/ingest/${src.id}`;
  let host = 'lissandra.service.consul', port = '8080', tls = 'Off';
  try {
    const u = new URL(base);
    host = u.hostname;
    port = u.port || (u.protocol === 'https:' ? '443' : '80');
    tls = u.protocol === 'https:' ? 'On' : 'Off';
  } catch { /* keep defaults */ }
  const tokenBox = h('code', null, '•'.repeat(24));
  let shown = false;
  let modal;
  const body = h('div', null,
    h('p', null, 'Send logs with an HTTP POST. Bodies can be a JSON object, a JSON array, NDJSON, or plain-text lines (indented continuation lines such as stack traces are folded into the previous line). gzip Content-Encoding is accepted.'),
    h('div', { class: 'kv' },
      h('div', null, 'Ingest URL'), h('div', { class: 'mono' }, url),
      h('div', null, 'Token'), h('div', { class: 'row' }, tokenBox,
        h('button', { type: 'button', class: 'small', onclick: () => { shown = !shown; tokenBox.textContent = shown ? src.ingest_token : '•'.repeat(24); } }, 'Show'),
        h('button', { type: 'button', class: 'small', onclick: () => copyText(src.ingest_token) }, 'Copy'),
        h('button', {
          type: 'button', class: 'small danger',
          onclick: async () => {
            if (!(await confirmDialog('Rotate the token? Existing senders will be rejected until they use the new one.', 'Rotate'))) return;
            const upd = await api('POST', `/api/sources/${src.id}/rotate-token`).catch(reportError);
            if (!upd) return;
            modal.close();
            showIngest(upd, { onRotated });
            if (onRotated) onRotated(upd); else route();
          },
        }, 'Rotate'))),
    h('p', { class: 'small muted' }, 'The token can be sent as "Authorization: Bearer <token>", an "X-Lissandra-Token" header, or a ?token= query parameter.'),
    h('h3', null, 'curl'),
    codeBox(`curl -X POST '${url}' \\\n  -H 'Authorization: Bearer ${src.ingest_token}' \\\n  -H 'Content-Type: application/json' \\\n  -d '{"level":"error","service":"checkout","message":"payment failed: connection refused to db:5432"}'`),
    h('h3', null, 'Fluent Bit'),
    codeBox(`[OUTPUT]\n    Name    http\n    Match   *\n    Host    ${host}\n    Port    ${port}\n    URI     /ingest/${src.id}\n    Format  json\n    tls     ${tls}\n    Header  Authorization Bearer ${src.ingest_token}`),
    h('h3', null, 'Vector'),
    codeBox(`[sinks.lissandra]\ntype = "http"\ninputs = ["app_logs"]\nuri = "${url}"\nencoding.codec = "json"\nframing.method = "newline_delimited"\nrequest.headers.Authorization = "Bearer ${src.ingest_token}"`));
  modal = openModal({ title: `Send logs to "${src.name}"`, body, wide: true, actions: [{ label: 'Done', primary: true, onClick: (c) => c() }] });
}

// editSource opens the source editor. onSaved, when given, is called with
// the saved source instead of re-rendering the page.
function editSource(src, { onSaved } = {}) {
  const isNew = !src;
  src = src ? JSON.parse(JSON.stringify(src)) : { name: '', type: 'push', enabled: true, parse: { format: 'auto' }, tags: [] };
  const pull = src.pull || { url: '', method: 'GET', interval_seconds: 60, timeout_seconds: 15, auth: { type: 'none' }, dedupe: true, headers: {} };
  pull.auth = pull.auth || { type: 'none' };

  const f = {
    name: h('input', { value: src.name, placeholder: 'e.g. payments-api' }),
    description: h('input', { value: src.description || '' }),
    tags: h('input', { value: (src.tags || []).join(', '), placeholder: 'prod, payments' }),
    enabled: checkbox('Enabled', src.enabled),
    type: select([['push', 'Push — senders POST logs to Lissandra'], ['pull', 'Pull — Lissandra polls an HTTP API']], src.type, { disabled: !isNew }),
    url: h('input', { value: pull.url, placeholder: 'https://logs.example.com/api/v1/logs?since={since_rfc3339}' }),
    method: select(['GET', 'POST'], pull.method),
    interval: h('input', { type: 'number', min: 5, value: pull.interval_seconds }),
    timeout: h('input', { type: 'number', min: 1, value: pull.timeout_seconds }),
    authType: select([['none', 'None'], ['bearer', 'Bearer token'], ['basic', 'Basic auth'], ['header', 'Custom header']], pull.auth.type),
    token: secretInput(pull.auth.token || '', { placeholder: '${env:LOGS_API_TOKEN}' }),
    username: h('input', { value: pull.auth.username || '' }),
    password: secretInput(pull.auth.password || ''),
    headerName: h('input', { value: pull.auth.header_name || '', placeholder: 'X-API-Key' }),
    headerValue: secretInput(pull.auth.header_value || ''),
    headers: h('textarea', { rows: 2, placeholder: 'Accept: application/json' }, headersToText(pull.headers)),
    body: h('textarea', { rows: 3, placeholder: '{"query": "level:error", "from": "{since_rfc3339}"}' }, pull.body || ''),
    dedupe: checkbox('Skip records already seen in earlier polls', pull.dedupe),
    insecure: checkbox('Skip TLS certificate verification', pull.insecure_skip_verify),
    format: select([['auto', 'Auto-detect'], ['json', 'JSON'], ['ndjson', 'NDJSON (one JSON object per line)'], ['text', 'Plain text lines']], src.parse.format),
    recordsPath: h('input', { value: src.parse.records_path || '', placeholder: 'data.items  (auto: records, logs, events, data, hits.hits…)' }),
    messageField: h('input', { value: src.parse.message_field || '', placeholder: 'auto: message, msg, log…' }),
    levelField: h('input', { value: src.parse.level_field || '', placeholder: 'auto: level, severity…' }),
    tsField: h('input', { value: src.parse.timestamp_field || '', placeholder: 'auto: @timestamp, timestamp, time…' }),
    engineModel: modelPicker(src.engine_model || '', ['', `Default (${engineSettings().model || 'english'})`]),
    groups: (cache.groups || []).map((g) => ({ id: g.id, box: checkbox(g.name, (src.alert_group_ids || []).includes(g.id)) })),
  };

  const authFields = h('div');
  const renderAuth = () => {
    clear(authFields);
    const t = f.authType.value;
    if (t === 'bearer') authFields.append(field('Token', f.token, 'Use ${env:NAME} to read it from the environment (e.g. a Nomad Variable).'));
    if (t === 'basic') authFields.append(h('div', { class: 'fields-2' }, field('Username', f.username), field('Password', f.password)));
    if (t === 'header') authFields.append(h('div', { class: 'fields-2' }, field('Header name', f.headerName), field('Header value', f.headerValue)));
  };
  f.authType.addEventListener('change', renderAuth);
  renderAuth();

  const bodyField = field('Request body', f.body, 'Sent for POST requests. Placeholders are expanded here too.');
  const toggleBody = () => { bodyField.hidden = f.method.value !== 'POST'; };
  f.method.addEventListener('change', toggleBody);
  toggleBody();

  const pullSection = h('fieldset', null, h('legend', null, 'Polling'),
    field('URL', f.url, 'Placeholders: {since_rfc3339} {since_unix} {since_unix_ms} {now_rfc3339} {now_unix} {now_unix_ms} — "since" is the previous successful poll.'),
    h('div', { class: 'fields-3' }, field('Method', f.method), field('Interval (s)', f.interval), field('Timeout (s)', f.timeout)),
    field('Authentication', f.authType), authFields,
    field('Extra headers', f.headers, 'One "Name: value" per line.'),
    bodyField,
    h('div', { class: 'row' }, f.dedupe.el, f.insecure.el));
  const preview = h('div');
  const toggleType = () => { pullSection.hidden = f.type.value !== 'pull'; };
  f.type.addEventListener('change', toggleType);
  toggleType();

  const body = h('div', null,
    h('div', { class: 'fields-2' }, field('Name', f.name), field('Type', f.type)),
    field('Description', f.description),
    h('div', { class: 'fields-2' }, field('Tags', f.tags, 'Added to every event from this source.'), h('div', { class: 'field' }, h('label', null, ' '), f.enabled.el)),
    pullSection,
    h('fieldset', null, h('legend', null, 'Parsing'),
      h('div', { class: 'fields-2' }, field('Format', f.format), field('Records path', f.recordsPath, 'Dot path to the array of log records in a JSON response.')),
      h('div', { class: 'fields-3' }, field('Message field', f.messageField), field('Level field', f.levelField), field('Timestamp field', f.tsField))),
    h('fieldset', null, h('legend', null, 'Classification'),
      field('Laya engine checkpoint', f.engineModel.el, 'The checkpoint that classifies this source\'s events when no rule does. Pick multilingual for logs that are not in English. The engine must have it loaded (its LAYA_MODELS): Lissandra never asks for one it hasn\'t, and those events keep the rules\' result.')),
    h('fieldset', null, h('legend', null, 'Alerts'),
      f.groups.length
        ? h('div', { class: 'field' }, h('label', null, 'Alert groups'), h('div', { class: 'chips' }, f.groups.map((x) => x.box.el)),
          h('div', { class: 'help' }, 'The teams and people alerted about this source\'s events, each with its own channels and minimum severity. Alert routes apply as well.'))
        : h('p', { class: 'muted small' }, 'No alert groups yet. ', h('a', { href: '#/groups' }, 'Create one'), ' to send this source\'s alerts to a team\'s channels and people.')),
    preview);

  const collect = () => {
    const out = {
      ...src,
      name: f.name.value.trim(),
      description: f.description.value.trim(),
      tags: splitList(f.tags.value),
      enabled: f.enabled.input.checked,
      type: f.type.value,
      engine_model: f.engineModel.value(),
      // Without the groups loaded there are no boxes: keep what is assigned.
      alert_group_ids: cache.groups ? f.groups.filter((x) => x.box.input.checked).map((x) => x.id) : src.alert_group_ids,
      parse: {
        format: f.format.value, records_path: f.recordsPath.value.trim(), message_field: f.messageField.value.trim(),
        level_field: f.levelField.value.trim(), timestamp_field: f.tsField.value.trim(),
      },
    };
    if (out.type === 'pull') {
      out.pull = {
        url: f.url.value.trim(), method: f.method.value, body: f.body.value,
        interval_seconds: Number(f.interval.value) || 60, timeout_seconds: Number(f.timeout.value) || 15,
        headers: textToHeaders(f.headers.value), dedupe: f.dedupe.input.checked, insecure_skip_verify: f.insecure.input.checked,
        auth: { type: f.authType.value, token: f.token.value, username: f.username.value, password: f.password.value, header_name: f.headerName.value, header_value: f.headerValue.value },
      };
    } else {
      delete out.pull;
    }
    return out;
  };

  const actions = [{ label: 'Cancel', onClick: (c) => c() }];
  const testBtn = {
    label: 'Test fetch',
    onClick: async () => {
      const draft = collect();
      if (draft.type !== 'pull') { toast('Test fetch is for pull sources. For push sources, use the curl example after saving.'); return false; }
      clear(preview).append(h('p', { class: 'muted' }, 'Fetching…'));
      try {
        const r = await api('POST', '/api/sources/test', draft);
        clear(preview).append(h('h3', null, `Preview — ${r.total} record(s) parsed`), previewTable(r.events));
      } catch (err) {
        clear(preview).append(h('div', { class: 'callout bad' }, err.message));
      }
      return false;
    },
  };
  actions.push({
    label: isNew ? 'Create source' : 'Save',
    primary: true,
    onClick: async (close) => {
      const draft = collect();
      const saved = isNew ? await api('POST', '/api/sources', draft) : await api('PUT', `/api/sources/${src.id}`, draft);
      close();
      toast('Source saved');
      if (onSaved) await onSaved(saved); else await route();
      if (isNew && saved.type === 'push') showIngest(saved, { onRotated: onSaved });
    },
  });
  openModal({ title: isNew ? 'Add source' : `Edit source "${src.name}"`, body, actions, wide: true, left: h('button', { type: 'button', onclick: () => testBtn.onClick().catch(reportError) }, 'Test fetch') });
}

function previewTable(items) {
  if (!items.length) return h('div', { class: 'empty' }, 'No records.');
  const tbl = h('table', null, h('tr', null, h('th', null, 'Severity'), h('th', null, 'Category'), h('th', null, 'Message'), h('th', null, 'Rules'), h('th', null, 'Routes')));
  for (const it of items) {
    const ev = it.event;
    tbl.append(h('tr', null,
      h('td', null, it.dropped ? h('span', { class: 'badge' }, 'dropped') : sevBadge(ev.severity)),
      h('td', null, it.dropped ? '—' : ev.category),
      h('td', { class: 'msg', title: ev.message }, ev.message, ' ', tags(ev.tags)),
      h('td', { class: 'small' }, (ev.matched_rules || []).join(', ') || '—'),
      h('td', { class: 'small' }, (it.routes || []).join(', ') || '—')));
  }
  return h('div', { class: 'preview' }, tbl);
}

// ---------- Rules ----------

const FIELD_SUGGESTIONS = ['message', 'pattern', 'level', 'severity', 'source', 'category', 'tags', 'status', 'status_code', 'http.status_code', 'service', 'host', 'logger', 'kubernetes.namespace_name'];
const OP_LABELS = {
  contains: 'contains', not_contains: 'does not contain', equals: 'equals', not_equals: 'does not equal',
  regex: 'matches regex', not_regex: 'does not match regex', starts_with: 'starts with', ends_with: 'ends with',
  exists: 'exists', missing: 'is missing', gt: '>', lt: '<',
};

// Rule tabs: learned rules await review until approved or rejected.
const RULE_TABS = [['all', 'All'], ['active', 'Active'], ['review', 'Under review'], ['rejected', 'Rejected']];
const ruleStatus = (r) => r.status || 'active';

// updateReviewBadge shows how many learned rules await review next to
// "Classification rules" in the sidebar.
function updateReviewBadge(n) {
  const el = document.getElementById('review-badge');
  if (!el) return;
  el.textContent = n > 0 ? String(n) : '';
  el.hidden = !(n > 0);
  el.title = n > 0 ? `${plural(n, 'learned rule')} awaiting review` : '';
}

pages.rules = async (main, _arg, params) => {
  await load('rules', 'sources', 'settings', 'routes');
  let tab = RULE_TABS.some(([k]) => k === params.get('status')) ? params.get('status') : 'all';
  const tabs = h('div', { class: 'row tabs' });
  const list = h('div', { class: 'card flush' });
  const tester = ruleTester();
  append(main,
    pageHead('Classification rules', 'Evaluated top to bottom after built-in severity detection. Every match adds tags and adjusts severity; the first match with a category sets it; "stop" ends evaluation. When no rule categorizes an event, the Laya engine does, and Lissandra learns a rule from its answer for you to review.',
      h('button', { type: 'button', class: 'primary', onclick: () => editRule(null) }, 'Add rule')),
    tabs,
    h('div', { class: 'grid cols-2 top' }, list, tester));
  const render = () => {
    const count = (k) => (k === 'all' ? cache.rules.length : cache.rules.filter((r) => ruleStatus(r) === k).length);
    clear(tabs).append(RULE_TABS.map(([k, label]) => h('button', {
      type: 'button', class: `small ${tab === k ? 'primary' : ''}`, 'aria-pressed': String(tab === k),
      onclick: () => {
        tab = k;
        history.replaceState(null, '', k === 'all' ? '#/rules' : `#/rules?status=${k}`);
        render();
      },
    }, label, h('span', { class: 'count' }, num(count(k))))));
    updateReviewBadge(count('review'));
    renderRules(list, tab, render);
  };
  render();
};

// review approves or rejects learned rules, then re-renders.
async function reviewRules(action, ids, rerender) {
  try {
    cache.rules = await api('POST', '/api/rules/review', { action, ids });
    toast(`${plural(ids.length, 'rule')} ${action === 'approve' ? 'approved' : 'rejected'}`);
  } catch (err) { reportError(err); }
  rerender();
}

function learnedNote(r) {
  const l = r.learned;
  if (!l) return null;
  const parts = [`Learned ${ago(l.at)} from the ${l.model || 'Laya'} checkpoint`, `${pct(l.confidence)} confident in ${l.category}`];
  if (l.severity) parts.push(`severity ${l.severity} (${pct(l.severity_confidence)})`);
  parts.push(`source ${sourceName(l.source_id)}`);
  return h('div', { class: 'learned' },
    h('div', { class: 'meta' }, parts.join(' · '), l.event_id ? [' · ', h('a', { href: `#/events/${l.event_id}` }, 'event')] : null),
    l.example ? h('div', { class: 'meta mono example', title: l.example }, l.example) : null);
}

function renderRules(root, tab = 'all', rerender = () => renderRules(root, tab)) {
  clear(root);
  const all = cache.rules;
  const rules = tab === 'all' ? all : all.filter((r) => ruleStatus(r) === tab);
  if (tab === 'review' && rules.length) {
    const ids = rules.map((r) => r.id);
    root.append(h('div', { class: 'list-item review-bar' },
      h('div', { class: 'main' }, h('div', { class: 'meta' },
        'Lissandra learned these from the engine\'s answers. They already classify and alert. Approve the ones that are right, edit the ones that are close, and reject the rest: a rejected rule keeps the engine away from its pattern.')),
      h('div', { class: 'actions' },
        h('button', { type: 'button', class: 'small', onclick: () => reviewRules('approve', ids, rerender) }, `Approve all ${num(ids.length)}`),
        h('button', {
          type: 'button', class: 'small danger',
          onclick: async () => {
            if (!(await confirmDialog(`Reject all ${plural(ids.length, 'learned rule')}? Their patterns stay uncategorized and aren't sent to the engine again.`, 'Reject all'))) return;
            reviewRules('reject', ids, rerender);
          },
        }, 'Reject all'))));
  }
  if (!rules.length) {
    const empty = {
      all: 'No rules. Events will only get heuristic severities.',
      active: 'No active rules.',
      review: engineSettings().learn_rules === false
        ? 'Nothing to review. Rule learning is off (Settings → Laya engine).'
        : 'Nothing to review. When the Laya engine categorizes a pattern no rule covers, the rule Lissandra learns from it appears here.',
      rejected: 'No rejected rules.',
    };
    root.append(h('div', { class: 'empty' }, empty[tab]));
  }
  const move = async (i, d) => {
    const ids = all.map((r) => r.id);
    [ids[i], ids[i + d]] = [ids[i + d], ids[i]];
    cache.rules = await api('POST', '/api/rules/reorder', { ids }).catch(reportError) || cache.rules;
    rerender();
  };
  rules.forEach((r) => {
    const i = all.indexOf(r);
    const conds = r.conditions.map((c) => `${c.field} ${OP_LABELS[c.op] || c.op}${['exists', 'missing'].includes(c.op) ? '' : ` "${c.value}"`}`).join(r.match === 'any' ? '  OR  ' : '  AND  ');
    const acts = ruleActions(r);
    const status = ruleStatus(r);
    const toggle = h('input', {
      type: 'checkbox', checked: r.enabled, title: 'Enabled',
      onchange: async (e) => {
        try {
          await api('PUT', `/api/rules/${r.id}`, { ...r, enabled: e.target.checked });
          r.enabled = e.target.checked;
          rerender();
        } catch (err) { e.target.checked = !e.target.checked; reportError(err); }
      },
    });
    root.append(h('div', { class: `list-item ${r.enabled ? '' : 'disabled-item'}` },
      // Order only makes sense against every rule.
      tab === 'all' ? h('div', { class: 'order-btns' },
        h('button', { type: 'button', disabled: i === 0, 'aria-label': 'Move up', onclick: () => move(i, -1) }, '▲'),
        h('button', { type: 'button', disabled: i === all.length - 1, 'aria-label': 'Move down', onclick: () => move(i, 1) }, '▼')) : null,
      toggle,
      h('div', { class: 'main' },
        h('div', { class: 'title' }, r.name,
          status === 'review' ? h('span', { class: 'badge warn' }, 'under review') : null,
          status === 'rejected' ? h('span', { class: 'badge bad' }, 'rejected') : null,
          status === 'active' && r.learned ? h('span', { class: 'badge', title: 'Learned from the Laya engine and approved' }, 'learned') : null,
          r.source_ids && r.source_ids.length ? h('span', { class: 'badge' }, `only: ${r.source_ids.map(sourceName).join(', ')}`) : null),
        h('div', { class: 'meta mono' }, conds),
        h('div', { class: 'meta' }, acts),
        status !== 'active' ? learnedNote(r) : null),
      h('div', { class: 'actions' },
        status === 'review' ? h('button', { type: 'button', class: 'small primary', onclick: () => reviewRules('approve', [r.id], rerender) }, 'Approve') : null,
        status === 'rejected' && r.learned ? h('button', { type: 'button', class: 'small', title: 'Apply what was learned again', onclick: () => reviewRules('approve', [r.id], rerender) }, 'Restore') : null,
        h('button', { type: 'button', class: 'small', onclick: () => editRule(r) }, 'Edit'),
        status === 'review' ? h('button', { type: 'button', class: 'small danger', onclick: () => reviewRules('reject', [r.id], rerender) }, 'Reject') : null,
        h('button', {
          type: 'button', class: 'small danger',
          onclick: async () => {
            const relearn = r.learned ? ' Lissandra may learn its pattern again when the engine next answers it; reject it instead to keep the engine away.' : '';
            if (!(await confirmDialog(`Delete rule "${r.name}"?${relearn}`))) return;
            await api('DELETE', `/api/rules/${r.id}`).catch(reportError);
            await load('rules');
            rerender();
          },
        }, 'Delete'))));
  });
}

// ruleActions are a rule's effects as badges.
function ruleActions(r) {
  const acts = [];
  if (r.drop) acts.push(h('span', { class: 'badge bad' }, 'drop'));
  if (r.category) acts.push(h('span', { class: 'badge' }, `category: ${r.category}`));
  if (r.severity) acts.push(h('span', { class: 'badge' }, `${r.severity_mode === 'at_least' ? 'severity ≥' : 'severity ='} ${r.severity}`));
  if (r.skip_engine) acts.push(h('span', { class: 'badge', title: 'Matching events are not sent to the Laya engine' }, 'no engine'));
  if (r.stop) acts.push(h('span', { class: 'badge' }, 'stop'));
  acts.push(tags(r.tags));
  return acts;
}

// ruleTester runs a sample through the saved rules, or through rules()
// when given (unsaved choices).
function ruleTester({ rules } = {}) {
  const sample = h('textarea', { rows: 7, placeholder: 'Paste log lines, a JSON object or NDJSON…', 'aria-label': 'Sample logs' },
    '2026-10-11T10:15:02Z ERROR payment failed: connection refused to db:5432\n{"level":"warn","msg":"login failed for user admin from 10.0.0.7"}\nGET /healthz 200 1ms');
  const srcSel = select([['', 'Default parsing'], ...cache.sources.map((x) => [x.id, x.name])], '', { 'aria-label': 'Parse as source' });
  const out = h('div');
  return h('div', { class: 'card sticky' },
    h('h2', null, 'Rule tester'),
    h('p', { class: 'muted small' }, rules
      ? 'Runs the rules as set on this page, saved or not, against a sample without storing anything or sending notifications.'
      : 'Runs the saved rules against a sample without storing anything or sending notifications.'),
    sample,
    h('div', { class: 'row mt' }, srcSel, h('span', { class: 'spacer' }), h('button', {
      type: 'button', class: 'primary',
      onclick: async () => {
        try {
          const r = await api('POST', '/api/rules/test', { sample: sample.value, source_id: srcSel.value, ...(rules ? { rules: rules() } : {}) });
          clear(out).append(previewTable(r.events));
        } catch (err) { clear(out).append(h('div', { class: 'callout bad' }, err.message)); }
      },
    }, 'Test')),
    out);
}

// suggestContains picks text from a log message that other occurrences of
// the same problem will share: the longest run without timestamps, numbers
// or ids, trimmed of punctuation.
function suggestContains(message) {
  const first = String(message || '').split('\n')[0];
  const parts = first
    .replace(/^\W*(\d{4}[-/]\d\d[-/]\d\d[T ][\d:.,]+\S*|[A-Z][a-z]{2} [ \d]\d [\d:]{8})\W*/, '')
    .split(/[0-9a-f]{8,}|\d+/i)
    .map((s) => s.replace(/^[^\p{L}]+|[^\p{L}\p{N})]+$/gu, ''))
    .filter((s) => s.length >= 6)
    .sort((a, b) => b.length - a.length);
  return (parts[0] || first).slice(0, 80);
}

// editRule opens the rule editor, for a new rule (from an event's message
// when fromEvent is given) or an existing one. onSaved, when given, is
// called with the saved rule instead of going to the Rules page.
function editRule(rule, fromEvent, { onSaved } = {}) {
  const isNew = !rule;
  rule = rule ? JSON.parse(JSON.stringify(rule)) : {
    name: '', enabled: true, match: 'all', severity_mode: 'set', tags: [], source_ids: [],
    conditions: [{ field: 'message', op: 'contains', value: fromEvent ? suggestContains(fromEvent.message) : '' }],
  };
  if (fromEvent) rule.name = `Events like "${suggestContains(fromEvent.message).slice(0, 40)}"`;

  const name = h('input', { value: rule.name });
  const desc = h('input', { value: rule.description || '' });
  const enabled = checkbox('Enabled', rule.enabled);
  const match = select([['all', 'all conditions match (AND)'], ['any', 'any condition matches (OR)']], rule.match);
  const condWrap = h('div');
  const dl = h('datalist', { id: 'field-suggestions' }, FIELD_SUGGESTIONS.map((x) => h('option', { value: x })));
  const conds = rule.conditions.map((c) => ({ ...c }));

  const renderConds = () => {
    clear(condWrap);
    conds.forEach((c, i) => {
      const fieldIn = h('input', { value: c.field, list: 'field-suggestions', 'aria-label': 'Field', oninput: (e) => { c.field = e.target.value; } });
      const op = select(Object.entries(OP_LABELS), c.op, { 'aria-label': 'Operator', onchange: (e) => { c.op = e.target.value; renderConds(); } });
      const val = h('input', { value: c.value || '', 'aria-label': 'Value', disabled: ['exists', 'missing'].includes(c.op), oninput: (e) => { c.value = e.target.value; } });
      const cs = h('label', { class: 'check small', title: 'Case sensitive' }, h('input', { type: 'checkbox', checked: c.case_sensitive, onchange: (e) => { c.case_sensitive = e.target.checked; } }), 'Aa');
      condWrap.append(h('div', { class: 'cond-row' }, fieldIn, op, val, cs,
        h('button', { type: 'button', class: 'small', disabled: conds.length === 1, 'aria-label': 'Remove condition', onclick: () => { conds.splice(i, 1); renderConds(); } }, '✕')));
    });
  };
  renderConds();

  const catIn = h('input', { value: rule.category || '', list: 'category-suggestions', placeholder: 'leave empty to not set' });
  const catList = h('datalist', { id: 'category-suggestions' }, categoryList().map((c) => h('option', { value: c.name }, c.description || null)));
  const sev = select([['', '— keep detected —'], ...SEVERITIES.map((x) => [x, x])], rule.severity || '');
  const sevMode = select([['set', 'set exactly'], ['at_least', 'raise to at least']], rule.severity_mode || 'set');
  const tagsIn = h('input', { value: (rule.tags || []).join(', ') });
  const drop = checkbox('Drop matching events (don\'t store or alert)', rule.drop);
  const stop = checkbox('Stop evaluating later rules', rule.stop);
  const skipEngine = checkbox('Don\'t ask the Laya engine about matching events', rule.skip_engine);
  const srcChecks = cache.sources.map((x) => ({ id: x.id, box: checkbox(x.name, (rule.source_ids || []).includes(x.id)) }));

  const testIn = h('textarea', { rows: 3, placeholder: 'Sample log to try this rule on…' }, fromEvent ? fromEvent.message : '');
  const testOut = h('div');

  const collect = () => ({
    ...rule,
    name: name.value.trim(), description: desc.value.trim(), enabled: enabled.input.checked, match: match.value,
    conditions: conds.map((c) => ({ field: c.field.trim(), op: c.op, value: c.value || '', case_sensitive: Boolean(c.case_sensitive) })),
    category: catIn.value.trim(), severity: sev.value, severity_mode: sevMode.value, tags: splitList(tagsIn.value),
    drop: drop.input.checked, stop: stop.input.checked, skip_engine: skipEngine.input.checked,
    source_ids: srcChecks.filter((x) => x.box.input.checked).map((x) => x.id),
  });
  const status = ruleStatus(rule);

  const body = h('div', null, dl, catList,
    rule.learned ? h('div', { class: `callout ${status === 'review' ? 'warn' : ''}` },
      status === 'review' ? 'Learned from the Laya engine and awaiting review. It already classifies matching events. ' : null,
      status === 'rejected' ? 'Rejected: it keeps the Laya engine away from its pattern. ' : null,
      learnedNote(rule)) : null,
    h('div', { class: 'fields-2' }, field('Name', name), field('Description', desc)),
    enabled.el,
    h('fieldset', { class: 'mt' }, h('legend', null, 'When'),
      field('Match', match),
      condWrap,
      h('button', { type: 'button', class: 'small', onclick: () => { conds.push({ field: 'message', op: 'contains', value: '' }); renderConds(); } }, '+ Add condition'),
      h('div', { class: 'field mt' }, h('label', null, 'Only for sources'), h('div', { class: 'chips' }, srcChecks.map((x) => x.box.el)), h('div', { class: 'help' }, 'Leave all unchecked to apply to every source.'))),
    h('fieldset', null, h('legend', null, 'Then'),
      h('div', { class: 'fields-3' }, field('Category', catIn), field('Severity', sev), field('Severity mode', sevMode)),
      field('Add tags', tagsIn, 'Comma-separated.'),
      h('div', { class: 'row' }, drop.el, stop.el, skipEngine.el)),
    h('fieldset', null, h('legend', null, 'Try it'),
      testIn,
      h('div', { class: 'row mt' }, h('button', {
        type: 'button', class: 'small',
        onclick: async () => {
          try {
            const r = await api('POST', '/api/rules/test', { sample: testIn.value, rules: [collect()] });
            clear(testOut).append(previewTable(r.events));
          } catch (err) { clear(testOut).append(h('div', { class: 'callout bad' }, err.message)); }
        },
      }, 'Test this rule alone')),
      testOut));

  // A rule under review can be saved as it is, or saved and approved.
  const save = (approve) => async (close) => {
    const r = collect();
    if (approve) r.status = '';
    const saved = isNew ? await api('POST', '/api/rules', r) : await api('PUT', `/api/rules/${rule.id}`, r);
    close();
    toast(approve ? 'Rule saved and approved' : 'Rule saved');
    if (onSaved) await onSaved(saved);
    else if (parseHash().page === 'rules') route();
    else location.hash = '#/rules';
  };
  const cancel = { label: 'Cancel', onClick: (c) => c() };
  openModal({
    title: isNew ? 'Add rule' : `Edit rule "${rule.name}"`, body, wide: true,
    actions: status === 'review'
      ? [cancel, { label: 'Save', onClick: save(false) }, { label: 'Save & approve', primary: true, onClick: save(true) }]
      : [cancel, { label: isNew ? 'Create rule' : 'Save', primary: true, onClick: save(false) }],
  });
}

// ---------- Notifiers ----------

pages.notifiers = async (main) => {
  await load('notifiers', 'types');
  const list = h('div', { class: 'card flush' });
  append(main,
    pageHead('Notifiers', 'Channels Lissandra can alert through. Alert routes decide which events go where.',
      h('button', { type: 'button', class: 'primary', onclick: () => editNotifier(null) }, 'Add notifier')),
    list);
  clear(list);
  if (!cache.notifiers.length) {
    list.append(h('div', { class: 'empty' }, 'No notifiers yet. Add email, Slack, Microsoft Teams, Discord, Google Chat, PagerDuty or a generic webhook.'));
  }
  for (const n of cache.notifiers) {
    const type = cache.types.find((t) => t.type === n.type);
    list.append(h('div', { class: `list-item ${n.enabled ? '' : 'disabled-item'}` },
      h('div', { class: 'main' },
        h('div', { class: 'title' }, n.name, h('span', { class: 'badge' }, type ? type.label : n.type), n.enabled ? null : h('span', { class: 'badge' }, 'disabled')),
        h('div', { class: 'meta' }, notifierSummary(n, type))),
      h('div', { class: 'actions' },
        h('button', { type: 'button', class: 'small', onclick: (e) => sendTest(n, e.currentTarget) }, 'Send test'),
        h('button', { type: 'button', class: 'small', onclick: () => editNotifier(n) }, 'Edit'),
        h('button', {
          type: 'button', class: 'small danger',
          onclick: async () => {
            if (!(await confirmDialog(`Delete notifier "${n.name}"? It will be removed from all routes and alert groups.`))) return;
            await api('DELETE', `/api/notifiers/${n.id}`).catch(reportError);
            route();
          },
        }, 'Delete'))));
  }
};

function notifierSummary(n, type) {
  if (!type) return '';
  return type.fields.filter((f) => !f.secret && n.settings[f.key] && f.type !== 'textarea').slice(0, 3)
    .map((f) => `${f.label}: ${n.settings[f.key]}`).join(' · ');
}

async function sendTest(n, btn) {
  if (btn) btn.disabled = true;
  try {
    const r = await api('POST', '/api/notifiers/test', n);
    if (r.ok) toast(`Test notification sent via "${n.name || n.type}"`);
    else toast(`Test failed: ${r.error}`, 'error');
  } catch (err) {
    reportError(err);
  } finally {
    if (btn) btn.disabled = false;
  }
}

// editNotifier opens the notifier editor. type preselects the type of a
// new one; onSaved, when given, is called with the saved notifier instead
// of re-rendering the page.
function editNotifier(n, { type, onSaved } = {}) {
  const isNew = !n;
  n = n ? JSON.parse(JSON.stringify(n)) : { name: '', type: type || cache.types[0].type, enabled: true, settings: {} };
  const name = h('input', { value: n.name, placeholder: 'e.g. #ops-alerts' });
  const typeSel = select(cache.types.map((t) => [t.type, t.label]), n.type, { disabled: !isNew });
  const enabled = checkbox('Enabled', n.enabled);
  const desc = h('div', { class: 'callout' });
  const fieldsWrap = h('div');
  let inputs = {};

  const renderFields = () => {
    const t = cache.types.find((x) => x.type === typeSel.value);
    clear(desc).append(t.description);
    clear(fieldsWrap);
    inputs = {};
    const settings = typeSel.value === n.type ? n.settings : {};
    for (const f of t.fields) {
      const v = settings[f.key] ?? '';
      let input;
      if (f.type === 'textarea') input = h('textarea', { rows: 4, placeholder: f.placeholder || '' }, v);
      else if (f.type === 'select') input = select(f.options, v || f.default);
      else if (f.type === 'checkbox') input = h('input', { type: 'checkbox', checked: v === 'true' });
      else if (f.type === 'password') input = secretInput(v, { placeholder: f.placeholder || '' });
      else input = h('input', { type: f.type === 'number' ? 'number' : 'text', value: v, placeholder: f.placeholder || f.default || '', autocomplete: 'off' });
      inputs[f.key] = { f, input };
      const label = f.label + (f.required ? ' *' : '');
      fieldsWrap.append(f.type === 'checkbox'
        ? h('div', { class: 'field' }, h('label', { class: 'check' }, input, f.label), f.help ? h('div', { class: 'help' }, f.help) : null)
        : field(label, input, f.help));
    }
  };
  typeSel.addEventListener('change', renderFields);
  renderFields();

  const collect = () => {
    const settings = {};
    for (const [k, { f, input }] of Object.entries(inputs)) {
      settings[k] = f.type === 'checkbox' ? (input.checked ? 'true' : '') : input.value;
    }
    return { ...n, name: name.value.trim(), type: typeSel.value, enabled: enabled.input.checked, settings };
  };

  const body = h('div', null,
    h('div', { class: 'fields-2' }, field('Name', name), field('Type', typeSel)),
    desc, fieldsWrap, enabled.el,
    h('p', { class: 'small muted' }, 'Tip: any value can be written as ${env:NAME} to read it from Lissandra\'s environment, so secrets can live in Nomad Variables or Vault instead of Lissandra\'s config file.'));
  openModal({
    title: isNew ? 'Add notifier' : `Edit notifier "${n.name}"`, body,
    left: h('button', { type: 'button', onclick: (e) => sendTest(collect(), e.currentTarget) }, 'Send test'),
    actions: [
      { label: 'Cancel', onClick: (c) => c() },
      {
        label: isNew ? 'Create notifier' : 'Save', primary: true,
        onClick: async (close) => {
          const data = collect();
          const saved = isNew ? await api('POST', '/api/notifiers', data) : await api('PUT', `/api/notifiers/${n.id}`, data);
          close();
          toast('Notifier saved');
          if (onSaved) await onSaved(saved); else route();
        },
      },
    ],
  });
}

// ---------- Routes ----------

const THROTTLES = [[0, 'No throttling'], [60, '1 minute'], [300, '5 minutes'], [900, '15 minutes'], [3600, '1 hour'], [21600, '6 hours'], [86400, '24 hours']];

pages.routes = async (main) => {
  await load('routes', 'notifiers', 'sources', 'settings');
  const list = h('div', { class: 'card flush' });
  append(main,
    pageHead('Alert routes', 'Send matching events to notifiers. Throttling limits repeats of the same problem (same fingerprint) per notifier.',
      h('button', { type: 'button', class: 'primary', onclick: () => editRoute(null) }, 'Add route')),
    list);
  if (!cache.notifiers.length) {
    main.insertBefore(h('div', { class: 'callout' }, 'Add a notifier first, then create routes that send events to it. ', h('a', { href: '#/notifiers' }, 'Go to notifiers →')), list);
  }
  if (!cache.routes.length) list.append(h('div', { class: 'empty' }, 'No routes yet — classified events are stored but nobody is notified.'));
  for (const r of cache.routes) {
    const notifierNames = r.notifier_ids.map((id) => (cache.notifiers.find((n) => n.id === id) || {}).name).filter(Boolean);
    const when = [`severity ≥ ${r.min_severity}`];
    if (r.categories && r.categories.length) when.push(`category in [${r.categories.join(', ')}]`);
    if (r.source_ids && r.source_ids.length) when.push(`source in [${r.source_ids.map(sourceName).join(', ')}]`);
    if (r.tags && r.tags.length) when.push(`tag in [${r.tags.join(', ')}]`);
    if (r.message_regex) when.push(`message ~ /${r.message_regex}/`);
    const throttle = THROTTLES.find((t) => t[0] === r.throttle_seconds);
    list.append(h('div', { class: `list-item ${r.enabled ? '' : 'disabled-item'}` },
      h('div', { class: 'main' },
        h('div', { class: 'title' }, r.name, r.enabled ? null : h('span', { class: 'badge' }, 'disabled')),
        h('div', { class: 'meta' }, 'When ', when.join(' and ')),
        h('div', { class: 'meta' }, '→ ', notifierNames.length ? notifierNames.join(', ') : h('span', { class: 'badge warn' }, 'no notifiers'),
          ' · ', r.throttle_seconds ? `throttle ${throttle ? throttle[1] : `${r.throttle_seconds}s`}` : 'no throttling')),
      h('div', { class: 'actions' },
        h('button', { type: 'button', class: 'small', onclick: () => editRoute(r) }, 'Edit'),
        h('button', {
          type: 'button', class: 'small danger',
          onclick: async () => {
            if (!(await confirmDialog(`Delete route "${r.name}"?`))) return;
            await api('DELETE', `/api/routes/${r.id}`).catch(reportError);
            route();
          },
        }, 'Delete'))));
  }
};

// editRoute opens the route editor. onSaved, when given, is called with
// the saved route instead of re-rendering the page.
function editRoute(r, { onSaved } = {}) {
  const isNew = !r;
  r = r ? JSON.parse(JSON.stringify(r)) : { name: '', enabled: true, min_severity: 'error', categories: [], source_ids: [], tags: [], notifier_ids: [], throttle_seconds: 900 };
  const name = h('input', { value: r.name, placeholder: 'e.g. Production errors to on-call' });
  const enabled = checkbox('Enabled', r.enabled);
  const minSev = select(SEVERITIES, r.min_severity);
  const descs = Object.fromEntries(categoryList().map((c) => [c.name, c.description]));
  const cats = [...new Set([...categories(), 'uncategorized', ...(r.categories || [])])].map((c) => ({ c, box: checkbox(c, (r.categories || []).includes(c)) }));
  for (const x of cats) if (descs[x.c]) x.box.el.title = descs[x.c];
  const srcs = cache.sources.map((x) => ({ id: x.id, box: checkbox(x.name, (r.source_ids || []).includes(x.id)) }));
  const nts = cache.notifiers.map((x) => ({ id: x.id, box: checkbox(x.name, (r.notifier_ids || []).includes(x.id)) }));
  const tagsIn = h('input', { value: (r.tags || []).join(', ') });
  const regex = h('input', { value: r.message_regex || '', placeholder: '(?i)payment|checkout' });
  const throttleOpts = THROTTLES.some((t) => t[0] === r.throttle_seconds) ? THROTTLES : [...THROTTLES, [r.throttle_seconds, `${r.throttle_seconds} seconds`]];
  const throttle = select(throttleOpts.map(([v, l]) => [String(v), l]), String(r.throttle_seconds));

  const body = h('div', null,
    field('Name', name), enabled.el,
    h('fieldset', { class: 'mt' }, h('legend', null, 'Match events'),
      field('Minimum severity', minSev),
      h('div', { class: 'field' }, h('label', null, 'Categories'), h('div', { class: 'chips' }, cats.map((x) => x.box.el)), h('div', { class: 'help' }, 'None checked = any category.')),
      h('div', { class: 'field' }, h('label', null, 'Sources'), h('div', { class: 'chips' }, srcs.map((x) => x.box.el)), h('div', { class: 'help' }, 'None checked = any source.')),
      h('div', { class: 'fields-2' }, field('Any of these tags', tagsIn), field('Message regex', regex))),
    h('fieldset', null, h('legend', null, 'Notify'),
      nts.length ? h('div', { class: 'chips' }, nts.map((x) => x.box.el)) : h('p', { class: 'muted' }, 'No notifiers configured yet.'),
      h('div', { class: 'mt' }, field('Throttle repeats', throttle, 'After alerting on a problem, suppress the same fingerprint on the same notifier for this long. The next alert reports how many were suppressed.'))));

  openModal({
    title: isNew ? 'Add alert route' : `Edit route "${r.name}"`, body, wide: true,
    actions: [
      { label: 'Cancel', onClick: (c) => c() },
      {
        label: isNew ? 'Create route' : 'Save', primary: true,
        onClick: async (close) => {
          const data = {
            ...r, name: name.value.trim(), enabled: enabled.input.checked, min_severity: minSev.value,
            categories: cats.filter((x) => x.box.input.checked).map((x) => x.c),
            source_ids: srcs.filter((x) => x.box.input.checked).map((x) => x.id),
            notifier_ids: nts.filter((x) => x.box.input.checked).map((x) => x.id),
            tags: splitList(tagsIn.value), message_regex: regex.value.trim(), throttle_seconds: Number(throttle.value),
          };
          const saved = isNew ? await api('POST', '/api/routes', data) : await api('PUT', `/api/routes/${r.id}`, data);
          close();
          toast('Route saved');
          if (onSaved) await onSaved(saved); else route();
        },
      },
    ],
  });
}

// ---------- Alert groups ----------

pages.groups = async (main) => {
  await load('groups', 'notifiers', 'sources', 'settings');
  const list = h('div', { class: 'card flush' });
  append(main,
    pageHead('Alert groups', 'Teams and people to alert about specific sources: each group has its channels, its members\' email addresses and a minimum severity. Assign sources here or in each source\'s settings.',
      h('button', { type: 'button', class: 'primary', onclick: () => editGroup(null) }, 'Add alert group')),
    list);
  if (!cache.notifiers.length) {
    main.insertBefore(h('div', { class: 'callout' }, 'Add a notifier first: a group is alerted through notifiers (a Slack or Teams channel, PagerDuty, or an email server for its members). ', h('a', { href: '#/notifiers' }, 'Go to notifiers →')), list);
  }
  if (!cache.groups.length) list.append(h('div', { class: 'empty' }, 'No alert groups yet.'));
  for (const g of cache.groups) {
    list.append(groupItem(g,
      h('button', { type: 'button', class: 'small', onclick: () => editGroup(g) }, 'Edit'),
      h('button', {
        type: 'button', class: 'small danger',
        onclick: async () => {
          if (!(await confirmDialog(`Delete alert group "${g.name}"? Its sources stop alerting it.`))) return;
          await api('DELETE', `/api/alert-groups/${g.id}`).catch(reportError);
          route();
        },
      }, 'Delete')));
  }
};

// groupItem shows an alert group in a list, with the given action buttons.
function groupItem(g, ...actions) {
  const chans = g.notifier_ids.map((id) => cache.notifiers.find((n) => n.id === id)).filter(Boolean);
  const when = [`severity ≥ ${g.min_severity}`];
  if (g.categories && g.categories.length) when.push(`category in [${g.categories.join(', ')}]`);
  const throttle = THROTTLES.find((t) => t[0] === g.throttle_seconds);
  // Only enabled channels deliver, and members are emailed only through an
  // enabled email notifier: anything else is shown as a warning.
  const reach = [];
  if (chans.some((n) => n.enabled)) reach.push(chans.filter((n) => n.enabled).map((n) => n.name).join(', '));
  for (const n of chans.filter((x) => !x.enabled)) reach.push(h('span', { class: 'badge warn' }, `${n.name} is off`));
  if ((g.emails || []).length) {
    const via = cache.notifiers.find((n) => n.id === g.email_notifier_id);
    const members = plural(g.emails.length, 'member');
    if (via && via.enabled) reach.push(`${members} by email (${via.name})`);
    else reach.push(h('span', { class: 'badge warn' }, !g.email_notifier_id ? `${members}, no email notifier` : `${members}, email notifier ${via ? 'off' : 'missing'}`));
  }
  return h('div', { class: `list-item ${g.enabled ? '' : 'disabled-item'}` },
    h('div', { class: 'main' },
      h('div', { class: 'title' }, g.name, g.enabled ? null : h('span', { class: 'badge' }, 'disabled')),
      g.description ? h('div', { class: 'meta' }, g.description) : null,
      h('div', { class: 'meta' }, 'Sources: ', g.source_ids.length ? g.source_ids.map(sourceName).join(', ') : h('span', { class: 'badge warn' }, 'none: the group gets no alerts')),
      h('div', { class: 'meta' }, 'When ', when.join(' and ')),
      h('div', { class: 'meta' }, '→ ', reach.some((x) => typeof x === 'string') ? reach.map((x, i) => [i ? ' · ' : '', x]) : [h('span', { class: 'badge warn' }, 'no channels or members that get alerts'), reach.map((x) => [' · ', x])],
        ' · ', g.throttle_seconds ? `throttle ${throttle ? throttle[1] : `${g.throttle_seconds}s`}` : 'no throttling')),
    h('div', { class: 'actions' }, actions));
}

// activeGroups are the alert groups that get alerts: enabled, with
// sources assigned.
function activeGroups() {
  return (cache.groups || []).filter((g) => g.enabled && g.source_ids.length);
}

function editGroup(g, { onSaved } = {}) {
  const isNew = !g;
  g = g ? JSON.parse(JSON.stringify(g)) : { name: '', enabled: true, min_severity: 'error', categories: [], notifier_ids: [], emails: [], source_ids: [], throttle_seconds: 900 };
  const name = h('input', { value: g.name, placeholder: 'e.g. Payments team' });
  const desc = h('input', { value: g.description || '' });
  const enabled = checkbox('Enabled', g.enabled);
  const minSev = select(SEVERITIES, g.min_severity);
  const cats = [...new Set([...categories(), 'uncategorized', ...(g.categories || [])])].map((c) => ({ c, box: checkbox(c, (g.categories || []).includes(c)) }));
  const srcs = cache.sources.map((x) => ({ id: x.id, box: checkbox(x.name, g.source_ids.includes(x.id)) }));
  const nts = cache.notifiers.map((x) => ({ id: x.id, box: checkbox(x.name, g.notifier_ids.includes(x.id)) }));
  const emailNotifiers = cache.notifiers.filter((n) => n.type === 'email');
  const emails = h('textarea', { rows: 3, placeholder: 'alice@example.com\nbob@example.com' }, (g.emails || []).join('\n'));
  const emailVia = select([['', emailNotifiers.length ? '— choose an email notifier —' : 'no email notifier configured'], ...emailNotifiers.map((n) => [n.id, n.name])], g.email_notifier_id || (emailNotifiers.length === 1 ? emailNotifiers[0].id : ''));
  const throttleOpts = THROTTLES.some((t) => t[0] === g.throttle_seconds) ? THROTTLES : [...THROTTLES, [g.throttle_seconds, `${g.throttle_seconds} seconds`]];
  const throttle = select(throttleOpts.map(([v, l]) => [String(v), l]), String(g.throttle_seconds));

  const body = h('div', null,
    h('div', { class: 'fields-2' }, field('Name', name), field('Description', desc)), enabled.el,
    h('fieldset', { class: 'mt' }, h('legend', null, 'Sources'),
      srcs.length ? h('div', { class: 'chips' }, srcs.map((x) => x.box.el)) : h('p', { class: 'muted' }, 'No sources yet.'),
      h('div', { class: 'help mt-small' }, 'The group is alerted about events from these sources only.')),
    h('fieldset', null, h('legend', null, 'Which events'),
      field('Minimum severity', minSev),
      h('div', { class: 'field' }, h('label', null, 'Categories'), h('div', { class: 'chips' }, cats.map((x) => x.box.el)), h('div', { class: 'help' }, 'None checked = any category.'))),
    h('fieldset', null, h('legend', null, 'Alert'),
      h('div', { class: 'field' }, h('label', null, 'Channels'),
        nts.length ? h('div', { class: 'chips' }, nts.map((x) => x.box.el)) : h('p', { class: 'muted' }, 'No notifiers configured yet.'),
        h('div', { class: 'help' }, 'Slack or Teams channels, PagerDuty, webhooks: the notifiers this group is reached on.')),
      h('div', { class: 'fields-2' },
        field('Members (email)', emails, 'One address per line (or comma-separated).'),
        field('Email them through', emailVia, 'An email notifier: its SMTP server and From address are used, with these members as the recipients.')),
      field('Throttle repeats', throttle, 'After alerting on a problem, suppress the same fingerprint on the same channel for this long.')));

  openModal({
    title: isNew ? 'Add alert group' : `Edit alert group "${g.name}"`, body, wide: true,
    actions: [
      { label: 'Cancel', onClick: (c) => c() },
      {
        label: isNew ? 'Create group' : 'Save', primary: true,
        onClick: async (close) => {
          const data = {
            ...g, name: name.value.trim(), description: desc.value.trim(), enabled: enabled.input.checked, min_severity: minSev.value,
            categories: cats.filter((x) => x.box.input.checked).map((x) => x.c),
            source_ids: srcs.filter((x) => x.box.input.checked).map((x) => x.id),
            notifier_ids: nts.filter((x) => x.box.input.checked).map((x) => x.id),
            emails: emails.value.split(/[\s,;]+/).map((x) => x.trim()).filter(Boolean),
            email_notifier_id: emailVia.value, throttle_seconds: Number(throttle.value),
          };
          const saved = isNew ? await api('POST', '/api/alert-groups', data) : await api('PUT', `/api/alert-groups/${g.id}`, data);
          close();
          toast('Alert group saved');
          if (onSaved) await onSaved(saved); else route();
        },
      },
    ],
  });
}

// ---------- Deliveries ----------

pages.deliveries = async (main) => {
  const wrap = h('div', { class: 'card flush' });
  append(main, pageHead('Deliveries', 'Recent notification attempts (kept in memory).', h('button', { type: 'button', onclick: () => refresh().catch(reportError) }, 'Refresh')), wrap);
  const refresh = async () => {
    const list = await GET('/api/deliveries?limit=200');
    clear(wrap);
    if (!list.length) { wrap.append(h('div', { class: 'empty' }, 'No notifications sent yet.')); return; }
    const tbl = h('table', null, h('tr', null, h('th', null, 'Time'), h('th', null, 'Status'), h('th', null, 'Notifier'), h('th', null, 'Route'), h('th', null, 'Event'), h('th', null, 'Attempts'), h('th', null, 'Detail')));
    for (const d of list) {
      const cls = d.status === 'sent' ? 'ok' : d.status === 'failed' ? 'bad' : 'warn';
      tbl.append(h('tr', null,
        h('td', null, timeCell(d.time)),
        h('td', null, h('span', { class: `badge ${cls}` }, d.status), d.test ? h('span', { class: 'badge' }, 'test') : null),
        h('td', { class: 'nowrap' }, d.notifier_name, h('div', { class: 'muted small' }, d.notifier_type)),
        h('td', null, d.route_name || '—'),
        h('td', { class: 'msg', title: d.title }, d.event_id && d.event_id !== 'test' ? h('a', { href: `#/events/${d.event_id}` }, d.title) : d.title),
        h('td', null, d.attempts, h('div', { class: 'muted small' }, `${d.duration_ms} ms`)),
        h('td', { class: 'small' }, d.error || '')));
    }
    wrap.append(tbl);
  };
  await refresh();
  return every(10000, refresh);
};

// ---------- Settings ----------

// The engine chooses less accurately among many options.
const MANY_CATEGORIES = 20;

pages.settings = async (main) => {
  await load('settings', 'info', 'sources', 'auth', 'onboarding');
  const st = cache.settings;
  const publicURL = h('input', { value: st.public_url || '', placeholder: cache.info.public_url });
  const cats = categoryEditor(categoryList());

  const e = st.engine || {};
  const f = {
    enabled: checkbox('Classify events with the Laya engine', e.enabled),
    url: h('input', { placeholder: 'http://127.0.0.1:8000 or ${env:LISSANDRA_ENGINE_URL}', autocomplete: 'off', spellcheck: 'false' }),
    apiKey: secretInput('', { placeholder: '${env:LISSANDRA_ENGINE_API_KEY}' }),
    model: modelPicker(''),
    minConf: h('input', { type: 'number', min: 0, max: 1, step: 0.05 }),
    minSev: select(SEVERITIES, ''),
    onlyUncat: checkbox('Only events no rule categorized (rules first)', false),
    setSev: checkbox('Let the engine judge severity when the record states no level (about doubles engine time; off by default)', false),
    inFlight: h('input', { type: 'number', min: 1, max: 16, step: 1 }),
    timeout: h('input', { type: 'number', min: 1, max: 300, step: 1 }),
    learn: checkbox('Learn rules from the engine\'s answers (marked for review under Classification rules)', false),
    learnConf: h('input', { type: 'number', min: 0, max: 1, step: 0.05 }),
  };
  const fill = (x) => {
    f.enabled.input.checked = Boolean(x.enabled);
    f.url.value = x.url || '';
    f.apiKey.value = x.api_key || '';
    f.apiKey.dispatchEvent(new Event('input'));
    f.model.set(x.model || 'english');
    f.minConf.value = x.min_confidence ?? 0.6;
    f.minSev.value = x.min_severity || 'warning';
    f.onlyUncat.input.checked = Boolean(x.only_uncategorized);
    f.setSev.input.checked = Boolean(x.set_severity);
    f.inFlight.value = x.max_in_flight || 1;
    f.timeout.value = x.timeout_seconds || 30;
    f.learn.input.checked = x.learn_rules !== false;
    f.learnConf.value = x.learn_min_confidence ?? 0.8;
  };
  fill(e);

  const numberIn = (input, name, lo, hi, whole) => {
    const raw = input.value.trim();
    const v = Number(raw);
    if (raw === '' || !Number.isFinite(v) || v < lo || v > hi || (whole && !Number.isInteger(v))) {
      throw new InputError(`${name} must be ${whole ? 'a whole number' : 'a number'} from ${lo} to ${hi}.`);
    }
    return v;
  };

  // Settings that can't be saved are shown next to the card they belong
  // to (categories mark their rows), as well as in a toast.
  const generalErr = h('div');
  const engineErr = h('div');
  const showErr = (box, text) => {
    const el = h('div', { class: 'callout bad', role: 'alert' }, text);
    clear(box).append(el);
    el.scrollIntoView({ block: 'nearest' });
  };
  const clearErrs = () => { clear(generalErr); clear(engineErr); cats.showError(''); };

  const statusBox = h('div');
  const renderStatus = async () => {
    const stats = await GET('/api/stats');
    const a = stats.engine || {};
    const p = stats.pipeline || {};
    const paused = a.paused_until && new Date(a.paused_until) > new Date();
    const warnings = a.warnings || [];
    const pausedCps = a.paused_checkpoints || [];
    // As on the dashboard: an error counts until the engine answers again
    // (or for 10 minutes).
    const recovered = a.last_success_at && a.last_error_at && new Date(a.last_success_at) > new Date(a.last_error_at);
    const recent = a.last_error && a.last_error_at && !recovered && Date.now() - new Date(a.last_error_at).getTime() < 10 * 60 * 1000;
    let status;
    if (!a.enabled) status = h('span', { class: 'badge' }, 'disabled');
    else if (!a.configured) status = h('span', { class: 'badge bad' }, a.config_error ? 'configuration error' : 'no URL');
    else if (paused) status = h('span', { class: 'badge warn' }, 'paused after errors');
    else if (pausedCps.includes(a.model)) status = h('span', { class: 'badge warn' }, `${a.model} paused`);
    else if (recent) status = h('span', { class: 'badge warn' }, `error ${ago(a.last_error_at)}`);
    else if (warnings.length || pausedCps.length) status = h('span', { class: 'badge warn' }, 'active, with warnings');
    else status = h('span', { class: 'badge ok' }, 'active');
    const inUse = a.models_in_use || (a.model ? [a.model] : []);
    const urls = a.urls || [];
    const perInstance = engineSettings().max_in_flight || 1;
    const checkpoints = h('div', null,
      inUse.map((m, i) => h('div', null, m, h('span', { class: 'muted' }, i === 0 ? ' (default) ' : ' (a source\'s) '),
        a.loaded ? h('span', { class: `badge ${a.loaded.includes(m) ? 'ok' : 'warn'}` }, a.loaded.includes(m) ? 'loaded' : 'not loaded') : null,
        pausedCps.includes(m) ? [' ', h('span', { class: 'badge warn', title: 'The engine answered with another checkpoint (it doesn\'t know this name). Events that need it keep the rules\' result for 5 minutes.' }, 'paused')] : null)),
      a.loaded
        ? h('div', { class: 'small muted' }, `Loaded on the engine: ${a.loaded.join(', ') || 'none'}.`)
        : a.enabled && a.configured ? h('div', { class: 'small muted' }, 'The engine has not listed its checkpoints yet: it is checked every 30 s, and lists them only to callers with its API key.') : null);
    clear(statusBox).append(h('div', { class: 'kv' },
      h('div', null, 'Status'), h('div', null, status,
        a.config_error ? h('div', { class: 'small' }, a.config_error)
          : a.enabled && !a.configured ? h('div', { class: 'small muted' }, 'Set the engine URL above (or LISSANDRA_ENGINE_URL in Lissandra\'s environment) and save.') : null),
      warnings.length ? [h('div', null, warnings.length > 1 ? 'Warnings' : 'Warning'),
        h('div', { class: 'warnings' }, warnings.map((w) => h('div', null, h('span', { class: 'badge warn' }, 'warning'), ' ', w)))] : null,
      urls.length ? [h('div', null, urls.length > 1 ? 'Engines' : 'Engine'), h('div', { class: 'mono' }, urls.join(', '))] : null,
      h('div', null, inUse.length > 1 ? 'Checkpoints' : 'Checkpoint'), checkpoints,
      h('div', null, 'Capacity'), h('div', null, `${plural(a.capacity, 'request')} at a time`,
        urls.length > 1 ? ` (${num(perInstance)} per instance × ${urls.length})` : '', ` · ${num(a.in_flight)} in flight · ${num(p.engine_queue_depth)} queued`),
      h('div', null, 'Requests'), h('div', null, `${num(a.calls_total)} sent · ${num(a.cache_hits)} answered from cache (${num(a.cached_results)} patterns cached)`),
      h('div', null, 'Answers'), h('div', null, `${num(a.applied)} applied · ${num(a.low_confidence)} below the threshold · ${num(a.truncated)} truncated`),
      h('div', null, 'Speed'), h('div', null, a.avg_latency_ms
        ? `${num(a.avg_inference_ms)} ms computing, ${num(a.avg_latency_ms)} ms round trip (recent average)` : '—'),
      h('div', null, 'Without the engine'), h('div', null, `${plural(a.skipped, 'live event')} kept the rules' result: engine busy, paused or too slow, or checkpoint not loaded`,
        p.engine_shed ? h('div', { class: 'small muted' }, `${num(p.engine_shed)} of them were not queued because they would have timed out.`) : null),
      h('div', null, 'Errors'), h('div', null, `${plural(a.errors, 'failed request')} · ${num(a.timed_out)} timed out`),
      a.last_error ? [h('div', null, 'Last error'), h('div', null, `${a.last_error} (${ago(a.last_error_at)})`,
        a.last_success_at && new Date(a.last_success_at) > new Date(a.last_error_at) ? h('span', { class: 'muted small' }, ` · answered again ${ago(a.last_success_at)}`) : null)] : null,
      paused ? [h('div', null, 'Paused until'), h('div', null, `${fmtTime(a.paused_until)}. Meanwhile events go on with the rules' result.`)] : null));
  };

  const save = async () => {
    clearErrs();
    let engine;
    try {
      engine = {
        ...(cache.settings.engine || {}),
        enabled: f.enabled.input.checked,
        url: f.url.value.trim(),
        api_key: f.apiKey.value,
        model: f.model.value(),
        min_confidence: numberIn(f.minConf, 'Minimum confidence', 0, 1, false),
        min_severity: f.minSev.value,
        only_uncategorized: f.onlyUncat.input.checked,
        set_severity: f.setSev.input.checked,
        max_in_flight: numberIn(f.inFlight, 'Requests per engine instance', 1, 16, true),
        timeout_seconds: numberIn(f.timeout, 'Timeout', 1, 300, true),
        learn_rules: f.learn.input.checked,
        learn_min_confidence: numberIn(f.learnConf, 'Confidence to learn a rule', 0, 1, false),
      };
    } catch (err) {
      if (err instanceof InputError) showErr(engineErr, err.message);
      throw err;
    }
    const data = { ...cache.settings, public_url: publicURL.value.trim(), categories: cats.value(), engine };
    try {
      cache.settings = await api('PUT', '/api/settings', data);
    } catch (err) {
      if (err.status !== 400) throw err;
      // The server refused a value: say which, where it is.
      const text = err.message.replace(/^settings: /, '');
      if (/categor/i.test(text)) cats.showError(`Not saved: ${text}`);
      else showErr(/engine/i.test(text) ? engineErr : generalErr, `Not saved: ${text}`);
      throw new InputError(`Settings not saved: ${text}`);
    }
    cats.set(categoryList());
    fill(cache.settings.engine || {});
    toast('Settings saved');
    renderStatus().catch(reportError);
  };

  const healthOut = h('div');
  const checkBtn = h('button', {
    type: 'button',
    onclick: () => busy(checkBtn, async () => {
      clear(healthOut).append(h('p', { class: 'muted' }, 'Checking…'));
      try {
        // The checkpoints in use (default and per-source) label the rows.
        const [health, stats] = await Promise.all([GET('/api/engine/health'), GET('/api/stats').catch(() => null)]);
        renderHealth(healthOut, health, stats && stats.engine && stats.engine.models_in_use);
      } catch (err) { clear(healthOut); throw err; }
    }),
  }, 'Check connection');

  const sample = h('textarea', { rows: 3, placeholder: 'A log line to classify…', 'aria-label': 'Log line to classify' },
    'kafka consumer group rebalancing took 48213ms, lag on orders topic now 120k messages');
  const sampleSrc = select([['', 'No source (default checkpoint)'],
    ...cache.sources.map((x) => [x.id, x.engine_model ? `As "${x.name}" (${x.engine_model})` : `As "${x.name}"`])], '', { 'aria-label': 'Classify as source' });
  const testOut = h('div');
  const tryBtn = h('button', {
    type: 'button', class: 'primary',
    onclick: () => busy(tryBtn, async () => {
      if (!sample.value.trim()) { toast('Enter a log line to classify.', 'error'); return; }
      clear(testOut).append(h('p', { class: 'muted' }, 'Asking the engine…'));
      try {
        const missing = await unloadedCheckpoint(sampleSrc.value);
        if (missing) { clear(testOut).append(notLoadedNote(missing)); return; }
        const r = await api('POST', '/api/engine/classify', { message: sample.value, source_id: sampleSrc.value || undefined });
        clear(testOut).append(engineAnswer(r));
      } catch (err) { clear(testOut); throw err; }
    }),
  }, 'Classify');

  const trainBtn = h('button', {
    type: 'button',
    onclick: () => busy(trainBtn, () => download('/api/engine/training-data', 'lissandra-training-live.jsonl',
      'No training data yet: no live event in memory was categorized by a rule.')),
  }, 'Download training data (JSONL)');

  const importFile = h('input', { type: 'file', accept: 'application/json', hidden: true });
  importFile.addEventListener('change', async () => {
    const file = importFile.files[0];
    importFile.value = '';
    if (file) await importConfigFile(file);
  });

  const exportCfg = (secrets) => download(`/api/config/export${secrets ? '?include_secrets=true' : ''}`,
    `lissandra-config-${new Date().toISOString().slice(0, 10)}.json`);

  const saveBtn = () => h('button', { type: 'button', class: 'primary', onclick: (ev) => busy(ev.currentTarget, save) }, 'Save settings');

  append(main,
    pageHead('Settings', null, saveBtn()),
    h('div', { class: 'grid cols-2 top' },
      h('div', null,
        accessCard(),
        h('div', { class: 'card mt' },
          h('h2', null, 'General'),
          field('Public URL', publicURL, 'How users and senders reach Lissandra (e.g. https://lissandra.example.com). Used for ingest URLs and links in notifications. Defaults to LISSANDRA_PUBLIC_URL or the address you are using now.'),
          generalErr,
          h('h3', null, 'Backup'),
          h('p', { class: 'small muted' }, 'Export the configuration as JSON (secrets masked unless included), or import a file to replace it — useful for promoting config between environments.'),
          h('div', { class: 'row' },
            h('button', { type: 'button', onclick: () => exportCfg(false).catch(reportError) }, 'Export'),
            h('button', { type: 'button', onclick: () => exportCfg(true).catch(reportError) }, 'Export with secrets'),
            h('button', { type: 'button', onclick: () => importFile.click() }, 'Import…'), importFile)),
        setupGuideCard(),
        h('div', { class: 'card mt' },
          h('h2', null, 'Categories'),
          h('p', { class: 'small muted' }, 'Rules set categories by name, and alert routes and filters use them. For events no rule categorizes, the Laya engine picks one of these names, or "none of the above": those events stay "uncategorized", which is built in. The order is kept: it is the order the engine sees and editors show.'),
          cats.el)),
      h('div', { class: 'card' },
        h('h2', null, 'Laya engine'),
        h('p', { class: 'small muted' }, 'Rules run first. The Laya engine (laya-serve) classifies what they leave: it picks one of the category names, or none, and when the record states no level it judges the severity (a stated severity is never replaced). It is asked once per message pattern and repeats reuse the answer. If it is down, slow or unsure, events go on with the rules\' result.'),
        f.enabled.el,
        h('div', { class: 'mt' }),
        field('Engine URL', f.url, 'Base URL of laya-serve. Several URLs, comma-separated, share the load and fail over. ${env:NAME} reads Lissandra\'s environment (or the file NAME_FILE names) and ${file:/path} a file in Lissandra\'s secrets or task directory (or LISSANDRA_FILE_REF_DIRS); both are re-read while Lissandra runs. Credentials in a URL are shown masked: leave them as they are to keep them, also when adding instances or changing a port.'),
        field('API key', f.apiKey, 'The engine\'s LAYA_API_KEY, if it has one. Prefer ${env:LISSANDRA_ENGINE_API_KEY} and keep the key in a Nomad Variable.'),
        field('Checkpoint', f.model.el, 'english reads English logs and is the best calibrated; multilingual handles other languages and is faster on CPU. The engine must have it loaded (its LAYA_MODELS): Lissandra never asks for one it hasn\'t. Sources can pick their own.'),
        h('div', { class: 'fields-2' },
          field('Minimum confidence', f.minConf, 'How sure the engine must be (0–1) before its category or severity is used. Below it, the rules\' result stands.'),
          field('Minimum severity', f.minSev, 'Less severe events are not sent to the engine.')),
        h('div', { class: 'checks' }, f.onlyUncat.el, f.setSev.el),
        h('div', { class: 'fields-2 mt' },
          field('Requests per engine instance', f.inFlight, 'At once, to each laya-serve (1–16, default 1). laya-serve computes one answer at a time, so more only wait inside it. For more throughput, run more instances and list every URL.'),
          field('Timeout (seconds)', f.timeout, 'Events wait at most this long (up to 300) for the engine, queueing included, then go on with the rules\' result.')),
        h('h3', null, 'Learning'),
        h('div', { class: 'checks' }, f.learn.el),
        h('div', { class: 'fields-2' },
          field('Confidence to learn a rule', f.learnConf, 'An answer at least this confident (and at least the minimum confidence above) becomes a rule for its message pattern and source, under review. Default 0.8.')),
        engineErr,
        h('h3', null, 'Status'), statusBox,
        h('h3', null, 'Connection'),
        h('div', { class: 'row' }, checkBtn, h('span', { class: 'small muted' }, 'Uses the saved settings.')),
        healthOut,
        h('h3', null, 'Try a log line'),
        sample,
        h('div', { class: 'row mt' }, sampleSrc, h('span', { class: 'spacer' }), tryBtn),
        h('p', { class: 'small muted' }, 'Asks the engine now with the saved settings, and shows what the rules alone decide. Nothing is stored or counted.'),
        testOut,
        h('h3', null, 'Training data'),
        h('div', { class: 'row' }, trainBtn),
        h('p', { class: 'small muted' }, 'Message patterns your rules categorized in live events, in laya-train JSONL with the exact question Lissandra asks: for fine-tuning a checkpoint on your logs.'))),
    h('div', { class: 'row mt' }, h('span', { class: 'spacer' }), saveBtn()));
  await renderStatus();
};

// importConfigFile replaces the configuration with a JSON file, once
// confirmed, then runs after (by default, re-renders the page).
async function importConfigFile(file, { after } = {}) {
  try {
    const cfg = JSON.parse(await file.text());
    if (!(await confirmDialog('Replace the entire configuration with this file? Masked secrets are kept for matching IDs.', 'Import'))) return;
    await api('POST', '/api/config/import', cfg);
    // The setup wizard's drafts were edits of the configuration replaced.
    // The access one is kept: an import keeps the admin token, and the user
    // may have copied the token it holds.
    clearSetupDrafts('access');
    toast('Configuration imported');
    if (after) await after(); else route();
  } catch (err) { reportError(err); }
}

// accessCard says where the admin token comes from, and sets, rotates or
// deletes the one stored in Lissandra.
function accessCard() {
  const card = h('div', { class: 'card' });
  const render = () => {
    const a = cache.auth;
    const parts = [];
    if (a.source === 'generated') {
      parts.push(h('div', { class: 'callout warn' }, 'Lissandra generated this token at startup and printed it in its log. It changes every time Lissandra restarts.'),
        h('button', { type: 'button', class: 'primary', onclick: () => tokenModal('Set a permanent admin token', render) }, 'Set a permanent token…'));
    } else if (a.source === 'stored') {
      parts.push(h('p', { class: 'wrap-anywhere' }, `Set in Lissandra on ${fmtDate(a.set_at)}. Stored as a keyed hash in ${a.file}.`),
        h('button', { type: 'button', onclick: () => tokenModal('Rotate the admin token', render) }, 'Rotate token…'),
        h('p', { class: 'small muted wrap-anywhere' }, `Lost it? Set LISSANDRA_ADMIN_TOKEN, or delete ${a.file}, and restart Lissandra.`));
    } else {
      parts.push(h('p', null, 'Set by LISSANDRA_ADMIN_TOKEN. Change it where it is set (on Nomad: the nomad/jobs/lissandra Variable), then restart.'));
      if (a.stored_ignored) {
        const note = a.stored_error
          ? storedTokenUnusable(a)
          : h('div', { class: 'callout warn' }, `A token set in Lissandra${storedOn(a)} is ignored while the variable is set. It becomes active again if the variable is removed.`);
        parts.push(note, deleteStoredButton(render));
      }
    }
    // A safety net for a token marked saved that wasn't: whoever has this
    // tab open is an admin already.
    const reveal = h('div');
    const showBtn = h('button', {
      type: 'button', class: 'link', 'aria-expanded': 'false',
      onclick: () => {
        const open = !reveal.firstChild;
        showBtn.setAttribute('aria-expanded', String(open));
        if (open) reveal.append(secretBox(() => token).el); else clear(reveal);
      },
    }, 'Show the token this tab is signed in with');
    append(clear(card), h('h2', null, 'Access'), parts, h('div', { class: 'mt' }, showBtn, reveal));
  };
  render();
  return card;
}

// storedOn is " on <date>" for the date an ignored stored token was set,
// or "" when it isn't known.
function storedOn(a) {
  return a.stored_set_at ? ` on ${fmtDate(a.stored_set_at)}` : '';
}

// storedTokenUnusable says why admin-token.json can't be used, and what
// that means while LISSANDRA_ADMIN_TOKEN is set.
function storedTokenUnusable(a) {
  return h('div', { class: 'callout warn' }, `${a.file} can't be used (${a.stored_error}). While it is there, Lissandra won't start without LISSANDRA_ADMIN_TOKEN: delete it.`);
}

// deleteStoredButton deletes admin-token.json, once confirmed, then runs
// after.
function deleteStoredButton(after) {
  return h('button', {
    type: 'button', class: 'danger',
    onclick: (e) => busy(e.currentTarget, async () => {
      if (!(await confirmDialog('Delete the admin token stored in Lissandra? LISSANDRA_ADMIN_TOKEN stays the admin token.'))) return;
      await api('DELETE', '/api/auth/token');
      await load('auth');
      toast('Stored admin token deleted');
      after();
    }),
  }, 'Delete stored token');
}

// tokenModal sets a new admin token from tokenPanel, then runs after.
function tokenModal(title, after) {
  const panel = tokenPanel({ current: token, source: cache.auth.source });
  openModal({
    title, body: panel.el,
    actions: [
      { label: 'Cancel', onClick: (close) => close() },
      {
        label: 'Save token', primary: true,
        onClick: async (close) => {
          if (!(await commitToken(panel))) return false;
          close();
          after();
        },
      },
    ],
  });
}

// setupGuideCard starts the setup wizard again, or resumes it.
function setupGuideCard() {
  const ob = cache.onboarding;
  const running = ob.status === 'pending' || ob.status === 'deferred';
  let text, label;
  if (running) {
    text = `In progress: ${plural((ob.remaining || []).length, 'step')} left.`;
    label = 'Resume setup';
  } else if (ob.status === 'completed' || ob.status === 'dismissed') {
    text = `${ob.status === 'completed' ? 'Finished' : 'Skipped'} on ${fmtDate(ob.finished_at)}.`;
    label = 'Run setup again';
  } else {
    text = 'Not run: this configuration predates it, or came from a seed.';
    label = 'Run setup';
  }
  return h('div', { class: 'card mt' },
    h('h2', null, 'Setup guide'),
    h('p', { class: 'small muted' }, 'Walks through access, sources, classification and alerts. Every step can be skipped.'),
    h('p', null, text),
    h('button', {
      type: 'button',
      onclick: (e) => busy(e.currentTarget, async () => {
        cache.onboarding = await api('POST', '/api/onboarding/start');
        location.hash = `#/setup/${running ? cache.onboarding.resume : 'welcome'}`;
      }),
    }, label));
}

const CATEGORY_DESC_HELP = 'Optional: only to tell similar names apart — plain names work best';

// categoryEditor edits the ordered list of {name, description}. It checks
// rows the way the server does (blank rows are dropped, the same name twice
// is merged unless the descriptions differ, "uncategorized" is reserved),
// and marks the rows a problem is about, found here or by the server.
function categoryEditor(initial) {
  let cats = initial.map((c) => ({ name: c.name, description: c.description || '' }));
  let problem = null; // {text, bad(row)}: shown until the next save or set
  const norm = (c) => ({ name: c.name.trim().toLowerCase(), description: c.description.trim() });
  const list = h('div', { class: 'cat-list' });
  const note = h('div', { class: 'cat-note' });
  const errBox = h('div', { class: 'cat-note' });
  const addBtn = h('button', {
    type: 'button', class: 'small',
    onclick: () => { cats.push({ name: '', description: '' }); render({ index: cats.length - 1, part: 'name' }); },
  }, '+ Add category');

  // focus: {index, part} of the control to focus after re-rendering.
  const render = (focus) => {
    clear(list);
    const controls = [];
    cats.forEach((c, i) => {
      const move = (d, part) => {
        [cats[i], cats[i + d]] = [cats[i + d], cats[i]];
        render({ index: i + d, part });
      };
      const bad = Boolean(problem && problem.bad(c));
      const ctl = {
        name: h('input', { value: c.name, placeholder: 'name', maxlength: 64, autocomplete: 'off', spellcheck: 'false', 'aria-label': `Category ${i + 1} name`, 'aria-invalid': bad ? 'true' : null, oninput: (e) => { c.name = e.target.value; } }),
        desc: h('input', { class: 'cat-desc', value: c.description, title: c.description || CATEGORY_DESC_HELP, placeholder: 'description (optional)', maxlength: 300, 'aria-label': `Category ${i + 1} description (optional)`, oninput: (e) => { c.description = e.target.value; e.target.title = e.target.value || CATEGORY_DESC_HELP; } }),
        up: h('button', { type: 'button', disabled: i === 0, 'aria-label': 'Move up', title: 'Move up', onclick: () => move(-1, i - 1 === 0 ? 'down' : 'up') }, '▲'),
        down: h('button', { type: 'button', disabled: i === cats.length - 1, 'aria-label': 'Move down', title: 'Move down', onclick: () => move(1, i + 1 === cats.length - 1 ? 'up' : 'down') }, '▼'),
        remove: h('button', { type: 'button', class: 'small', 'aria-label': 'Remove category', title: 'Remove', onclick: () => { cats.splice(i, 1); render({ index: Math.min(i, cats.length - 1), part: 'remove' }); } }, '✕'),
      };
      controls.push(ctl);
      list.append(h('div', { class: `cat-row ${bad ? 'invalid' : ''}` }, h('div', { class: 'order-btns' }, ctl.up, ctl.down), ctl.name, ctl.desc, ctl.remove));
    });
    if (!cats.length) list.append(h('p', { class: 'muted small' }, 'No categories: events stay "uncategorized" unless a rule sets one, and the engine only judges severity.'));
    clear(note);
    if (cats.length > MANY_CATEGORIES) {
      note.append(h('div', { class: 'callout warn' }, `${cats.length} categories. The engine is less accurate choosing among many options: consider merging some (more than ${MANY_CATEGORIES} is not recommended).`));
    }
    clear(errBox);
    if (problem) errBox.append(h('div', { class: 'callout bad', role: 'alert' }, problem.text));
    if (focus) {
      const el = focus.index >= 0 && controls[focus.index] ? controls[focus.index][focus.part] : addBtn;
      (el && !el.disabled ? el : addBtn).focus();
    }
  };

  // showError marks the rows text is about: the names it quotes, or the
  // described rows without a name. An empty text clears it.
  const showError = (text) => {
    if (!text) {
      if (problem) { problem = null; render(); }
      return;
    }
    const names = [...text.matchAll(/"([^"]+)"/g)].map((m) => m[1].toLowerCase());
    const bad = /needs a name/.test(text)
      ? (c) => !norm(c).name && Boolean(norm(c).description)
      : (c) => names.includes(norm(c).name);
    problem = { text, bad };
    render();
    errBox.scrollIntoView({ block: 'nearest' });
  };
  const fail = (text) => {
    showError(text);
    throw new InputError(text);
  };
  render();

  return {
    el: h('div', null, list, h('div', { class: 'row' }, addBtn),
      h('div', { class: 'small muted cat-help' }, h('strong', null, 'Descriptions. '), `${CATEGORY_DESC_HELP}.`), note, errBox),
    set: (next) => { cats = next.map((c) => ({ name: c.name, description: c.description || '' })); problem = null; render(); },
    // rows returns the rows as they are, unchecked: a draft to restore.
    rows: () => cats.map((c) => ({ name: c.name, description: c.description })),
    showError,
    // value returns the list to save, or throws (and marks the rows) on one
    // that can't be saved.
    value: () => {
      const out = [];
      for (const c of cats) {
        const { name, description } = norm(c);
        if (!name && !description) continue;
        if (!name) fail(`The category described as "${description.slice(0, 40)}" needs a name.`);
        if (name === 'uncategorized') fail('"uncategorized" is built in: events no category fits get it (the engine is offered it as "none of the above"). Remove that row.');
        const prev = out.find((x) => x.name === name);
        if (prev) {
          // The same category again (names ignore case): merged, unless the
          // two rows describe it differently.
          if (description && prev.description && description !== prev.description) {
            fail(`"${name}" is listed twice with different descriptions: keep one row, or give them different names.`);
          }
          prev.description = prev.description || description;
          continue;
        }
        out.push({ name, description });
      }
      showError('');
      return out;
    },
  };
}

// renderHealth shows GET /api/engine/health: each engine's reachability,
// what it has loaded, and whether every checkpoint in use (inUse: the
// default and per-source ones, from the status) is resident. The server
// reports each instance's missing checkpoints.
function renderHealth(out, r, inUse) {
  clear(out);
  if (!r.endpoints || !r.endpoints.length) {
    out.append(h('div', { class: 'callout bad mt' }, r.error || 'No engine URL is configured.'));
    return;
  }
  const model = r.model || engineSettings().model;
  inUse = inUse && inUse.length ? inUse : [model];
  const many = r.endpoints.length > 1;
  const missing = [...new Set(r.endpoints.flatMap((ep) => ep.missing || []))];
  const them = missing.length > 1 ? 'them' : 'it';
  let summary;
  if (r.ok && !missing.length) {
    summary = h('div', { class: 'callout ok mt' }, `Connected: ${many ? 'every engine has' : 'the engine has'} ${inUse.join(', ')} loaded.`);
  } else if (missing.length && r.endpoints.every((ep) => ep.ok)) {
    summary = h('div', { class: 'callout warn mt' }, `Connected, but ${missing.join(', ')} ${missing.length > 1 ? 'are' : 'is'} not loaded${many ? ' everywhere' : ''}. Lissandra won't ask for ${them} there, so events that need ${them} keep the rules' result: add ${them} to the engine's LAYA_MODELS.`);
  } else {
    summary = h('div', { class: 'callout bad mt' }, 'Not ready: see below.');
  }
  out.append(summary);
  const notLoaded = h('span', null, h('span', { class: 'badge warn' }, 'not loaded'),
    ' Not requested (laya-serve would load it: about 2 GB more memory); add it to the engine\'s LAYA_MODELS.');
  for (const ep of r.endpoints) {
    const kv = h('div', { class: 'kv' });
    const row = (k, v) => kv.append(h('div', null, k), h('div', null, v));
    const epMissing = ep.missing || [];
    if (!ep.ok) {
      row('Error', ep.error || 'unreachable');
    } else {
      row('Loaded', (ep.loaded || []).join(', ') || 'none');
      row('Device', ep.device || '—');
      if (ep.revision) row('Revision', h('span', { class: 'mono', title: ep.revision }, ep.revision.slice(0, 12)));
      for (const m of [...inUse, ...epMissing.filter((x) => !inUse.includes(x))]) {
        row(m === model ? `${m} (default)` : `${m} (a source's)`,
          epMissing.includes(m) ? notLoaded.cloneNode(true) : h('span', { class: 'badge ok' }, 'loaded'));
      }
    }
    const badge = !ep.ok ? h('span', { class: 'badge bad' }, 'error')
      : epMissing.length ? h('span', { class: 'badge warn' }, 'checkpoint missing') : h('span', { class: 'badge ok' }, 'ready');
    out.append(h('div', { class: 'endpoint' },
      h('div', { class: 'row' }, badge,
        h('span', { class: 'mono url' }, ep.url), h('span', { class: 'spacer' }), h('span', { class: 'muted small' }, `${num(ep.latency_ms)} ms`)),
      kv));
  }
}

// ---------- Log files ----------

// Uploads in flight, by import id. They outlive page changes, so the report
// page can show progress and offer "Re-analyze".
const uploads = {};
const uploadQueue = [];
let uploading = false;

function newImportId() {
  return randomHex(8);
}

function queueUpload(file, opts) {
  const id = newImportId();
  uploads[id] = { id, file, opts, state: 'queued', loaded: 0, total: file.size };
  uploadQueue.push(id);
  pumpUploads();
  return id;
}

function pumpUploads() {
  if (uploading || !uploadQueue.length) return;
  const up = uploads[uploadQueue.shift()];
  uploading = true;
  up.state = 'uploading';
  const q = new URLSearchParams({ id: up.id, name: up.file.name });
  for (const k of ['source', 'format', 'tz', 'engine']) if (up.opts[k]) q.set(k, up.opts[k]);
  const xhr = new XMLHttpRequest();
  up.xhr = xhr;
  xhr.open('POST', `/api/imports?${q}`);
  xhr.setRequestHeader('Authorization', `Bearer ${token}`);
  xhr.upload.onprogress = (e) => { up.loaded = e.loaded; up.total = e.total || up.total; };
  xhr.upload.onload = () => { up.state = 'processing'; };
  const done = (state, error) => {
    up.state = state;
    up.error = error;
    up.xhr = null;
    uploading = false;
    pumpUploads();
  };
  xhr.onload = () => {
    if (xhr.status === 401) { showLogin(TOKEN_REJECTED); done('error', 'Sign in required'); return; }
    let data = null;
    try { data = JSON.parse(xhr.responseText); } catch { /* not JSON */ }
    if (xhr.status >= 400 && (!data || !data.status)) done('error', (data && data.error) || `${xhr.status} ${xhr.statusText}`);
    else done('done');
  };
  xhr.onerror = () => done('error', 'The upload failed (connection lost, or the server is busy with another file).');
  const fd = new FormData();
  fd.append('file', up.file, up.file.name);
  xhr.send(fd);
}

function fmtBytes(n) {
  n = Number(n || 0);
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GB`;
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

function fmtSpan(ms) {
  const s = ms / 1000;
  if (s >= 2 * 86400) return `${(s / 86400).toFixed(1)} days`;
  if (s >= 3600) return `${(s / 3600).toFixed(1)} hours`;
  if (s >= 60) return `${Math.round(s / 60)} min`;
  return `${Math.round(s)} s`;
}

function stepLabel(sec) {
  const units = [[86400, 'day'], [3600, 'hour'], [60, 'min']];
  for (const [n, u] of units) {
    if (sec % n === 0) {
      const k = sec / n;
      return k === 1 ? u : `${k} ${u}${u === 'min' ? '' : 's'}`;
    }
  }
  return `${sec}s`;
}

function fmtIn(iso, tz, withDate = true) {
  if (!iso) return '—';
  const opts = withDate
    ? { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' }
    : { hour: '2-digit', minute: '2-digit' };
  try { return new Date(iso).toLocaleString([], { ...opts, timeZone: tz || 'UTC' }); } catch { return fmtTime(iso); }
}

const STOP_REASONS = {
  upload_limit: 'The file is larger than the upload limit, so only its first part was analyzed. Gzip it, or run `lissandra analyze` locally for the whole file.',
  decompressed_limit: 'The decompressed file is larger than the analysis limit; only the first part was analyzed. Use `lissandra analyze` for the whole file.',
  ratio_limit: 'The file decompresses to far more than real logs do, so analysis stopped as a precaution.',
  deadline: 'The analysis took too long and was stopped. Use `lissandra analyze` for very large files.',
  client_aborted: 'The upload was interrupted; results cover the part that arrived.',
  gzip_truncated: 'The gzip data ended early (truncated file?); results cover the part that decompressed.',
  syntax_error: 'The file has malformed data part-way through; results cover everything before it.',
  record_too_large: 'A single record exceeded the per-record limit: a very large JSON value (convert the file to NDJSON, one object per line), or a CSV row with an unbalanced quote.',
  read_error: 'Reading the file failed part-way through.',
};

function timeZones() {
  const browser = Intl.DateTimeFormat().resolvedOptions().timeZone;
  let all = [];
  try { all = Intl.supportedValuesOf('timeZone'); } catch { /* older browsers */ }
  return { browser, all };
}

pages.files = async (main, arg, params) => {
  await load('info', 'sources', 'settings');
  if (arg) return renderImport(main, arg);
  const lim = (cache.info.imports || {});
  const { browser, all } = timeZones();

  const srcSel = select([['', 'Default parsing'], ...cache.sources.map((s) => [s.id, `As source "${s.name}"`])], params.get('source') || '');
  const fmtSel = select([['', 'Auto-detect'], ['text', 'Plain text lines'], ['ndjson', 'NDJSON'], ['json', 'JSON (array or export)'], ['csv', 'CSV with header row']], '');
  const tzIn = h('input', { value: 'UTC', list: 'tz-list', 'aria-label': 'Time zone', placeholder: 'UTC' });
  const tzList = h('datalist', { id: 'tz-list' }, ['UTC', browser, ...all].filter((v, i, a) => v && a.indexOf(v) === i).map((z) => h('option', { value: z })));
  const tzBrowser = browser && browser !== 'UTC'
    ? h('button', { type: 'button', class: 'link small', onclick: () => { tzIn.value = browser; } }, `Use ${browser}`) : null;
  const picker = h('input', { type: 'file', multiple: true, hidden: true });
  const msg = h('div');
  const useEngine = checkbox('Ask the Laya engine about patterns no rule categorizes', false);

  const start = (fileList) => {
    clear(msg);
    const files = Array.from(fileList || []);
    if (!files.length) return;
    const tooBig = files.filter((f) => lim.max_upload_bytes && f.size > lim.max_upload_bytes);
    if (tooBig.length) {
      msg.append(h('div', { class: 'callout bad' }, `${tooBig.map((f) => f.name).join(', ')}: larger than the ${fmtBytes(lim.max_upload_bytes)} upload limit. Gzip the file (logs shrink 5–30×) or run \`lissandra analyze\` locally.`));
    }
    const ok = files.filter((f) => !tooBig.includes(f));
    if (!ok.length) return;
    const opts = { source: srcSel.value, format: fmtSel.value, tz: tzIn.value.trim() || 'UTC', engine: useEngine.input.checked ? '1' : '' };
    const ids = ok.map((f) => queueUpload(f, opts));
    location.hash = `#/files/${ids[0]}`;
  };
  picker.addEventListener('change', () => { start(picker.files); picker.value = ''; });

  const zone = h('div', {
    class: 'dropzone', tabindex: 0, role: 'button', 'aria-label': 'Choose log files to analyze',
    onclick: () => picker.click(),
    onkeydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); picker.click(); } },
    ondragover: (e) => { e.preventDefault(); zone.classList.add('over'); },
    ondragleave: () => zone.classList.remove('over'),
    ondrop: (e) => { e.preventDefault(); zone.classList.remove('over'); start(e.dataTransfer.files); },
  },
  h('div', { class: 'dz-title' }, 'Drop log files here, or click to choose'),
  h('div', { class: 'muted small' }, `Text, JSON, NDJSON or CSV, optionally gzipped · up to ${fmtBytes(lim.max_upload_bytes)} each`));

  const list = h('div', { class: 'card flush mt' });
  append(main,
    pageHead('Log files', 'Parse and classify a log file with your current rules. Results stay here: nothing is added to live events and no alerts are sent.'),
    h('div', { class: 'card' },
      zone, picker, msg,
      h('div', { class: 'fields-3 mt' },
        field('Parse as', srcSel, 'A source applies its parsing settings and tags.'),
        field('Format', fmtSel),
        h('div', { class: 'field' }, h('label', null, 'Time zone'), tzIn, tzList, h('div', { class: 'help' }, 'For timestamps without a zone. ', tzBrowser))),
      engineSettings().enabled ? h('div', { class: 'field' }, useEngine.el,
        h('div', { class: 'help' }, `One request per pattern, at most ${num(lim.engine_calls_per_file || 100)} per file and only while no live event is waiting for the engine, so it is slower. Without it, only answers the engine already gave for live events are used.`)) : null,
      h('p', { class: 'small muted' }, 'Very large files are faster with the command line: ', h('code', null, 'lissandra analyze -config config.json app.log'), '.')),
    list);

  const refresh = async () => {
    const reports = await GET('/api/imports');
    clear(list);
    list.append(h('div', { class: 'list-item' }, h('div', { class: 'main' }, h('h2', null, 'Recent analyses'),
      h('div', { class: 'meta' }, `The last ${lim.keep || 5} are kept in memory and cleared when Lissandra restarts.`))));
    if (!reports.length) { list.append(h('div', { class: 'empty' }, 'No files analyzed yet.')); return; }
    const tbl = h('table', null, h('tr', null, h('th', null, 'File'), h('th', null, 'Status'), h('th', null, 'Events'), h('th', null, 'Errors'), h('th', null, 'Lines'), h('th', null, 'Size'), h('th', null, 'Analyzed')));
    for (const r of reports) {
      const bad = (r.by_severity.error || 0) + (r.by_severity.critical || 0);
      tbl.append(h('tr', { class: 'clickable', onclick: () => { location.hash = `#/files/${encodeURIComponent(r.id)}`; } },
        h('td', { class: 'fname', title: r.file_name }, r.file_name, h('div', { class: 'muted small' }, r.source_name)),
        h('td', null, importBadge(r)),
        h('td', null, num(r.events)),
        h('td', null, bad ? h('span', { class: 'badge sev sev-error' }, num(bad)) : '0'),
        h('td', null, num(r.lines)),
        h('td', null, fmtBytes(r.bytes)),
        h('td', null, timeCell(r.started_at))));
    }
    list.append(tbl);
  };
  await refresh();
  return every(4000, refresh);
};

function importBadge(r) {
  if (r.status === 'running') return h('span', { class: 'badge warn' }, 'analyzing…');
  if (r.status === 'failed') return h('span', { class: 'badge bad' }, 'failed');
  if (r.truncated) return h('span', { class: 'badge warn' }, 'partial');
  return h('span', { class: 'badge ok' }, 'done');
}

async function renderImport(main, id) {
  const root = h('div');
  main.append(h('div', { class: 'row small' }, h('a', { href: '#/files' }, '← Log files')), root);
  const filter = { min_severity: '', category: '', q: '', fingerprint: '' };
  let rendered = false;
  let stopped = false;

  const tick = async () => {
    if (stopped) return;
    let rep = null;
    try {
      rep = await GET(`/api/imports/${encodeURIComponent(id)}`);
    } catch (err) {
      if (err instanceof AuthError) return;
      const up = uploads[id];
      if (up && (up.state === 'queued' || up.state === 'uploading' || up.state === 'processing')) {
        renderPending(root, up);
        return;
      }
      clear(root).append(h('div', { class: 'callout bad mt' }, up && up.error ? up.error : err.message));
      stopped = true;
      return;
    }
    if (rep.status === 'running') {
      renderProgress(root, rep, uploads[id]);
      return;
    }
    if (!rendered) {
      rendered = true;
      stopped = true;
      renderReport(root, rep, filter);
    }
  };
  await tick();
  const stop = every(1500, tick);
  return () => { stopped = true; stop(); };
}

function progressBar(frac) {
  const fill = h('div', { class: 'progress-fill' });
  fill.style.width = `${Math.max(2, Math.min(100, frac * 100))}%`;
  return h('div', { class: 'progress' }, fill);
}

function renderPending(root, up) {
  const frac = up.total ? up.loaded / up.total : 0;
  clear(root).append(h('div', { class: 'card mt' },
    h('h2', null, up.file.name),
    up.state === 'queued' ? h('p', { class: 'muted' }, 'Waiting for the previous file to finish…') : [
      h('p', { class: 'muted' }, `Uploading ${fmtBytes(up.loaded)} of ${fmtBytes(up.total)}`),
      progressBar(frac)]));
}

function renderProgress(root, rep, up) {
  const parts = [];
  if (up && up.state === 'uploading' && up.total) {
    parts.push(h('p', { class: 'muted' }, `Uploaded ${fmtBytes(up.loaded)} of ${fmtBytes(up.total)} · analyzed ${num(rep.lines)} lines, ${num(rep.events)} events so far`));
    parts.push(progressBar(up.loaded / up.total));
  } else {
    parts.push(h('p', { class: 'muted' }, `Analyzing… ${num(rep.lines)} lines, ${num(rep.events)} events, ${fmtBytes(rep.bytes)} read`));
    parts.push(h('div', { class: 'progress indeterminate' }, h('div', { class: 'progress-fill' })));
  }
  if (rep.yielded_ms > 1000) parts.push(h('p', { class: 'small muted' }, `Paused ${fmtSpan(rep.yielded_ms)} so live ingest could keep up.`));
  clear(root).append(h('div', { class: 'card mt' }, h('h2', null, rep.file_name), parts));
}

function renderReport(root, rep, filter) {
  clear(root);
  const tz = rep.time_zone || 'UTC';
  const up = uploads[rep.id];
  const bySev = rep.by_severity || {};
  const bad = (bySev.error || 0) + (bySev.critical || 0);

  const downloadReport = () => {
    const blob = new Blob([JSON.stringify(rep, null, 2)], { type: 'application/json' });
    saveBlob(blob, `lissandra-${rep.id}-report.json`);
  };
  const downloadEvents = () => {
    const p = new URLSearchParams({ format: 'ndjson' });
    for (const [k, v] of Object.entries(filter)) if (v) p.set(k, v);
    return download(`/api/imports/${encodeURIComponent(rep.id)}/events?${p}`, `lissandra-${rep.id}-events.ndjson`);
  };
  const downloadTraining = () => download(`/api/imports/${encodeURIComponent(rep.id)}/training-data`, `lissandra-${rep.id}-training.jsonl`,
    'No training data: no event kept from this file was categorized by a rule.');
  const actions = [
    h('button', { type: 'button', onclick: downloadReport }, 'Download report'),
    rep.retained ? h('button', { type: 'button', onclick: () => downloadEvents().catch(reportError) }, 'Download events') : null,
    rep.retained ? h('button', {
      type: 'button',
      title: 'laya-train JSONL: one record per message pattern a rule categorized in the events kept from this file, with the question Lissandra asks the engine. For fine-tuning a checkpoint.',
      onclick: (e) => {
        const btn = e.currentTarget;
        btn.disabled = true;
        downloadTraining().catch(reportError).finally(() => { btn.disabled = false; });
      },
    }, 'Training data (JSONL)') : null,
    up && up.file ? h('button', { type: 'button', onclick: () => { location.hash = `#/files/${queueUpload(up.file, up.opts)}`; } }, 'Re-analyze with current rules') : null,
    h('button', {
      type: 'button', class: 'danger',
      onclick: async () => {
        if (!(await confirmDialog(`Delete the analysis of "${rep.file_name}"?`))) return;
        await api('DELETE', `/api/imports/${encodeURIComponent(rep.id)}`).catch(reportError);
        location.hash = '#/files';
      },
    }, 'Delete'),
  ];
  const describe = [rep.parse.mode || '—', rep.compressed ? 'gzip' : null, rep.parse.encoding && rep.parse.encoding !== 'utf-8' ? rep.parse.encoding : null].filter(Boolean).join(', ');
  append(root, pageHead(rep.file_name, `${rep.source_name} · parsed as ${describe} · ${fmtBytes(rep.bytes)} · analyzed ${fmtTime(rep.started_at)}`, actions));

  if (rep.status === 'failed') root.append(h('div', { class: 'callout bad' }, rep.error || 'The analysis failed.'));
  else if (rep.truncated) root.append(h('div', { class: 'callout bad' }, STOP_REASONS[rep.stop_reason] || rep.error, rep.error && STOP_REASONS[rep.stop_reason] ? h('div', { class: 'small muted' }, rep.error) : null));
  if (rep.events_evicted) root.append(h('div', { class: 'callout' }, 'Browsable events for this file were freed to make room for newer analyses; the summary below is complete.'));

  const ts = rep.parse;
  const kpi = (label, value, sub, alert) => h('div', { class: `card kpi ${alert ? 'alert' : ''}` }, h('div', { class: 'label' }, label), h('div', { class: 'value' }, value), h('div', { class: 'sub' }, sub));
  root.append(h('div', { class: 'kpis' },
    kpi('Lines', num(rep.lines), `${num(ts.folded_lines || 0)} folded into multi-line records`),
    kpi('Events', num(rep.events), `${num(rep.dropped)} dropped by rules`),
    kpi('Errors + critical', num(bad), `${num(bySev.critical || 0)} critical`, bad > 0),
    kpi('Time span', rep.first_timestamp ? fmtSpan(new Date(rep.last_timestamp) - new Date(rep.first_timestamp)) : '—',
      rep.first_timestamp ? `${fmtIn(rep.first_timestamp, tz)} → ${fmtIn(rep.last_timestamp, tz)} (${tz})` : 'no timestamps recognised'),
    kpi('Patterns', num(rep.distinct_patterns), rep.other_pattern_events ? `+${num(rep.other_pattern_events)} events in rare patterns` : 'distinct messages'),
    rep.engine_events ? kpi('Laya engine', num(rep.engine_events), `events answered · ${num(rep.engine_applied)} applied`) : null));

  const tl = rep.timeline;
  if (tl && tl.points && tl.points.length) {
    const outside = (tl.before || 0) + (tl.after || 0);
    root.append(h('div', { class: 'card' },
      h('h2', null, 'Events over log time'),
      timelineChart(tl.points, {
        left: fmtIn(tl.start, tz), mid: '', right: fmtIn(tl.end, tz),
        unit: stepLabel(tl.step_seconds), tip: (t) => fmtIn(t, tz),
      }),
      outside ? h('p', { class: 'small muted' }, `${num(outside)} events with outlying timestamps are not charted (${num(tl.before || 0)} earlier, ${num(tl.after || 0)} later).`) : null,
      h('p', { class: 'small muted' }, `${num(ts.timestamps_parsed)} records had a timestamp, ${num(ts.timestamps_inferred)} took the previous record's, ${num(ts.timestamps_missing)} had none. Times without a zone were read as ${tz}.`)));
  }

  const bars = (title, obj, onPick) => {
    const entries = Object.entries(obj || {}).sort((a, b) => b[1] - a[1]).slice(0, 12);
    const max = Math.max(1, ...entries.map((e) => e[1]));
    const card = h('div', { class: 'card' }, h('h2', null, title));
    if (!entries.length) card.append(h('div', { class: 'muted' }, 'None.'));
    for (const [k, v] of entries) {
      const fill = h('div', { class: 'fill' });
      fill.style.width = `${(v / max) * 100}%`;
      card.append(h('div', { class: 'hbar' }, onPick ? h('button', { type: 'button', class: 'link name', onclick: () => onPick(k) }, k) : h('span', { class: 'name' }, k), h('div', { class: 'track' }, fill), h('span', { class: 'n' }, num(v))));
    }
    return card;
  };
  const routesCard = h('div', { class: 'card' }, h('h2', null, 'Alert routes that would have fired'));
  if (!rep.routes || !rep.routes.length) {
    routesCard.append(h('p', { class: 'muted' }, 'None of your alert routes match events in this file.'));
  } else {
    const t = h('table', null, h('tr', null, h('th', null, 'Route'), h('th', null, 'Events'), h('th', null, 'Distinct problems')));
    for (const r of rep.routes) t.append(h('tr', null, h('td', null, r.name), h('td', null, num(r.events)), h('td', null, num(r.patterns))));
    routesCard.append(t, h('p', { class: 'small muted' }, 'Nothing was sent. With throttling on, a route alerts about once per distinct problem.'));
  }

  const eventsCard = h('div', { class: 'card flush mt' });
  const setFilter = (k, v) => { Object.assign(filter, { [k]: v }); loadEvents(eventsCard, rep, filter, setFilter); eventsCard.scrollIntoView({ behavior: 'smooth', block: 'start' }); };

  root.append(h('div', { class: 'grid cols-2 mt' },
    bars('By category', rep.by_category, (k) => setFilter('category', k)),
    bars('Rules matched', rep.by_rule)),
  h('div', { class: 'mt' }, routesCard));

  root.append(patternsCard(rep, setFilter));
  root.append(eventsCard);
  loadEvents(eventsCard, rep, filter, setFilter);

  const diag = h('div', { class: 'kv' });
  const drow = (k, v) => { if (v !== undefined && v !== null && v !== '' && v !== 0) diag.append(h('div', null, k), h('div', null, String(v))); };
  drow('Mode', ts.mode); drow('Encoding', ts.encoding); drow('Compressed', rep.compressed ? 'gzip' : 'no');
  drow('Long lines cut', ts.long_lines); drow('Continuation lines dropped', ts.dropped_continuation_lines); drow('Malformed JSON lines', ts.json_line_errors);
  drow('Kept to browse', `${num(rep.retained)} events (${fmtBytes(rep.retained_bytes)})`);
  if (rep.yielded_ms) drow('Paused for live ingest', fmtSpan(rep.yielded_ms));
  root.append(h('div', { class: 'card mt' }, h('h2', null, 'How the file was read'), diag,
    h('p', { class: 'small muted' }, 'This analysis lives in memory and is cleared when Lissandra restarts.')));
}

function saveBlob(blob, name) {
  const a = h('a', { href: URL.createObjectURL(blob), download: name });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

function patternsCard(rep, setFilter) {
  const card = h('div', { class: 'card flush mt' });
  const body = h('div');
  let tab = 'top';
  const tabs = h('div', { class: 'row' });
  const renderTabs = () => {
    clear(tabs).append(
      h('button', { type: 'button', class: `small ${tab === 'top' ? 'primary' : ''}`, onclick: () => { tab = 'top'; renderTabs(); show(); } }, 'Top issues'),
      h('button', { type: 'button', class: `small ${tab === 'uncat' ? 'primary' : ''}`, onclick: () => { tab = 'uncat'; renderTabs(); show(); } }, 'Uncategorized patterns'));
  };
  const table = (patterns, withRule) => {
    if (!patterns.length) return h('div', { class: 'empty' }, tab === 'uncat' ? 'Every pattern was categorized by a rule.' : 'No events.');
    const t = h('table', null, h('tr', null, h('th', null, 'Severity'), h('th', null, 'Count'), h('th', null, 'Pattern'), h('th', null, 'Category'), h('th', null, 'First line'), withRule ? h('th', null, '') : null));
    for (const p of patterns) {
      t.append(h('tr', { class: 'clickable', onclick: () => setFilter('fingerprint', p.fingerprint) },
        h('td', null, sevBadge(p.severity)),
        h('td', null, num(p.count)),
        h('td', { class: 'msg', title: p.sample }, p.sample),
        h('td', { class: 'nowrap' }, p.category),
        h('td', null, p.first_line || '—'),
        withRule ? h('td', null, h('button', {
          type: 'button', class: 'small',
          onclick: (e) => { e.stopPropagation(); editRule(null, { message: p.sample }); },
        }, 'Create rule…')) : null));
    }
    return t;
  };
  const show = async () => {
    if (tab === 'top') { clear(body).append(table(rep.top_patterns || [], false)); return; }
    clear(body).append(h('div', { class: 'empty' }, 'Loading…'));
    const pats = await GET(`/api/imports/${encodeURIComponent(rep.id)}/patterns?category=uncategorized&limit=100`).catch((e) => { reportError(e); return []; });
    clear(body).append(table(pats, true));
  };
  renderTabs();
  show();
  card.append(h('div', { class: 'list-item' }, h('div', { class: 'main' }, h('h2', null, 'Patterns'),
    h('div', { class: 'meta' }, 'Same message shape, numbers and ids ignored. Click one to see its events.')), tabs), body);
  return card;
}

async function loadEvents(card, rep, filter, setFilter) {
  clear(card);
  const sevSel = select([['', 'Any severity'], ...SEVERITIES.map((x) => [x, `≥ ${x}`])], filter.min_severity);
  const catSel = select([['', 'Any category'], ...Object.keys(rep.by_category || {}).sort().map((x) => [x, x])], filter.category);
  const q = h('input', { type: 'search', placeholder: 'Search messages…', value: filter.q });
  const chips = h('span');
  if (filter.fingerprint) {
    chips.append(h('span', { class: 'badge' }, `pattern ${filter.fingerprint} `,
      h('button', { type: 'button', class: 'link', 'aria-label': 'Clear pattern filter', onclick: () => setFilter('fingerprint', '') }, '×')));
  }
  sevSel.addEventListener('change', () => setFilter('min_severity', sevSel.value));
  catSel.addEventListener('change', () => setFilter('category', catSel.value));
  let qTimer;
  q.addEventListener('input', () => { clearTimeout(qTimer); qTimer = setTimeout(() => setFilter('q', q.value.trim()), 350); });
  const tableWrap = h('div');
  const more = h('button', { type: 'button' }, 'Load more');
  card.append(
    h('div', { class: 'list-item' }, h('div', { class: 'main' }, h('h2', null, 'Events'),
      h('div', { class: 'meta' }, rep.retained < rep.events
        ? `${num(rep.retained)} of ${num(rep.events)} events were kept to browse: the most severe ones, plus a few examples of every pattern.`
        : 'All events are available to browse.'))),
    h('div', { class: 'row pad' }, sevSel, catSel, q, chips),
    tableWrap,
    h('div', { class: 'row pad' }, more));
  if (!rep.retained) {
    clear(tableWrap).append(h('div', { class: 'empty' }, rep.events_evicted ? 'Events were freed to save memory.' : 'No events to show.'));
    more.hidden = true;
    return;
  }
  let offset = 0;
  const tz = rep.time_zone || 'UTC';
  const tbl = h('table', null, h('tr', null, h('th', null, 'Line'), h('th', null, 'Time'), h('th', null, 'Severity'), h('th', null, 'Category'), h('th', null, 'Message')));
  const page = async () => {
    const p = new URLSearchParams({ offset: String(offset), limit: '100' });
    for (const [k, v] of Object.entries(filter)) if (v) p.set(k, v);
    const r = await GET(`/api/imports/${encodeURIComponent(rep.id)}/events?${p}`);
    if (offset === 0) {
      clear(tableWrap);
      if (!r.events.length) {
        const pat = r.pattern;
        tableWrap.append(h('div', { class: 'empty' }, pat && pat.count
          ? `This pattern occurred ${num(pat.count)} times but none were kept to browse. First seen at line ${pat.first_line || '?'}: ${pat.sample}`
          : 'No matching events.'));
      } else {
        tableWrap.append(tbl);
      }
    }
    for (const ev of r.events) {
      const when = ev.time_source === 'none' ? '—' : `${ev.time_source === 'inferred' ? '≈ ' : ''}${fmtIn(ev.timestamp, tz)}`;
      tbl.append(h('tr', { class: 'clickable', onclick: () => showEvent({ importId: rep.id, id: ev.id }, { onFingerprint: (fp) => setFilter('fingerprint', fp) }) },
        h('td', { class: 'muted' }, ev.line || ''),
        h('td', { class: 'nowrap small', title: ev.time_source === 'inferred' ? 'No timestamp on this record; taken from the previous one' : '' }, when),
        h('td', null, sevBadge(ev.severity)),
        h('td', { class: 'nowrap' }, ev.category),
        h('td', { class: 'msg', title: ev.message }, ev.message, ' ', tags(ev.tags))));
    }
    offset += r.events.length;
    more.hidden = offset >= r.total;
    more.textContent = `Load more (${num(r.total - offset)} left)`;
  };
  more.onclick = () => page().catch(reportError);
  await page().catch(reportError);
}

// ---------- Admin token ----------

// checkTokenLocally applies the server's rules for a new admin token and
// returns what is wrong, or ''. The rules catch mistakes: a token's
// strength comes from it being random.
function checkTokenLocally(t) {
  if (!/^[\x21-\x7e]*$/.test(t)) return 'Use printable ASCII characters only: no spaces or accented letters.';
  if (t.length < 32) return `Use at least 32 characters (this has ${t.length}).`;
  if (t.length > 256) return 'Use at most 256 characters.';
  const counts = {};
  for (const ch of t) counts[ch] = (counts[ch] || 0) + 1;
  if (Object.keys(counts).length < 10) return 'Use at least 10 different characters.';
  if (Math.max(...Object.values(counts)) * 4 > t.length) return 'No single character may make up more than a quarter of the token.';
  return '';
}

// newAdminToken returns 24 random bytes as hex, the format of the tokens
// Lissandra generates. The rare draw that fails the variety rules is redrawn.
function newAdminToken() {
  let t;
  do { t = randomHex(24); } while (checkTokenLocally(t));
  return t;
}

// secretBox shows value() masked, with Show and Copy, and any extra
// buttons. The box is a status region: the token is announced when shown.
function secretBox(value, ...extra) {
  let shown = false;
  const box = h('code', { class: 'token-box', role: 'status' });
  const toggle = h('button', { type: 'button', class: 'small', onclick: () => { shown = !shown; paint(); } });
  const paint = () => {
    box.textContent = shown ? value() : '•'.repeat(24);
    toggle.textContent = shown ? 'Hide' : 'Show';
  };
  paint();
  return {
    el: h('div', { class: 'row token-row' }, box, toggle, h('button', { type: 'button', class: 'small', onclick: () => copyText(value()) }, 'Copy'), extra),
    paint,
  };
}

// tokenPanel picks a new admin token: one generated here (the default) or
// the user's own. Nothing is sent until the user has seen the token and
// ticked that they saved it. draft holds the panel's state, so the setup
// wizard can move between pages without replacing a token the user may
// have copied. current is the token in use, from source.
function tokenPanel({ current = '', source = '', draft = {}, onChange } = {}) {
  if (!draft.generated) draft.generated = newAdminToken();
  if (!draft.mode) draft.mode = 'generate';
  const changed = () => { if (onChange) onChange(); };
  const errBox = h('div');
  const own = secretInput(draft.own || '', { placeholder: 'at least 32 characters' });
  const repeat = secretInput(draft.repeat || '', {});
  const ack = checkbox('I have saved this token somewhere safe, such as a password manager.', draft.acked, (v) => { draft.acked = v; clearError(); changed(); });
  ack.input.required = true;
  const setAck = (v) => { draft.acked = v; ack.input.checked = v; };
  const clearError = () => {
    clear(errBox);
    for (const el of [own, repeat, ack.input]) el.removeAttribute('aria-invalid');
  };
  // The tick is for the token as it is: an edit clears it.
  for (const [input, key] of [[own, 'own'], [repeat, 'repeat']]) {
    input.setAttribute('autocomplete', 'new-password');
    input.addEventListener('input', () => { draft[key] = input.value; setAck(false); clearError(); changed(); });
  }

  const gen = secretBox(() => draft.generated,
    h('button', { type: 'button', class: 'small', onclick: () => saveBlob(new Blob([`${draft.generated}\n`], { type: 'text/plain' }), 'lissandra-admin-token.txt') }, 'Download'),
    h('button', { type: 'button', class: 'small', onclick: () => { draft.generated = newAdminToken(); setAck(false); clearError(); gen.paint(); changed(); } }, 'New token'));
  const genPart = h('div', { class: 'token-part' }, gen.el,
    h('div', { class: 'help' }, 'If you download it, delete the file once the token is in your password manager.'));
  const ownPart = h('div', { class: 'token-part' },
    field('Token', own, ['Use a random value from a password manager\'s generator, or ', h('code', null, 'openssl rand -hex 24'),
      '. Don\'t use a password you use elsewhere: Lissandra stores a fast keyed hash meant for random tokens.']),
    field('Repeat it', repeat));
  const group = `token-mode-${++fieldSeq}`;
  const radios = {};
  const radio = (value, label) => {
    radios[value] = h('input', {
      type: 'radio', name: group, value, checked: draft.mode === value,
      onchange: () => { draft.mode = value; setAck(false); clearError(); toggle(); changed(); },
    });
    return h('label', { class: 'check' }, radios[value], label);
  };
  const toggle = () => {
    genPart.hidden = draft.mode !== 'generate';
    ownPart.hidden = draft.mode !== 'own';
  };
  const el = h('div', { class: 'token-panel' },
    h('fieldset', null, h('legend', null, 'New admin token'),
      radio('generate', 'Generate a token (recommended)'), genPart,
      radio('own', 'Use my own token'), ownPart),
    ack.el,
    h('p', { class: 'small muted' }, `This tab stays signed in. Other tabs and API scripts will need the new token, and ${source === 'generated' ? 'the token in Lissandra\'s log' : 'the current one'} stops working.`),
    errBox);
  toggle();

  // showError shows text in the panel, on input when given.
  const showError = (text, input) => {
    if (input) fieldError(errBox, input, text);
    else clear(errBox).append(h('div', { class: 'callout bad', role: 'alert' }, text));
  };
  const fail = (text, input) => {
    showError(text, input);
    throw new InputError(text);
  };
  return {
    el,
    mode: () => draft.mode,
    generated: () => draft.generated,
    // value returns the token to save, or shows what is wrong and throws.
    value: () => {
      clearError();
      if (draft.mode === 'generate') return draft.generated;
      const t = own.value;
      if (!t) fail('Enter your token, or choose "Generate a token".', own);
      if (t === current) {
        fail(source === 'generated' ? 'That\'s the token you signed in with, which is in Lissandra\'s log. Use a new one.' : 'That\'s the token in use now. Use a new one.', own);
      }
      const problem = checkTokenLocally(t);
      if (problem) fail(problem, own);
      if (repeat.value !== t) fail('The two tokens don\'t match.', repeat);
      return t;
    },
    acked: () => Boolean(draft.acked),
    requireAck: () => { if (!draft.acked) fail('Tick "I have saved this token somewhere safe" once it is.', ack.input); },
    dirty: () => draft.mode === 'own' && Boolean(own.value || repeat.value),
    reset: () => {
      draft.mode = 'generate';
      radios.generate.checked = true;
      own.value = '';
      repeat.value = '';
      draft.own = '';
      draft.repeat = '';
      setAck(false);
      clearError();
      toggle();
      changed();
    },
    showError,
  };
}

// commitToken saves the panel's token (with generatedOnly, its generated
// one) as the admin token, showing any problem in the panel. It returns
// whether the token was changed.
async function commitToken(panel, { generatedOnly } = {}) {
  let next;
  try {
    panel.requireAck();
    next = generatedOnly ? panel.generated() : panel.value();
  } catch (err) {
    if (err instanceof InputError) return false;
    throw err;
  }
  try {
    await changeAdminToken(next);
  } catch (err) {
    if (err instanceof AuthError) throw err;
    panel.showError(err.message);
    return false;
  }
  return true;
}

const sleep = (ms) => new Promise((resolve) => { setTimeout(resolve, ms); });

// probeToken returns the status of GET /api/auth signed in with t, or 0
// when Lissandra can't be reached.
async function probeToken(t) {
  try {
    return (await fetch('/api/auth', { headers: { Authorization: `Bearer ${t}` } })).status;
  } catch {
    return 0;
  }
}

// changeAdminToken makes next the admin token. The PUT is a plain fetch:
// api() would wait on tokenChange, which waits on this request. It throws,
// with a message for the panel, when the token was not changed or it is
// not known whether it was.
async function changeAdminToken(next) {
  const sent = token;
  const wasGenerated = cache.auth && cache.auth.source === 'generated';
  let settle;
  tokenChange = new Promise((resolve) => { settle = resolve; });
  try {
    let res = null;
    try {
      res = await fetch('/api/auth/token', {
        method: 'PUT',
        headers: { Authorization: `Bearer ${sent}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: next }),
      });
    } catch {
      await settleChange(sent, next);
    }
    if (res && !res.ok) {
      if (res.status === 401) {
        showLogin(TOKEN_CHANGED_ELSEWHERE);
        throw new AuthError('Sign in required');
      }
      const data = await res.json().catch(() => null);
      throw new Error((data && data.error) || `${res.status} ${res.statusText}`);
    }
    setToken(next);
  } finally {
    tokenChange = null;
    settle();
  }
  // The token is changed whatever this refresh does.
  await load('auth', 'info').catch(reportError);
  toast(`Admin token saved. This tab stays signed in. Other tabs and API scripts need the new token, and ${wasGenerated ? 'the token in Lissandra\'s log' : 'the old one'} no longer works.`);
}

// settleChange finds out whether a token change that got no answer was
// applied: it returns once Lissandra accepts next, and throws while it still
// accepts sent, or after 30 seconds without reaching it. The new token is
// never dropped while that is unknown.
async function settleChange(sent, next) {
  const until = Date.now() + 30000;
  while (Date.now() < until) {
    const withNext = await probeToken(next);
    if (withNext === 200) return;
    const withSent = await probeToken(sent);
    if (withSent === 200) throw new Error('Not changed; try again.');
    if (withNext === 401 && withSent === 401) {
      showLogin(TOKEN_CHANGED_ELSEWHERE);
      throw new AuthError('Sign in required');
    }
    await sleep(3000);
  }
  throw new Error('Lissandra couldn\'t be reached. If signing in with your old token fails later, use this one.');
}

// ---------- Setup wizard ----------

// The wizard's pages, in order. The five tracked steps record an outcome
// (done, defaults or skipped); welcome and review don't. summary is a
// step's line on the welcome page: for access, it depends on the token in
// use (a, the /api/auth status).
const SETUP_PAGES = [
  { id: 'welcome', title: 'Welcome to Lissandra', label: 'Welcome', render: setupWelcome },
  {
    id: 'access', step: true, title: 'Secure access to Lissandra', label: 'Access', render: setupAccess,
    summary: (a) => {
      if (a.source === 'generated') return 'Replace the token Lissandra generated at startup with a permanent one.';
      if (a.source === 'stored') return 'A permanent token is set in Lissandra; you can rotate it.';
      return a.stored_error ? 'Set by LISSANDRA_ADMIN_TOKEN. Delete admin-token.json, which Lissandra can\'t use.' : 'Set by LISSANDRA_ADMIN_TOKEN; nothing to change here.';
    },
  },
  {
    id: 'sources', step: true, title: 'Where logs come from', label: 'Sources', render: setupSources,
    summary: 'See where to send logs, and check that they arrive.',
  },
  {
    id: 'classify', step: true, title: 'How logs are classified', label: 'Classification', render: setupClassify,
    summary: 'Choose the categories, and the rules that set them.',
  },
  {
    id: 'channels', step: true, title: 'Alert channels', label: 'Channels', render: setupChannels,
    summary: 'Add email, Slack, PagerDuty or another channel to alert through.',
  },
  {
    id: 'notify', step: true, title: 'Who gets notified', label: 'Who gets notified', render: setupNotify,
    summary: 'Pick who receives each channel\'s alerts and which alerts they get, and add teams to alert about their own sources.',
  },
  { id: 'review', title: 'Review your setup', label: 'Review', render: setupReview },
];
const SETUP_STEPS = SETUP_PAGES.filter((p) => p.step);
const setupPage = (id) => SETUP_PAGES.find((p) => p.id === id);

// Unsaved edits per page, kept while the user moves between pages and
// dropped once the step is recorded. Only in memory: they can hold
// recipients and tokens.
const setupDrafts = {};

// clearSetupDrafts drops the drafts, except keep's (a page id).
function clearSetupDrafts(keep) {
  for (const k of Object.keys(setupDrafts)) if (k !== keep) delete setupDrafts[k];
}

const STEP_STATES = {
  done: { label: 'Done', cls: 'ok', sr: 'done' },
  defaults: { label: 'Defaults', cls: 'ok', sr: 'defaults used' },
  skipped: { label: 'Skipped', cls: 'warn', sr: 'skipped' },
  todo: { label: 'Not yet', cls: '', sr: 'not done yet' },
  na: { label: 'Not needed', cls: '', sr: 'not needed' },
};

// stepState is a step's outcome, else todo, or na while it doesn't apply
// (there is no channel to notify through).
function stepState(state, id) {
  if ((state.not_needed || []).includes(id)) return 'na';
  const s = (state.steps || {})[id];
  return s && STEP_STATES[s.outcome] ? s.outcome : 'todo';
}

function stepBadge(state, id) {
  const st = STEP_STATES[stepState(state, id)];
  return h('span', { class: `badge ${st.cls}` }, st.label);
}

function nextPage(id, state) {
  if (id === 'channels') return (state.not_needed || []).includes('notify') ? 'review' : 'notify';
  const i = SETUP_PAGES.findIndex((p) => p.id === id);
  return SETUP_PAGES[Math.min(i + 1, SETUP_PAGES.length - 1)].id;
}

function prevPage(id, state) {
  if (id === 'review') return (state.not_needed || []).includes('notify') ? 'channels' : 'notify';
  const i = SETUP_PAGES.findIndex((p) => p.id === id);
  return SETUP_PAGES[Math.max(i - 1, 0)].id;
}

// goTo renders hash, also when it is the current one.
function goTo(hash) {
  if (location.hash === hash) route(); else location.hash = hash;
}

// setupApi sends a setup request. When setup was finished, skipped or
// deferred in another session in a way that stops this one, it says so
// and goes to the dashboard.
async function setupApi(method, path, body) {
  try {
    return await api(method, path, body);
  } catch (err) {
    if (err.status !== 409 || err.code !== 'setup_not_running') throw err;
    clearSetupDrafts();
    toast('Setup was finished or skipped in another session');
    goTo('#/dashboard');
    throw new SetupStopped(err.message);
  }
}

// markStep records a step's outcome ({state, changes}), or with dryRun
// asks what "Use defaults" would add ({changes}).
async function markStep(step, outcome, { dryRun } = {}) {
  const r = await setupApi('POST', `/api/onboarding/steps/${step}`, dryRun ? { outcome, dry_run: true } : { outcome });
  if (r.state) cache.onboarding = r.state;
  return r;
}

async function finishLater() {
  await setupApi('POST', '/api/onboarding/defer');
  clearSetupDrafts();
  location.hash = '#/dashboard';
}

async function skipSetup() {
  if (!(await confirmDialog('Skip setup? Lissandra keeps its current configuration. You can run setup again from Settings.', 'Skip setup'))) return;
  await setupApi('POST', '/api/onboarding/dismiss');
  clearSetupDrafts();
  location.hash = '#/dashboard';
}

const CHANGE_KINDS = { source: 'sources', category: 'categories', route: 'routes' };

// changeSummary words what defaults add: 'source "default"', or
// '2 categories and route "Ops: error and above"'.
function changeSummary(changes) {
  const parts = [...new Set(changes.map((c) => c.kind))].map((k) => {
    const of = changes.filter((c) => c.kind === k);
    return of.length === 1 ? `${k} "${of[0].name}"` : `${num(of.length)} ${CHANGE_KINDS[k] || `${k}s`}`;
  });
  return parts.length > 1 ? `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}` : parts[0];
}

// defaultsLabel is the effect a defaults button names: "adds 2
// categories", or "no changes".
function defaultsLabel(changes) {
  return changes && changes.length ? `adds ${changeSummary(changes)}` : 'no changes';
}

// changeList names every change, for a confirmation.
function changeList(changes) {
  return changes.map((c) => `${c.kind} "${c.name}"`).join(', ');
}

pages.setup = async (main, arg) => {
  // A render the user has moved on from by the time it loads changes
  // nothing: not the title, not the URL.
  const gen = routeGen;
  await load('onboarding', 'auth', 'info');
  if (gen !== routeGen) return null;
  const state = cache.onboarding;
  if (state.status !== 'pending' && state.status !== 'deferred') {
    setupNotRunning(main, state);
    return null;
  }
  let page = setupPage(arg);
  if (!page) {
    page = setupPage(state.resume) || SETUP_PAGES[0];
    history.replaceState(null, '', `#/setup/${page.id}`);
  }
  const n = SETUP_STEPS.indexOf(page) + 1;
  document.title = `Set up Lissandra · ${n ? `Step ${n} of ${SETUP_STEPS.length}: ${page.label}` : page.label}`;
  const ctx = {
    state, page,
    drafts: setupDrafts[page.id] || (setupDrafts[page.id] = {}),
    // An action that finishes after the user left the page leaves them
    // where they went.
    go: (id) => { if (gen === routeGen) goTo(`#/setup/${id}`); },
    mark: (outcome) => markStep(page.id, outcome),
    refresh: () => route(),
  };
  const view = await page.render(ctx);
  main.append(setupShell(state, page, view));
  return view.cleanup || null;
};

// setupNotRunning is the setup page once setup is over, or was never
// offered, with a way to run it again. Drafts of a run that ended in
// another session don't carry over into the next one.
function setupNotRunning(main, state) {
  clearSetupDrafts();
  let when = 'never offered: this configuration predates it, or came from a seed';
  if (state.status === 'completed') when = `finished on ${fmtDate(state.finished_at)}`;
  else if (state.status === 'dismissed') when = `skipped on ${fmtDate(state.finished_at)}`;
  const title = h('h1', { id: 'setup-title', tabindex: -1 }, 'Setup isn\'t running');
  append(main, h('div', { class: 'setup-head' }, brandMark()),
    h('section', { class: 'card', 'aria-labelledby': 'setup-title' }, title,
      h('p', null, `Setup was ${when}.`),
      h('div', { class: 'row' },
        h('button', {
          type: 'button', class: 'primary',
          onclick: (e) => busy(e.currentTarget, async () => {
            cache.onboarding = await api('POST', '/api/onboarding/start');
            goTo('#/setup/welcome');
          }),
        }, state.status === 'none' ? 'Run setup' : 'Run setup again'),
        h('button', { type: 'button', onclick: () => { location.hash = '#/dashboard'; } }, 'Back to dashboard'))));
  requestAnimationFrame(() => title.focus());
}

// setupShell lays out a wizard page: header, stepper, the page under its
// h1 (focused when the page opens, so a screen reader starts there), and
// the footer.
function setupShell(state, page, view) {
  const title = h('h1', { id: 'setup-title', tabindex: -1 }, page.title);
  requestAnimationFrame(() => title.focus());
  return h('div', { class: 'setup' },
    setupHeader(),
    setupStepper(state, page.id),
    setupCompact(state, page.id),
    h('section', { class: 'setup-page', 'aria-labelledby': 'setup-title' }, title, view.body),
    view.footer);
}

// setupHeader has its own Sign out: the sidebar is hidden in the wizard.
function setupHeader() {
  return h('header', { class: 'setup-head' }, brandMark(), h('span', { class: 'muted' }, 'Set up Lissandra'),
    h('div', { class: 'actions' },
      h('button', { type: 'button', class: 'small', onclick: (e) => busy(e.currentTarget, finishLater) }, 'Finish later'),
      h('button', { type: 'button', class: 'small', onclick: (e) => busy(e.currentTarget, skipSetup) }, 'Skip setup'),
      h('button', { type: 'button', class: 'small', onclick: () => signOut() }, 'Sign out')));
}

// stepList is the steps as links. The CSS draws each state as a number
// or a mark; the words are there for screen readers.
function stepList(state, pageId) {
  const link = (p, sr) => h('a', { href: `#/setup/${p.id}`, 'aria-current': p.id === pageId ? 'step' : null },
    h('span', { class: 'label' }, p.label), sr ? h('span', { class: 'sr-only' }, `, ${sr}`) : null);
  return h('ol', { class: 'stepper' },
    SETUP_STEPS.map((p) => {
      const st = stepState(state, p.id);
      return h('li', { class: `num st-${st}` }, link(p, STEP_STATES[st].sr));
    }),
    h('li', null, link(setupPage('review'))));
}

function setupStepper(state, pageId) {
  return h('nav', { class: 'setup-steps', 'aria-label': 'Setup steps' }, stepList(state, pageId));
}

// setupCompact stands in for the stepper on narrow screens.
function setupCompact(state, pageId) {
  const i = SETUP_STEPS.findIndex((p) => p.id === pageId);
  const where = i >= 0 ? `Step ${i + 1} of ${SETUP_STEPS.length} · ${SETUP_STEPS[i].label}` : setupPage(pageId).label;
  let frac = 0;
  if (i >= 0) frac = (i + 1) / SETUP_STEPS.length;
  else if (pageId === 'review') frac = 1;
  return h('div', { class: 'setup-compact' },
    h('div', { class: 'small muted' }, where),
    progressBar(frac),
    h('details', { class: 'stepper-all' }, h('summary', null, 'All steps'), stepList(state, pageId)));
}

// setupFooter lays out a page's buttons in one order, back, skip, defaults,
// save, which stays the tab order when they stack on a phone. Each is
// {label, onClick, hidden, blocked, help}. A blocked button (blocked is
// the reason) stays focusable with aria-disabled, and a click focuses the
// reason. help is a line the button is described by. An action's error is
// shown above the buttons, unless it is an InputError: those are shown
// next to their field.
function setupFooter(spec) {
  const keys = ['back', 'skip', 'defaults', 'save'].filter((k) => spec[k]);
  const errBox = h('div', { class: 'setup-error' });
  const btns = {};
  const why = {};
  const help = {};
  const reasonOf = {};
  const run = async (k) => {
    if (reasonOf[k]) { reasonOf[k].focus(); return; }
    const btn = btns[k];
    clear(errBox);
    keys.forEach((x) => { btns[x].disabled = true; });
    try {
      await spec[k].onClick();
    } catch (err) {
      if (!(err instanceof AuthError || err instanceof SetupStopped || err instanceof InputError)) {
        console.error(err);
        errBox.append(h('div', { class: 'callout bad', role: 'alert' }, err.message || String(err)));
      }
    } finally {
      keys.forEach((x) => { btns[x].disabled = false; });
      // Disabling the button dropped its focus: give it back, unless an
      // error moved it to a field.
      if (!document.activeElement || document.activeElement === document.body) btn.focus();
    }
  };
  for (const k of keys) {
    btns[k] = h('button', { type: 'button', class: k === 'save' ? 'save primary' : k, onclick: () => run(k) });
    why[k] = h('div', { class: 'why', id: `setup-${k}-why`, tabindex: -1 });
    help[k] = h('div', { class: 'why', id: `setup-${k}-help` });
  }
  const sync = () => {
    const seen = {};
    for (const k of keys) {
      const b = spec[k];
      const btn = btns[k];
      btn.textContent = b.label;
      btn.hidden = Boolean(b.hidden);
      const reason = b.hidden ? '' : b.blocked || '';
      // Buttons blocked for the same reason share its line.
      if (reason && !seen[reason]) seen[reason] = why[k];
      reasonOf[k] = reason ? seen[reason] : null;
      why[k].textContent = reason;
      why[k].hidden = !reason || seen[reason] !== why[k];
      help[k].textContent = b.help || '';
      help[k].hidden = !b.help || Boolean(b.hidden);
      if (reason) btn.setAttribute('aria-disabled', 'true'); else btn.removeAttribute('aria-disabled');
      const ids = [reasonOf[k] ? reasonOf[k].id : '', help[k].hidden ? '' : help[k].id].filter(Boolean);
      if (ids.length) btn.setAttribute('aria-describedby', ids.join(' ')); else btn.removeAttribute('aria-describedby');
    }
  };
  sync();
  return {
    el: h('div', { class: 'setup-foot' }, errBox, keys.map((k) => btns[k]), keys.map((k) => [help[k], why[k]])),
    set: (k, patch) => { Object.assign(spec[k], patch); sync(); },
  };
}

// stepFooter is a tracked step's footer, with the meaning every step
// shares: Back records nothing, Skip changes nothing, Use defaults applies
// the step's default on the server, and Save & continue saves the page's
// edits (opts.save, which returns false to stay) before recording done.
// opts.changes is the dry run of Use defaults, opts.dirty() says whether
// there are unsaved edits and opts.discard() drops them; opts.back, skip,
// defaults and saveBtn change parts of each button.
function stepFooter(ctx, opts) {
  const id = ctx.page.id;
  const name = opts.defaultsName || 'Use defaults';
  let changes = opts.changes || [];
  const dirty = () => Boolean(opts.dirty && opts.dirty());
  const discard = opts.discard || (() => { delete setupDrafts[id]; });
  const next = () => ctx.go(nextPage(id, cache.onboarding));
  const f = setupFooter({
    back: { label: 'Back', onClick: () => ctx.go(prevPage(id, cache.onboarding)), ...opts.back },
    skip: {
      label: 'Skip',
      onClick: async () => {
        if (dirty() && !(await confirmDialog('Skip this step and discard your changes on it?', 'Skip'))) return;
        await ctx.mark('skipped');
        discard();
        next();
      },
      ...opts.skip,
    },
    defaults: {
      label: `${name} (${defaultsLabel(changes)})`,
      onClick: async () => {
        const lost = dirty() ? ' Your changes on this step are discarded.' : '';
        if ((changes.length || lost) && !(await confirmDialog(`${changes.length ? `This adds ${changeList(changes)}.` : 'Nothing is added.'}${lost}`, name))) return;
        const r = await ctx.mark('defaults');
        discard();
        if (r.changes && r.changes.length) toast(`Added ${changeSummary(r.changes)}`);
        next();
      },
      ...opts.defaults,
    },
    save: {
      label: 'Save & continue',
      onClick: async () => {
        if ((await opts.save()) === false) return;
        await ctx.mark('done');
        delete setupDrafts[id];
        next();
      },
      ...opts.saveBtn,
    },
  });
  return {
    ...f,
    setChanges: (c) => {
      changes = c || [];
      f.set('defaults', { label: `${name} (${defaultsLabel(changes)})` });
    },
  };
}

// --- welcome ---

async function setupWelcome(ctx) {
  await load('sources', 'rules', 'settings', 'notifiers', 'routes', 'groups');
  const { state } = ctx;
  const recorded = SETUP_STEPS.some((p) => (state.steps || {})[p.id]);
  const configured = cache.sources.length > 1 || cache.notifiers.length > 0 || cache.routes.length > 0 || cache.groups.length > 0;
  const picker = h('input', { type: 'file', accept: 'application/json', hidden: true });
  picker.addEventListener('change', async () => {
    const file = picker.files[0];
    picker.value = '';
    // An import keeps this instance's setup progress and admin token.
    if (file) await importConfigFile(file, { after: () => ctx.go(cache.auth.source === 'generated' ? 'access' : 'review') });
  });
  const tile = (title, text) => h('div', { class: 'tile' }, h('strong', null, title), h('span', { class: 'small muted' }, text));
  const on = cache.rules.filter((r) => r.enabled).length;
  const cats = categories().length;
  const body = h('div', { class: 'card' },
    h('p', { class: 'lead' }, 'Lissandra receives your logs, classifies them by severity and category, and alerts the right people when something needs attention.'),
    h('div', { class: 'tile-grid flow' },
      tile('Ingest', 'Senders push logs to an ingest URL, or Lissandra polls an API for them.'),
      tile('Classify', 'Rules, and the Laya engine if you run it, give each event a severity and a category.'),
      tile('Alert', 'Routes send the events that matter to email, Slack, PagerDuty and other channels.')),
    h('h2', { class: 'mt' }, 'What setup covers'),
    h('ul', { class: 'checklist' }, SETUP_STEPS.map((p) => h('li', null, stepBadge(state, p.id),
      h('div', null, h('strong', null, p.label), h('div', { class: 'small muted' }, typeof p.summary === 'function' ? p.summary(cache.auth) : p.summary))))),
    h('p', { class: 'muted' }, 'About 5 minutes. Every step can be skipped, and you can come back later.'),
    configured ? h('div', { class: 'callout' },
      h('div', null, `Already configured: ${[plural(cache.sources.length, 'source'), `${plural(cache.rules.length, 'rule')} (${num(on)} on)`,
        `${num(cats)} ${cats === 1 ? 'category' : 'categories'}`, plural(cache.notifiers.length, 'channel'), plural(cache.routes.length, 'route'),
        plural(cache.groups.length, 'alert group')].join(' · ')}`),
      h('div', { class: 'small muted' }, 'Nothing changes unless you save.')) : null,
    h('div', { class: 'row' }, h('button', { type: 'button', class: 'link', onclick: () => picker.click() }, 'Import a configuration…'), picker));
  const footer = setupFooter({
    skip: { label: 'Skip setup', onClick: skipSetup },
    defaults: { label: 'Use defaults for everything', onClick: () => useAllDefaults(ctx) },
    save: { label: 'Start setup', onClick: () => ctx.go(recorded ? state.resume : 'access') },
  });
  return { body, footer: footer.el, dirty: () => false };
}

// useAllDefaults applies the default of every step not done yet, once the
// user has seen what it adds.
async function useAllDefaults(ctx) {
  const dry = await setupApi('POST', '/api/onboarding/defaults', { dry_run: true });
  const changes = dry.changes || [];
  const what = changes.length ? `This adds ${changeList(changes)}.` : 'Nothing needs adding.';
  if (!(await confirmDialog(`Use the defaults for every step not done yet? ${what} Your admin token is not changed.`, 'Use defaults'))) return;
  const r = await setupApi('POST', '/api/onboarding/defaults', { dry_run: false });
  cache.onboarding = r.state;
  clearSetupDrafts();
  if (r.changes && r.changes.length) toast(`Added ${changeSummary(r.changes)}`);
  ctx.go('review');
}

// --- access ---

async function setupAccess(ctx) {
  const a = cache.auth;
  const { drafts } = ctx;
  const body = h('div', { class: 'card' });
  const panelBox = h('div');
  let panel = null;
  let committed = false;
  let footer = null;
  const unsaved = 'Tick "I have saved this token somewhere safe" first.';
  const sync = () => {
    if (!footer) return;
    const reason = panel && !committed && !panel.acked() ? unsaved : '';
    if (a.source === 'stored') footer.set('save', { blocked: reason, label: panel ? 'Rotate & continue' : 'Continue' });
    if (a.source !== 'generated') return;
    footer.set('save', { blocked: reason });
    // The default is the generated token: committing it while the user's
    // own is selected (and ticked as saved) could lock them out.
    footer.set('defaults', { blocked: !committed && panel.mode() === 'own' ? 'Select "Generate a token" to use the generated one.' : reason });
  };
  const openPanel = () => {
    panel = tokenPanel({ current: token, source: a.source, draft: drafts, onChange: sync });
    clear(panelBox).append(panel.el);
  };
  // commit saves the panel's token, then forgets it here: it is not kept
  // on the page or in the drafts once it is in use.
  const commit = async (generatedOnly) => {
    if (committed) return true;
    if (!(await commitToken(panel, { generatedOnly }))) return false;
    committed = true;
    clear(panelBox).append(h('div', { class: 'callout ok' }, 'Admin token saved.'));
    for (const k of Object.keys(drafts)) delete drafts[k];
    return true;
  };
  // Skipping keeps a generated token the user may already have copied.
  const discard = () => { for (const k of Object.keys(drafts)) if (k !== 'generated') delete drafts[k]; };

  if (a.source === 'generated') {
    openPanel();
    const local = ['localhost', '127.0.0.1', '[::1]'].includes(location.hostname);
    append(body,
      h('div', { class: 'callout warn' }, 'You signed in with a token Lissandra generated when it started and printed in its log. It changes every time Lissandra restarts, and anyone who can read the log has it.'),
      panelBox,
      location.protocol === 'http:' && !local ? h('div', { class: 'callout warn' }, 'This page isn\'t using HTTPS: the token crosses the network unencrypted.') : null,
      a.on_nomad ? h('p', { class: 'small muted' }, 'On Nomad, prefer LISSANDRA_ADMIN_TOKEN in the nomad/jobs/lissandra Variable. A token set here is stored on Lissandra\'s volume and is lost with it.') : null,
      h('p', { class: 'small muted wrap-anywhere' }, `Stored as a keyed hash in ${a.file}. If that directory isn't on a persistent volume, set LISSANDRA_ADMIN_TOKEN instead.`));
    footer = stepFooter(ctx, {
      dirty: () => panel.dirty(),
      discard,
      defaults: {
        label: 'Use the generated token',
        help: 'Default: the generated token shown above.',
        onClick: async () => {
          if (!(await commit(true))) return;
          await ctx.mark('defaults');
          delete setupDrafts.access;
          ctx.go(nextPage('access', cache.onboarding));
        },
      },
      save: () => commit(false),
    });
  } else if (a.source === 'stored') {
    const toggle = h('button', {
      type: 'button', class: 'link', 'aria-expanded': String(Boolean(drafts.rotating)),
      onclick: () => {
        drafts.rotating = !drafts.rotating;
        if (drafts.rotating) openPanel(); else { panel = null; clear(panelBox); }
        toggle.setAttribute('aria-expanded', String(drafts.rotating));
        toggle.textContent = drafts.rotating ? 'Keep the current token' : 'Rotate token…';
        sync();
      },
    }, drafts.rotating ? 'Keep the current token' : 'Rotate token…');
    if (drafts.rotating) openPanel();
    append(body, h('div', { class: 'callout ok' }, `A permanent admin token set in Lissandra on ${fmtDate(a.set_at)} is in use.`), toggle, panelBox);
    footer = stepFooter(ctx, {
      dirty: () => Boolean(panel && panel.dirty()),
      discard,
      defaults: { hidden: true },
      saveBtn: { label: 'Continue' },
      save: () => (panel ? commit(false) : true),
    });
  } else {
    append(body,
      h('div', { class: 'callout ok' }, 'The admin token comes from LISSANDRA_ADMIN_TOKEN in Lissandra\'s environment. Change it where it is set (on Nomad: the nomad/jobs/lissandra Variable).'),
      // A file Lissandra can't use is a startup failure waiting: it can be
      // deleted from here.
      a.stored_error ? [storedTokenUnusable(a), deleteStoredButton(ctx.refresh)] : null,
      a.stored_ignored && !a.stored_error ? h('p', null, `A token set in Lissandra${storedOn(a)} is ignored while that variable is set. Delete it in Settings → Access.`) : null);
    footer = stepFooter(ctx, { skip: { hidden: true }, defaults: { hidden: true }, saveBtn: { label: 'Continue' }, save: () => true });
  }
  sync();
  return { body, footer: footer.el, dirty: () => Boolean(panel && panel.dirty()) };
}

// --- sources ---

async function setupSources(ctx) {
  const { drafts } = ctx;
  const [dry] = await Promise.all([markStep('sources', 'defaults', { dryRun: true }), load('sources', 'info', 'settings', 'routes', 'groups')]);
  const saved = () => cache.settings.public_url || '';
  const urlErr = h('div');
  const url = h('input', {
    value: drafts.publicURL ?? saved(), placeholder: cache.info.public_url, autocomplete: 'off', spellcheck: 'false',
    oninput: (e) => { drafts.publicURL = e.target.value; clear(urlErr); url.removeAttribute('aria-invalid'); },
  });
  const list = h('div', { class: 'card flush' });
  const check = h('div', { class: 'card mt' });
  const hasEnabled = () => cache.sources.some((x) => x.enabled);
  const blocked = () => (hasEnabled() ? '' : 'Add a source, or use the default one');

  // Arrival is checked against the newest event seen, not the browser's
  // clock, which can be off from Lissandra's.
  let watched = null; // {src, baseline}
  const waiting = h('div', { class: 'listening' });
  const arrived = h('div', { class: 'arrived', role: 'status' });
  const sendOut = h('div', { class: 'small', role: 'status' });
  const newest = async (id) => (await GET(`/api/events?source=${encodeURIComponent(id)}&limit=1`))[0] || null;
  const watch = async (src) => {
    const w = { src, baseline: null };
    watched = w;
    clear(arrived);
    clear(sendOut);
    waiting.textContent = `Waiting for events on '${src.name}'…`;
    waiting.hidden = false;
    const ev = await newest(src.id);
    w.baseline = ev ? ev.id : '';
  };
  const poll = async () => {
    const w = watched;
    if (!w || w.baseline === null) return;
    const ev = await newest(w.src.id);
    if (w !== watched || !ev || ev.id === w.baseline) return;
    // Every new event is announced, and answers a test event's "Sent".
    w.baseline = ev.id;
    waiting.hidden = true;
    clear(sendOut);
    clear(arrived).append('Received ', timeCell(ev.received_at), ': ', sevBadge(ev.severity), ` ${ev.category} · ${String(ev.message).split('\n')[0].slice(0, 200)}`);
  };

  const renderCheck = async () => {
    const pushes = cache.sources.filter((x) => x.type === 'push' && x.enabled);
    const pulls = cache.sources.filter((x) => x.type === 'pull' && x.enabled);
    clear(check).append(h('h2', null, 'Check that logs arrive'));
    if (!pushes.length) {
      watched = null;
      check.append(h('p', { class: 'muted' }, 'No enabled push source to send to.'));
    } else {
      // An edit or token rotation replaced the source: send with the new one.
      const cur = watched && pushes.find((x) => x.id === watched.src.id);
      if (cur) watched.src = cur; else await watch(pushes[0]);
      const pick = pushes.length > 1 ? select(pushes.map((x) => [x.id, x.name]), watched.src.id, {
        onchange: (e) => watch(pushes.find((x) => x.id === e.target.value)).catch(reportError),
      }) : null;
      const sendBtn = h('button', { type: 'button', onclick: () => busy(sendBtn, () => sendTestEvent(watched.src, sendOut)) }, 'Send a test event');
      append(check,
        pick ? field('Source', pick) : null,
        waiting, arrived,
        h('div', { class: 'row mt' }, sendBtn, h('span', { class: 'small muted' }, 'Optional: one info event, as a sender would post it.')),
        sendOut);
    }
    if (pulls.length) check.append(h('p', { class: 'small muted' }, 'Lissandra polls pull sources itself. See the Sources page for the last poll.'));
  };

  let footer = null;
  const refreshList = async () => {
    const [d] = await Promise.all([markStep('sources', 'defaults', { dryRun: true }), load('sources')]);
    renderList();
    await renderCheck();
    footer.setChanges(d.changes);
    footer.set('save', { blocked: blocked() });
  };
  const renderList = () => {
    clear(list);
    if (!cache.sources.length) list.append(h('div', { class: 'empty' }, 'No sources yet.'));
    for (const src of cache.sources) {
      list.append(h('div', { class: `list-item ${src.enabled ? '' : 'disabled-item'}` },
        h('div', { class: 'main' },
          h('div', { class: 'title' }, src.name, h('span', { class: 'badge' }, src.type), src.enabled ? null : h('span', { class: 'badge' }, 'disabled')),
          h('div', { class: 'meta mono' }, src.type === 'push' ? `POST ${cache.info.public_url}/ingest/${src.id}` : `${src.pull.method} ${src.pull.url}`)),
        h('div', { class: 'actions' },
          src.type === 'push' ? h('button', { type: 'button', class: 'small', onclick: () => showIngest(src, { onRotated: refreshList }) }, 'Setup instructions') : null,
          h('button', { type: 'button', class: 'small', onclick: () => editSource(src, { onSaved: refreshList }) }, 'Edit'))));
    }
    list.append(h('div', { class: 'row pad' }, h('button', { type: 'button', onclick: () => editSource(null, { onSaved: refreshList }) }, 'Add a source…')));
  };
  renderList();
  await renderCheck();

  const body = h('div', null,
    h('div', { class: 'card' },
      h('h2', null, 'Lissandra\'s address'),
      field('Public URL', url, 'Used in ingest URLs and in links in notifications.'),
      urlErr),
    h('h2', { class: 'mt' }, 'Sources'),
    list,
    check);
  const dirty = () => url.value.trim() !== saved();
  footer = stepFooter(ctx, {
    changes: dry.changes,
    dirty,
    defaults: { help: 'Default: adds a push source when no source is enabled, named "default" (or "default-2" and so on, if that name is taken).' },
    saveBtn: { blocked: blocked() },
    save: async () => {
      if (!dirty()) return true;
      try {
        // A partial update: the server merges it into the saved settings.
        cache.settings = await api('PUT', '/api/settings', { public_url: url.value.trim() });
      } catch (err) {
        if (err.status !== 400) throw err;
        fieldError(urlErr, url, `Not saved: ${err.message.replace(/^settings: /, '')}`);
        throw new InputError(err.message);
      }
      await load('info');
      toast('Public URL saved');
      return true;
    },
  });
  return { body, footer: footer.el, dirty, cleanup: every(3000, poll) };
}

// sendTestEvent posts one info event to a push source, the way a sender
// would, after asking when it would notify people. An info event is never
// judged by the engine, which at its default minimum severity never sees
// it.
async function sendTestEvent(src, out) {
  const body = JSON.stringify({ level: 'info', message: 'Hello from the Lissandra setup wizard', lissandra_test: true });
  const preview = await api('POST', '/api/rules/test', { sample: body, source_id: src.id });
  const names = new Set();
  for (const row of preview.events || []) for (const name of row.routes || []) names.add(name);
  for (const r of cache.routes) if (r.enabled && (r.min_severity === 'debug' || r.min_severity === 'info')) names.add(r.name);
  if (names.size && !(await confirmDialog(`This test event matches route(s) ${[...names].join(', ')} and will notify people. Send it anyway?`, 'Send'))) return;
  clear(out).append('Sending…');
  let res;
  try {
    // A plain fetch, not api(): a 401 here is about the ingest token, and
    // must not open the admin sign-in.
    res = await fetch(`/ingest/${encodeURIComponent(src.id)}`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${src.ingest_token}`, 'Content-Type': 'application/json' },
      body,
    });
  } catch {
    clear(out).append('Not sent: Lissandra couldn\'t be reached.');
    return;
  }
  const outcomes = {
    202: 'Sent. Waiting for Lissandra to classify it…',
    403: 'This source is disabled.',
    401: 'The ingest token changed. Reload this page.',
    503: 'Lissandra\'s queue is full. Retry in a few seconds.',
  };
  clear(out).append(outcomes[res.status] || `Not sent: ${res.status} ${res.statusText}`);
}

// --- classify ---

// engineLine says in a few words whether the Laya engine classifies what
// the rules leave.
function engineLine(e) {
  if (!e.enabled) return 'Laya engine: off. Rules classify on their own.';
  if (!e.configured) return 'Laya engine: not configured. Rules classify on their own.';
  const paused = e.paused_until && new Date(e.paused_until) > new Date();
  return `Laya engine: ${paused ? 'paused after errors' : 'active'} (${e.model || engineSettings().model || 'english'})`;
}

// draftRules is the rules with the page's unsaved toggles applied.
function draftRules(draft) {
  return cache.rules.map((r) => (r.id in draft.enabled ? { ...r, enabled: draft.enabled[r.id] } : r));
}

// setupRuleList lists rules with a toggle each. Toggles go into
// draft.enabled and are saved with the step, so Skip changes nothing (the
// Rules page's list saves and reorders at once). onSaved runs after a rule
// is edited or added.
function setupRuleList(rules, draft, onSaved) {
  const root = h('div', { class: 'card flush' });
  if (!rules.length) root.append(h('div', { class: 'empty' }, 'No rules. Events will only get heuristic severities.'));
  for (const r of rules) {
    const on = r.id in draft.enabled ? draft.enabled[r.id] : r.enabled;
    const row = h('div', { class: `list-item ${on ? '' : 'disabled-item'}` });
    const box = h('input', {
      type: 'checkbox', checked: on, 'aria-label': `Enable rule "${r.name}"`,
      onchange: (e) => {
        if (e.target.checked === r.enabled) delete draft.enabled[r.id]; else draft.enabled[r.id] = e.target.checked;
        row.classList.toggle('disabled-item', !e.target.checked);
      },
    });
    append(row, box,
      h('div', { class: 'main' },
        h('div', { class: 'title' }, r.name),
        r.description ? h('div', { class: 'meta' }, r.description) : null,
        r.name === 'Drop health checks' ? h('div', { class: 'meta' }, 'Off by default. Turn it on to ignore load-balancer probes.') : null,
        h('div', { class: 'meta' }, ruleActions(r))),
      h('div', { class: 'actions' }, h('button', { type: 'button', class: 'small', onclick: () => editRule(r, null, { onSaved }) }, 'Edit')));
    root.append(row);
  }
  root.append(h('div', { class: 'row pad' }, h('button', { type: 'button', onclick: () => editRule(null, null, { onSaved }) }, 'Add a rule…')));
  return root;
}

async function setupClassify(ctx) {
  const { drafts } = ctx;
  if (!drafts.enabled) drafts.enabled = {};
  const [dry, stats] = await Promise.all([markStep('classify', 'defaults', { dryRun: true }), GET('/api/stats'), load('rules', 'settings', 'sources')]);
  const cats = categoryEditor(drafts.cats || categoryList());
  const catsDirty = () => {
    const rows = cats.rows().map((c) => ({ name: c.name.trim().toLowerCase(), description: c.description.trim() })).filter((c) => c.name || c.description);
    return JSON.stringify(rows) !== JSON.stringify(categoryList());
  };
  const rulesBox = h('div');
  const paintRules = () => clear(rulesBox).append(setupRuleList(cache.rules, drafts, refreshRules));
  // The editor saved the rule's own toggle: drop its draft one.
  const refreshRules = async (saved) => {
    if (saved && saved.id) delete drafts.enabled[saved.id];
    await load('rules');
    paintRules();
  };
  paintRules();

  const body = h('div', null,
    h('div', { class: 'card' },
      h('h2', null, 'Categories'),
      h('p', { class: 'small muted' }, 'Rules set categories by name, and alert routes can pick them. For events no rule categorizes, the Laya engine picks one of these names, or leaves the event "uncategorized".'),
      cats.el),
    h('div', { class: 'card mt' },
      h('h2', null, 'Rules'),
      h('p', { class: 'small muted' }, 'Evaluated top to bottom: matches set severity, category and tags. The switches here are saved with Save & continue.'),
      rulesBox,
      h('p', { class: 'small' }, engineLine(stats.engine || {}), ' · ', h('a', { href: '#/settings' }, 'Engine settings')),
      h('details', { class: 'try-it' }, h('summary', null, 'Try it'), ruleTester({ rules: () => draftRules(drafts) }))));

  const dirty = () => catsDirty() || Object.keys(drafts.enabled).length > 0;
  const footer = stepFooter(ctx, {
    changes: dry.changes,
    dirty,
    defaults: { help: 'Default: adds back the built-in categories that are missing. Rules are not changed.' },
    save: async () => {
      let next;
      try {
        next = cats.value();
      } catch (err) {
        focusInvalid(cats.el);
        throw err;
      }
      let saved = false;
      if (JSON.stringify(next) !== JSON.stringify(categoryList())) {
        try {
          cache.settings = await api('PUT', '/api/settings', { categories: next });
        } catch (err) {
          if (err.status !== 400) throw err;
          cats.showError(`Not saved: ${err.message.replace(/^settings: /, '')}`);
          focusInvalid(cats.el);
          throw new InputError(err.message);
        }
        cats.set(categoryList());
        saved = true;
      }
      for (const r of cache.rules) {
        if (!(r.id in drafts.enabled)) continue;
        const enabled = drafts.enabled[r.id];
        await api('PUT', `/api/rules/${r.id}`, { ...r, enabled });
        r.enabled = enabled;
        delete drafts.enabled[r.id];
        saved = true;
      }
      if (saved) toast('Classification saved');
      return true;
    },
  });
  // The category rows are kept only when edited, so a later change made
  // elsewhere isn't hidden behind an old copy.
  const cleanup = () => { if (catsDirty()) drafts.cats = cats.rows(); else delete drafts.cats; };
  return { body, footer: footer.el, dirty, cleanup };
}

function focusInvalid(root) {
  const el = root.querySelector('[aria-invalid="true"]');
  if (el) el.focus();
}

// --- channels ---

async function setupChannels(ctx) {
  await load('notifiers', 'types');
  const list = h('div', { class: 'card flush' });
  const enabledAny = () => cache.notifiers.some((n) => n.enabled);
  let footer = null;
  const refresh = async () => {
    await load('notifiers');
    paint();
    footer.set('defaults', { hidden: enabledAny() });
    footer.set('save', { blocked: enabledAny() ? '' : 'Add a channel, or choose No alerts for now' });
  };
  const paint = () => {
    clear(list);
    if (!cache.notifiers.length) list.append(h('div', { class: 'empty' }, 'No channels yet. Pick a type above to add one.'));
    for (const n of cache.notifiers) {
      const type = cache.types.find((t) => t.type === n.type);
      list.append(h('div', { class: `list-item ${n.enabled ? '' : 'disabled-item'}` },
        h('div', { class: 'main' },
          h('div', { class: 'title' }, n.name, h('span', { class: 'badge' }, type ? type.label : n.type), n.enabled ? null : h('span', { class: 'badge' }, 'disabled')),
          h('div', { class: 'meta' }, notifierSummary(n, type))),
        h('div', { class: 'actions' },
          h('button', { type: 'button', class: 'small', onclick: (e) => sendTest(n, e.currentTarget) }, 'Send test'),
          h('button', { type: 'button', class: 'small', onclick: () => editNotifier(n, { onSaved: refresh }) }, 'Edit'))));
    }
  };
  paint();
  const body = h('div', null,
    h('div', { class: 'card' },
      h('h2', null, 'Add a channel'),
      h('div', { class: 'tile-grid' }, cache.types.map((t) => h('button', {
        type: 'button', class: 'tile', onclick: () => editNotifier(null, { type: t.type, onSaved: refresh }),
      }, h('strong', null, t.label), h('span', { class: 'small muted' }, t.description)))),
      h('p', { class: 'small muted' }, 'Keep webhook URLs and passwords in Lissandra\'s environment: write ${env:NAME}.')),
    h('h2', { class: 'mt' }, 'Your channels'),
    list);
  footer = stepFooter(ctx, {
    defaults: { label: 'No alerts for now', help: 'Events are still classified and shown in Lissandra. Nobody is notified.', hidden: enabledAny() },
    saveBtn: { blocked: enabledAny() ? '' : 'Add a channel, or choose No alerts for now' },
    save: () => true,
  });
  return { body, footer: footer.el, dirty: () => false };
}

// --- notify ---

// notifyCard edits who receives a channel's alerts (its recipient fields)
// and which alerts it gets: through the first route that targets only this
// channel, or a new one. draft keeps the edits; onRouteSaved runs after
// the route is edited in its own editor. It returns {el, dirty(), save()},
// where save returns whether anything was saved.
function notifyCard(n, routes, draft, onRouteSaved) {
  const type = cache.types.find((t) => t.type === n.type) || { label: n.type, fields: [] };
  const own = routes.find((r) => r.notifier_ids.length === 1 && r.notifier_ids[0] === n.id);
  const others = routes.filter((r) => r !== own && r.notifier_ids.includes(n.id));
  const teams = activeGroups().filter((g) => g.notifier_ids.includes(n.id));
  // As in the server's suggested routes: a team's channel stays scoped to
  // the team's sources unless the user asks for more.
  const reached = teams.length > 0 || routes.some((r) => r.enabled && r.notifier_ids.includes(n.id));
  const base = own
    ? { on: own.enabled, sev: own.min_severity, cats: own.categories || [], throttle: own.throttle_seconds }
    // PagerDuty pages someone: only for critical by default.
    : { on: !reached, sev: n.type === 'pagerduty' ? 'critical' : 'error', cats: [], throttle: 900 };
  const v = (k) => (k in draft ? draft[k] : base[k]);
  const errBox = h('div');

  const recips = type.fields.filter((f) => f.recipient).map((f) => ({
    f,
    input: h('input', {
      value: (draft.recipients || {})[f.key] ?? n.settings[f.key] ?? '', placeholder: f.placeholder || '', autocomplete: 'off',
      oninput: (e) => { draft.recipients = { ...draft.recipients, [f.key]: e.target.value }; },
    }),
  }));
  const on = checkbox(own ? 'Enabled' : 'Create a route for this channel', v('on'), (x) => { draft.on = x; });
  // Editing the route keeps its on/off as shown, so a repaint (after an
  // alert group is added, say) can't switch it off underneath the edit.
  const edit = (k, x) => { if (!('on' in draft)) draft.on = v('on'); draft[k] = x; };
  const sev = select(SEVERITIES, v('sev'), { onchange: (e) => edit('sev', e.target.value) });
  const catBoxes = [...new Set([...categories(), 'uncategorized', ...v('cats')])].map((c) => ({
    c, box: checkbox(c, v('cats').includes(c), () => edit('cats', catBoxes.filter((x) => x.box.input.checked).map((x) => x.c))),
  }));
  const throttleOpts = THROTTLES.some((t) => t[0] === v('throttle')) ? THROTTLES : [...THROTTLES, [v('throttle'), `${v('throttle')} seconds`]];
  const throttle = select(throttleOpts.map(([x, l]) => [String(x), l]), String(v('throttle')), { onchange: (e) => edit('throttle', Number(e.target.value)) });
  const extra = own ? [
    own.source_ids && own.source_ids.length ? `sources ${own.source_ids.map(sourceName).join(', ')}` : '',
    own.tags && own.tags.length ? `tags ${own.tags.join(', ')}` : '',
    own.message_regex ? `message ~ /${own.message_regex}/` : '',
  ].filter(Boolean) : [];
  const catsLabel = `cats-${++fieldSeq}`;

  const el = h('div', { class: 'card mt notify-card' },
    h('div', { class: 'row' }, h('h2', null, n.name), h('span', { class: 'badge' }, type.label), h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'small', onclick: (e) => sendTest(n, e.currentTarget) }, 'Send test')),
    h('h3', null, 'Who receives these alerts'),
    type.audience ? h('p', { class: 'small' }, type.audience) : null,
    recips.map(({ f, input }) => field(f.label + (f.required ? ' *' : ''), input, f.help)),
    h('h3', null, 'Which alerts'),
    own ? h('p', { class: 'small muted' }, `Edits route "${own.name}"`) : null,
    on.el,
    h('div', { class: 'fields-2 mt' }, field('Minimum severity', sev), field('Throttle repeats', throttle)),
    h('div', { class: 'field' }, h('label', { id: catsLabel }, 'Categories'),
      h('div', { class: 'chips', role: 'group', 'aria-labelledby': catsLabel }, catBoxes.map((x) => x.box.el)),
      h('div', { class: 'help' }, 'None checked = any category.')),
    extra.length ? h('p', { class: 'small muted' }, `The route also filters by ${extra.join(' · ')}.`) : null,
    own ? h('button', {
      type: 'button', class: 'small',
      onclick: () => editRoute(own, {
        onSaved: async () => {
          for (const k of ['on', 'sev', 'cats', 'throttle']) delete draft[k];
          await onRouteSaved();
        },
      }),
    }, 'Edit route…') : null,
    others.length ? h('p', { class: 'small muted' }, `Also receives alerts from: ${others.map((r) => (r.enabled ? r.name : `${r.name} (off)`)).join(', ')}`) : null,
    teams.length ? h('p', { class: 'small muted' }, `A channel of ${teams.map((g) => `the alert group "${g.name}"`).join(', ')}, for ${teams.length === 1 ? 'its' : 'their'} sources' alerts.`) : null,
    errBox);

  const want = () => ({ enabled: v('on'), min_severity: v('sev'), categories: v('cats'), throttle_seconds: v('throttle') });
  const showError = (text) => fieldError(errBox, recips.length ? recips[0].input : sev, text);
  return {
    el,
    id: n.id,
    showError,
    dirty: () => ['recipients', 'on', 'sev', 'cats', 'throttle'].some((k) => k in draft),
    save: async () => {
      clear(errBox);
      let saved = false;
      try {
        const changed = recips.filter(({ f, input }) => input.value.trim() !== (n.settings[f.key] || ''));
        if (changed.length) {
          // Secrets come back masked, and the server keeps the stored ones.
          const settings = { ...n.settings };
          for (const { f, input } of changed) settings[f.key] = input.value.trim();
          await api('PUT', `/api/notifiers/${n.id}`, { ...n, settings });
          n.settings = settings;
          saved = true;
        }
        const w = want();
        if (own) {
          const cur = { enabled: own.enabled, min_severity: own.min_severity, categories: own.categories || [], throttle_seconds: own.throttle_seconds };
          if (JSON.stringify(cur) !== JSON.stringify(w)) {
            Object.assign(own, await api('PUT', `/api/routes/${own.id}`, { ...own, ...w }));
            saved = true;
          }
        } else if (w.enabled || draft.createdId) {
          const route = { name: `${n.name}: ${w.min_severity} and above`, ...w, notifier_ids: [n.id] };
          // A retry updates the route the first attempt created, also to
          // switch it off.
          if (draft.createdId) await api('PUT', `/api/routes/${draft.createdId}`, { ...route, id: draft.createdId });
          else draft.createdId = (await api('POST', '/api/routes', route)).id;
          saved = true;
        }
      } catch (err) {
        if (err instanceof AuthError) throw err;
        showError(`Not saved: ${err.message}`);
        throw new InputError(`Not saved: ${err.message}`);
      }
      return saved;
    },
  };
}

async function setupNotify(ctx) {
  const { drafts } = ctx;
  if (!drafts.cards) drafts.cards = {};
  await load('notifiers', 'types', 'routes', 'settings', 'sources', 'groups');
  const enabled = () => cache.notifiers.filter((n) => n.enabled);
  if (!enabled().length) {
    const footer = setupFooter({
      back: { label: 'Back', onClick: () => ctx.go('channels') },
      save: { label: 'Continue', onClick: () => ctx.go('review') },
    });
    return {
      body: h('div', { class: 'callout' }, 'No alert channels yet. Go back to add one, or continue: Lissandra keeps events either way.'),
      footer: footer.el,
      dirty: () => false,
    };
  }
  const dry = await markStep('notify', 'defaults', { dryRun: true });
  const cardsBox = h('div');
  let cards = [];
  let footer = null;
  let stale = false; // a save failed, and the cards weren't painted again since
  const teamsBox = h('div', { class: 'card flush' });
  const paint = () => {
    cards = enabled().map((n) => notifyCard(n, cache.routes, drafts.cards[n.id] || (drafts.cards[n.id] = {}), refresh));
    append(clear(cardsBox), cards.map((c) => c.el));
    clear(teamsBox);
    if (!cache.groups.length) teamsBox.append(h('div', { class: 'empty' }, 'No alert groups yet.'));
    for (const g of cache.groups) {
      // The list is rebuilt on save: focus goes back to the new Edit button.
      const onSaved = async () => {
        await refresh();
        const b = teamsBox.querySelector(`[data-group-id="${CSS.escape(g.id)}"]`);
        if (b) b.focus();
      };
      teamsBox.append(groupItem(g, h('button', { type: 'button', class: 'small', 'data-group-id': g.id, onclick: () => editGroup(g, { onSaved }) }, 'Edit')));
    }
  };
  const refresh = async () => {
    const [d] = await Promise.all([markStep('notify', 'defaults', { dryRun: true }), load('routes', 'notifiers', 'groups')]);
    paint();
    stale = false;
    footer.setChanges(d.changes);
  };
  paint();
  const body = h('div', null,
    h('p', { class: 'muted' }, 'Lissandra has no user accounts: people get alerts through channels. For each channel, set who receives its alerts and which alerts go to it.'),
    cardsBox,
    h('h2', { class: 'mt' }, 'Teams'),
    h('p', { class: 'muted' }, 'Optional. An alert group alerts a team about its own sources only: through the team\'s channels, and by email to its members through an email channel\'s server, with its own minimum severity and categories.'),
    teamsBox,
    h('div', { class: 'row mt' }, h('button', { type: 'button', onclick: () => editGroup(null, { onSaved: refresh }) }, 'Add an alert group…')));
  const dirty = () => cards.some((c) => c.dirty());
  footer = stepFooter(ctx, {
    changes: dry.changes,
    dirty,
    defaultsName: 'Use suggested routes',
    defaults: { help: 'Default: a route for each channel no route or alert group reaches, for error and above (critical for PagerDuty), any category, repeats throttled for 15 minutes.' },
    save: async () => {
      // A request that failed may still have been applied, only its answer
      // lost. So after a failure the cards are painted again, from Lissandra's
      // routes and their drafts: a route a lost create made is then its
      // card's own, and a retry edits it instead of making another. When
      // that reload fails too, the retry reloads first.
      if (stale) await refresh();
      let saved = false;
      for (const c of cards) {
        try {
          saved = (await c.save()) || saved;
        } catch (err) {
          if (!(err instanceof InputError)) throw err;
          stale = true;
          await refresh().catch(() => {}); // else the card keeps its error
          const again = cards.find((x) => x.id === c.id);
          if (again && again !== c) again.showError(err.message);
          throw err;
        }
      }
      if (saved) toast('Recipients and routes saved');
      return true;
    },
  });
  return { body, footer: footer.el, dirty };
}

// --- review ---

// routeLine sums up which events a route sends: "≥ error · any category ·
// throttle 15 minutes".
function routeLine(r) {
  const t = THROTTLES.find((x) => x[0] === r.throttle_seconds);
  const parts = [`≥ ${r.min_severity}`, r.categories && r.categories.length ? r.categories.join(', ') : 'any category'];
  if (r.source_ids && r.source_ids.length) parts.push(`sources ${r.source_ids.map(sourceName).join(', ')}`);
  if (r.tags && r.tags.length) parts.push(`tags ${r.tags.join(', ')}`);
  if (r.message_regex) parts.push(`message ~ /${r.message_regex}/`);
  parts.push(r.throttle_seconds ? `throttle ${t ? t[1] : `${r.throttle_seconds}s`}` : 'no throttling');
  return parts.join(' · ');
}

// whoGetsWhat lists, per enabled channel, who receives its alerts and the
// enabled routes and alert groups that reach it; then the members of each
// alert group that emails its members.
function whoGetsWhat() {
  const chans = cache.notifiers.filter((n) => n.enabled);
  if (!chans.length) return h('p', { class: 'muted' }, 'No alert channels: nobody is notified.');
  const teams = activeGroups();
  const tbl = h('table', null, h('tr', null, h('th', null, 'Channel'), h('th', null, 'Who receives'), h('th', null, 'Which alerts')));
  for (const n of chans) {
    const type = cache.types.find((t) => t.type === n.type) || { label: n.type, fields: [] };
    const who = type.fields.filter((f) => f.recipient && n.settings[f.key]).map((f) => `${f.label}: ${n.settings[f.key]}`);
    const lines = [
      ...cache.routes.filter((r) => r.enabled && r.notifier_ids.includes(n.id)).map((r) => h('div', { title: r.name }, routeLine(r))),
      ...teams.filter((g) => g.notifier_ids.includes(n.id)).map((g) => h('div', { title: `Alert group "${g.name}"` }, `${g.name}: ${routeLine(g)}`)),
    ];
    tbl.append(h('tr', null,
      h('td', null, n.name, h('div', { class: 'muted small' }, type.label)),
      h('td', null, who.map((x) => h('div', null, x)), type.audience ? h('div', { class: who.length ? 'muted small' : '' }, type.audience) : null),
      h('td', null, lines.length ? lines : h('span', { class: 'badge warn' }, 'No route or alert group: gets no alerts'))));
  }
  for (const g of (cache.groups || []).filter((x) => x.enabled && (x.emails || []).length)) {
    const via = cache.notifiers.find((n) => n.id === g.email_notifier_id);
    // As the dispatcher: members are emailed only through an enabled email
    // channel, about the group's sources.
    let which = h('span', { class: 'badge warn' }, 'No sources: gets no alerts');
    if (!via || !via.enabled) which = h('span', { class: 'badge warn' }, 'Email channel missing or off: gets no alerts');
    else if (g.source_ids.length) which = h('div', null, routeLine(g));
    tbl.append(h('tr', null,
      h('td', null, `${g.name} members`, h('div', { class: 'muted small' }, via ? `Alert group, email via ${via.name}` : 'Alert group')),
      h('td', null, g.emails.map((x) => h('div', null, x))),
      h('td', null, which)));
  }
  return h('div', { class: 'table-wrap' }, tbl);
}

async function setupReview(ctx) {
  const { state } = ctx;
  const [stats, latest] = await Promise.all([GET('/api/stats'), GET('/api/events?limit=1'),
    load('auth', 'sources', 'rules', 'settings', 'notifiers', 'types', 'routes', 'groups')]);
  const a = cache.auth;
  const row = (id, ...what) => {
    const p = setupPage(id);
    return h('div', { class: 'list-item' }, stepBadge(state, id),
      h('div', { class: 'main' }, h('div', { class: 'title' }, p.label), h('div', { class: 'meta' }, what)),
      h('a', { href: `#/setup/${id}`, class: 'small', 'aria-label': `Change ${p.label}` }, 'Change'));
  };
  const push = cache.sources.find((x) => x.type === 'push' && x.enabled);
  const rulesOn = cache.rules.filter((r) => r.enabled).length;
  const cats = categories().length;
  const chans = cache.notifiers.filter((n) => n.enabled);
  const routesOn = cache.routes.filter((r) => r.enabled).length;
  let access = 'Managed by LISSANDRA_ADMIN_TOKEN';
  if (a.source === 'stored') access = 'Permanent token set in Lissandra';
  else if (a.source === 'generated') access = h('span', { class: 'badge warn' }, 'Changes every time Lissandra restarts');

  const body = h('div', null,
    h('div', { class: 'card flush' },
      row('access', access),
      row('sources',
        h('div', null, cache.sources.map((x) => x.name).join(', ') || 'No sources'),
        push ? h('div', { class: 'row' }, h('span', { class: 'mono' }, `${cache.info.public_url}/ingest/${push.id}`),
          h('button', { type: 'button', class: 'small', onclick: () => showIngest(push) }, 'Setup instructions')) : null,
        h('div', null, latest.length ? `Last event received ${ago(latest[0].received_at)}` : 'No event received yet')),
      row('classify', `${num(cats)} ${cats === 1 ? 'category' : 'categories'} · ${num(rulesOn)} of ${plural(cache.rules.length, 'rule')} on · ${engineLine(stats.engine || {})}`),
      row('channels', chans.length
        ? chans.map((n) => h('div', { class: 'row' }, n.name, h('button', { type: 'button', class: 'small', onclick: (e) => sendTest(n, e.currentTarget) }, 'Send test')))
        : 'None: nobody is notified'),
      row('notify', `${plural(routesOn, 'enabled route')} · ${plural(cache.groups.filter((g) => g.enabled).length, 'enabled alert group')}`)),
    h('div', { class: 'card mt' }, h('h2', null, 'Who gets what'), whoGetsWhat()));
  const footer = setupFooter({
    back: { label: 'Back', onClick: () => ctx.go(prevPage('review', state)) },
    skip: { label: 'Finish later', onClick: finishLater },
    save: {
      label: 'Finish setup',
      onClick: async () => {
        cache.onboarding = await setupApi('POST', '/api/onboarding/complete');
        clearSetupDrafts();
        toast('Lissandra is set up');
        location.hash = '#/dashboard';
      },
    },
  });
  return { body, footer: footer.el, dirty: () => false };
}

// ---------- Boot ----------

let booted = false;
async function boot() {
  try {
    await load('info');
  } catch (err) {
    reportError(err);
    return;
  }
  document.getElementById('version').textContent = `v${cache.info.version}`;
  GET('/api/stats').then((st) => updateReviewBadge(st.learning ? st.learning.review : 0)).catch(() => {});
  if (!booted) {
    booted = true;
    window.addEventListener('hashchange', route);
    document.getElementById('logout').addEventListener('click', signOut);
  }
  // A pending setup takes over the default landing page only, at boot and
  // sign-in: deep links and later visits to the dashboard stay where they
  // are. replaceState fires no hashchange, so the page renders once.
  const ob = cache.info.onboarding;
  const { page } = parseHash();
  if (ob && ob.status === 'pending' && (!location.hash || location.hash === '#' || page === 'dashboard')) {
    history.replaceState(null, '', `#/setup/${SETUP_PAGES.some((p) => p.id === ob.resume) ? ob.resume : 'welcome'}`);
  }
  route();
}

if (token) boot(); else showLogin();
