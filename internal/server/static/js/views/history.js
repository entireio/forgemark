// History: browse results/*.json (CLI and server runs alike) and overlay any
// selection as sweep curves — ops/s vs concurrency and p95 vs concurrency.
// A single selected run with a stored live series also replays its timeline.

import { h } from '../dom.js';
import { api } from '../api.js';
import { sweepChart, timeChart, targetColor, fmtNum, fmtMs } from '../charts.js';

// preselect (from #history/<file-or-run-id>) checks that run on load, so a
// finished run can deep-link straight into its comparison view.
export function renderHistory(app, preselect) {
  const selected = new Map(); // file → doc
  const listBox = h('div', { class: 'card' }, h('div', { class: 'empty' }, 'Loading…'));
  const compareBox = h('div');
  let liveCharts = [];

  app.append(
    h('h1', {}, 'History'),
    h('p', { class: 'sub' }, 'Every stored result — CLI and GUI runs alike. Select runs to overlay their sweep curves.'),
    listBox, compareBox);

  async function toggle(file, checked) {
    if (checked) {
      try { selected.set(file, await api.historyDoc(file)); }
      catch (err) { alert(String(err.message || err)); return; }
    } else {
      selected.delete(file);
    }
    renderCompare();
  }

  function seriesList() {
    // One series per (run, target): identity is stable across both charts.
    const out = [];
    for (const [file, doc] of selected) {
      for (const t of doc.targets || []) {
        if (!t.levels || !t.levels.length) continue;
        const runTag = selected.size > 1 ? ` (${doc.run_id || file})` : '';
        out.push({ label: `${t.name || 'target'}${runTag}`, target: t });
      }
    }
    return out.map((s, i) => ({ ...s, color: targetColor(i) }));
  }

  function renderCompare() {
    for (const c of liveCharts) c.destroy && c.destroy();
    liveCharts = [];
    compareBox.innerHTML = '';
    const series = seriesList();
    if (!series.length) return;

    const mkCard = (title, note) => {
      const el = h('div');
      compareBox.append(h('div', { class: 'card chart-card' },
        h('div', { class: 'chart-head' }, h('h2', {}, title), h('span', { class: 'u-note' }, note)), el));
      return el;
    };

    liveCharts.push(sweepChart(mkCard('Throughput vs concurrency', 'successful ops/s per level (x is log₂)'), {
      seriesData: series.map((s) => ({
        label: s.label, color: s.color,
        points: s.target.levels.map((l) => [l.concurrency, l.ops_per_sec]),
      })),
      unit: fmtNum,
    }));
    liveCharts.push(sweepChart(mkCard('p95 latency vs concurrency', 'per-op p95 in the measured window'), {
      seriesData: series.map((s) => ({
        label: s.label, color: s.color,
        points: s.target.levels.map((l) => [l.concurrency, l.p95_ms]),
      })),
      unit: fmtMs,
    }));

    // Timeline replay when exactly one run is selected and it stored series.
    if (selected.size === 1) {
      const doc = [...selected.values()][0];
      const withSeries = (doc.targets || []).filter((t) => t.series && t.series.length);
      if (withSeries.length) {
        const tputEl = mkCard('Timeline replay — throughput', 'stored 1s buckets from the live run');
        const latEl = mkCard('Timeline replay — p95 latency', 'rolling 10s window as recorded');
        const colors = new Map(seriesList().map((s) => [s.target, s.color]));
        const ts = [...new Set(withSeries.flatMap((t) => t.series.map((p) => p.t)))].sort((a, b) => a - b);
        const mkRows = (val) => ts.map((x) => [x, ...withSeries.map((t) => {
          const p = t.series.find((q) => q.t === x);
          return p ? val(p) : null;
        })]);
        // Clone-strategy series carry their numbers in the clone_* fields.
        const isClone = doc.strategy === 'clone';
        const okOf = (p) => (isClone ? p.clone_ok || 0 : p.ok);
        const p95Of = (p) => (isClone ? p.clone_p95_ms : p.p95_ms);
        const tput = timeChart(tputEl, { series: withSeries.map((t) => ({ label: t.name, color: colors.get(t) || targetColor(0) })), unit: fmtNum });
        tput.setAll(mkRows(okOf));
        const lat = timeChart(latEl, { series: withSeries.map((t) => ({ label: t.name, color: colors.get(t) || targetColor(0) })), unit: fmtMs });
        lat.setAll(mkRows((p) => (okOf(p) > 0 ? p95Of(p) : null)));
        liveCharts.push(tput, lat);
      }
    }
  }

  api.history().then((items) => {
    listBox.innerHTML = '';
    if (!items || !items.length) {
      listBox.append(h('div', { class: 'empty' }, 'No results yet. Finished runs land here as JSON docs.'));
      return;
    }
    for (const it of items) {
      const cb = h('input', { type: 'checkbox', style: { width: 'auto' }, onchange: (e) => toggle(it.file, e.target.checked) });
      if (preselect && (it.file === preselect || it.run_id === preselect)) {
        cb.checked = true;
        toggle(it.file, true);
      }
      listBox.append(h('div', { class: 'hist-row' },
        cb,
        h('span', { class: 'file' }, it.run_id || it.file),
        it.state ? h('span', { class: `badge ${it.state}` }, it.state) : null,
        h('span', { class: 'meta' }, `${it.strategy} · ${(it.targets || []).join(' vs ') || '—'} · ${it.levels} level${it.levels === 1 ? '' : 's'}`),
        h('span', { class: 'spacer' }),
        h('span', { class: 'meta' }, new Date(it.mtime).toLocaleString())));
    }
  }).catch((err) => {
    listBox.innerHTML = '';
    listBox.append(h('div', { class: 'banner error' }, String(err.message || err)));
  });

  return () => { for (const c of liveCharts) c.destroy && c.destroy(); };
}
