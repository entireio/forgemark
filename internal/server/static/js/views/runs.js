// Runs list: active + recent runs of this server session.

import { h } from '../dom.js';
import { api } from '../api.js';

export function renderRuns(app) {
  const box = h('div', { class: 'card' }, h('div', { class: 'empty' }, 'Loading…'));
  app.append(h('h1', {}, 'Runs'), h('p', { class: 'sub' }, 'Active and recent runs of this server session. Older runs live in History.'), box);

  api.listRuns().then((runs) => {
    box.innerHTML = '';
    if (!runs || !runs.length) {
      box.append(h('div', { class: 'empty' }, 'No runs yet — start one from ', h('a', { href: '#new', style: { color: 'var(--accent)' } }, 'New run'), '.'));
      return;
    }
    for (const r of runs) {
      box.append(h('div', { class: 'hist-row' },
        h('a', { class: 'file', href: `#run/${r.id}`, style: { color: 'var(--ink)' } }, r.id),
        h('span', { class: `badge ${r.state}` }, r.state),
        h('span', { class: 'meta' }, `${r.workload.strategy} · sweep ${r.levels.join(',')} · ${r.targets.map((t) => t.name).join(' vs ')}`),
        h('span', { class: 'spacer' }),
        h('span', { class: 'meta' }, new Date(r.started_at).toLocaleString())));
    }
  }).catch((err) => {
    box.innerHTML = '';
    box.append(h('div', { class: 'banner error' }, String(err.message || err)));
  });
}
