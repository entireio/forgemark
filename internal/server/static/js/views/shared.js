// Helpers shared by the run views (dashboard, race). Both derive their state
// from the same SSE stream, so the level-countdown math and the terminal
// "results saved" banner must agree — they live here once.

import { h } from '../dom.js';
import { api } from '../api.js';

// levelCountdown folds a level_start payload into clock state: total/elapsed
// seconds, whole seconds remaining, and whether the level is still warming up.
export function levelCountdown(level) {
  const t0 = Date.parse(level.at) / 1000;
  const total = level.warmup_sec + level.duration_sec;
  const elapsed = Math.min(Date.now() / 1000 - t0, total);
  return {
    total,
    elapsed,
    remain: Math.max(0, Math.ceil(total - elapsed)),
    warming: elapsed < level.warmup_sec,
  };
}

// cancelRunButton is the run views' shared stop control: hidden until the
// caller reveals it on hello, hidden again on run_done. Disable-on-click stops
// double-fire; a failed cancel re-enables so the operator can retry. One
// factory so a live load generator is stoppable the same way from every view.
export function cancelRunButton(runId) {
  const btn = h('button', { class: 'danger small', style: { display: 'none' }, onclick: async () => {
    btn.disabled = true;
    try { await api.cancelRun(runId); } catch { btn.disabled = false; }
  } }, 'Cancel run');
  return btn;
}

// resultsSavedBanner renders the run_done "Results saved to … compare in
// history" banner, or null when the run persisted nothing.
export function resultsSavedBanner(ev) {
  if (!ev.results_file) return null;
  return h('div', { class: 'banner' },
    'Results saved to ', h('code', {}, ev.results_file), ' — ',
    h('a', { href: `#history/${ev.results_file}`, style: { color: 'var(--accent)' } }, 'compare in history'));
}
