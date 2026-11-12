/*
 * Small helpers every view uses: markup builders on Z-Dash (status chips,
 * page heads, buttons with busy faces, copy buttons, field errors),
 * formatting of sizes, token counts, speeds and times, the engine's state
 * in words, and the snackbar.
 */

const DS = window.DS;
export const M = DS.motion;

export const $ = (selector, root = document) => root.querySelector(selector);
export const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
export const esc = (text) => String(text ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
export const icon = (name, size = 20, className) => DS.icons.icon(DS.icons[name], size, className);
export const plural = (count, one, many = `${one}s`) => `${num(count)} ${count === 1 ? one : many}`;

/* ── Numbers, sizes, speeds and times ──────────────────────────────── */

/** A count with thin spaces between the thousands: "2 048". */
export function num(value) {
  return Math.round(Number(value) || 0).toLocaleString('en-US').replace(/,/g, ' ');
}

/** Memory (VRAM, RAM) in GiB, written GB as the tools do: "16.9 GB", "512 MB". */
export function mem(bytes) {
  const gib = (Number(bytes) || 0) / 1024 ** 3;
  if (gib > 0 && gib < 0.1) return `${Math.round(gib * 1024)} MB`;
  return `${gib >= 100 ? Math.round(gib) : gib.toFixed(1)} GB`;
}

/** Files and disks in decimal units, as Hugging Face and the drive label write them: "16.8 GB", "1.76 TB". */
export function size(bytes) {
  const gb = (Number(bytes) || 0) / 1e9;
  if (gb >= 1000) return `${(gb / 1000).toFixed(gb >= 10000 ? 1 : 2)} TB`;
  if (gb > 0 && gb < 0.1) return `${Math.round(gb * 1000)} MB`;
  return `${gb >= 100 ? Math.round(gb) : gb.toFixed(1)} GB`;
}

/** Tokens per second: "38.5", "1 450". */
export function tps(value) {
  const v = Number(value) || 0;
  return v >= 100 ? num(v) : v.toFixed(1);
}

/** A short duration: "640 ms", "6.6 s", "1 min 5 s". */
export function dur(ms) {
  const v = Math.max(0, Number(ms) || 0);
  if (v < 1000) return `${Math.round(v)} ms`;
  if (v < 60_000) return `${(v / 1000).toFixed(1)} s`;
  const s = Math.round(v / 1000);
  return `${Math.floor(s / 60)} min ${s % 60} s`;
}

/** A span of time: "7 s", "3 min", "1 h 20 min". */
export function span(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 90) return `${s} s`;
  const m = Math.round(s / 60);
  if (m < 90) return `${m} min`;
  const h = Math.floor(m / 60);
  if (h >= 48) return `${Math.floor(h / 24)} days`;
  return m % 60 ? `${h} h ${m % 60} min` : `${h} h`;
}

export function ago(ms) {
  if (!ms) return 'never';
  const seconds = Math.round((Date.now() - ms) / 1000);
  if (seconds < 45) return 'just now';
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours} h ago`;
  const days = Math.round(hours / 24);
  return days === 1 ? 'yesterday' : `${days} days ago`;
}

export const clock = (ms) => new Date(ms).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', second: '2-digit' });
export const day = (ms) => new Date(ms).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric' });
export const isoTime = (ms) => new Date(ms).toISOString();
export const pct = (share) => `${Math.round((Number(share) || 0) * 100)} %`;

/** Bytes per second: "112 MB/s". */
export const rate = (bytesPerSec) => `${Math.round((Number(bytesPerSec) || 0) / 1e6)} MB/s`;

/** Parameters: "27.8B", "475M". */
export function params(count) {
  const v = Number(count) || 0;
  if (!v) return '';
  return v >= 1e9 ? `${(v / 1e9).toFixed(v >= 1e11 ? 0 : 1)}B` : `${Math.round(v / 1e6)}M`;
}

/** A share 0..1, clamped. */
export const share = (part, whole) => (whole > 0 ? Math.max(0, Math.min(1, part / whole)) : 0);

/* ── Markup ────────────────────────────────────────────────────────── */

const TONES = { ok: 'success' };

/** A status chip: always an icon (or the spinner) and a word. */
export function chip(kind, label, glyph) {
  const mark = kind === 'running' ? '<span class="ds-spinner ds-status__spinner" aria-hidden="true"></span>' : icon(glyph || 'info', 16);
  const tone = kind === 'running' ? ' ds-status--running' : kind && kind !== 'neutral' ? ` ds-tone-${TONES[kind] || kind}` : '';
  return `<span class="ds-status${tone}">${mark}<span>${esc(label)}</span></span>`;
}

/** The page head: the way back, the title (it takes focus after navigation), the lead and the actions. */
export function head(title, lead, actions = '', back = null) {
  return '<header class="ds-page-head">' +
    (back ? `<a class="ds-btn ds-btn--text ds-sm ds-page-head__back" href="${back.href}">${icon('arrowLeft', 18)}<span class="ds-sr">Back to </span>${esc(back.label)}</a>` : '') +
    `<div class="ds-page-head__row"><div class="ds-page-head__text"><h1 class="ds-page-head__title" tabindex="-1">${esc(title)}</h1>` +
    (lead ? `<p class="ds-page-head__lead">${lead}</p>` : '') + `</div>${actions ? `<div class="ds-page-head__actions">${actions}</div>` : ''}</div></header>`;
}

/** A button with the busy and done faces of DS.motion.busy(). */
export function busyButton(className, label, busyLabel, doneLabel, attributes = '', glyph = '') {
  const type = /\btype=/.test(attributes) ? '' : 'type="button" ';
  return `<button class="${className}" ${type}${attributes}><span class="ds-btn__faces">` +
    `<span class="ds-btn__face">${glyph ? icon(glyph, 16) : ''}${esc(label)}</span>` +
    `<span class="ds-btn__face ds-btn__face--busy" aria-hidden="true"><span class="ds-spinner ds-spinner--ink ds-sm"></span>${esc(busyLabel)}</span>` +
    `<span class="ds-btn__face ds-btn__face--done" aria-hidden="true">${icon('check', 16)}${esc(doneLabel)}</span></span></button>`;
}

/** The copy button of the element with the id `target`. */
export function copyButton(target, label) {
  return `<button class="ds-btn ds-btn--quiet ds-btn--icon" type="button" aria-label="Copy: ${esc(label)}" data-tip="Copy" data-copy="${target}">` +
    `<span class="ds-icon-swap" data-swap="a"><span class="ds-icon-swap__a">${icon('copy', 20)}</span><span class="ds-icon-swap__b">${icon('check', 20)}</span></span></button>`;
}

/** A code block with its copy button. */
export function codeWithCopy(id, code, label) {
  return `<div class="ds-field__row ds-field__row--top"><pre class="ds-code-block ds-code-block--wrap so-grow" id="${id}" tabindex="0" aria-label="${esc(label)}"><code>${esc(code)}</code></pre>` +
    `${copyButton(id, label)}</div>`;
}

/** A read-only field with its copy button. */
export function copyField(id, label, value, hint = '') {
  return `<div class="ds-field"><label class="ds-field__label" for="${id}">${esc(label)}</label>` +
    `<div class="ds-field__row"><input class="ds-input ds-mono" id="${id}" value="${esc(value)}" readonly spellcheck="false"${hint ? ` aria-describedby="${id}-hint"` : ''}>${copyButton(id, label)}</div>` +
    (hint ? `<small class="ds-field__hint" id="${id}-hint">${hint}</small>` : '') + '</div>';
}

/** Copies the text of the element a [data-copy] button names; the icon turns into a check for a moment. */
export async function copyFrom(button) {
  const source = document.getElementById(button.getAttribute('data-copy'));
  if (!source) return;
  const text = source.value ?? source.textContent;
  let copied = false;
  try {
    await navigator.clipboard.writeText(text);
    copied = true;
  } catch (error) {
    // Without the clipboard API (http on another host): select it, so Ctrl+C works.
    if (typeof source.select === 'function') source.select();
    else {
      const range = document.createRange();
      range.selectNodeContents(source);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
    }
    copied = document.execCommand && document.execCommand('copy');
  }
  if (!copied) {
    toast('The text is selected: press Ctrl+C (or Cmd+C) to copy it.', { kind: 'info' });
    return;
  }
  const swap = $('.ds-icon-swap', button);
  if (swap) {
    swap.setAttribute('data-swap', 'b');
    setTimeout(() => swap.setAttribute('data-swap', 'a'), M.motionDuration(swap, '--ds-hold-success') + 600);
  }
  toast('Copied.');
}

export function empty({ glyph = 'box', title, text = '', action = '', id = '' }) {
  return `<div class="ds-empty"${id ? ` id="${id}"` : ''}>` +
    `<span class="ds-empty__disc" aria-hidden="true">${icon(glyph, 24)}</span>` +
    `<p class="ds-empty__title">${esc(title)}</p>${text ? `<p class="ds-empty__text">${text}</p>` : ''}${action}</div>`;
}

export function switchHtml({ attrs = '', label, checked, hint = '', id }) {
  const labelId = `${id}-label`;
  const hintId = `${id}-hint`;
  return `<label class="ds-switch"><input type="checkbox" role="switch" id="${id}" ${attrs}${checked ? ' checked' : ''} aria-labelledby="${labelId}"${hint ? ` aria-describedby="${hintId}"` : ''}>` +
    `<span class="ds-switch__track" aria-hidden="true"></span><span class="ds-switch__label" id="${labelId}">${esc(label)}</span>` +
    (hint ? `<small class="ds-switch__hint" id="${hintId}">${hint}</small>` : '') +
    '<span class="ds-switch__state" aria-hidden="true"><span class="ds-switch__on">On</span><span class="ds-switch__off">Off</span></span></label>';
}

/** An alert; kind is '', 'success', 'warning' or 'error'. */
export function alertHtml(kind, title, text, actions = '', attrs = '') {
  const glyph = { error: 'alertCircle', warning: 'alert', success: 'checkCircle' }[kind] || 'info';
  const role = kind === 'error' ? ' role="alert"' : '';
  return `<div class="ds-alert${kind ? ` ds-tone-${kind}` : ''}"${role} ${attrs}>${icon(glyph, 20)}<div class="ds-alert__text">` +
    (title ? `<p class="ds-alert__title">${esc(title)}</p>` : '') + (text ? `<p>${text}</p>` : '') + '</div>' +
    (actions ? `<div class="ds-alert__actions">${actions}</div>` : '') + '</div>';
}

/** A labelled progress bar with a detail line under it; without a known share it runs indeterminate. */
export function progressHtml(value, label, labelText, detail = '', indeterminate = false) {
  const v = Math.max(0, Math.min(1, Number(value) || 0));
  const aria = indeterminate ? `aria-valuetext="${esc(labelText)}"` : `aria-valuenow="${Math.round(v * 100)}"`;
  return `<div class="ds-progress${indeterminate ? ' ds-progress--indeterminate' : ''}"${indeterminate ? '' : ` style="--ds-progress: ${v.toFixed(3)}"`}>` +
    `<div class="ds-progress__track" role="progressbar" aria-label="${esc(label)}" aria-valuemin="0" aria-valuemax="100" ${aria}><div class="ds-progress__bar"></div></div>` +
    `<p class="ds-progress__label">${esc(labelText)}</p>${detail ? `<p class="ds-progress__detail">${esc(detail)}</p>` : ''}</div>`;
}

/* ── Feedback ──────────────────────────────────────────────────────── */

export function toast(text, options = {}) {
  return DS.overlay.toast(null, text, options);
}

export function toastError(error, key) {
  return DS.overlay.toast(null, error?.message || String(error), { kind: 'error', key });
}

/** aria-invalid and an error hint with its icon, linked by aria-describedby. */
export function setError(input, message) {
  if (!input) return false;
  const id = `${input.id}-error`;
  const hint = document.getElementById(id);
  const described = (input.getAttribute('aria-describedby') || '').split(' ').filter((token) => token && token !== id);
  if (!message) {
    input.removeAttribute('aria-invalid');
    if (hint) hint.remove();
    if (described.length) input.setAttribute('aria-describedby', described.join(' '));
    else input.removeAttribute('aria-describedby');
    return false;
  }
  input.setAttribute('aria-invalid', 'true');
  input.setAttribute('aria-describedby', [...described, id].join(' '));
  if (hint) hint.lastChild.textContent = message;
  else {
    const host = input.closest('.ds-field') || input.parentElement;
    host.insertAdjacentHTML('beforeend', `<small class="ds-field__hint ds-field__hint--error" id="${id}">${icon('alertCircle', 16)}<span>${esc(message)}</span></small>`);
  }
  return true;
}

/** Shows the field errors of an ApiError on the inputs named data-field="key"; focuses the first. */
export function showFieldErrors(root, fields) {
  let first = null;
  for (const input of $$('[data-field]', root)) {
    if (setError(input, fields?.[input.getAttribute('data-field')] || '') && !first) first = input;
  }
  if (first) first.focus();
  return first;
}

export function clearErrors(root) {
  for (const input of $$('[aria-invalid="true"]', root)) setError(input, '');
}

/* ── The engine ────────────────────────────────────────────────────── */

/** The engine's state as a chip: kind, word and glyph. */
export function engineState(engine) {
  const e = engine || { state: 'stopped' };
  switch (e.state) {
    case 'ready':
      return ['ok', 'Ready', 'check'];
    case 'loading':
      return ['running', e.progress != null ? `Loading ${pct(e.progress)}` : 'Loading'];
    case 'planning':
      return ['running', 'Planning'];
    case 'starting':
      return ['running', 'Starting'];
    case 'error':
      return ['error', 'Error', 'alertCircle'];
    case 'crashed':
      return ['error', 'Crashed', 'alertCircle'];
    default:
      return ['', e.modelId ? 'Stopped' : 'No model', 'stopCircle'];
  }
}

export function engineChip(engine) {
  const [kind, label, glyph] = engineState(engine);
  return chip(kind, label, glyph);
}

/** Whether the engine works towards ready. */
export const engineBusy = (engine) => !!engine && ['planning', 'starting', 'loading'].includes(engine.state);

/** The state of an installed model as a chip. */
export function modelChip(m, active, engine) {
  if (active) {
    const [kind, label, glyph] = engineState(engine);
    return chip(kind, kind === 'ok' ? 'Active' : label, glyph);
  }
  switch (m.state) {
    case 'queued':
      return chip('', 'Queued', 'clock');
    case 'downloading':
      return chip('running', `Downloading ${pct(m.progress)}`);
    case 'converting':
      return chip('running', `Converting ${pct(m.progress)}`);
    case 'failed':
      return chip('error', 'Failed', 'alertCircle');
    default:
      return '';
  }
}
