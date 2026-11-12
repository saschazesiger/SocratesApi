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
  $, M, esc, icon, head, busyButton, alertHtml, empty, toast, toastError, showFieldErrors, clearErrors,
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

const state = { query: '', timer: 0, seq: 0, nameTouched: false };

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

/* ── A repository ─────────────────────────────────────────────────── */

function installedQuant(d, repo, quant) {
  return d.models.some((m) => m.repo === repo && m.quant === quant);
}

function variantsHtml(d, r) {
  const pickIndex = Math.max(0, r.variants.findIndex((v) => v.recommended));
  return `<fieldset class="ds-field"><legend class="ds-field__label">Quantisation</legend>` +
    '<p class="ds-field__hint">Smaller files fit better and run faster; larger ones answer a little better. The fit is for this machine with the default 32k context.</p>' +
    `<div class="ds-radios so-variants" role="radiogroup" aria-label="Quantisation">${r.variants.map((v, i) => {
      const installed = installedQuant(d, r.id, v.quant);
      const [, fitLabel, fitGlyph] = FITS[v.fit] || FITS.no;
      return `<label class="ds-radio"><input type="radio" name="variant" value="${i}"${i === pickIndex ? ' checked' : ''}>` +
        `<span><strong>${esc(v.quant)} · ${size(v.size)}</strong>` +
        (v.recommended ? `<small>${icon('star', 16)} Recommended for this machine</small>` : '') +
        (installed ? `<small>${icon('check', 16)} Installed already</small>` : '') +
        `<small>${icon(fitGlyph, 16)} <b>${esc(fitLabel)}</b></small><small>${esc(v.note)}${v.files.length > 1 ? ` · ${v.files.length} files` : ''}</small></span></label>`;
    }).join('')}</div></fieldset>`;
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
    const [models, settings, repo] = await Promise.all([
      api.get('/api/models'),
      api.get('/api/settings'),
      id ? api.get(`/api/hf/repo?id=${encodeURIComponent(id)}`).then((r) => ({ r }), (error) => ({ error })) : Promise.resolve({}),
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
      state.timer = setTimeout(() => {
        const q = t.value.trim();
        if (/huggingface\.co\/|hf\.co\//i.test(q) && repoFrom(q)) {
          // A pasted link: open its repository at once.
          ctx.go(`#/models/add?repo=${encodeURIComponent(repoFrom(q))}`);
          return;
        }
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
      if (repo) ctx.go(`#/models/add?repo=${encodeURIComponent(repo)}`);
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

