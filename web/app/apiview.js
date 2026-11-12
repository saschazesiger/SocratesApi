/*
 * API: what an app needs to use the active model. The base URL and the
 * model id with copy buttons, ready-made configurations (opencode, the
 * OpenAI Python SDK, curl, Claude Code), the endpoints with an example
 * each, the API keys (create shows the secret once, revoke asks first),
 * the latest requests live, and the settings: the Hugging Face token,
 * whether a key is required, the admin password.
 */
import * as api from './api.js';
import { ctx } from './main.js';
import {
  $, M, esc, icon, head, chip, busyButton, alertHtml, switchHtml, empty, toast, toastError, showFieldErrors, clearErrors,
  copyField, copyButton, num, tps, dur, ago, clock, day, isoTime,
} from './ui.js';

const DS = window.DS;

/* ── Snippets ─────────────────────────────────────────────────────── */

const KEY = 'sk-soc-...';

function origin(baseUrl) {
  return String(baseUrl || '').replace(/\/v1\/?$/, '');
}

function snippets(d) {
  const base = d.baseUrl;
  const e = (ctx.status && ctx.status.engine) || {};
  const id = e.modelId || 'MODEL_ID';
  const name = e.modelName || 'Socrates model';
  return [
    ['opencode', 'opencode.json', 'json', JSON.stringify({
      $schema: 'https://opencode.ai/config.json',
      provider: {
        socrates: {
          npm: '@ai-sdk/openai-compatible',
          name: 'Socrates API',
          options: { baseURL: base, apiKey: '{env:SOCRATES_API_KEY}' },
          // opencode compacts the conversation before it outgrows the context.
          models: { [id]: { name, limit: { context: e.ctx || 32768, output: Math.min(32768, Math.floor((e.ctx || 32768) / 2)) } } },
        },
      },
      model: `socrates/${id}`,
    }, null, 2)],
    ['python', 'OpenAI Python', 'python', `import os
from openai import OpenAI

client = OpenAI(base_url="${base}", api_key=os.environ["SOCRATES_API_KEY"])

stream = client.chat.completions.create(
    model="${id}",
    messages=[{"role": "user", "content": "Hello! Who are you?"}],
    stream=True,
)
for chunk in stream:
    print(chunk.choices[0].delta.content or "", end="", flush=True)`],
    ['curl', 'curl', 'sh', `curl ${base}/chat/completions \\
  -H "Authorization: Bearer $SOCRATES_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${id}",
    "messages": [{"role": "user", "content": "Hello! Who are you?"}],
    "stream": true
  }'`],
    ['claude', 'Anthropic', 'sh', `# The Anthropic Messages API of Socrates, for Claude Code and other Anthropic clients
export ANTHROPIC_BASE_URL="${origin(base)}"
export ANTHROPIC_AUTH_TOKEN="$SOCRATES_API_KEY"
export ANTHROPIC_MODEL="${id}"
export ANTHROPIC_DEFAULT_HAIKU_MODEL="${id}"
claude`],
  ];
}

function snippetsHtml(d) {
  const list = snippets(d);
  const key = `snip-${((ctx.status && ctx.status.engine.modelId) || 'none')}-${d.baseUrl}`;
  return `<figure class="ds-code" data-ds-code data-ds-key="${esc(key)}">` +
    '<div class="ds-code__head ds-sm">' +
      `<div class="ds-tabs ds-tabs--flush" role="tablist" aria-label="Client">${list.map(([id, label], i) =>
        `<button class="ds-tabs__tab" role="tab" type="button" id="tab-${id}" aria-selected="${i === 0}" aria-controls="snip-${id}"${i ? ' tabindex="-1"' : ''} data-label="${esc(label)}">${esc(label)}</button>`).join('')}</div>` +
      '<span class="ds-code__lang"></span>' +
      '<span class="ds-code__actions">' +
        `<button class="ds-btn ds-btn--quiet ds-btn--icon" type="button" data-ds-copy aria-label="Copy the snippet" data-tip="Copy">` +
        `<span class="ds-icon-swap"><span class="ds-icon-swap__a">${icon('copy', 20)}</span><span class="ds-icon-swap__b">${icon('check', 20)}</span></span></button>` +
      '</span>' +
    '</div>' +
    list.map(([id, , lang, code], i) =>
      `<pre class="ds-code-block" id="snip-${id}" role="tabpanel" aria-labelledby="tab-${id}" tabindex="0" data-ds-code-lang="${lang}"${i ? ' hidden' : ''}><code>${esc(code)}</code></pre>`).join('') +
    '</figure>';
}

/* ── Endpoints ────────────────────────────────────────────────────── */

function endpoints(d) {
  const base = d.baseUrl;
  const e = (ctx.status && ctx.status.engine) || {};
  const id = e.modelId || 'MODEL_ID';
  const auth = `-H "Authorization: Bearer ${KEY}"`;
  return [
    ['POST', '/v1/chat/completions', 'OpenAI chat', 'Chat in the OpenAI format: streaming, tools (function calling), images for vision models and the thinking as reasoning_content. What almost every client uses.',
      `curl ${base}/chat/completions ${auth} \\\n  -H "Content-Type: application/json" \\\n  -d '{"model": "${id}", "messages": [{"role": "user", "content": "Hi"}]}'`],
    ['POST', '/v1/completions', 'OpenAI completion', 'Continues a plain text prompt, without a chat template.',
      `curl ${base}/completions ${auth} \\\n  -H "Content-Type: application/json" \\\n  -d '{"model": "${id}", "prompt": "Once upon a time", "max_tokens": 64}'`],
    ['GET', '/v1/models', 'Models', 'The model this server runs, in the OpenAI format. Clients use it to find the model id.',
      `curl ${base}/models ${auth}`],
    ['POST', '/v1/embeddings', 'Embeddings', 'Vectors for texts, for search and RAG. Works only while an embedding model is the active model.',
      `curl ${base}/embeddings ${auth} \\\n  -H "Content-Type: application/json" \\\n  -d '{"model": "${id}", "input": "The quick brown fox"}'`, 'Embedding models only'],
    ['POST', '/v1/messages', 'Anthropic messages', 'Chat in the Anthropic Messages format with streaming, tools and thinking: for Claude Code and the Anthropic SDKs.',
      `curl ${base}/messages \\\n  -H "x-api-key: ${KEY}" -H "anthropic-version: 2023-06-01" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model": "${id}", "max_tokens": 512, "messages": [{"role": "user", "content": "Hi"}]}'`],
    ['POST', '/v1/messages/count_tokens', 'Count tokens', 'Counts the tokens of an Anthropic request without running it.',
      `curl ${base}/messages/count_tokens \\\n  -H "x-api-key: ${KEY}" -H "anthropic-version: 2023-06-01" \\\n  -H "Content-Type: application/json" \\\n  -d '{"model": "${id}", "messages": [{"role": "user", "content": "Hi"}]}'`],
    ['GET', '/health', 'Health', 'Whether the model is ready: 200 when it is, 503 while it loads. Needs no key; for monitoring and load balancers.',
      `curl ${origin(base)}/health`],
  ];
}

function endpointsHtml(d) {
  return `<div class="ds-disclosure-list">${endpoints(d).map(([method, path, title, text, curl, tag], i) =>
    `<details class="ds-collapsible ds-disclosure ds-disclosure--row" data-ds-key="ep-${i}">` +
      `<summary><span class="ds-disclosure__title"><code>${method}</code> <code>${esc(path)}</code></span><span class="ds-disclosure__meta">${esc(title)}</span>${icon('chevronDown', 20, 'ds-chevron')}</summary>` +
      `<div class="ds-disclosure__body ds-stack"><p>${esc(text)}${tag ? ` <span class="ds-tag ds-tone-neutral">${esc(tag)}</span>` : ''}</p>` +
      `<div class="ds-field__row ds-field__row--top"><pre class="ds-code-block ds-code-block--wrap so-grow" id="ep-curl-${i}" tabindex="0" aria-label="Example: ${esc(path)}"><code>${esc(curl)}</code></pre>${copyButton(`ep-curl-${i}`, `the example of ${path}`)}</div>` +
      '</div></details>').join('')}</div>`;
}

/* ── Keys ─────────────────────────────────────────────────────────── */

function keysHtml(d) {
  if (!d.keys.length) {
    return empty({ glyph: 'key', title: 'No API key yet', text: d.settings.requireKey ? 'Create one for each app, so you can revoke it alone.' : 'The API is open right now; keys are optional.',
      action: '<button class="ds-btn ds-btn--tonal" type="button" data-act="new-key">Create a key</button>' });
  }
  return '<div class="ds-table-wrap"><table class="ds-table"><caption class="ds-sr">API keys</caption><thead><tr>' +
    '<th scope="col">Name</th><th scope="col">Key</th><th scope="col" class="ds-col--secondary">Created</th><th scope="col">Last used</th>' +
    '<th scope="col" class="ds-num">Requests</th><th scope="col" class="ds-num ds-col--secondary">Tokens in / out</th><th scope="col"><span class="ds-sr">Actions</span></th></tr></thead><tbody>' +
    d.keys.map((k) => `<tr data-ds-key="${esc(k.id)}">` +
      `<td class="ds-col--text">${esc(k.name)}</td>` +
      `<td><code class="ds-nowrap">${esc(k.prefix)}…</code></td>` +
      `<td class="ds-col--secondary ds-nowrap"><time datetime="${isoTime(k.createdAt)}">${day(k.createdAt)}</time></td>` +
      `<td class="ds-nowrap">${k.lastUsedAt ? `<time datetime="${isoTime(k.lastUsedAt)}">${ago(k.lastUsedAt)}</time>` : '<span class="ds-muted">Never</span>'}</td>` +
      `<td class="ds-num">${num(k.requests)}</td>` +
      `<td class="ds-num ds-col--secondary ds-nowrap">${num(k.tokensIn)} / ${num(k.tokensOut)}</td>` +
      `<td class="ds-num"><button class="ds-btn ds-btn--quiet ds-btn--icon ds-sm" type="button" data-revoke="${esc(k.id)}" aria-label="Revoke ${esc(k.name)}" data-tip="Revoke">${icon('trash', 18)}</button></td></tr>`).join('') +
    '</tbody></table></div>';
}

/* ── Requests ─────────────────────────────────────────────────────── */

function statusChip(r) {
  if (r.status >= 200 && r.status < 300) return chip('ok', String(r.status), 'check');
  if (r.status === 499) return chip('', 'Stopped', 'ban');
  return chip('error', String(r.status), 'alertCircle');
}

function requestsHtml(d) {
  if (!d.requests.length) return empty({ glyph: 'activity', title: 'No request yet', text: 'Requests to the API and from the chat show up here as they finish.' });
  return '<div class="ds-table-wrap"><table class="ds-table ds-density-compact"><caption class="ds-sr">Recent requests</caption><thead><tr>' +
    '<th scope="col" class="so-col-time">Time</th><th scope="col">From</th><th scope="col" class="ds-col--secondary">Path</th>' +
    '<th scope="col" class="ds-num">Tokens</th><th scope="col" class="ds-num so-col-num">Speed</th><th scope="col" class="ds-num ds-col--secondary">Took</th><th scope="col">Status</th></tr></thead><tbody>' +
    d.requests.slice(0, 20).map((r) => `<tr data-ds-key="${esc(r.id)}"${r.error ? ` data-tip="${esc(r.error)}"` : ''}>` +
      `<td class="ds-nowrap"><time datetime="${isoTime(r.at)}">${clock(r.at)}</time></td>` +
      `<td class="ds-nowrap">${r.source === 'chat' ? `${icon('chat', 16)} Chat` : `${icon('key', 16)} ${esc(r.keyName || 'no key')}`}</td>` +
      `<td class="ds-col--secondary"><code>${esc(r.path)}</code></td>` +
      `<td class="ds-num ds-nowrap">${r.promptTokens || r.completionTokens ? `${num(r.promptTokens)}${r.cachedTokens ? ` <span class="ds-muted">(${num(r.cachedTokens)} cached)</span>` : ''} → ${num(r.completionTokens)}` : '<span class="ds-muted">–</span>'}</td>` +
      `<td class="ds-num ds-nowrap">${r.genTps ? `${tps(r.genTps)} tok/s` : '<span class="ds-muted">–</span>'}</td>` +
      `<td class="ds-num ds-col--secondary ds-nowrap">${dur(r.ms)}</td>` +
      `<td>${statusChip(r)}</td></tr>`).join('') +
    '</tbody></table></div>';
}

/* ── Settings ─────────────────────────────────────────────────────── */

function tokenHtml(s) {
  return '<form class="ds-form" id="hf-form" novalidate>' +
    '<div class="ds-field"><label class="ds-field__label" for="hf-token">Hugging Face token</label>' +
    `<input class="ds-input ds-mono" id="hf-token" type="password" autocomplete="off" spellcheck="false" data-field="hfToken" placeholder="${s.hfToken ? 'A token is set: type a new one to replace it' : 'hf_...'}" aria-describedby="hf-token-hint">` +
    `<small class="ds-field__hint" id="hf-token-hint">${s.hfToken ? `${icon('checkCircle', 16)}A token is set. ` : ''}Needed for gated models (accept their licence on huggingface.co first) and faster downloads. A read token is enough; it is stored on this server only.</small></div>` +
    `<div class="ds-form-actions">${busyButton('ds-btn', 'Save token', 'Saving', 'Saved', 'type="submit"')}` +
    (s.hfToken ? busyButton('ds-btn ds-btn--text', 'Remove the token', 'Removing', 'Removed', 'data-act="clear-token"') : '') + '</div></form>';
}

function passwordHtml() {
  return '<form class="ds-form" id="password-form" novalidate>' +
    '<div class="ds-field"><label class="ds-field__label" for="pw-current">Current password</label><input class="ds-input" id="pw-current" type="password" autocomplete="current-password" data-field="current"></div>' +
    '<div class="ds-form-grid ds-form-grid--2">' +
      '<div class="ds-field"><label class="ds-field__label" for="pw-next">New password</label><input class="ds-input" id="pw-next" type="password" autocomplete="new-password" data-field="password" aria-describedby="pw-next-hint">' +
        '<small class="ds-field__hint" id="pw-next-hint">At least 10 characters.</small></div>' +
      '<div class="ds-field"><label class="ds-field__label" for="pw-again">Repeat it</label><input class="ds-input" id="pw-again" type="password" autocomplete="new-password"></div>' +
    '</div>' +
    `<div class="ds-form-actions">${busyButton('ds-btn', 'Change password', 'Changing', 'Changed', 'type="submit"')}</div></form>`;
}

function quickHtml(d) {
  const e = (ctx.status && ctx.status.engine) || {};
  return copyField('base-url', 'Base URL', d.baseUrl, 'For OpenAI-compatible clients. Anthropic clients take it without /v1.') +
    (e.modelId
      ? copyField('model-id', 'Model', e.modelId, `${esc(e.modelName)}, the active model. Requests always go to it.`)
      : alertHtml('warning', 'No model is active', 'The API answers once you activate a model on the <a href="#/models">Models page</a>.')) +
    (d.settings.requireKey
      ? `<p class="ds-meta">${icon('lock', 16)}Every request needs an API key: <code>Authorization: Bearer sk-soc-...</code> (Anthropic clients: <code>x-api-key</code>).</p>`
      : alertHtml('warning', 'The API is open', 'Anyone who can reach this server can use the model without a key. Turn on "Require an API key" below if that is not what you want.'));
}

/* ── The view ─────────────────────────────────────────────────────── */

let shownModel = null;

export default {
  title: 'API',
  topics: ['keys', 'requests', 'settings'],

  async load() {
    const [keys, requests, settings] = await Promise.all([api.get('/api/keys'), api.get('/api/requests'), api.get('/api/settings')]);
    return { keys: keys.keys || [], baseUrl: keys.baseUrl, requests: requests.requests || [], settings };
  },

  html(d) {
    shownModel = ctx.status ? ctx.status.engine.modelId : '';
    return head('API', 'Use the active model from your apps, through an OpenAI- and an Anthropic-compatible API.') +
      '<div class="ds-grid-2">' +
        `<section class="ds-card" aria-labelledby="quick-title"><h2 class="ds-card__title" id="quick-title">Quick start</h2><div class="ds-stack" id="quick">${quickHtml(d)}</div></section>` +
        '<section class="ds-card" aria-labelledby="connect-title"><h2 class="ds-card__title" id="connect-title">Connect an app</h2>' +
          '<p class="ds-muted">Put your key into <code>SOCRATES_API_KEY</code>, then use one of these.</p>' +
          `<div id="snippets">${snippetsHtml(d)}</div></section>` +
      '</div>' +
      '<section class="ds-card" aria-labelledby="keys-title">' +
        `<div class="ds-card__row"><h2 class="ds-card__title" id="keys-title">API keys</h2><button class="ds-btn ds-btn--tonal" type="button" data-act="new-key">${icon('plus', 18)}Create a key</button></div>` +
        `<div id="keys">${keysHtml(d)}</div>` +
      '</section>' +
      '<section class="ds-card" aria-labelledby="requests-title">' +
        '<div class="ds-card__row"><h2 class="ds-card__title" id="requests-title">Recent requests</h2><span class="ds-meta">Live · the latest 20</span></div>' +
        `<div id="requests">${requestsHtml(d)}</div>` +
      '</section>' +
      '<section class="ds-card" aria-labelledby="endpoints-title"><h2 class="ds-card__title" id="endpoints-title">Endpoints</h2>' +
        `<p class="ds-muted">All under <code>${esc(d.baseUrl)}</code>, except <code>/health</code>. Open one for an example.</p>` +
        `<div id="endpoints">${endpointsHtml(d)}</div></section>` +
      '<section class="ds-card" aria-labelledby="settings-title"><h2 class="ds-card__title" id="settings-title">Settings</h2>' +
        `<div id="require-key">${switchHtml({ id: 'require-key-input', attrs: 'data-act="require-key"', label: 'Require an API key', checked: d.settings.requireKey, hint: 'Off: anyone who can reach port 8080 can use the model. The chat on this page never needs a key.' })}</div>` +
        '<hr class="ds-divider">' +
        `<div id="hf-slot">${tokenHtml(d.settings)}</div>` +
        '<hr class="ds-divider">' +
        '<h3 class="ds-card__subtitle">Admin password</h3><p class="ds-muted">Signs you in to this page. Changing it signs out every other browser.</p>' +
        passwordHtml() +
      '</section>';
  },

  update(d) {
    const page = ctx.page;
    DS.dom.morphHtml($('#quick', page), quickHtml(d));
    const keys = $('#keys', page);
    M.flip(keys, (morph) => DS.dom.morphHtml(keys, keysHtml(d), morph), { live: true });
    const requests = $('#requests', page);
    M.flip(requests, (morph) => DS.dom.morphHtml(requests, requestsHtml(d), morph), { live: true });
    const input = $('#require-key-input', page);
    if (input && document.activeElement !== input) input.checked = d.settings.requireKey;
    const hf = $('#hf-slot', page);
    if (hf && !hf.contains(document.activeElement)) DS.dom.morphHtml(hf, tokenHtml(d.settings));
  },

  status(s) {
    // The model's id is in the snippets and examples: draw them again when another model becomes active.
    if (!ctx.data || s.engine.modelId === shownModel) return;
    shownModel = s.engine.modelId;
    const page = ctx.page;
    DS.dom.morphHtml($('#quick', page), quickHtml(ctx.data));
    DS.dom.morphHtml($('#snippets', page), snippetsHtml(ctx.data));
    DS.dom.morphHtml($('#endpoints', page), endpointsHtml(ctx.data));
  },

  async on(type, event) {
    const t = event.target;
    const d = ctx.data;
    if (type === 'change' && t.id === 'require-key-input') {
      const want = t.checked;
      try {
        const s = await api.put('/api/settings', { requireKey: want });
        d.settings = s;
        DS.dom.morphHtml($('#quick', ctx.page), quickHtml(d));
        toast(want ? 'Requests need an API key now.' : 'The API is open: requests need no key.');
      } catch (error) {
        t.checked = !want;
        toastError(error);
      }
      return;
    }
    if (type === 'submit' && t.id === 'hf-form') {
      event.preventDefault();
      clearErrors(t);
      const input = $('#hf-token', t);
      const value = input.value.trim();
      if (!value) {
        showFieldErrors(t, { hfToken: 'Paste a token first.' });
        return;
      }
      try {
        d.settings = await M.busy($('[type="submit"]', t), () => api.put('/api/settings', { hfToken: value }));
        input.value = '';
        DS.dom.morphHtml($('#hf-slot', ctx.page), tokenHtml(d.settings));
        toast('The Hugging Face token is saved.');
      } catch (error) {
        if (!showFieldErrors(t, error.fields)) toastError(error);
      }
      return;
    }
    if (type === 'submit' && t.id === 'password-form') {
      event.preventDefault();
      clearErrors(t);
      const current = $('#pw-current', t);
      const next = $('#pw-next', t);
      const again = $('#pw-again', t);
      if (next.value.length < 10) {
        showFieldErrors(t, { password: 'Use at least 10 characters.' });
        return;
      }
      if (next.value !== again.value) {
        showFieldErrors(t, { password: 'The two new passwords differ.' });
        return;
      }
      try {
        const answer = await M.busy($('[type="submit"]', t), () => api.post('/api/password', { current: current.value, password: next.value }));
        t.reset();
        toast(answer.message || 'The password was changed.');
      } catch (error) {
        if (!showFieldErrors(t, error.fields)) toastError(error);
      }
      return;
    }
    if (type !== 'click') return;
    let el;
    if ((el = t.closest('[data-act="clear-token"]'))) {
      try {
        d.settings = await M.busy(el, () => api.put('/api/settings', { hfToken: '' }));
        DS.dom.morphHtml($('#hf-slot', ctx.page), tokenHtml(d.settings));
        toast('The Hugging Face token was removed.');
      } catch (error) {
        toastError(error);
      }
    } else if ((el = t.closest('[data-act="new-key"]'))) {
      let created = null;
      const ask = {
        title: 'Create an API key', label: 'Name', value: '', confirmLabel: 'Create key', opener: el,
        hint: 'Who uses it, such as opencode or my-laptop. One key per app lets you revoke it alone.',
        errorText: 'The key could not be created. Try another name.',
        onConfirm: async (value) => {
          try {
            created = await api.post('/api/keys', { name: value.trim() });
          } catch (error) {
            // The dialog stays open and says the server's reason.
            ask.errorText = error.message;
            throw error;
          }
        },
      };
      const name = await DS.overlay.prompt(ask);
      if (!name || !created) return;
      $('#secret-name').textContent = `“${created.key.name}”`;
      $('#secret-value').value = created.secret;
      const box = $('#secret-stored');
      box.checked = false;
      box.dispatchEvent(new Event('change'));
      DS.overlay.open($('#secret-dialog'), { returnTo: () => $('#keys-title') });
    } else if ((el = t.closest('[data-revoke]'))) {
      const k = d.keys.find((x) => x.id === el.getAttribute('data-revoke'));
      if (!k) return;
      const ok = await DS.overlay.confirm({
        title: `Revoke ${k.name}?`,
        text: `Apps that use it get "401 Unauthorized" at once. ${k.lastUsedAt ? `It was last used ${ago(k.lastUsedAt)}.` : 'It was never used.'}`,
        confirmLabel: `Revoke ${k.name}`, destructive: true, opener: el,
        onConfirm: () => api.del(`/api/keys/${encodeURIComponent(k.id)}`),
        errorText: `${k.name} could not be revoked. Try again.`,
      });
      if (ok) toast(`${k.name} was revoked.`);
    }
  },
};
