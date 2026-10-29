// EventSource wrapper for a run's event stream. The browser reconnects
// automatically and re-sends Last-Event-ID; the server replays anything
// missed, so handlers can treat the stream as gapless and in-order. The
// caller closes it on run_done — after that the stream only ever replays.

export function openRunStream(runId, handlers) {
  const es = new EventSource(`/api/runs/${encodeURIComponent(runId)}/events`);
  for (const [name, fn] of Object.entries(handlers)) {
    es.addEventListener(name, (ev) => fn(JSON.parse(ev.data)));
  }
  return es;
}
