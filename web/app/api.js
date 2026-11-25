/*
 * The internal JSON API of Socrates API, through the Z-Dash server's shell
 * (/z-dash/server/api.js): every request that changes something carries the
 * header X-Z-Dash, which the server requires against cross-site requests,
 * and a failed request rejects with an ApiError whose message is meant for
 * the user, as the server wrote it. A 401 is the shell's: it shows the
 * sign-in card. The change notices of the lists (models, keys, requests,
 * settings) are the shell's too (on() of /z-dash/server/shell.js, wired in
 * main.js); the live status and the engine log come on a stream of their own.
 */

import { ApiError, CSRF_HEADER, request, get } from '/z-dash/server/api.js';

export { ApiError, request, get, post, put, del } from '/z-dash/server/api.js';

export const patch = (path, body = {}) => request('PATCH', path, body);

const NOT_ANSWERING = 'Socrates API does not answer. Check that the container is running and that this computer can reach it.';

/*
 * The live status and the engine log over server-sent events (GET
 * /api/stream): "status" carries the whole status object, "log" new lines of
 * the engine log. EventSource reconnects by itself while the server is away;
 * a refused stream (signed out) stays closed. onUp tells whether it is up.
 */
export function live({ onStatus, onLog, onUp = () => {} }) {
  const source = new EventSource('/api/stream');
  const read = (event) => {
    try {
      return JSON.parse(event.data);
    } catch (error) {
      return null;
    }
  };
  source.addEventListener('open', () => onUp(true));
  source.addEventListener('error', () => onUp(source.readyState === EventSource.OPEN));
  source.addEventListener('status', (event) => {
    const data = read(event);
    if (data) onStatus(data);
  });
  source.addEventListener('log', (event) => {
    const data = read(event);
    if (data && Array.isArray(data.lines) && data.lines.length) onLog(data.lines);
  });
  return source;
}

/*
 * The chat stream: POST /api/chat answers with the server-sent events of
 * /v1/chat/completions. Each parsed chunk goes to onChunk; the promise
 * settles at [DONE], at the end of the stream, or rejects with an ApiError
 * (an answer that is no stream, or an error chunk). An abort rejects with
 * the AbortError of fetch.
 */
export async function chatStream(body, { signal, onChunk }) {
  let response;
  try {
    response = await fetch('/api/chat', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { [CSRF_HEADER]: '1', 'Content-Type': 'application/json', Accept: 'text/event-stream' },
      body: JSON.stringify(body),
      signal,
    });
  } catch (error) {
    if (error.name === 'AbortError') throw error;
    throw new ApiError(0, NOT_ANSWERING);
  }
  if (!response.ok || !response.body) {
    let data = {};
    try {
      data = await response.json();
    } catch (error) {
      data = {};
    }
    // The session ended: a request through the shell's helper lets the shell hear it and show the sign-in.
    if (response.status === 401) get('/api/status').catch(() => {});
    throw new ApiError(response.status, data.error || `The server answered ${response.status}.`, data.fields, data);
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let end;
    while ((end = buffer.indexOf('\n')) >= 0) {
      const line = buffer.slice(0, end).replace(/\r$/, '');
      buffer = buffer.slice(end + 1);
      if (!line.startsWith('data:')) continue;
      const data = line.slice(5).trim();
      if (data === '[DONE]') return;
      let chunk;
      try {
        chunk = JSON.parse(data);
      } catch (error) {
        continue;
      }
      if (chunk.error) {
        const message = typeof chunk.error === 'string' ? chunk.error : chunk.error.message;
        throw new ApiError(chunk.error.code || 500, message || 'The model stopped with an error.');
      }
      onChunk(chunk);
    }
  }
}
