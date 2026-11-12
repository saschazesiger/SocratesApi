/*
 * Markdown of the model's answers, through the renderer Z-Dash ships
 * (DS.markdown): CommonMark's subset plus tables, task lists and
 * strikethrough, every character escaped and no HTML let through, links
 * only to http, https and mailto. Line breaks are kept (a poem stays a
 * poem) and code blocks come in a frame with their language and a copy
 * button. Hostile input renders in linear time, so a long streamed answer
 * can be rendered again on every chunk.
 */

const DS = window.DS;

const OPTIONS = { breaks: true, code: 'block', images: false, headingOffset: 2, target: '_blank', rel: 'noopener noreferrer' };

/** The markup of an answer; `idPrefix` keeps the ids of two answers apart. */
export function renderMarkdown(source, idPrefix = '') {
  return DS.markdown.render(String(source || ''), { ...OPTIONS, idPrefix });
}
