/*
 * The internal JSON API of Socrates API and its live updates. Every request
 * that changes something carries the header X-Socrates, which the server
 * requires against cross-site requests. A failed request rejects with an
 * ApiError whose message is meant for the user, as the server wrote it.
 */

export class ApiError extends Error {
  constructor(status, message, fields = {}, body = {}) {
    super(message);
    this.status = status;
    this.fields = fields || {};
    this.body = body;
  }
}

let onUnauthorized = () => {};

/** Called when the server says the session is gone. */
export function whenSignedOut(handler) {
  onUnauthorized = handler;
}

export const NOT_ANSWERING = 'Socrates API does not answer. Check that the container is running and that this computer can reach it.';

export async function request(method, path, body) {
  const init = { method, credentials: 'same-origin', headers: { Accept: 'application/json' } };
  if (method !== 'GET') init.headers['X-Socrates'] = '1';
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let response;
  try {
    response = await fetch(path, init);
  } catch (error) {
    throw new ApiError(0, NOT_ANSWERING);
  }
  let data = {};
  try {
    data = await response.json();
  } catch (error) {
    data = {};
  }
  if (!response.ok) {
    if (response.status === 401 && path !== '/api/login') onUnauthorized();
    throw new ApiError(response.status, data.error || `The server answered ${response.status}. Try again.`, data.fields, data);
  }
  return data;
}

export const get = (path) => request('GET', path);
export const post = (path, body = {}) => request('POST', path, body);
export const put = (path, body = {}) => request('PUT', path, body);
export const patch = (path, body = {}) => request('PATCH', path, body);
export const del = (path) => request('DELETE', path);

/*
 * Live updates over server-sent events: "status" carries the whole status
 * object, "change" the topics whose lists moved (the page refetches them),
 * "log" new lines of the engine log. EventSource reconnects by itself;
 * onUp tells whether the stream is up.
 */
export function live({ onStatus, onChange, onLog, onUp = () => {} }) {
  const source = new EventSource('/api/events');
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
  source.addEventListener('change', (event) => {
    const data = read(event);
    onChange(new Set(data && Array.isArray(data.topics) ? data.topics : ['models', 'keys', 'requests', 'settings']));
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
      headers: { 'X-Socrates': '1', 'Content-Type': 'application/json', Accept: 'text/event-stream' },
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
    if (response.status === 401) onUnauthorized();
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
