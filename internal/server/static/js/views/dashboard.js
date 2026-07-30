// Live run dashboard. All state derives from the SSE event stream — a fresh
// page load, a late join, and a mid-run reconnect are the same code path
// (server replays from Last-Event-ID).

import { h } from '../dom.js';
import { api } from '../api.js';
import { openRunStream } from '../sse.js';
import { timeChart, targetColor, fmtNum, fmtMs } from '../charts.js';
import { levelCountdown, resultsSavedBanner } from './shared.js';

export function renderDashboard(app, runId) {
  const state = {
    hello: null,
    strategy: '',
    warmups: [],      // [{from, to}] unix-sec ranges for shading
    latRows: [],      // [ts, ...per-target {p50,p95,p99}] for percentile switching
    totals: {},       // per target id: {ok, cas, err, lastOps, p95}
    curLevel: null,   // latest level_start payload
    done: null,
  };
  let charts = {};
  let es = null;
  let ticker = null;
  let pct = 'p95';

  // --- static skeleton ---
  const badge = h('span', { class: 'badge running' }, 'connecting…');
  const cancelBtn = h('button', { class: 'danger small', style: { display: 'none' }, onclick: async () => {
    cancelBtn.disabled = true;
    try { await api.cancelRun(runId); } catch { cancelBtn.disabled = false; }
  } }, 'Cancel run');
  const workloadNote = h('span', { class: 'progress-note' });
  const levelsStrip = h('div', { class: 'levels-strip' });
  const banners = h('div');
  const tiles = h('div', { class: 'tiles' });
  const chartsBox = h('div');
  const resultsBox = h('div');

  app.append(
    h('div', { class: 'run-head' },
      h('h1', {}, `Run ${runId}`), badge, workloadNote, h('span', { class: 'spacer' }),
      h('a', { class: 'race-alt', href: `#race/${runId}` }, '⚡ race view'), cancelBtn),
    banners, levelsStrip, tiles, chartsBox, resultsBox);

  const warned = sessionStorage.getItem(`fm-warn-${runId}`);
  if (warned) {
    for (const wtext of JSON.parse(warned)) banners.append(h('div', { class: 'banner warn' }, '⚠ ' + wtext));
  }

  // --- charts are built once targets are known (hello) ---
  function buildCharts(targets) {
    const isClone = state.strategy === 'clone';
    const isSession = state.strategy === 'session';
    const bands = () => state.warmups;

    const mk = (title, note, opts) => {
      const el = h('div');
      const head = h('div', { class: 'chart-head' }, h('h2', {}, title), note ? h('span', { class: 'u-note' }, note) : null);
      const card = h('div', { class: 'card chart-card' }, head, el);
      chartsBox.append(card);
      return { el, head, chart: timeChart(el, opts) };
    };

    const tputSeries = targets.map((t) => ({ label: t.name, color: targetColor(t.id) }));
    if (isSession) {
      for (const t of targets) tputSeries.push({ label: `${t.name} clones`, color: targetColor(t.id), dash: [5, 5] });
    }
    charts.tput = mk(isClone ? 'Clone throughput' : 'Push throughput',
      isClone ? 'successful clones per second' : 'successful pushes per second' + (isSession ? ' · dashed = clones/s' : ''),
      { series: tputSeries, unit: fmtNum, bands }).chart;

    const latHead = mk('Latency', `rolling 10s window · shaded = warm-up`, {
      series: targets.map((t) => ({ label: t.name, color: targetColor(t.id) })),
      unit: fmtMs, bands,
    });
    charts.lat = latHead.chart;
    const seg = h('div', { class: 'seg' }, ['p50', 'p95', 'p99'].map((p) =>
      h('button', { type: 'button', class: p === pct ? 'on' : '', onclick: (ev) => {
        pct = p;
        for (const b of seg.querySelectorAll('button')) b.classList.toggle('on', b.textContent === p);
        refeedLatency();
      } }, p)));
    latHead.head.append(seg);

    const errSeries = [];
    for (const t of targets) errSeries.push({ label: `${t.name} err`, color: targetColor(t.id) });
    for (const t of targets) errSeries.push({ label: `${t.name} cas`, color: targetColor(t.id), dash: [5, 5] });
    charts.err = mk('Errors & contention', 'failures/s (solid) and CAS rejections/s (dashed)',
      { series: errSeries, unit: fmtNum, bands }).chart;
  }

  function refeedLatency() {
    if (!charts.lat) return;
    charts.lat.setAll(state.latRows.map((r) => [r.ts, ...r.vals.map((v) => (v ? v[pct] : null))]));
  }

  function buildTiles(targets) {
    tiles.innerHTML = '';
    for (const t of targets) {
      state.totals[t.id] = { cas: 0, err: 0 };
      t._tile = {
        ops: h('b', {}, '–'), p95: h('b', {}, '–'), errs: h('b', {}, '0'),
        label: h('div', { class: 'tlabel' }, t.remote),
        err: h('div', { class: 'terr', style: { display: 'none' } }),
        root: null,
      };
      t._tile.root = h('div', { class: 'tile', style: { '--tcolor': targetColor(t.id) } },
        h('div', { class: 'tname' }, h('span', { class: 'dot', style: { '--tcolor': targetColor(t.id) } }), t.name),
        t._tile.label,
        h('div', { class: 'stats' },
          h('div', { class: 'stat' }, t._tile.ops, h('span', {}, 'ops/s')),
          h('div', { class: 'stat' }, t._tile.p95, h('span', {}, 'p95 (10s)')),
          h('div', { class: 'stat' }, t._tile.errs, h('span', {}, 'errors'))),
        t._tile.err);
      tiles.append(t._tile.root);
    }
  }

  function renderLevels() {
    levelsStrip.innerHTML = '';
    const levels = state.hello ? state.hello.levels : [];
    const curIdx = state.curLevel ? state.curLevel.level_index : -1;
    levels.forEach((c, i) => {
      const cls = state.done || i < curIdx ? 'done' : i === curIdx ? 'current' : '';
      levelsStrip.append(h('span', { class: `level-chip ${cls}` }, `c=${c}`));
    });
    levelsStrip.append(progressBar, progressNote);
  }
  const progressBar = h('div', { class: 'progress' }, h('div', { style: { width: '0%' } }));
  const progressNote = h('span', { class: 'progress-note' });

  function tickProgress() {
    if (!state.curLevel || state.done) { progressBar.firstChild.style.width = state.done ? '100%' : '0%'; return; }
    const l = state.curLevel;
    const { total, elapsed, remain, warming } = levelCountdown(l);
    progressBar.firstChild.style.width = `${(elapsed / total) * 100}%`;
    progressNote.textContent =
      `level ${l.level_index + 1}: ${warming ? 'warming up' : 'measuring'} · ${remain}s left`;
  }

  // --- results table ---
  const resultCols = [
    ['c', (r) => r.concurrency], ['ok', (r) => r.ok], ['ops/s', (r) => r.ops_per_sec.toFixed(1)],
    ['p50', (r) => fmtMs(r.p50_ms)], ['p95', (r) => fmtMs(r.p95_ms)], ['p99', (r) => fmtMs(r.p99_ms)],
    ['p99.9', (r) => fmtMs(r.p999_ms)], ['max', (r) => fmtMs(r.max_ms)],
    ['cas', (r) => r.cas_failures], ['err', (r) => r.other_errors],
  ];
  let resultsTable = null;
  function appendLevelResult(ev) {
    if (!resultsTable) {
      resultsTable = h('table', { class: 'results' },
        h('thead', {}, h('tr', {}, h('th', {}, 'target'), resultCols.map(([name]) => h('th', {}, name)))),
        h('tbody'));
      resultsBox.append(h('div', { class: 'card' }, h('h2', {}, 'Level results'), resultsTable));
    }
    const tbody = resultsTable.querySelector('tbody');
    let first = true;
    for (const t of state.hello.targets) {
      const r = ev.targets[String(t.id)];
      if (!r) continue;
      tbody.append(h('tr', { class: first ? 'lvl-first' : '' },
        h('td', { class: 'tname' }, h('span', { class: 'dot', style: { '--tcolor': targetColor(t.id), marginRight: '6px' } }), t.name),
        resultCols.map(([, fn], ci) => h('td', { class: ci >= 8 && fn(r) > 0 ? 'num-bad' : '' }, String(fn(r))))));
      first = false;
    }
  }

  // --- event handlers ---
  const handlers = {
    hello(ev) {
      state.hello = ev;
      state.strategy = ev.workload.strategy;
      workloadNote.textContent = `${ev.workload.strategy} · sweep ${ev.levels.join(', ')} · ${ev.workload.duration_sec}s/level`;
      buildTiles(ev.targets);
      buildCharts(ev.targets);
      renderLevels();
      cancelBtn.style.display = '';
      badge.className = 'badge running';
      badge.textContent = 'running';
    },
    target_ready(ev) {
      const t = state.hello?.targets.find((x) => x.id === ev.target);
      if (t && t._tile) t._tile.label.textContent = `${ev.label} · ${ev.object_format}${ev.nodes > 1 ? ` · ${ev.nodes} nodes` : ''}`;
    },
    level_start(ev) {
      state.curLevel = ev;
      const t0 = Date.parse(ev.at) / 1000;
      state.warmups.push({ from: t0, to: t0 + ev.warmup_sec });
      renderLevels();
    },
    bucket(ev) {
      if (!state.hello) return;
      const targets = state.hello.targets;
      const isClone = state.strategy === 'clone';
      const vals = targets.map((t) => ev.targets[String(t.id)] || null);

      // throughput (+ dashed clone series for session)
      const tputVals = vals.map((v) => (v ? (isClone ? v.clone_ok || 0 : v.ok) : null));
      if (state.strategy === 'session') tputVals.push(...vals.map((v) => (v ? v.clone_ok || 0 : null)));
      charts.tput.push(ev.t, tputVals);

      // latency rows retained for percentile switching. Gate on the rolling
      // window having data (p95 > 0), NOT this second's completion count: the
      // collector keeps a 10s window precisely so a quiet second doesn't blank
      // the line, and BucketStats.ok is only that one second's successes.
      const latVals = vals.map((v) => {
        if (!v) return null;
        const p = isClone
          ? { p50: v.clone_p50_ms, p95: v.clone_p95_ms, p99: v.clone_p99_ms }
          : { p50: v.p50_ms, p95: v.p95_ms, p99: v.p99_ms };
        return p.p95 > 0 ? p : null;
      });
      state.latRows.push({ ts: ev.t, vals: latVals });
      charts.lat.push(ev.t, latVals.map((v) => (v ? v[pct] : null)));

      // Total failed operations = push errors + clone errors. Summing both is
      // correct for every strategy (the other is 0), and it's essential for
      // session, where a run whose clones all fail would otherwise show zero
      // errors and zero throughput.
      charts.err.push(ev.t, [
        ...vals.map((v) => (v ? (v.err || 0) + (v.clone_err || 0) : null)),
        ...vals.map((v) => (v ? v.cas : null)),
      ]);

      // tiles
      targets.forEach((t, i) => {
        const v = vals[i];
        if (!v || !t._tile) return;
        const tot = state.totals[t.id];
        tot.err += (v.err || 0) + (v.clone_err || 0);
        tot.cas += v.cas || 0;
        t._tile.ops.textContent = fmtNum(isClone ? v.clone_ok || 0 : v.ok);
        t._tile.p95.textContent = fmtMs(latVals[i] ? latVals[i].p95 : null) || '–';
        t._tile.errs.textContent = fmtNum(tot.err + tot.cas);
        t._tile.errs.style.color = tot.err + tot.cas > 0 ? 'var(--critical)' : '';
      });
    },
    level_result(ev) { appendLevelResult(ev); },
    target_error(ev) {
      banners.append(h('div', { class: 'banner error' },
        `✖ ${targetName(ev.target)}: ${ev.message}${ev.fatal ? ' — target removed from the run' : ''}`));
      const t = state.hello?.targets.find((x) => x.id === ev.target);
      if (t && t._tile && ev.fatal) {
        t._tile.root.classList.add('dead');
        t._tile.err.style.display = '';
        t._tile.err.textContent = ev.message;
      }
    },
    run_done(ev) {
      state.done = ev;
      badge.className = `badge ${ev.state}`;
      badge.textContent = ev.state;
      cancelBtn.style.display = 'none';
      renderLevels();
      tickProgress();
      const saved = resultsSavedBanner(ev);
      if (saved) banners.append(saved);
      if (es) { es.close(); es = null; }
    },
  };

  function targetName(id) {
    const t = state.hello?.targets.find((x) => x.id === id);
    return t ? t.name : (id < 0 ? 'run' : `target ${id}`);
  }

  es = openRunStream(runId, handlers);
  es.onerror = () => { if (!state.done) { badge.className = 'badge'; badge.textContent = 'reconnecting…'; } };
  es.onopen = () => { if (!state.done && state.hello) { badge.className = 'badge running'; badge.textContent = 'running'; } };
  ticker = setInterval(tickProgress, 500);

  return () => {
    if (es) es.close();
    clearInterval(ticker);
    Object.values(charts).forEach((c) => c.destroy && c.destroy());
  };
}
