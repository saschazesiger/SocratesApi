/*
 * Chat: try the active model. A transcript of messages (the answers as
 * Markdown, the thinking in a collapsible part), a composer with send and
 * stop, options (system prompt, temperature, max tokens, thinking) and
 * conversations kept in this browser.
 *
 * Above the composer one line says, live, what happens: waiting for the
 * model to load, queued, reading the prompt, thinking, writing, with
 * tokens and speed; after an answer its figures. It also says when the
 * engine is down. The engine log opens beside it for the details.
 */
import * as api from './api.js';
import { ctx } from './main.js';
import { $, esc, icon, head, switchHtml, toast, num, tps, dur, pct, ago, clock, isoTime, empty } from './ui.js';
import { renderMarkdown } from './markdown.js';

const DS = window.DS;

/* ── What this browser keeps ──────────────────────────────────────── */

const KEY_CHATS = 'socrates.chats';
const KEY_CURRENT = 'socrates.chat.current';
const KEY_OPTIONS = 'socrates.chat.options';
const MAX_CHATS = 40;

function read(key, fallback) {
  try {
    const value = JSON.parse(localStorage.getItem(key));
    return value ?? fallback;
  } catch (error) {
    return fallback;
  }
}

function write(key, value) {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch (error) {
    // Full or blocked: the chat still works, it is just not kept.
  }
}

const store = {
  chats: read(KEY_CHATS, []),
  currentId: read(KEY_CURRENT, ''),
  options: { system: '', temperature: '', maxTokens: '', thinking: true, ...read(KEY_OPTIONS, {}) },
};

let saveTimer = 0;
function save(now = false) {
  clearTimeout(saveTimer);
  const run = () => {
    store.chats.sort((a, b) => b.updatedAt - a.updatedAt);
    store.chats = store.chats.filter((c) => c.messages.length).slice(0, MAX_CHATS);
    write(KEY_CHATS, store.chats.map((c) => ({ ...c, messages: c.messages.map(({ live, ...m }) => m) })));
    write(KEY_CURRENT, store.currentId);
  };
  if (now) run();
  else saveTimer = setTimeout(run, 1500);
}

const uid = () => Math.random().toString(36).slice(2, 10);

function current() {
  let chat = store.chats.find((c) => c.id === store.currentId);
  if (!chat) {
    chat = { id: uid(), title: '', createdAt: Date.now(), updatedAt: Date.now(), messages: [] };
    store.chats.unshift(chat);
    store.currentId = chat.id;
  }
  return chat;
}

/* ── One answer that streams ──────────────────────────────────────── */

/* The run in flight (one at a time): its message, phase and figures. It outlives the view, so leaving the page does not stop it. */
let run = null;
let lastStats = null;

function phaseOf(chunk, r) {
  const choice = chunk.choices && chunk.choices[0];
  const delta = (choice && choice.delta) || {};
  if (chunk.prompt_progress) {
    const p = chunk.prompt_progress;
    r.phase = 'prompt';
    r.progress = p;
    const read = Math.max(0, (p.processed || 0) - (p.cache || 0));
    r.tps = p.time_ms ? (read / p.time_ms) * 1000 : 0;
  }
  if (chunk.timings) r.timings = chunk.timings;
  if (chunk.usage) r.usage = chunk.usage;
  if (delta.reasoning_content) {
    if (r.phase !== 'thinking') {
      r.phase = 'thinking';
      r.phaseStart = r.timings ? r.timings.predicted_n - 1 : r.count;
      r.msg.reasoningSince = Date.now();
    }
    r.msg.reasoning = (r.msg.reasoning || '') + delta.reasoning_content;
    r.count += 1;
  }
  if (delta.content) {
    if (r.phase !== 'writing') {
      if (r.phase === 'thinking') r.msg.reasoningMs = Date.now() - r.msg.reasoningSince;
      r.phase = 'writing';
      r.phaseStart = r.timings ? r.timings.predicted_n - 1 : r.count;
    }
    r.msg.content += delta.content;
    r.count += 1;
  }
  if (r.timings && (delta.content || delta.reasoning_content)) r.tps = r.timings.predicted_per_second || r.tps;
}

/** The figures of a finished answer, from the last timings (llama.cpp) or the usage. */
function statsOf(r) {
  const t = r.timings;
  const u = r.usage || {};
  const ms = Date.now() - r.startedAt;
  if (t) {
    return { prompt: (t.prompt_n || 0) + (t.cache_n || 0), cached: t.cache_n || 0, generated: t.predicted_n || 0,
      tps: t.predicted_per_second || 0, promptTps: t.prompt_per_second || 0, ms };
  }
  return { prompt: u.prompt_tokens || 0, cached: (u.prompt_tokens_details && u.prompt_tokens_details.cached_tokens) || 0,
    generated: u.completion_tokens || r.count, tps: r.count && ms ? (r.count / ms) * 1000 : 0, promptTps: 0, ms };
}

function body(chat) {
  const o = store.options;
  const messages = [];
  if (o.system.trim()) messages.push({ role: 'system', content: o.system.trim() });
  for (const m of chat.messages) {
    if (m.live) continue;
    if (m.role === 'assistant' && m.state === 'error' && !m.content) continue;
    messages.push({ role: m.role, content: m.content });
  }
  const out = { messages, stream: true, chat_template_kwargs: { enable_thinking: !!o.thinking } };
  const engine = ctx.status && ctx.status.engine;
  if (engine && engine.modelId) out.model = engine.modelId;
  if (o.temperature !== '' && Number.isFinite(Number(o.temperature))) out.temperature = Number(o.temperature);
  if (o.maxTokens !== '' && Number(o.maxTokens) > 0) out.max_tokens = Math.round(Number(o.maxTokens));
  return out;
}

async function send(text) {
  if (run) return;
  const chat = current();
  const now = Date.now();
  if (text) {
    chat.messages.push({ id: uid(), role: 'user', content: text, at: now });
    if (!chat.title) chat.title = text.replace(/\s+/g, ' ').slice(0, 80);
  }
  const engine = ctx.status && ctx.status.engine;
  const msg = { id: uid(), role: 'assistant', content: '', reasoning: '', at: now, state: 'streaming', live: true,
    model: engine && engine.modelName ? engine.modelName : 'Assistant' };
  const request = body(chat);
  chat.messages.push(msg);
  chat.updatedAt = now;
  const r = { msg, chat, phase: 'sending', progress: null, count: 0, tps: 0, timings: null, usage: null, startedAt: now, phaseStart: 0, controller: new AbortController() };
  run = r;
  lastStats = null;
  renderLog();
  setBusy(true);
  stickToEnd();
  paintStatus();
  try {
    await api.chatStream(request, { signal: r.controller.signal, onChunk: (chunk) => { phaseOf(chunk, r); paintSoon(); } });
    msg.state = 'done';
    if (!msg.content && !msg.reasoning) {
      msg.state = 'error';
      msg.error = 'The model answered with nothing.';
    }
  } catch (error) {
    if (error.name === 'AbortError') msg.state = 'partial';
    else {
      msg.state = 'error';
      msg.error = error.message;
    }
  }
  if (msg.reasoning && !msg.reasoningMs && msg.reasoningSince) msg.reasoningMs = Date.now() - msg.reasoningSince;
  if (msg.state !== 'error' || r.timings) {
    msg.stats = statsOf(r);
    lastStats = msg.stats;
  }
  delete msg.live;
  run = null;
  chat.updatedAt = Date.now();
  save(true);
  setBusy(false);
  renderLog();
  paintStatus();
  if (msg.state === 'done') DS.overlay.announce('The answer is complete.');
}

function stop() {
  if (run) run.controller.abort();
}

/* ── Markup ───────────────────────────────────────────────────────── */

const md = (source, id) => renderMarkdown(source, `m${id}-`);

function reasoningHtml(m) {
  if (!m.reasoning) return '';
  const running = !!m.live && !m.reasoningMs;
  const open = running || m.reasoningOpen;
  const summary = running ? 'Thinking' : `Thought for ${dur(m.reasoningMs || 0)}`;
  return `<details class="ds-collapsible ds-disclosure ds-reasoning" data-ds-reasoning data-msg="${m.id}"${m.reasoningSince ? ` data-ds-reasoning-since="${m.reasoningSince}"` : ''}` +
    `${running ? '' : ` data-ds-reasoning-done="${Math.round(m.reasoningMs || 0)}"`}${open ? ' open' : ''}>` +
    `<summary><span class="ds-reasoning__icon">${icon('sparkles', 18)}</span><span class="ds-reasoning__summary" id="r-sum-${m.id}">${summary}</span> <time class="ds-reasoning__time"></time>${icon('chevronDown', 20, 'ds-chevron')}</summary>` +
    `<div class="ds-disclosure__body ds-reasoning__body"><div class="ds-reasoning__text" tabindex="0" aria-label="Thoughts" id="r-${m.id}">${thoughtsHtml(m.reasoning)}</div></div></details>`;
}

const thoughtsHtml = (text) => text.trim().split(/\n{2,}/).map((p) => `<p>${esc(p)}</p>`).join('');

function statsText(s) {
  if (!s) return '';
  const cached = s.cached ? ` (${num(s.cached)} cached)` : '';
  return `${num(s.prompt)} prompt tokens${cached} · ${num(s.generated)} generated · ${tps(s.tps)} tok/s · ${dur(s.ms)}`;
}

function messageHtml(m, last) {
  const time = `<time class="ds-message__time" datetime="${isoTime(m.at)}">${clock(m.at).slice(0, 5)}</time>`;
  if (m.role === 'user') {
    return `<article class="ds-message ds-message--user" data-ds-key="${m.id}" aria-labelledby="a-${m.id}"><div class="ds-message__main">` +
      `<header class="ds-message__head"><span class="ds-message__author" id="a-${m.id}">You</span>${time}</header>` +
      `<div class="ds-message__content">${esc(m.content)}</div></div></article>`;
  }
  const state = m.live ? 'streaming' : m.state === 'done' ? '' : m.state;
  const note = m.state === 'error' && !m.live
    ? `<p class="ds-message__note">${icon('alertCircle', 16)}${esc(m.error || 'The answer did not complete.')}</p>` : '';
  const footer = m.live ? '' : '<footer class="ds-message__footer" role="group" aria-label="Message actions">' +
    (m.content ? `<button class="ds-btn ds-btn--quiet ds-btn--icon ds-sm" type="button" aria-label="Copy the answer" data-tip="Copy" data-act="copy-msg" data-msg="${m.id}">` +
      `<span class="ds-icon-swap" data-swap="a"><span class="ds-icon-swap__a">${icon('copy', 18)}</span><span class="ds-icon-swap__b">${icon('check', 18)}</span></span></button>` : '') +
    (last ? `<button class="ds-btn ds-btn--quiet ds-btn--icon ds-sm" type="button" aria-label="Answer again" data-tip="Answer again" data-act="retry-msg">${icon('refresh', 18)}</button>` : '') +
    (m.stats ? `<span class="ds-meta">${esc(statsText(m.stats))}</span>` : '') +
    '</footer>';
  return `<article class="ds-message ds-message--assistant" data-ds-key="${m.id}" data-ds-message="${state}" aria-labelledby="a-${m.id}">` +
    `<div class="ds-message__avatar" aria-hidden="true">${icon('sparkle', 20)}</div><div class="ds-message__main">` +
    `<header class="ds-message__head"><span class="ds-message__author" id="a-${m.id}">${esc(m.model || 'Assistant')}</span>${time}</header>` +
    reasoningHtml(m) +
    `<div class="ds-message__content ds-prose ds-prose--compact" id="c-${m.id}">${m.content ? md(m.content, m.id) : ''}</div>` +
    note + footer + '</div></article>';
}

const SUGGESTIONS = [
  ['How do you use my GPU, RAM and NVMe?', 'Where do you run?'],
  ['Write a haiku about a hard-working GPU.', 'A haiku'],
  ['Show me a Python client that streams from this API.', 'A Python client'],
];

function logHtml(chat) {
  if (!chat.messages.length) {
    return `<div data-ds-key="empty">${empty({ glyph: 'chat', title: 'Ask the model something', text: 'The answers come from the active model on this machine. Nothing leaves it.' })}` +
      `<div class="ds-suggestions ds-suggestions--wrap" role="group" aria-label="Suggestions" id="chat-suggestions">${SUGGESTIONS.map(([value, label]) =>
        `<button class="ds-chip" type="button" data-suggest="${esc(value)}"><span class="ds-chip__body">${icon('sparkles', 18, 'ds-chip__icon')}<span class="ds-chip__label">${esc(label)}</span></span></button>`).join('')}</div></div>`;
  }
  const lastAssistant = [...chat.messages].reverse().find((m) => m.role === 'assistant');
  return chat.messages.map((m) => messageHtml(m, m === lastAssistant && !run)).join('');
}

/* ── The live line ────────────────────────────────────────────────── */

function busyLine(word, rest) {
  return `<span class="ds-spinner ds-sm" aria-hidden="true"></span><span class="ds-shimmer" data-shimmer="${esc(word)}">${esc(word)}</span>${rest ? `<span class="ds-meta so-statusline__rest">${esc(rest)}</span>` : ''}`;
}

function quietLine(glyph, text, link = '') {
  return `<span class="ds-meta so-statusline__rest">${icon(glyph, 16)}${text}${link}</span>`;
}

/** The one line above the composer: always what happens right now. */
function statusLine() {
  const s = ctx.status;
  const e = s && s.engine;
  if (run) {
    const r = run;
    if (r.phase === 'sending') {
      if (e && (e.state === 'loading' || e.state === 'planning' || e.state === 'starting')) {
        return busyLine('Waiting for the model to load', e.progress != null ? `${e.message} · ${pct(e.progress)}` : e.message);
      }
      const reqs = (s && s.activity && s.activity.requests) || [];
      const mine = reqs.find((q) => q.source === 'chat' && q.phase === 'queued');
      if (mine) {
        const ahead = reqs.filter((q) => q !== mine && (q.phase !== 'queued' || q.startedAt < mine.startedAt)).length;
        return busyLine('Queued', ahead ? `behind ${ahead} ${ahead === 1 ? 'request' : 'requests'}` : 'waiting for the engine');
      }
      return busyLine('Sending', '');
    }
    if (r.phase === 'prompt' && r.progress) {
      const p = r.progress;
      return busyLine('Reading prompt', `${num(p.processed)} / ${num(p.total)} tokens${r.tps ? ` · ${tps(r.tps)} tok/s` : ''}${p.cache ? ` · ${num(p.cache)} cached` : ''}`);
    }
    const n = r.timings ? Math.max(0, r.timings.predicted_n - r.phaseStart) : r.count;
    const word = r.phase === 'thinking' ? 'Thinking' : 'Writing';
    return busyLine(word, `${num(n)} tokens${r.tps ? ` · ${tps(r.tps)} tok/s` : ''}`);
  }
  if (!e) return quietLine('info', 'Connecting ...');
  const models = '<a href="#/models">Models</a>';
  if (e.state === 'error' || e.state === 'crashed') {
    return quietLine('alertCircle', `${e.state === 'crashed' ? 'The engine crashed' : 'The engine could not start'}: ${esc(e.error || e.message)} · `, models);
  }
  if (!e.modelId || e.state === 'stopped') return quietLine('stopCircle', 'No model is loaded · activate one on ', models);
  if (e.state !== 'ready') {
    return quietLine('clock', `${esc(e.modelName)} is loading${e.progress != null ? ` · ${pct(e.progress)}` : ''} · you can send already, the answer waits for it`);
  }
  if (lastStats) return quietLine('checkCircle', esc(statsText(lastStats)));
  return quietLine('checkCircle', `Ready · ${esc(e.modelName)}${e.ctx ? ` · ${num(e.ctx)} tokens context` : ''}`);
}

/* ── Painting ─────────────────────────────────────────────────────── */

let painting = 0;

function paintSoon() {
  if (painting) return;
  painting = requestAnimationFrame(() => {
    painting = 0;
    paintRun();
    paintStatus();
  });
}

/* Only the parts of the streaming message that grow: the thoughts and the answer. */
function paintRun() {
  if (!run) return;
  const m = run.msg;
  const page = ctx.page;
  if (m.reasoning && !$(`#r-${m.id}`, page)) {
    renderLog();
    return;
  }
  const thoughts = $(`#r-${m.id}`, page);
  if (thoughts) DS.dom.morphHtml(thoughts, thoughtsHtml(m.reasoning));
  if (m.reasoningMs) {
    const details = thoughts && thoughts.closest('details');
    if (details && !details.hasAttribute('data-ds-reasoning-done')) {
      details.setAttribute('data-ds-reasoning-done', String(Math.round(m.reasoningMs)));
      $(`#r-sum-${m.id}`, page).textContent = `Thought for ${dur(m.reasoningMs)}`;
      if (!m.reasoningOpen) details.open = false;
    }
  }
  const content = $(`#c-${m.id}`, page);
  if (content && m.content) DS.dom.morphHtml(content, md(m.content, m.id));
  save();
}

function paintStatus() {
  const line = $('#chat-status', ctx.page);
  if (line) DS.dom.morphHtml(line, statusLine());
}

function renderLog() {
  const list = $('#chat-log', ctx.page);
  if (list) DS.dom.morphHtml(list, logHtml(current()));
}

let composer = null;
let transcript = null;

function setBusy(on) {
  if (composer && ctx.view === view) composer.set({ busy: on });
}

function stickToEnd() {
  if (transcript && ctx.view === view) transcript.stick();
}

/* ── Conversations and options ────────────────────────────────────── */

function historyHtml() {
  const chats = store.chats.filter((c) => c.messages.length);
  if (!chats.length) return empty({ glyph: 'history', title: 'No conversation yet', text: 'Each chat is kept in this browser, up to the last 40.' });
  return `<ul class="ds-list ds-list--interactive" aria-label="Conversations">${chats.map((c) =>
    `<li class="ds-list__item" data-ds-key="h-${c.id}"${c.id === store.currentId ? ' aria-current="true"' : ''}>` +
      `<span class="ds-list__text"><span class="ds-list__title"><button class="ds-list__action" type="button" data-open-chat="${c.id}">${esc(c.title || 'Untitled')}</button></span>` +
      `<span class="ds-list__meta">${c.messages.length} messages · ${ago(c.updatedAt)}</span></span>` +
      `<span class="ds-list__actions"><button class="ds-btn ds-btn--quiet ds-btn--icon ds-sm" type="button" data-delete-chat="${c.id}" aria-label="Delete ${esc(c.title || 'this conversation')}" data-tip="Delete">${icon('trash', 18)}</button></span></li>`).join('')}</ul>`;
}

function optionsHtml() {
  const o = store.options;
  return '<div class="ds-popover" id="chat-options" role="dialog" aria-labelledby="chat-options-title" hidden>' +
    '<div class="ds-popover__head"><h2 class="ds-popover__title" id="chat-options-title">Chat options</h2>' +
      `<button class="ds-btn ds-btn--quiet ds-btn--icon ds-popover__close" type="button" aria-label="Close" data-ds-close>${icon('close', 20)}</button></div>` +
    '<div class="ds-popover__body ds-stack">' +
      '<div class="ds-field"><label class="ds-field__label" for="opt-system">System prompt</label>' +
        `<textarea class="ds-input" id="opt-system" rows="3" placeholder="You are a helpful assistant." data-opt="system">${esc(o.system)}</textarea></div>` +
      '<div class="ds-form-grid ds-form-grid--2">' +
        '<div class="ds-field"><label class="ds-field__label" for="opt-temp">Temperature</label>' +
          `<input class="ds-input" id="opt-temp" type="number" min="0" max="2" step="0.05" placeholder="Model default" value="${esc(o.temperature)}" data-opt="temperature"></div>` +
        '<div class="ds-field"><label class="ds-field__label" for="opt-max">Max tokens</label>' +
          `<input class="ds-input" id="opt-max" type="number" min="1" step="1" placeholder="No limit" value="${esc(o.maxTokens)}" data-opt="maxTokens"></div>` +
      '</div>' +
      switchHtml({ id: 'opt-thinking', attrs: 'data-opt="thinking"', label: 'Thinking', checked: o.thinking, hint: 'Let models with a thinking mode reason before they answer.' }) +
      '<p class="ds-meta">For the next messages, in every chat. Kept in this browser.</p>' +
    '</div></div>';
}

/* ── The view ─────────────────────────────────────────────────────── */

const view = {
  title: 'Chat',
  fixed: true,
  load: () => Promise.resolve(null),

  html() {
    const chat = current();
    return head('Chat', 'Try the active model. Conversations stay in this browser.',
      `<button class="ds-btn ds-btn--quiet ds-btn--icon" type="button" data-act="history" aria-label="Past conversations" data-tip="Past conversations">${icon('history', 20)}</button>` +
      `<button class="ds-btn ds-btn--quiet ds-btn--icon" type="button" aria-haspopup="dialog" aria-expanded="false" aria-controls="chat-options" data-ds-popover="#chat-options" data-ds-popover-align="end" aria-label="Chat options" data-tip="Options">${icon('sliders', 20)}</button>` +
      optionsHtml() +
      `<button class="ds-btn ds-btn--tonal" type="button" data-act="new-chat">${icon('compose', 18)}New chat</button>`) +
      '<div class="so-chat">' +
        '<div class="ds-transcript so-fill" id="transcript" role="region" aria-label="Conversation" tabindex="0">' +
          `<div class="ds-transcript__list" role="log" aria-label="Messages" id="chat-log">${logHtml(chat)}</div>` +
        '</div>' +
        '<div class="so-statusline">' +
          `<p class="so-statusline__text" id="chat-status" aria-live="off">${statusLine()}</p>` +
          `<button class="ds-btn ds-btn--text ds-sm" type="button" data-act="engine-log">${icon('terminal', 16)}Engine log</button>` +
        '</div>' +
        '<form class="ds-composer so-composer" id="composer">' +
          '<textarea class="ds-composer__input ds-input--grow" rows="1" aria-label="Message" placeholder="Message the model"></textarea>' +
          '<button class="ds-btn ds-btn--filled ds-btn--icon" type="submit" aria-label="Send" data-tip="Send">' +
            `<span class="ds-icon-swap" data-swap="a"><span class="ds-icon-swap__a">${icon('send', 20)}</span><span class="ds-icon-swap__b">${icon('square', 20)}</span></span></button>` +
        '</form>' +
      '</div>';
  },

  mount() {
    const page = ctx.page;
    transcript = DS.transcript.create($('#transcript', page));
    composer = DS.composer.create($('#composer', page), { draft: 'chat' });
    composer.on('send', ({ text }) => {
      const value = String(text || '').trim();
      if (!value) return;
      composer.set({ text: '' });
      send(value);
    });
    composer.on('stop', stop);
    if (run) composer.set({ busy: true });
    transcript.stick();
    composer.focus();
  },

  status() {
    paintStatus();
  },

  leave() {
    if (composer) composer.destroy();
    if (transcript) transcript.destroy();
    composer = null;
    transcript = null;
    const options = $('#chat-options');
    if (options && !options.hidden) DS.overlay.close(options);
  },

  on(type, event) {
    const t = event.target;
    if ((type === 'input' || type === 'change') && t.hasAttribute && t.hasAttribute('data-opt')) {
      const key = t.getAttribute('data-opt');
      store.options[key] = t.type === 'checkbox' ? t.checked : t.value;
      write(KEY_OPTIONS, store.options);
      return;
    }
    if (type !== 'click') return;
    const suggest = t.closest('[data-suggest]');
    if (suggest) {
      send(suggest.getAttribute('data-suggest'));
      return;
    }
    const act = t.closest('[data-act]');
    if (!act) return;
    const what = act.getAttribute('data-act');
    if (what === 'new-chat') {
      if (run) stop();
      const chat = current();
      if (chat.messages.length) store.currentId = '';
      lastStats = null;
      current();
      save(true);
      renderLog();
      paintStatus();
      if (composer) composer.focus();
    } else if (what === 'history') {
      ctx.openSheet({ title: 'Conversations', subtitle: 'Kept in this browser', html: historyHtml(), opener: act });
    } else if (what === 'copy-msg') {
      const m = current().messages.find((x) => x.id === act.getAttribute('data-msg'));
      if (!m) return;
      navigator.clipboard.writeText(m.content).then(() => {
        const swap = $('.ds-icon-swap', act);
        if (swap) {
          swap.setAttribute('data-swap', 'b');
          setTimeout(() => swap.setAttribute('data-swap', 'a'), 1600);
        }
        toast('Copied.');
      }, () => toast('The browser did not allow copying.', { kind: 'error' }));
    } else if (what === 'retry-msg') {
      if (run) return;
      const chat = current();
      const i = chat.messages.map((m) => m.role).lastIndexOf('assistant');
      if (i >= 0) chat.messages.splice(i, 1);
      lastStats = null;
      send('');
    }
  },

  sheet(type, event) {
    if (type !== 'click') return;
    const open = event.target.closest('[data-open-chat]');
    if (open) {
      if (run) stop();
      store.currentId = open.getAttribute('data-open-chat');
      lastStats = null;
      save(true);
      renderLog();
      paintStatus();
      ctx.closeSheet();
      stickToEnd();
      return;
    }
    const del = event.target.closest('[data-delete-chat]');
    if (del) {
      const id = del.getAttribute('data-delete-chat');
      if (run && run.chat.id === id) stop();
      store.chats = store.chats.filter((c) => c.id !== id);
      if (store.currentId === id) store.currentId = '';
      save(true);
      DS.dom.morphHtml(ctx.sheetBody, historyHtml());
      renderLog();
      toast('The conversation was deleted.');
    }
  },
};

// The thoughts a reader opened stay open when the transcript renders again.
document.addEventListener('toggle', (event) => {
  const details = event.target;
  if (!details.matches || !details.matches('details[data-msg]')) return;
  const m = current().messages.find((x) => x.id === details.getAttribute('data-msg'));
  if (m && !m.live) m.reasoningOpen = details.open;
}, true);

export default view;
