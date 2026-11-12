/*
 * The settings of one model, in four groups (memory and offloading,
 * performance, behaviour, advanced), saved through the save bar; saving
 * restarts the engine when the model is active. "Preview placement" asks
 * the planner what the unsaved settings would put where, without loading
 * anything, and shows it with the same memory bars as the active model.
 */
import * as api from './api.js';
import { ctx } from './main.js';
import {
  $, $$, M, esc, icon, head, chip, busyButton, alertHtml, switchHtml, toast, toastError, showFieldErrors, clearErrors,
  num, mem, size, plural,
} from './ui.js';
import { placementHtml, othersOf, FITS, SPEED } from './placement.js';
import { tags, parts } from './models.js';

const DS = window.DS;
const savebar = DS.overlay.savebar($('#savebar'), { focusTarget: () => $('#settings-form input') });

/*
 * Every setting of docs/internal-api.md: key, kind, label, hint (a function of the model), and where it shows.
 * Number fields left empty are not sent: the server uses its default (shown as the placeholder).
 */
const GROUPS = [
  ['memory', 'Memory and offloading', 'Where the weights and the context go. The fit is automatic; change it only to trade context for speed.'],
  ['performance', 'Performance', ''],
  ['behaviour', 'Behaviour', 'Defaults for requests that do not set them.'],
  ['advanced', 'Advanced', ''],
];

const FIELDS = [
  { key: 'context', group: 'memory', kind: 'context', label: 'Context',
    hint: (m) => `How much text the model sees at once: prompt, history and answer. Automatic takes as much as fits on the GPU, from 32k to 128k; below 32k layers move to RAM instead. This model was trained for ${num(m.meta.ctxTrain)} tokens.` },
  { key: 'gpuLayers', group: 'memory', kind: 'auto', label: 'Layers on the GPU', unit: 'layers', min: 0,
    hint: (m) => `Empty: as many of the ${num(m.meta.layers)} layers as fit. Fewer layers leave VRAM for a longer context, but each layer on the CPU slows the model down.` },
  { key: 'cpuMoe', group: 'memory', kind: 'auto', label: 'MoE: layers whose experts stay in RAM', unit: 'layers', min: 0, show: (m) => m.meta.moe,
    hint: (m) => `The experts of this many layers run on the CPU from RAM, attention stays on the GPU: the fast way to run a MoE model larger than the GPU. Empty: just enough to fit${m.meta.experts ? ` (${m.meta.expertsUsed} of ${m.meta.experts} experts work per token)` : ''}.` },
  { key: 'kvType', group: 'memory', kind: 'select', label: 'KV cache precision', def: 'q8_0',
    options: [['f16', 'f16: full precision, twice the memory of q8_0'], ['q8_0', 'q8_0: half the memory, practically no loss'], ['q4_0', 'q4_0: a quarter of the memory, a small loss; for very long contexts']],
    hint: () => 'The KV cache holds the context. A smaller type fits more context or more layers on the GPU.' },
  { key: 'loadMode', group: 'memory', kind: 'radios', label: 'Weights outside the GPU', def: 'mmap',
    options: [
      ['mmap', 'Memory-mapped (default)', 'They stay in the file and the system\'s page cache: a fast start, and what does not fit into RAM is read from the NVMe when needed (slow, but it runs).'],
      ['mlock', 'Locked in RAM', 'Like memory-mapped, but pinned: never dropped or swapped out, so speed stays even. Needs enough free RAM.'],
      ['ram', 'Copied into RAM', 'Read into the engine\'s own memory at the start. Nothing is read from the NVMe later; the start is slower and needs enough free RAM.'],
    ] },
  { key: 'preload', group: 'memory', kind: 'switch', label: 'Read the RAM part right after the start', def: true,
    hint: () => 'So the first answer is not slowed down by reading weights from the disk.' },
  { key: 'gpuReserveMb', group: 'memory', kind: 'number', label: 'VRAM kept free', unit: 'MB', placeholder: '1024', min: 0, step: 256,
    hint: () => 'What the automatic fit leaves for the desktop and other programs on the GPU.' },

  { key: 'parallel', group: 'performance', kind: 'number', label: 'Requests at the same time', unit: 'slots', placeholder: '1', min: 1, max: 64,
    hint: () => 'They share the context: with 4, each request gets a quarter of it. More than one helps when several apps use the API.' },
  { key: 'threads', group: 'performance', kind: 'number', label: 'CPU threads', unit: 'threads', placeholder: 'Automatic', min: 0,
    hint: () => 'For the layers and experts that run on the CPU. Empty or 0: automatic.' },
  { key: 'batch', group: 'performance', kind: 'number', label: 'Prompt batch', unit: 'tokens', placeholder: '2048', min: 32, step: 256,
    hint: () => 'How many prompt tokens are read per step. Larger reads long prompts faster.' },
  { key: 'ubatch', group: 'performance', kind: 'number', label: 'Physical batch', unit: 'tokens', placeholder: '512', min: 32, step: 128,
    hint: () => 'Larger is faster for prompts on the GPU but needs larger compute buffers.' },
  { key: 'flashAttn', group: 'performance', kind: 'select', label: 'Flash attention', def: 'auto',
    options: [['auto', 'Automatic'], ['on', 'On'], ['off', 'Off']], hint: () => 'Less memory for the context and usually faster. Automatic turns it on where the GPU supports it.' },
  { key: 'speculative', group: 'performance', kind: 'select', label: 'Multi-token prediction', def: (m) => (m.meta.mtp ? 'mtp' : 'off'), show: (m) => m.meta.mtp,
    options: [['mtp', 'On: the model drafts several tokens at once'], ['off', 'Off']], hint: () => 'This model has MTP layers: it guesses the next few tokens and checks them in one step, which answers faster.' },

  { key: 'reasoning', group: 'behaviour', kind: 'select', label: 'Thinking', def: 'auto',
    options: [['auto', 'As the request asks (on by default)'], ['on', 'Always think first'], ['off', 'Never think']], hint: () => 'For models with a thinking mode: they reason before they answer, which helps with hard questions and costs time.' },
  { key: 'reasoningBudget', group: 'behaviour', kind: 'number', label: 'Thinking budget', unit: 'tokens', placeholder: '-1', min: -1,
    hint: () => '-1: unlimited. 0 turns thinking off.' },
  { key: 'vision', group: 'behaviour', kind: 'switch', label: 'Load the image projector', def: true, show: (m) => !!m.mmproj,
    hint: (m) => `Lets the model read images (${esc(m.mmproj)}). Off saves its VRAM.` },
  { key: 'temperature', group: 'behaviour', kind: 'number', label: 'Temperature', placeholder: 'Model card', min: 0, max: 5, step: 0.05, sampling: true },
  { key: 'topP', group: 'behaviour', kind: 'number', label: 'Top P', placeholder: 'Model card', min: 0, max: 1, step: 0.01, sampling: true },
  { key: 'topK', group: 'behaviour', kind: 'number', label: 'Top K', placeholder: 'Model card', min: 0, step: 1, sampling: true },
  { key: 'minP', group: 'behaviour', kind: 'number', label: 'Min P', placeholder: 'Model card', min: 0, max: 1, step: 0.01, sampling: true },
  { key: 'presencePenalty', group: 'behaviour', kind: 'number', label: 'Presence penalty', placeholder: 'Model card', min: -2, max: 2, step: 0.1, sampling: true },
  { key: 'repeatPenalty', group: 'behaviour', kind: 'number', label: 'Repeat penalty', placeholder: 'Model card', min: 0, max: 2, step: 0.05, sampling: true },

  { key: 'extraArgs', group: 'advanced', kind: 'text', label: 'More llama-server flags', placeholder: '--no-warmup --cache-reuse 256',
    hint: () => 'As on a command line. They come last, so they win over what Socrates sets. A wrong flag keeps the engine from starting: the engine log says why.' },
];

const CONTEXTS = [8192, 16384, 32768, 65536, 131072, 262144];

const shown = (f, m) => !f.show || f.show(m);
const defOf = (f, m) => (typeof f.def === 'function' ? f.def(m) : f.def);

function fieldHtml(f, m) {
  const s = m.settings || {};
  const id = `set-${f.key}`;
  const value = s[f.key];
  const hint = f.hint ? f.hint(m) : '';
  const hintAttr = hint ? ` aria-describedby="${id}-hint"` : '';
  const hintHtml = hint ? `<small class="ds-field__hint" id="${id}-hint">${hint}</small>` : '';
  switch (f.kind) {
    case 'context': {
      const v = Number(value) || 0;
      const presets = CONTEXTS.filter((n) => n <= m.meta.ctxTrain);
      const custom = v > 0 && !presets.includes(v);
      return `<div class="ds-field"><label class="ds-field__label" for="${id}">${esc(f.label)}</label>` +
        `<select class="ds-input" id="${id}" name="${f.key}" data-field="${f.key}"${hintAttr}>` +
          `<option value="0"${v === 0 ? ' selected' : ''}>Automatic: as much as fits</option>` +
          presets.map((n) => `<option value="${n}"${v === n ? ' selected' : ''}>${n / 1024}k · ${num(n)} tokens</option>`).join('') +
          `<option value="custom"${custom ? ' selected' : ''}>Custom size ...</option></select>${hintHtml}</div>` +
        `<div class="ds-field" id="context-custom"${custom ? '' : ' hidden'}><label class="ds-field__label" for="set-context-custom">Custom context</label>` +
          `<div class="ds-input-group"><input class="ds-input" id="set-context-custom" name="contextCustom" type="number" inputmode="numeric" min="512" max="${m.meta.ctxTrain}" step="256" data-field="contextCustom" value="${custom ? v : ''}" placeholder="49152">` +
          '<span class="ds-input-group__suffix" aria-hidden="true">tokens</span></div></div>';
    }
    case 'number':
    case 'auto': {
      const shownValue = value === undefined || value === null || value === 'auto' ? '' : String(value);
      const attrs = `${f.min !== undefined ? ` min="${f.min}"` : ''}${f.max !== undefined ? ` max="${f.max}"` : ''} step="${f.step || 'any'}"`;
      const input = `<input class="ds-input" id="${id}" name="${f.key}" type="number" inputmode="decimal"${attrs} data-field="${f.key}" value="${esc(shownValue)}" placeholder="${esc(f.kind === 'auto' ? 'Automatic' : f.placeholder || '')}"${hintAttr}>`;
      return `<div class="ds-field"><label class="ds-field__label" for="${id}">${esc(f.label)}</label>` +
        (f.unit ? `<div class="ds-input-group">${input}<span class="ds-input-group__suffix" aria-hidden="true">${esc(f.unit)}</span></div>` : input) + hintHtml + '</div>';
    }
    case 'select': {
      const current = value ?? defOf(f, m);
      return `<div class="ds-field"><label class="ds-field__label" for="${id}">${esc(f.label)}</label>` +
        `<select class="ds-input" id="${id}" name="${f.key}" data-field="${f.key}"${hintAttr}>${f.options.map(([v, text]) => `<option value="${v}"${v === current ? ' selected' : ''}>${esc(text)}</option>`).join('')}</select>` +
        hintHtml + '</div>';
    }
    case 'radios': {
      const current = value ?? defOf(f, m);
      return `<fieldset class="ds-field"><legend class="ds-field__label">${esc(f.label)}</legend>` +
        `<div class="ds-radios" role="radiogroup" aria-label="${esc(f.label)}">${f.options.map(([v, title, text]) =>
          `<label class="ds-radio"><input type="radio" name="${f.key}" value="${v}"${v === current ? ' checked' : ''}><span><strong>${esc(title)}</strong><small>${esc(text)}</small></span></label>`).join('')}</div></fieldset>`;
    }
    case 'switch':
      return switchHtml({ id, attrs: `name="${f.key}" data-field="${f.key}"`, label: f.label, checked: value ?? defOf(f, m), hint });
    default:
      return `<div class="ds-field"><label class="ds-field__label" for="${id}">${esc(f.label)}</label>` +
        `<input class="ds-input ds-mono" id="${id}" name="${f.key}" data-field="${f.key}" autocomplete="off" spellcheck="false" value="${esc(value || '')}" placeholder="${esc(f.placeholder || '')}"${hintAttr}>${hintHtml}</div>`;
  }
}

function groupHtml([key, title, lead], m) {
  const fields = FIELDS.filter((f) => f.group === key && shown(f, m));
  const plain = fields.filter((f) => !f.sampling);
  const sampling = fields.filter((f) => f.sampling);
  const numbers = plain.filter((f) => ['number', 'auto', 'select', 'context'].includes(f.kind));
  const others = plain.filter((f) => !numbers.includes(f));
  return `<section class="ds-card" aria-labelledby="group-${key}">` +
    `<h2 class="ds-card__title" id="group-${key}">${esc(title)}</h2>${lead ? `<p class="ds-muted">${lead}</p>` : ''}<div class="ds-form">` +
    (numbers.length ? `<div class="ds-form-grid ds-form-grid--2">${numbers.map((f) => fieldHtml(f, m)).join('')}</div>` : '') +
    others.map((f) => fieldHtml(f, m)).join('') +
    (sampling.length ? '<fieldset class="ds-field"><legend class="ds-field__label">Sampling</legend><p class="ds-field__hint">Empty: what the model\'s card recommends. A request\'s own values win.</p>' +
      `<div class="ds-form-grid ds-form-grid--3">${sampling.map((f) => fieldHtml(f, m)).join('')}</div></fieldset>` : '') +
    '</div></section>';
}

function formHtml(m) {
  return '<form class="ds-stack ds-stack--page" id="settings-form" novalidate>' +
    '<section class="ds-card" aria-labelledby="group-general"><h2 class="ds-card__title" id="group-general">Name</h2>' +
      '<div class="ds-field"><label class="ds-field__label ds-sr" for="set-name">Name</label>' +
      `<input class="ds-input" id="set-name" name="name" data-field="name" autocomplete="off" maxlength="80" value="${esc(m.name)}"></div></section>` +
    GROUPS.map((g) => groupHtml(g, m)).join('') +
    '</form>';
}

/* ── What the form holds ──────────────────────────────────────────── */

/** The settings as the form shows them now, as the server takes them. */
function collect(m) {
  const form = $('#settings-form');
  const out = {};
  for (const f of FIELDS) {
    if (!shown(f, m)) continue;
    if (f.kind === 'radios') {
      const checked = $(`input[name="${f.key}"]:checked`, form);
      if (checked) out[f.key] = checked.value;
      continue;
    }
    const input = $(`[name="${f.key}"]`, form);
    if (!input) continue;
    if (f.kind === 'context') {
      const custom = $('#set-context-custom', form);
      out[f.key] = input.value === 'custom' ? Math.round(Number(custom.value) || 0) : Number(input.value);
      continue;
    }
    if (f.kind === 'switch') out[f.key] = input.checked;
    else if (f.kind === 'select' || f.kind === 'text') {
      const value = input.value.trim();
      if (f.kind === 'select' || value) out[f.key] = value;
    } else {
      const raw = input.value.trim();
      if (raw === '') {
        if (f.kind === 'auto') out[f.key] = 'auto';
      } else {
        const n = Number(raw);
        out[f.key] = Number.isFinite(n) ? n : raw;
      }
    }
  }
  return { name: $('#set-name', form).value.trim(), settings: out };
}

const state = { saved: '', model: null, plan: null, planning: 0 };

function sync() {
  const m = state.model;
  if (!m || !$('#settings-form')) return;
  const now = collect(m);
  const before = JSON.parse(state.saved);
  let changed = now.name !== before.name ? 1 : 0;
  const keys = new Set([...Object.keys(now.settings), ...Object.keys(before.settings)]);
  for (const key of keys) if (JSON.stringify(now.settings[key]) !== JSON.stringify(before.settings[key])) changed += 1;
  if (changed) {
    savebar.update(() => {
      $('#savebar-text').textContent = `${plural(changed, 'unsaved change')}${isActive(m) ? ' · saving restarts the engine' : ''}`;
      $('#savebar-label').textContent = isActive(m) ? 'Save and restart' : 'Save';
    });
    savebar.show();
  } else {
    savebar.hide();
  }
}

const isActive = (m) => !!ctx.status && ctx.status.engine.modelId === m.id;

function settle(m) {
  state.model = m;
  state.saved = JSON.stringify(collect(m));
}

/* ── The preview ──────────────────────────────────────────────────── */

function planHtml(plan) {
  if (!plan) {
    return `<p class="ds-muted">${icon('info', 16)} Shows where the weights, the context and the buffers would go with the settings on this page, and whether it fits. Nothing is loaded.</p>`;
  }
  if (plan.failed) return alertHtml('error', 'The planner did not answer', esc(plan.failed));
  const m = state.model;
  const [kind, label, glyph] = FITS[plan.fits] || FITS.no;
  const s = ctx.status || {};
  return '<div class="ds-stack">' +
    `<div class="ds-cluster">${chip(kind, label, glyph)}${plan.tokensPerSecondHint ? chip('', SPEED[plan.tokensPerSecondHint] || plan.tokensPerSecondHint, 'zap') : ''}</div>` +
    (plan.ok ? `<p>${esc(plan.summary)}</p>` : alertHtml('error', 'This does not fit', esc(plan.error || plan.summary))) +
    '<ul class="ds-kv-list">' +
      `<li><span>Context</span><strong>${num(plan.ctx)} tokens</strong></li>` +
      `<li><span>Layers on the GPU</span><strong>${num(plan.gpuLayers)} of ${num(m.meta.layers)}</strong></li>` +
      (m.meta.moe ? `<li><span>Experts in RAM</span><strong>${plan.cpuMoe ? plural(plan.cpuMoe, 'layer') : 'none'}</strong></li>` : '') +
    '</ul>' +
    (plan.placement ? placementHtml(plan.placement, s.system, othersOf(s)) : '') +
    (plan.placement && plan.placement.notes && plan.placement.notes.length ? `<ul class="ds-plain-list ds-muted">${plan.placement.notes.map((n) => `<li>${esc(n)}</li>`).join('')}</ul>` : '') +
    (plan.args && plan.args.length
      ? `<details class="ds-collapsible ds-disclosure"><summary>Command line${icon('chevronDown', 20, 'ds-chevron')}</summary><div class="ds-disclosure__body">` +
        `<pre class="ds-code-block ds-code-block--wrap" tabindex="0" aria-label="The planned command line"><code>${esc(`llama-server ${plan.args.join(' ')}`)}</code></pre></div></details>`
      : '') +
    '</div>';
}

async function preview(button) {
  const m = state.model;
  const { settings } = collect(m);
  const seq = ++state.planning;
  try {
    const plan = await M.busy(button, () => api.post(`/api/models/${encodeURIComponent(m.id)}/plan`, { settings }));
    if (seq !== state.planning) return;
    state.plan = plan;
  } catch (error) {
    if (seq !== state.planning) return;
    if (error.fields && Object.keys(error.fields).length) {
      showFieldErrors($('#settings-form'), error.fields);
      return;
    }
    state.plan = { failed: error.message };
  }
  const slot = $('#plan-slot');
  if (slot) DS.dom.morphHtml(slot, planHtml(state.plan));
  DS.overlay.announce(state.plan.failed ? 'The preview failed.' : `Preview: ${state.plan.summary || state.plan.error}`);
}

/* The save bar's discard: back to what is saved. */
$('#savebar-discard').addEventListener('click', () => {
  if (ctx.view !== view || !state.model) return;
  clearErrors($('#settings-form'));
  DS.dom.morphHtml($('#settings-slot'), formHtml(state.model));
  for (const form of $$('#settings-form')) form.reset();
  savebar.hide();
});

/* ── The view ─────────────────────────────────────────────────────── */

const view = {
  title: (d) => (d ? `Settings of ${d.model.name}` : 'Model settings'),
  nav: 'models',
  topics: ['models'],

  async load(params) {
    const id = params.get('id') || '';
    const { models } = await api.get('/api/models');
    const model = models.find((m) => m.id === id);
    if (!model) throw new Error('This model does not exist (any more). It may have been deleted.');
    return { model };
  },

  html(d) {
    const m = d.model;
    state.plan = null;
    state.model = m;
    const facts = [esc(m.repo), esc(m.quant), size(m.size), `${num(m.meta.layers)} layers`, `trained for ${num(m.meta.ctxTrain)} tokens`];
    return head(`Settings of ${m.name}`, `${parts(facts)} ${tags(m)}`, '', { href: '#/models', label: 'Models' }) +
      (isActive(m) ? alertHtml('', '', 'This model is active. Saving restarts the engine with the new settings; requests that run at that moment are cut off.') : '') +
      '<div class="ds-split ds-split--stretch so-settings">' +
        `<div class="ds-split__main" id="settings-slot">${formHtml(m)}</div>` +
        '<aside class="ds-split__side" aria-labelledby="preview-title"><section class="ds-card so-sticky">' +
          '<h2 class="ds-card__title" id="preview-title">Preview placement</h2>' +
          `<div id="plan-slot">${planHtml(null)}</div>` +
          `<div class="ds-cluster">${busyButton('ds-btn ds-btn--tonal', 'Preview placement', 'Planning', 'Planned', 'data-act="preview"', 'layers')}</div>` +
        '</section></aside>' +
      '</div>';
  },

  mount(d) {
    settle(d.model);
    savebar.hide();
  },

  update(d) {
    // Another browser saved, or the model went: keep the edits here, take the new saved state only when nothing is edited.
    if (!savebar.shown) {
      DS.dom.morphHtml($('#settings-slot'), formHtml(d.model));
      settle(d.model);
    } else {
      state.model = { ...d.model, settings: state.model.settings };
    }
  },

  leave() {
    savebar.hide();
    state.planning += 1;
  },

  async on(type, event) {
    const t = event.target;
    if ((type === 'input' || type === 'change') && t.closest('#settings-form')) {
      if (t.id === 'set-context') {
        const custom = $('#context-custom', ctx.page);
        custom.hidden = t.value !== 'custom';
        if (!custom.hidden && type === 'change') $('#set-context-custom', ctx.page).focus();
      }
      sync();
      return;
    }
    if (type === 'click') {
      const button = t.closest('[data-act="preview"]');
      if (button) preview(button);
      return;
    }
    if (type === 'submit' && t.id === 'settings-form') {
      event.preventDefault();
      const m = state.model;
      const form = t;
      clearErrors(form);
      const body = collect(m);
      const active = isActive(m);
      try {
        const saved = await M.busy($('#savebar-save'), () => api.patch(`/api/models/${encodeURIComponent(m.id)}`, body));
        settle(saved && saved.id ? saved : { ...m, name: body.name, settings: body.settings });
        savebar.hide();
        toast(active ? 'Saved. The engine restarts with the new settings.' : 'Saved. The settings apply when this model is activated.');
      } catch (error) {
        if (!showFieldErrors(form, error.fields)) toastError(error);
      }
    }
  },
};

export default view;
