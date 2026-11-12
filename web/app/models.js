/*
 * Models: the active model with the engine's state, what it does right now
 * and where its weights live (GPU, RAM, NVMe), all live from the status
 * stream; then the installed models with their downloads, and the actions
 * activate, settings, retry and delete.
 */
import * as api from './api.js';
import { ctx } from './main.js';
import {
  $, M, esc, icon, head, chip, busyButton, alertHtml, progressHtml, empty, toast, toastError,
  engineChip, engineBusy, modelChip, num, mem, size, span, dur, pct, rate, tps, plural, params,
} from './ui.js';
import { placementHtml, othersOf } from './placement.js';

const DS = window.DS;

const STAGES = { text_model: 'model weights', spec_model: 'draft model', mmproj_model: 'vision projector' };

/** The parts of a meta line, kept apart by the system's dots. */
const parts = (list) => `<span class="ds-parts"><span class="ds-parts__list">${list.filter(Boolean).map((p) => `<span>${p}</span>`).join('')}</span></span>`;

function tags(m) {
  const out = [];
  if (m.meta && m.meta.moe) out.push(`<span class="ds-tag ds-tone-neutral">MoE ${m.meta.experts ? `· ${m.meta.expertsUsed} of ${m.meta.experts} experts` : ''}</span>`);
  if (m.mmproj || (m.meta && m.meta.vision)) out.push('<span class="ds-tag ds-tone-neutral">Vision</span>');
  if (m.meta && m.meta.mtp) out.push('<span class="ds-tag ds-tone-neutral">MTP</span>');
  return out.join(' ');
}

/* ── The active model ─────────────────────────────────────────────── */

/** What the engine does with requests right now, in one line. */
function activityText(s) {
  const a = s.activity;
  if (!a) return '';
  const now = a.requests || [];
  const working = now.find((r) => r.phase !== 'queued');
  const queued = now.filter((r) => r.phase === 'queued').length;
  const who = (r) => (r.source === 'chat' ? 'the chat' : r.key ? `key ${esc(r.key)}` : 'the API');
  let text = '';
  if (working) {
    if (working.phase === 'prompt') text = `Reading a prompt from ${who(working)} · ${num(working.promptDone)} / ${num(working.promptTokens)} tokens · ${tps(working.tps)} tok/s`;
    else text = `${working.phase === 'thinking' ? 'Thinking' : 'Writing'} for ${who(working)} · ${num(working.generated)} tokens · ${tps(working.tps)} tok/s`;
    if (queued) text += ` · ${queued} waiting`;
    return `${icon('activity', 16)}${text}`;
  }
  if (queued) return `${icon('clock', 16)}${plural(queued, 'request')} waiting for the model`;
  if (a.last) {
    const l = a.last;
    return `${icon('checkCircle', 16)}Idle · last answer: ${num(l.promptTokens)} prompt tokens${l.cachedTokens ? ` (${num(l.cachedTokens)} cached)` : ''}, ${num(l.completionTokens)} generated at ${tps(l.genTps)} tok/s in ${dur(l.ms)}`;
  }
  return `${icon('checkCircle', 16)}Idle · no request yet`;
}

function liveHtml(d, s) {
  const e = s.engine;
  if (!e.modelId) {
    const ready = d.models.some((m) => m.state === 'ready');
    return `<div class="ds-card__row"><h2 class="ds-card__title">Active model</h2>${engineChip(e)}</div>` +
      empty({ glyph: 'layers', title: 'No model is loaded',
        text: ready ? 'Activate one of your models below. It stays loaded until you choose another, so the API answers at once.' : 'Add a model from Hugging Face to start. It is downloaded once and kept in the models folder.',
        action: ready ? '' : '<a class="ds-btn ds-btn--tonal" href="#/models/add">Add a model</a>' });
  }
  const m = d.models.find((x) => x.id === e.modelId);
  const facts = [
    m ? esc(m.repo) : '',
    m ? esc(m.quant) : '',
    e.ctx ? `${num(e.ctx)} tokens context` : '',
    e.slots > 1 ? `${e.slots} parallel` : '',
    e.state === 'ready' && e.uptimeMs ? `up ${span(e.uptimeMs)}` : '',
    e.restarts ? plural(e.restarts, 'restart') : '',
    e.version ? `llama.cpp ${esc(e.version)}` : '',
  ];
  let body = '';
  if (engineBusy(e)) {
    const known = e.progress != null;
    const stage = e.state === 'loading' && STAGES[e.stage] ? `Loading the ${STAGES[e.stage]}` : e.message;
    body = progressHtml(e.progress || 0, 'Engine start', `${e.message}${known ? ` · ${pct(e.progress)}` : ''}`,
      `${stage} · for ${span(Date.now() - e.since)}`, !known);
  } else if (e.state === 'error' || e.state === 'crashed') {
    body = alertHtml('error', e.state === 'crashed' ? 'The engine crashed' : 'The engine could not start', esc(e.error || e.message),
      '<button class="ds-btn ds-btn--text ds-sm" type="button" data-act="engine-log">Engine log</button>');
  } else if (e.state === 'stopped') {
    body = alertHtml('', '', 'The engine is stopped. Restart it to load this model again.');
  }
  let placement = '';
  if (s.placement) {
    const p = s.placement;
    const notes = (p.notes || []).filter((n) => !/^Everything fits/.test(n));
    placement = '<div class="ds-stack">' +
      `<div class="ds-card__row"><h3 class="ds-card__subtitle">Where the model lives</h3><span class="ds-meta">${p.source === 'measured' ? `${icon('check', 16)}Measured from the running engine` : `${icon('clock', 16)}Estimated until the engine reports`}</span></div>` +
      placementHtml(p, s.system, othersOf(s), { live: true }) +
      (notes.length ? `<ul class="ds-plain-list ds-muted">${notes.map((n) => `<li>${esc(n)}</li>`).join('')}</ul>` : '') +
      '</div>';
  }
  return `<div class="ds-card__row"><h2 class="ds-card__title">${esc(e.modelName || (m && m.name) || e.modelId)}</h2>${engineChip(e)}</div>` +
    `<p class="ds-meta">${parts(facts)}</p>` +
    (e.summary && !engineBusy(e) ? `<p class="ds-muted">${esc(e.summary)}</p>` : '') +
    (m && tags(m) ? `<p>${tags(m)}</p>` : '') +
    body +
    (e.state === 'ready' ? `<p class="ds-meta" role="status">${activityText(s)}</p>` : '') +
    placement;
}

function actionsKey(s) {
  const e = s.engine;
  return `${e.modelId}|${e.modelId ? 'on' : 'off'}|${(e.args || []).join(' ')}`;
}

function actionsHtml(s) {
  const e = s.engine;
  if (!e.modelId) return '';
  return '<div class="ds-cluster">' +
    busyButton('ds-btn ds-btn--tonal', 'Restart', 'Restarting', 'Restarted', 'data-act="restart"', 'refresh') +
    busyButton('ds-btn', 'Unload', 'Unloading', 'Unloaded', 'data-act="stop"', 'power') +
    `<a class="ds-btn ds-btn--text" href="#/models/settings?id=${encodeURIComponent(e.modelId)}">${icon('sliders', 16)}Settings</a>` +
    `<button class="ds-btn ds-btn--text" type="button" data-act="engine-log">${icon('terminal', 16)}Engine log</button>` +
    '</div>' +
    (e.args && e.args.length
      ? `<details class="ds-collapsible ds-disclosure"><summary>Command line${icon('chevronDown', 20, 'ds-chevron')}</summary><div class="ds-disclosure__body">` +
        `<div class="ds-field__row ds-field__row--top"><pre class="ds-code-block ds-code-block--wrap so-grow" id="engine-args" tabindex="0" aria-label="The llama-server command line"><code>${esc(`llama-server ${e.args.join(' ')}`)}</code></pre>` +
        `<button class="ds-btn ds-btn--quiet ds-btn--icon" type="button" aria-label="Copy the command line" data-tip="Copy" data-copy="engine-args"><span class="ds-icon-swap" data-swap="a"><span class="ds-icon-swap__a">${icon('copy', 20)}</span><span class="ds-icon-swap__b">${icon('check', 20)}</span></span></button></div>` +
        '</div></details>'
      : '');
}

/* ── Installed models ─────────────────────────────────────────────── */

function downloadOf(s, m) {
  return (s.downloads || []).find((x) => x.modelId === m.id) || null;
}

function transferHtml(m, t, s) {
  const done = t ? t.done : m.progress * m.size;
  const total = t ? t.total : m.size;
  const share = total ? done / total : m.progress || 0;
  const left = t && t.bytesPerSec ? (total - done) / t.bytesPerSec : 0;
  const convert = m.state === 'converting' && (s.converts || []).find((c) => c.modelId === m.id);
  if (convert) {
    const overall = ((convert.step - 1) + (convert.progress || 0)) / (convert.steps || 1);
    return progressHtml(overall, `Conversion of ${m.name}`, `Step ${convert.step} of ${convert.steps}: ${convert.title} · ${pct(convert.progress)}`,
      `${convert.message || ''}${t && t.bytesPerSec ? ` · ${rate(t.bytesPerSec)} · ${span(left * 1000)} left` : ''}`);
  }
  if (m.state === 'converting') {
    const phase = t && t.state === 'converting' && t.bytesPerSec ? 'Downloading the full-precision weights' : m.progress >= 0.85 ? 'Quantising' : m.progress >= 0.7 ? 'Converting to GGUF' : 'Preparing';
    return progressHtml(m.progress || 0, `Conversion of ${m.name}`, `${phase} · ${pct(m.progress)}`,
      t && t.bytesPerSec ? `${size(done)} of ${size(total)} · ${rate(t.bytesPerSec)} · ${span(left * 1000)} left · ${t.file}` : (t ? t.file : ''));
  }
  if (m.state === 'queued') return progressHtml(0, `Download of ${m.name}`, 'Waiting to start', '', true);
  return progressHtml(share, `Download of ${m.name}`, `${pct(share)}${t && t.bytesPerSec ? ` · ${rate(t.bytesPerSec)} · ${span(left * 1000)} left` : ''}`,
    `${size(done)} of ${size(total)}${t ? ` · ${t.file}` : ''}`);
}

function rowHtml(model, s) {
  let m = model;
  const active = m.id === s.engine.modelId;
  const working = m.state === 'downloading' || m.state === 'converting' || m.state === 'queued';
  const t = downloadOf(s, m);
  if (t && m.state === 'downloading' && t.total) m = { ...m, progress: t.done / t.total };
  const name = esc(m.name);
  const meta = [esc(m.repo), esc(m.quant), size(m.size), m.meta && m.meta.params ? `${params(m.meta.params)} parameters` : ''];
  const actions = [];
  if (m.state === 'ready' && !active) actions.push(busyButton('ds-btn ds-btn--tonal ds-sm', 'Activate', 'Activating', 'Activated', `data-activate="${esc(m.id)}" aria-label="Activate ${name}"`));
  if (m.state === 'failed') actions.push(busyButton('ds-btn ds-btn--tonal ds-sm', 'Retry', 'Retrying', 'Retrying', `data-retry="${esc(m.id)}" aria-label="Retry ${name}"`, 'refresh'));
  if (m.state === 'ready') actions.push(`<a class="ds-btn ds-btn--quiet ds-btn--icon ds-sm" href="#/models/settings?id=${encodeURIComponent(m.id)}" aria-label="Settings of ${name}" data-tip="Settings">${icon('sliders', 18)}</a>`);
  actions.push(`<button class="ds-btn ds-btn--quiet ds-btn--icon ds-sm" type="button" data-delete="${esc(m.id)}" aria-label="${working ? 'Cancel and delete' : 'Delete'} ${name}" data-tip="${working ? 'Cancel and delete' : 'Delete'}">${icon('trash', 18)}</button>`);
  return `<li class="ds-list__item" data-ds-key="${esc(m.id)}"${active ? ' aria-current="true"' : ''}>${icon(active ? 'zap' : 'layers', 20, 'ds-list__icon')}` +
    '<span class="ds-list__text">' +
      `<span class="ds-list__title">${name}</span>` +
      `<span class="ds-list__meta">${parts(meta)}</span>` +
      (tags(m) ? `<span class="ds-list__meta">${tags(m)}</span>` : '') +
      (working ? `<span class="ds-list__description"><span class="so-transfer">${transferHtml(m, t, s)}</span></span>` : '') +
      (m.state === 'failed' ? `<span class="ds-list__description">${esc(m.error || 'The download failed.')}</span>` : '') +
    '</span>' +
    modelChip(m, active, s.engine) +
    `<span class="ds-list__actions">${actions.join('')}</span></li>`;
}

function sorted(d, s) {
  const rank = (m) => (m.id === s.engine.modelId ? 0 : m.state === 'ready' ? 1 : m.state === 'failed' ? 3 : 2);
  return [...d.models].sort((a, b) => rank(a) - rank(b) || (b.createdAt || 0) - (a.createdAt || 0));
}

function listHtml(d, s) {
  if (!d.models.length) {
    return empty({ glyph: 'download', title: 'No model yet', text: 'Search Hugging Face for a GGUF model, or paste a link to one.',
      action: '<a class="ds-btn ds-btn--tonal" href="#/models/add">Add a model</a>' });
  }
  return `<ul class="ds-list" id="model-list" aria-label="Installed models">${sorted(d, s).map((m) => rowHtml(m, s)).join('')}</ul>`;
}

function freeText(d, s) {
  const free = s.system && s.system.disk ? s.system.disk.free : d.freeBytes;
  return `${icon('hardDrive', 16)}In ${esc(d.dataDir || 'the models folder')} · ${size(free)} free`;
}

/* ── The view ─────────────────────────────────────────────────────── */

let shownActions = '';

const statusOf = () => ctx.status || { engine: { state: 'stopped' }, downloads: [] };

function render(d) {
  const s = statusOf();
  const page = ctx.page;
  const live = $('#active-live', page);
  if (live) DS.dom.morphHtml(live, liveHtml(d, s));
  const key = actionsKey(s);
  const actions = $('#active-actions', page);
  if (actions && key !== shownActions) {
    shownActions = key;
    DS.dom.morphHtml(actions, actionsHtml(s));
  }
  const slot = $('#list-slot', page);
  if (slot) DS.dom.morphHtml(slot, listHtml(d, s));
  const free = $('#free-space', page);
  if (free) DS.dom.morphHtml(free, freeText(d, s));
}

async function act(button, work, done) {
  try {
    await M.busy(button, work);
    if (done) toast(done);
  } catch (error) {
    toastError(error);
  }
}

export default {
  title: 'Models',
  topics: ['models'],
  load: () => api.get('/api/models'),

  html(d) {
    const s = statusOf();
    shownActions = actionsKey(s);
    return head('Models', 'The active model stays loaded and answers the API and the chat. Everything here updates live.',
      `<a class="ds-btn ds-btn--filled" href="#/models/add">${icon('plus', 18)}Add a model</a>`) +
      '<section class="ds-card" aria-label="Active model">' +
        `<div class="ds-stack" id="active-live">${liveHtml(d, s)}</div>` +
        `<div class="ds-stack" id="active-actions">${actionsHtml(s)}</div>` +
      '</section>' +
      '<section class="ds-card" aria-labelledby="installed-title">' +
        `<div class="ds-card__row"><h2 class="ds-card__title" id="installed-title">Installed</h2><span class="ds-meta" id="free-space">${freeText(d, s)}</span></div>` +
        `<div id="list-slot">${listHtml(d, s)}</div>` +
      '</section>';
  },

  update(d) {
    render(d);
  },

  status() {
    if (ctx.data) render(ctx.data);
  },

  async on(type, event) {
    if (type !== 'click') return;
    const t = event.target;
    const d = ctx.data;
    let el;
    if ((el = t.closest('[data-activate]'))) {
      const m = d.models.find((x) => x.id === el.getAttribute('data-activate'));
      act(el, () => api.post(`/api/models/${encodeURIComponent(m.id)}/activate`), `Loading ${m.name}. The engine switches as soon as it is ready.`);
    } else if ((el = t.closest('[data-retry]'))) {
      act(el, () => api.post(`/api/models/${encodeURIComponent(el.getAttribute('data-retry'))}/retry`), 'The download starts again.');
    } else if ((el = t.closest('[data-act="restart"]'))) {
      act(el, () => api.post('/api/engine/restart'), 'The engine restarts.');
    } else if ((el = t.closest('[data-act="stop"]'))) {
      const name = statusOf().engine.modelName;
      const ok = await DS.overlay.confirm({
        title: `Unload ${name}?`,
        text: 'The API and the chat stop answering until you activate a model again. Nothing is deleted.',
        confirmLabel: 'Unload', opener: el,
        onConfirm: () => api.post('/api/engine/stop'),
        errorText: 'The engine could not be stopped. Try again.',
      });
      if (ok) toast(`${name} was unloaded.`);
    } else if ((el = t.closest('[data-delete]'))) {
      const m = d.models.find((x) => x.id === el.getAttribute('data-delete'));
      if (!m) return;
      const active = m.id === statusOf().engine.modelId;
      const working = m.state !== 'ready' && m.state !== 'failed';
      const ok = await DS.overlay.confirm({
        title: `Delete ${m.name}?`,
        text: `${working ? 'The download stops and what came so far' : `Its files (${size(m.size)})`} are removed from the disk.${active ? ' It is the active model: the engine stops and the API no longer answers until you activate another one.' : ''}`,
        confirmLabel: working ? 'Cancel and delete' : `Delete ${m.name}`, destructive: true, opener: el,
        onConfirm: () => api.del(`/api/models/${encodeURIComponent(m.id)}`),
        errorText: `${m.name} could not be deleted. Try again.`,
      });
      if (ok) toast(`${m.name} was deleted.`);
    }
  },
};

export { tags, parts };
