/*
 * Add a model: search Hugging Face (debounced) or paste owner/name or a
 * huggingface.co link; a repository shows its quantisations as radio cards
 * with their size and how they fit this machine, the vision projector and
 * a name, then Download. A repository without GGUF files points to GGUF
 * versions by others and, where llama.cpp can convert it, offers to convert
 * it here.
 */
import * as api from './api.js';
import { ctx } from './main.js';
import {
  $, M, esc, icon, head, chip, busyButton, alertHtml, empty, toast, toastError, showFieldErrors, clearErrors,
  num, size, params, ago,
} from './ui.js';
import { FITS } from './placement.js';

const DS = window.DS;

const QUANTS = [
  ['Q4_K_M', 4.85, 'Small and good: the usual choice'],
  ['Q5_K_M', 5.7, 'A little better, a little larger'],
  ['Q6_K', 6.6, 'Close to the original'],
  ['Q8_0', 8.5, 'Practically the original, twice the size of Q4'],
];

const state = { query: '', timer: 0, seq: 0, nameTouched: false, prefetched: null };

/** A repository id from what was typed or pasted: owner/name, or a link to huggingface.co. */
export function repoFrom(text) {
  const t = String(text || '').trim();
  const link = /^(?:https?:\/\/)?(?:www\.)?(?:huggingface\.co|hf\.co)\/([\w.-]+\/[\w.-]+)/i.exec(t);
  if (link) return link[1].replace(/\.git$/, '');
  if (/^[\w.-]+\/[\w.-]+$/.test(t) && !/^\.|\.$/.test(t)) return t;
  return '';
}

const shortName = (id) => id.split('/')[1].replace(/-GGUF$/i, '').replace(/[-_]+/g, ' ');

/* ── Search ───────────────────────────────────────────────────────── */

function resultsHtml(found, query) {
  if (!found) return '';
  const direct = repoFrom(query);
  if (direct && !found.some((m) => m.id.toLowerCase() === direct.toLowerCase())) {
    // A repository typed or pasted that the search does not list (private to the search, or very new): open it as it is.
    found = [{ id: direct, downloads: 0, likes: 0, gguf: /gguf/i.test(direct), direct: true }, ...found];
  }
  if (!found.length) {
    return empty({ glyph: 'search', title: 'Nothing found', text: `Hugging Face has no model matching “${esc(query)}”. Try fewer words, or paste the repository's name.` });
  }
  const back = query ? `&q=${encodeURIComponent(query)}` : '';
  return `<p class="ds-meta" id="hf-results-title">${query ? `Results for “${esc(query)}”` : 'Popular GGUF models'}</p>` +
    `<ul class="ds-list ds-list--interactive" aria-labelledby="hf-results-title">${found.slice(0, 20).map((m) =>
      `<li class="ds-list__item" data-ds-key="${esc(m.id)}">${icon(m.gguf ? 'box' : 'fileCode', 20, 'ds-list__icon')}` +
      `<span class="ds-list__text"><span class="ds-list__title"><a class="ds-list__action" href="#/models/add?repo=${encodeURIComponent(m.id)}${back}">${esc(m.id)}</a></span>` +
      (m.direct ? '<span class="ds-list__meta">Open this repository</span></span>'
        : `<span class="ds-list__meta"><span class="ds-parts"><span class="ds-parts__list"><span>${icon('download', 14)} ${num(m.downloads)}</span><span>${icon('heart', 14)} ${num(m.likes)}</span>` +
        `${m.pipeline ? `<span>${esc(m.pipeline)}</span>` : ''}${m.updatedAt ? `<span>updated ${ago(m.updatedAt)}</span>` : ''}</span></span></span></span>` +
        `${m.gguf ? '<span class="ds-tag">GGUF</span>' : '<span class="ds-tag ds-tone-neutral">No GGUF</span>'}`) +
      `${icon('chevronRight', 20, 'ds-list__chevron')}</li>`).join('')}</ul>`;
}

async function search(query) {
  const seq = ++state.seq;
  const slot = $('#hf-results', ctx.page);
  if (!slot) return;
  slot.setAttribute('aria-busy', 'true');
  slot.classList.add('ds-stale');
  let found;
  let error = null;
  try {
    found = (await api.get(`/api/hf/search?q=${encodeURIComponent(query || 'GGUF')}`)).models;
  } catch (err) {
    error = err;
  }
  if (seq !== state.seq || !slot.isConnected) return;
  slot.removeAttribute('aria-busy');
  slot.classList.remove('ds-stale');
  DS.dom.morphHtml(slot, error ? alertHtml('error', 'Hugging Face could not be searched', esc(error.message)) : resultsHtml(found, query));
}

/** Opens a repository named by owner/name if Hugging Face knows it; false (and nothing changes) if not. */
async function openRepo(id) {
  const seq = ++state.seq;
  const slot = $('#hf-results', ctx.page);
  if (slot) slot.classList.add('ds-stale');
  let repo = null;
  try {
    repo = await api.get(`/api/hf/repo?id=${encodeURIComponent(id)}`);
  } catch (error) {
    repo = null;
  }
  if (slot) slot.classList.remove('ds-stale');
  if (seq !== state.seq) return true;
  if (!repo) return false;
  state.prefetched = repo;
  ctx.go(`#/models/add?repo=${encodeURIComponent(repo.id || id)}`);
  return true;
}

/* ── A repository ─────────────────────────────────────────────────── */

function installedQuant(d, repo, quant) {
  return d.models.some((m) => m.repo === repo && m.quant === quant);
}

/*
 * The variants shown at once: the common K-quants and Q8_0 that run here, under 16 bits. The rest (I-quants, legacy
 * Q4_0 ... Q5_1, _S and _L flavours, full precision, unknown formats, too large) waits behind "Show all".
 */
const COMMON = /^(UD-)?Q[2-6]_K(_M|_XL)?$|^(UD-)?Q8_(0|K_XL)$/i;
const usual = (v) => v.fit !== 'no' && v.bits > 0 && v.bits < 16;
const common = (v) => usual(v) && COMMON.test(v.quant);

/** The variant's note without what its fit chip says already ("Fits on the GPU with 32k context" → "With 32k context"). */
function noteOf(v) {
  const [, label] = FITS[v.fit] || FITS.no;
  let note = String(v.note || '').trim();
  if (note.toLowerCase().startsWith(label.toLowerCase())) note = note.slice(label.length).replace(/^[\s,:;.-]+/, '');
  else if (v.fit === 'gpu') note = note.replace(/^fits on the gpu\s*/i, '');
  return note ? note[0].toUpperCase() + note.slice(1) : '';
}

function fitChipHtml(v) {
  const [kind, label, glyph] = FITS[v.fit] || FITS.no;
  return chip(kind, label, glyph);
}

/* One line per variant: quant · size · fit; the note only as the tooltip, it shows below the list once chosen. */
function variantRow(d, r, v, i, checked) {
  const installed = installedQuant(d, r.id, v.quant);
  const note = noteOf(v);
  return `<div><label class="ds-check"${note ? ` data-tip="${esc(note)}"` : ''}><input type="radio" name="variant" value="${i}"${checked ? ' checked' : ''}>` +
    `<span>${esc(v.quant)} · ${size(v.size)}${v.files.length > 1 ? ` · ${v.files.length} files` : ''}${installed ? ' · installed' : ''}</span> ${fitChipHtml(v)}</label></div>`;
}

function chosenHtml(v) {
  if (!v) return '';
  const note = noteOf(v);
  return `${fitChipHtml(v)} <span class="ds-meta">${esc(v.quant)} · ${size(v.size)}${note ? ` · ${esc(note)}` : ''}</span>`;
}

function variantsHtml(d, r) {
  const indexed = r.variants.map((v, i) => ({ v, i })).sort((a, b) => a.v.size - b.v.size);
  const best = indexed.find(({ v }) => v.recommended) || indexed.find(({ v }) => usual(v)) || indexed[0];
  const rest = indexed.filter((x) => x !== best);
  const shown = rest.filter(({ v }) => common(v));
  const hidden = rest.filter(({ v }) => !common(v));
  const b = best.v;
  const bestNote = noteOf(b);
  return '<fieldset class="ds-field"><legend class="ds-field__label">Quantisation</legend>' +
    '<p class="ds-field__hint">Smaller files fit better and run faster; larger ones answer a little better. The fit is for this machine.</p>' +
    '<div class="ds-stack">' +
      `<div class="ds-radios"><label class="ds-radio"><input type="radio" name="variant" value="${best.i}" checked>` +
        `<span><strong>${esc(b.quant)} · ${size(b.size)}</strong>` +
        `<small>${icon('star', 16)} ${b.recommended ? 'Recommended for this machine' : 'A good start'}${installedQuant(d, r.id, b.quant) ? ' · installed already' : ''}</small>` +
        `<small>${icon((FITS[b.fit] || FITS.no)[2], 16)} <b>${esc((FITS[b.fit] || FITS.no)[1])}</b>${bestNote ? ` · ${esc(bestNote)}` : ''}</small></span></label></div>` +
      (shown.length ? `<div role="group" aria-label="Other sizes"><p class="ds-meta">Other sizes</p>${shown.map(({ v, i }) => variantRow(d, r, v, i, false)).join('')}</div>` : '') +
      (hidden.length
        ? `<details class="ds-collapsible ds-disclosure"><summary>Show all ${r.variants.length} (${hidden.length} more: I-quants, legacy, full precision, too large)${icon('chevronDown', 20, 'ds-chevron')}</summary>` +
          `<div class="ds-disclosure__body" role="group" aria-label="More sizes">${hidden.map(({ v, i }) => variantRow(d, r, v, i, false)).join('')}</div></details>`
        : '') +
      `<p id="add-chosen" aria-live="polite" data-best="${best.i}"></p>` +
    '</div></fieldset>';
}

function visionHtml(r) {
  if (!r.mmproj || !r.mmproj.length) return '';
  if (r.mmproj.length === 1) {
    const p = r.mmproj[0];
    return `<label class="ds-check"><input type="checkbox" id="add-mmproj" value="${esc(p.file)}" checked><span>Vision: also download the image projector (${esc(p.file)}, ${size(p.size)})</span></label>`;
  }
  return '<div class="ds-field"><label class="ds-field__label" for="add-mmproj">Vision</label>' +
    `<select class="ds-input" id="add-mmproj" aria-describedby="add-mmproj-hint">${r.mmproj.map((p, i) => `<option value="${esc(p.file)}"${i === 0 ? ' selected' : ''}>${esc(p.file)} · ${size(p.size)}</option>`).join('')}<option value="">No vision</option></select>` +
    '<small class="ds-field__hint" id="add-mmproj-hint">The image projector lets the model read pictures. It needs a little more memory.</small></div>';
}

function ggufHtml(d, r) {
  const pick = r.variants.find((v) => v.recommended) || r.variants[0];
  const free = d.freeBytes;
  return '<form class="ds-form" id="add-form" novalidate>' +
    variantsHtml(d, r) +
    visionHtml(r) +
    '<div class="ds-field"><label class="ds-field__label" for="add-name">Name</label>' +
      `<input class="ds-input" id="add-name" data-field="name" autocomplete="off" maxlength="80" value="${esc(pick ? `${shortName(r.id)} · ${pick.quant}` : shortName(r.id))}" aria-describedby="add-name-hint">` +
      '<small class="ds-field__hint" id="add-name-hint">How Socrates shows it. The API uses a short id made from it.</small></div>' +
    `<div class="ds-form-actions">${busyButton('ds-btn ds-btn--filled', 'Download', 'Starting', 'Started', 'type="submit"', 'download')}` +
      `<p class="ds-meta" id="add-room">${roomText(pick, free)}</p></div>` +
    '</form>';
}

function roomText(v, free) {
  if (!v) return '';
  if (free && v.size > free) return `${icon('alert', 16)}Needs ${size(v.size)}, but only ${size(free)} are free on the disk.`;
  return `${icon('hardDrive', 16)}Downloads ${size(v.size)} · ${size(free)} free`;
}

function convertHtml(r) {
  const full = r.params ? r.params * 2 : 0;
  const q = QUANTS[0];
  return '<section class="ds-card ds-card--filled" aria-labelledby="convert-title">' +
    '<h3 class="ds-card__subtitle" id="convert-title">Convert it myself</h3>' +
    `<p class="ds-muted">Socrates downloads the full-precision weights${full ? ` (${size(full)})` : ''}, converts them to GGUF and quantises them on this machine. ` +
    `That takes a while and needs ${full ? `about ${size(full + (r.params * q[1]) / 8)}` : 'a lot of'} free disk space while it runs; afterwards only the quantised file stays.</p>` +
    '<form class="ds-form" id="convert-form" novalidate><div class="ds-form-grid ds-form-grid--2">' +
      '<div class="ds-field"><label class="ds-field__label" for="convert-quant">Quantisation</label>' +
        `<select class="ds-input" id="convert-quant">${QUANTS.map(([name, bits, text]) => `<option value="${name}">${name}${r.params ? ` · about ${size((r.params * bits) / 8)}` : ''}: ${text}</option>`).join('')}</select></div>` +
      '<div class="ds-field"><label class="ds-field__label" for="convert-name">Name</label>' +
        `<input class="ds-input" id="convert-name" data-field="name" autocomplete="off" maxlength="80" value="${esc(`${shortName(r.id)} · ${q[0]}`)}"></div>` +
    `</div><div class="ds-form-actions">${busyButton('ds-btn ds-btn--tonal', 'Convert', 'Starting', 'Started', 'type="submit"', 'wand')}</div></form>` +
    '</section>';
}

function noGgufHtml(r) {
  const others = r.quantized || [];
  let out = alertHtml('', 'This repository has no GGUF files',
    `Socrates runs models in the GGUF format. ${others.length ? 'Others have published GGUF versions of it: pick one below.' : ''}${r.convertible ? ' You can also convert it yourself.' : ''}`);
  if (others.length) {
    out += '<h3 class="ds-card__subtitle" id="others-title">GGUF versions by others</h3>' +
      `<ul class="ds-list ds-list--interactive" aria-labelledby="others-title">${others.map((o) =>
        `<li class="ds-list__item" data-ds-key="${esc(o.id)}">${icon('box', 20, 'ds-list__icon')}<span class="ds-list__text"><span class="ds-list__title">` +
        `<a class="ds-list__action" href="#/models/add?repo=${encodeURIComponent(o.id)}">${esc(o.id)}</a></span><span class="ds-list__meta">${icon('download', 14)} ${num(o.downloads)} downloads</span></span>` +
        `${icon('chevronRight', 20, 'ds-list__chevron')}</li>`).join('')}</ul>`;
  }
  if (r.convertible) out += convertHtml(r);
  else if (!others.length) {
    out += empty({ glyph: 'ban', title: 'This model cannot be used here', text: 'llama.cpp cannot convert its architecture, and nobody has published a GGUF version of it yet.' });
  }
  return out;
}

function repoHtml(d) {
  if (d.repoError) {
    return '<section class="ds-card" id="repo-card">' + alertHtml('error', 'This repository could not be opened', esc(d.repoError)) + '</section>';
  }
  const r = d.repo;
  if (!r) return '';
  const facts = [
    r.params ? `${params(r.params)} parameters` : '',
    r.pipeline ? esc(r.pipeline) : '',
    r.license ? `licence ${esc(r.license)}` : '',
    r.baseModel ? `based on <a href="#/models/add?repo=${encodeURIComponent(r.baseModel)}">${esc(r.baseModel)}</a>` : '',
  ].filter(Boolean);
  const gated = r.gated && !d.settings.hfToken
    ? alertHtml('warning', 'This model is gated', 'Accept its licence on Hugging Face, then add a Hugging Face token on the <a href="#/api">API page</a>. Without it the download fails.')
    : '';
  return '<section class="ds-card" id="repo-card" aria-labelledby="repo-title">' +
    `<div class="ds-card__row"><h2 class="ds-card__title" id="repo-title">${esc(r.id)}</h2>` +
      `<a class="ds-btn ds-btn--text ds-sm" href="https://huggingface.co/${esc(r.id)}" target="_blank" rel="noopener">Hugging Face${icon('external', 16)}</a></div>` +
    `<p class="ds-meta"><span class="ds-parts"><span class="ds-parts__list">${facts.map((f) => `<span>${f}</span>`).join('')}</span></span></p>` +
    gated +
    (r.gguf ? (r.variants.length ? ggufHtml(d, r) : alertHtml('warning', '', 'This repository has no GGUF file that Socrates can load.')) : noGgufHtml(r)) +
    '</section>';
}

/* ── The view ─────────────────────────────────────────────────────── */

export default {
  title: 'Add a model',
  nav: 'models',
  topics: ['settings'],

  async load(params) {
    const id = params.get('repo') || '';
    const known = state.prefetched && state.prefetched.id.toLowerCase() === id.toLowerCase() ? state.prefetched : null;
    state.prefetched = null;
    const [models, settings, repo] = await Promise.all([
      api.get('/api/models'),
      api.get('/api/settings'),
      known ? Promise.resolve({ r: known }) : id ? api.get(`/api/hf/repo?id=${encodeURIComponent(id)}`).then((r) => ({ r }), (error) => ({ error })) : Promise.resolve({}),
    ]);
    return { models: models.models, freeBytes: models.freeBytes, dataDir: models.dataDir, settings, repo: repo.r || null, repoError: repo.error ? repo.error.message : '', q: params.get('q') || '' };
  },

  html(d, params) {
    state.nameTouched = false;
    const q = params.get('q') || '';
    const repo = params.get('repo') || '';
    return head('Add a model', 'Search Hugging Face, or paste a repository such as <code>owner/name</code> or a link to huggingface.co.', '', { href: '#/models', label: 'Models' }) +
      '<section class="ds-card" aria-label="Find a model">' +
        '<form id="hf-search-form" role="search" novalidate>' +
          `<div class="ds-search">${icon('search', 20)}<input class="ds-input" id="hf-q" type="search" autocomplete="off" spellcheck="false" aria-label="Search Hugging Face" placeholder="Search, or paste owner/name or a link" value="${esc(q)}"></div>` +
        '</form>' +
        `<div id="hf-results" class="ds-stack"${repo ? ' hidden' : ''}></div>` +
        (repo ? `<p class="ds-meta"><a href="#/models/add${q ? `?q=${encodeURIComponent(q)}` : ''}">${icon('arrowLeft', 14)} Back to the results</a></p>` : '') +
      '</section>' +
      `<div id="repo-slot">${repoHtml(d)}</div>`;
  },

  mount(d, params) {
    if (!params.get('repo')) {
      search(params.get('q') || '');
      $('#hf-q', ctx.page).focus();
    }
  },

  update(d) {
    // A Hugging Face token was added: the gated notice may go.
    const slot = $('#repo-slot', ctx.page);
    if (slot && d.repo && d.repo.gated) DS.dom.morphHtml(slot, repoHtml(d));
  },

  leave() {
    clearTimeout(state.timer);
    state.seq += 1;
  },

  async on(type, event) {
    const t = event.target;
    const d = ctx.data;
    if (type === 'input' && t.id === 'hf-q') {
      clearTimeout(state.timer);
      // A pasted owner/name or link opens its repository at once; typed text searches (a half-typed name may exist too).
      const pasted = event.inputType === 'insertFromPaste' || event.inputType === 'insertFromDrop' || /huggingface\.co\/|hf\.co\//i.test(t.value);
      state.timer = setTimeout(async () => {
        const q = t.value.trim();
        if (pasted && repoFrom(q) && await openRepo(repoFrom(q))) return;
        history.replaceState(null, '', `#/models/add${q ? `?q=${encodeURIComponent(q)}` : ''}`);
        const results = $('#hf-results', ctx.page);
        if (results && results.hidden) {
          ctx.go(`#/models/add${q ? `?q=${encodeURIComponent(q)}` : ''}`, { replace: true });
          return;
        }
        search(q);
      }, 350);
      return;
    }
    if (type === 'submit' && t.id === 'hf-search-form') {
      event.preventDefault();
      clearTimeout(state.timer);
      const value = $('#hf-q', ctx.page).value;
      const repo = repoFrom(value);
      if (repo && await openRepo(repo)) return;
      if (repo) search(value.trim());
      else ctx.go(`#/models/add${value.trim() ? `?q=${encodeURIComponent(value.trim())}` : ''}`, { replace: true });
      return;
    }
    if (type === 'input' && t.id === 'add-name') {
      state.nameTouched = true;
      return;
    }
    if (type === 'change' && t.name === 'variant') {
      const v = d.repo.variants[Number(t.value)];
      const name = $('#add-name', ctx.page);
      if (name && !state.nameTouched) name.value = `${shortName(d.repo.id)} · ${v.quant}`;
      DS.dom.morphHtml($('#add-room', ctx.page), roomText(v, d.freeBytes));
      const chosen = $('#add-chosen', ctx.page);
      DS.dom.morphHtml(chosen, t.value === chosen.getAttribute('data-best') ? '' : `Chosen: ${chosenHtml(v)}`);
      return;
    }
    if (type === 'change' && t.id === 'convert-quant') {
      const name = $('#convert-name', ctx.page);
      if (name && !state.nameTouched) name.value = `${shortName(d.repo.id)} · ${t.value}`;
      return;
    }
    if (type === 'submit' && t.id === 'add-form') {
      event.preventDefault();
      const form = t;
      clearErrors(form);
      const checked = $('input[name="variant"]:checked', form);
      if (!checked) {
        toast('Choose a quantisation first.', { kind: 'error' });
        return;
      }
      const v = d.repo.variants[Number(checked.value)];
      const vision = $('#add-mmproj', form);
      const mmproj = !vision ? '' : vision.type === 'checkbox' ? (vision.checked ? vision.value : '') : vision.value;
      const name = $('#add-name', form).value.trim();
      try {
        const model = await M.busy($('[type="submit"]', form), () => api.post('/api/models', { repo: d.repo.id, files: v.files, mmproj, name }));
        toast(`Downloading ${model.name || name}. You can follow it on the Models page.`);
        ctx.go('#/models');
      } catch (error) {
        if (!showFieldErrors(form, error.fields)) toastError(error);
      }
      return;
    }
    if (type === 'submit' && t.id === 'convert-form') {
      event.preventDefault();
      const form = t;
      clearErrors(form);
      const name = $('#convert-name', form).value.trim();
      try {
        const model = await M.busy($('[type="submit"]', form), () => api.post('/api/models/convert', { repo: d.repo.id, quant: $('#convert-quant', form).value, name }));
        toast(`Converting ${model.name || name}. It shows on the Models page while it works.`);
        ctx.go('#/models');
      } catch (error) {
        if (!showFieldErrors(form, error.fields)) toastError(error);
      }
    }
  },
};

