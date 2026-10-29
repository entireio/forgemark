// Hash router. Each view renders into #app and may return a cleanup function
// (close EventSources, destroy charts, clear timers) called on navigation.

import { renderNewRun } from './views/newrun.js';
import { renderDashboard } from './views/dashboard.js';
import { renderHistory } from './views/history.js';
import { renderRuns } from './views/runs.js';
import { renderRace } from './views/race.js';

let cleanup = null;

function route() {
  if (cleanup) { try { cleanup(); } catch { /* view teardown is best-effort */ } cleanup = null; }
  const app = document.getElementById('app');
  app.innerHTML = '';

  const hash = location.hash || '#new';
  const [, view, arg] = hash.match(/^#([^/]*)\/?(.*)$/) || [];

  for (const a of document.querySelectorAll('[data-nav]')) {
    a.classList.toggle('active', a.dataset.nav === view || ((view === 'run' || view === 'race') && a.dataset.nav === 'runs'));
  }

  let out = null;
  if (view === 'run' && arg) out = renderDashboard(app, arg);
  else if (view === 'race' && arg) out = renderRace(app, arg);
  else if (view === 'runs') out = renderRuns(app);
  else if (view === 'history') out = renderHistory(app, arg);
  else out = renderNewRun(app);
  if (typeof out === 'function') cleanup = out;
}

window.addEventListener('hashchange', route);
route();
