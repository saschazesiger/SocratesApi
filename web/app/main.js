/*
 * The frame of the interface: hash routes with DS.motion.transition(), the
 * drawer, the side sheets (a shared one and the engine log), live updates,
 * the engine's state in the top bar, and the events every view gets
 * delegated. Sign-in, the first password, the account capsule, sign-out and
 * the change notices are the Z-Dash server's shell
 * (/z-dash/server/shell.js): start() resolves once someone is signed in,
 * on(topic) hears the notices. The live status and the engine log come on
 * Socrates' own stream (/api/stream).
 *
 * A view is an object:
 *   title, nav           the document title and the drawer item it belongs to
 *   fixed                the view never scrolls; its last child does (the chat)
 *   load(params)         fetches its data (a Promise)
 *   html(data, params)   its markup, rendered with DS.dom.morphHtml
 *   mount(data, params)  after the first render (optional)
 *   update(data, params) a live refresh in place after a change of its topics (optional)
 *   topics               the change topics that refresh it (models, keys, requests, settings)
 *   status(status)       every new status object, while it shows (optional)
 *   on(type, event)      click, change, input, submit and keydown inside the page
 *   sheet(type, event)   the same inside the side sheet it opened
 *   leave()              before another view replaces it (optional)
 */
import { start as startShell, on, current } from '/z-dash/server/shell.js';
import * as api from './api.js';
import { $, $$, M, esc, head, alertHtml, copyFrom, toast, engineChip } from './ui.js';
import models from './models.js';
import addModel from './addmodel.js';
import modelSettings from './modelsettings.js';
import chat from './chat.js';
import apiView from './apiview.js';
import settings from './settings.js';

const DS = window.DS;
const app = $('#app');
const page = $('#page');
const view = $('#main');
const drawer = $('#drawer');
const nav = $('#nav');
const sheet = $('#sheet');
const sheetBody = $('#sheet-body');
const logSheet = $('#log-sheet');
const menuButton = $('#menu');

const VIEWS = {
  models,
  'models/add': addModel,
  'models/settings': modelSettings,
  chat,
  api: apiView,
  settings,
};

/** What the views share: the session, the live status, the current route and its data. */
export const ctx = {
  session: null,
  status: null,
  route: '',
  params: new URLSearchParams(),
  view: null,
  data: null,
  sheetOwner: null,
  go,
  refresh,
  openSheet,
  closeSheet,
  openLog,
  page,
  sheetBody,
};

/* ── Routes ───────────────────────────────────────────────────────── */

function parse(hash) {
  const match = /^#\/([a-z/]+)(?:\?(.*))?$/.exec(hash || '');
  let route = match ? match[1] : 'models';
  if (!VIEWS[route]) route = 'models';
  return { route, params: new URLSearchParams(match ? match[2] || '' : '') };
}

let navigation = 0;

async function go(hash, { push = true, replace = false, type = 'fade-through' } = {}) {
  const { route, params } = parse(hash);
  if (replace) history.replaceState(null, '', hash);
  else if (push && location.hash !== hash) history.pushState(null, '', hash);
  const next = VIEWS[route];
  const token = ++navigation;
  let data;
  let error = null;
  try {
    data = await next.load(params);
  } catch (err) {
    error = err;
  }
  if (token !== navigation) return;
  // A destination: fade-through; the modal drawer closes inside the update.
  await M.transition(() => {
    if (app.hasAttribute('data-drawer-open')) DS.overlay.close(drawer);
    if (sheet.open) DS.overlay.close(sheet);
    show(next, route, params, data, error);
    return page;
  }, { type, focus: true });
  if (!error && next.mount && ctx.view === next) next.mount(data, params);
}

function show(next, route, params, data, error) {
  if (ctx.view && ctx.view.leave) ctx.view.leave();
  ctx.view = next;
  ctx.route = route;
  ctx.params = params;
  ctx.data = data;
  view.classList.toggle('ds-view--fixed', !!next.fixed && !error);
  DS.dom.morphHtml(page, error ? failedHtml(next, error) : next.html(data, params), { replace: true });
  const current = next.nav || route;
  for (const item of $$('[data-route]', drawer)) {
    if (item.getAttribute('data-route') === current) item.setAttribute('aria-current', 'page');
    else item.removeAttribute('aria-current');
  }
  document.title = `${typeof next.title === 'function' ? next.title(data, params) : next.title} · Socrates API`;
  view.scrollTop = 0;
}

function failedHtml(next, error) {
  const title = typeof next.title === 'function' ? next.title(null, ctx.params) : next.title;
  return head(title, '') + alertHtml('error', 'This page could not be loaded', esc(error.message),
    '<button class="ds-btn ds-btn--text ds-sm" type="button" data-act="retry">Try again</button>');
}

/* ── Live updates ─────────────────────────────────────────────────── */

let refreshing = false;
let pendingTopics = null;

/** Refetches and updates the current view in place, once at a time. */
async function refresh(topics) {
  const current = ctx.view;
  if (!current || !current.update) return;
  const wanted = current.topics || [];
  if (topics && !wanted.some((topic) => topics.has(topic))) return;
  if (refreshing) {
    pendingTopics = pendingTopics || new Set();
    for (const topic of topics || wanted) pendingTopics.add(topic);
    return;
  }
  refreshing = true;
  try {
    const data = await current.load(ctx.params);
    if (ctx.view === current) {
      ctx.data = data;
      current.update(data, ctx.params);
    }
  } catch (error) {
    // The next change or the stream's reconnect brings fresh data.
  } finally {
    refreshing = false;
  }
  if (pendingTopics) {
    const again = pendingTopics;
    pendingTopics = null;
    refresh(again);
  }
}

/*
 * The shell's change notices: one refresh() per burst, whatever topics it
 * names. A notice for every topic (an empty set: the stream came back, the
 * tab woke, someone signed in again) refreshes the view and reopens the
 * status stream if a sign-out closed it.
 */
const TOPICS = ['models', 'keys', 'requests', 'settings'];
let notices = null;

function onTopics(topics) {
  if (!notices) {
    notices = new Set();
    queueMicrotask(() => {
      const all = notices.has('*');
      const these = notices;
      notices = null;
      if (all) {
        ctx.session = current() || ctx.session;
        connect();
      }
      refresh(all ? null : these);
    });
  }
  if (!topics.size) notices.add('*');
  for (const topic of topics) notices.add(topic);
}

for (const topic of TOPICS) on(topic, onTopics);

/* The live status and the engine log. EventSource reconnects by itself; a refused stream (signed out) is opened again
   by the next notice for every topic, which the shell sends once someone is signed in again. */
let stream = null;

function connect() {
  if (stream && stream.readyState !== EventSource.CLOSED) return;
  stream = api.live({ onStatus: takeStatus, onLog: appendLog });
}

/* ── The engine in the top bar ────────────────────────────────────── */

const engineLink = $('#engine-link');

function takeStatus(status) {
  // Signed out (the shell hid the app): nothing more to show until someone signs in again, which reopens it.
  if (app.hidden && stream) {
    stream.close();
    return;
  }
  ctx.status = status;
  renderEngineLink(status.engine);
  if (ctx.view && ctx.view.status) ctx.view.status(status);
}

function renderEngineLink(engine) {
  const name = engine && engine.modelName ? engine.modelName : 'No model loaded';
  const words = `Engine: ${engine ? engine.message || engine.state : 'unknown'}. ${engine && engine.modelName ? `Model: ${engine.modelName}.` : ''} Open Models.`;
  engineLink.setAttribute('aria-label', words);
  engineLink.setAttribute('data-tip', engine && engine.message ? engine.message : 'Models');
  DS.dom.morphHtml(engineLink, `${engineChip(engine)}<span class="so-engine__model">${esc(name)}</span>`);
}

/* ── The side sheets ──────────────────────────────────────────────── */

/** Opens the sheet with a record of the current view; the view gets its events. */
function openSheet({ title, subtitle = '', html, opener }) {
  ctx.sheetOwner = ctx.view;
  $('#sheet-title').textContent = title;
  $('#sheet-subtitle').textContent = subtitle;
  DS.dom.morphHtml(sheetBody, html);
  sheetBody.scrollTop = 0;
  if (!sheet.open) DS.overlay.open(sheet, { opener });
}

function closeSheet() {
  if (sheet.open) DS.overlay.close(sheet);
}

for (const type of ['click', 'change', 'input', 'submit', 'keydown']) {
  sheetBody.addEventListener(type, (event) => {
    if (type === 'click') {
      const copy = event.target.closest('[data-copy]');
      if (copy) {
        copyFrom(copy);
        return;
      }
    }
    if (ctx.sheetOwner && ctx.sheetOwner.sheet) ctx.sheetOwner.sheet(type, event);
  });
}

/* The engine log: made on the first open from the last lines, then fed by the stream (also while closed). */
let logView = null;
let logLoading = null;

async function openLog(opener) {
  if (!logSheet.open) DS.overlay.open(logSheet, { opener });
  if (logView || logLoading) return;
  logLoading = api.get('/api/logs').catch(() => ({ lines: [] }));
  const { lines } = await logLoading;
  logView = DS.logview.create($('#engine-log'), { lines: (lines || []).join('\n'), follow: true, time: 'off', limit: 5000 });
}

function appendLog(lines) {
  if (logView) logView.append(lines);
}

/* ── Delegated events of the page ────────────────────────────────── */

for (const type of ['click', 'change', 'input', 'submit', 'keydown']) {
  page.addEventListener(type, (event) => {
    if (type === 'click') {
      const copy = event.target.closest('[data-copy]');
      if (copy) {
        copyFrom(copy);
        return;
      }
      if (event.target.closest('[data-act="retry"]')) {
        go(location.hash, { push: false });
        return;
      }
      const log = event.target.closest('[data-act="engine-log"]');
      if (log) {
        openLog(log);
        return;
      }
    }
    if (ctx.view && ctx.view.on) ctx.view.on(type, event);
  });
}

// Copy buttons in dialogs.
document.addEventListener('click', (event) => {
  const copy = event.target.closest('dialog [data-copy]');
  if (copy && !sheet.contains(copy)) copyFrom(copy);
});

// Links to a route navigate in place; every other link loads.
document.addEventListener('click', (event) => {
  const link = event.target.closest('a[href^="#/"]');
  if (!link || event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  event.preventDefault();
  go(link.getAttribute('href'));
});
window.addEventListener('popstate', () => go(location.hash || '#/models', { push: false }));

// An error message goes once the field is corrected.
document.addEventListener('input', (event) => {
  const input = event.target;
  if (input.getAttribute && input.getAttribute('aria-invalid') === 'true') {
    input.removeAttribute('aria-invalid');
    const hint = document.getElementById(`${input.id}-error`);
    if (hint) hint.remove();
  }
});

/* ── Wiring ───────────────────────────────────────────────────────── */

DS.init(document);
DS.icons.fill(document);
for (const element of $$('[data-icon-done]')) element.insertAdjacentHTML('afterbegin', DS.icons.icon(DS.icons.check, 16));
DS.ripple.attach(document);
DS.overlay.tooltips(document);
DS.indicator.create(nav, { active: '[aria-current="page"]', className: 'ds-drawer__indicator' });
DS.dom.scrollEdges(nav, { axis: 'block' });
DS.dom.watchScrolled(view);
DS.dom.followViewport();

menuButton.addEventListener('click', () => {
  if (app.hasAttribute('data-drawer-open')) DS.overlay.close(drawer);
  else DS.overlay.open(drawer, { opener: menuButton });
});

/* The dialog of a new key: Done only once the box says the key is stored. */
const secretStored = $('#secret-stored');
const secretDone = $('#secret-done');
secretStored.addEventListener('change', () => {
  if (secretStored.checked) {
    secretDone.removeAttribute('aria-disabled');
    secretDone.removeAttribute('data-tip');
  } else {
    secretDone.setAttribute('aria-disabled', 'true');
    secretDone.setAttribute('data-tip', 'Tick the box first: the key is not shown again');
  }
});
secretDone.addEventListener('click', () => {
  if (secretDone.getAttribute('aria-disabled') === 'true') return;
  $('#secret-value').value = '';
  DS.overlay.close($('#secret-dialog'));
});

const SKELETON = '<div class="ds-skeleton" role="status"><span class="ds-sr">Loading ...</span>' +
  '<span class="ds-loadbar" aria-hidden="true"><span class="ds-loadbar__bar"></span></span>' +
  '<span class="ds-skeleton__line ds-skeleton__line--heading" aria-hidden="true"></span>' +
  '<span class="ds-skeleton__line ds-skeleton__line--lead" aria-hidden="true"></span>' +
  '<div class="ds-skeleton__card" aria-hidden="true"><span class="ds-skeleton__line ds-skeleton__line--title"></span><span class="ds-skeleton__line"></span>' +
  '<span class="ds-skeleton__line ds-skeleton__line--short"></span><span class="ds-skeleton__line ds-skeleton__line--mid"></span></div>' +
  '<div class="ds-skeleton__card" aria-hidden="true"><span class="ds-skeleton__line ds-skeleton__line--title"></span><span class="ds-skeleton__line"></span>' +
  '<span class="ds-skeleton__line ds-skeleton__line--mid"></span></div></div>';

/* The first load, once the shell has someone signed in: a skeleton while the first view's data comes, then the view
   staggers in once. */
async function start() {
  ctx.session = await startShell();
  const startedAt = Date.now();
  DS.dom.morphHtml(page, SKELETON, { replace: true });
  connect();
  try {
    takeStatus(await api.get('/api/status'));
  } catch (error) {
    renderEngineLink(null);
  }
  const { route, params } = parse(location.hash);
  if (!location.hash) history.replaceState(null, '', `#/${route}`);
  const next = VIEWS[route];
  let data;
  let error = null;
  try {
    data = await next.load(params);
  } catch (err) {
    error = err;
  }
  await M.settleLoader(startedAt, { element: $('.ds-skeleton', page) });
  show(next, route, params, data, error);
  if (!error && next.mount) next.mount(data, params);
  M.staggerOnce(page);
  document.documentElement.setAttribute('data-so-ready', '');
}

start();

export { toast };
