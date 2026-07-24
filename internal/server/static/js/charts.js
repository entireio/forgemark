// uPlot helpers. All charts share the dark chart chrome and the fixed
// categorical target palette: color follows the target index, never the
// series' position in a particular chart.

export const PALETTE = [
  '#3987e5', '#199e70', '#c98500', '#008300',
  '#9085e9', '#e66767', '#d55181', '#d95926',
];
export const targetColor = (i) => PALETTE[i % PALETTE.length];

const INK_MUTED = '#898781';
const GRID = '#2c2c2a';
const BASELINE = '#383835';

const axisFont = '12px system-ui, sans-serif';

function baseAxes(unitFmt) {
  return [
    {
      stroke: INK_MUTED, font: axisFont,
      grid: { stroke: GRID, width: 1 },
      ticks: { stroke: BASELINE, width: 1 },
    },
    {
      stroke: INK_MUTED, font: axisFont, size: 56,
      grid: { stroke: GRID, width: 1 },
      ticks: { show: false },
      values: (u, splits) => splits.map((v) => (v == null ? '' : unitFmt(v))),
    },
  ];
}

export const fmtNum = (v) => {
  if (v == null) return '';
  if (v >= 1000) return (v / 1000).toFixed(1) + 'k';
  return v >= 100 ? v.toFixed(0) : +v.toFixed(1) + '';
};
export const fmtMs = (v) => (v == null ? '' : v >= 1000 ? (v / 1000).toFixed(1) + 's' : Math.round(v) + 'ms');

// timeChart: a streaming time-series chart. `series` is
// [{label, color, dash?}] — one per plotted line. `bands()` returns
// [{from,to}] x-ranges (unix secs) shaded as warm-up. push()/setAll() update
// the data; the chart resizes with its container.
export function timeChart(el, { series, unit = fmtNum, height = 240, bands = () => [] }) {
  const data = [[], ...series.map(() => [])];

  const opts = {
    width: el.clientWidth || 800,
    height,
    ms: false,
    cursor: { points: { size: 7 } },
    scales: { x: { time: true } },
    axes: baseAxes(unit),
    legend: { live: true },
    series: [
      { value: (u, ts) => (ts == null ? '' : new Date(ts * 1000).toLocaleTimeString()) },
      ...series.map((s) => ({
        label: s.label,
        stroke: s.color,
        width: 2,
        dash: s.dash,
        spanGaps: false,
        points: { show: false },
        value: (u, v) => (v == null ? '–' : unit(v)),
      })),
    ],
    hooks: {
      drawClear: [
        (u) => {
          // Warm-up shading: quieter than data, under the series.
          const { ctx } = u;
          ctx.save();
          ctx.fillStyle = 'rgba(137, 135, 129, 0.07)';
          for (const b of bands()) {
            const x0 = u.valToPos(b.from, 'x', true);
            const x1 = u.valToPos(b.to, 'x', true);
            ctx.fillRect(x0, u.bbox.top, x1 - x0, u.bbox.height);
          }
          ctx.restore();
        },
      ],
    },
  };

  const u = new uPlot(opts, data, el);
  const ro = new ResizeObserver(() => u.setSize({ width: el.clientWidth, height }));
  ro.observe(el);

  // setData re-feeds the full arrays (a complete redraw), so pushes coalesce
  // through one requestAnimationFrame: an SSE replay that dispatches thousands
  // of historical buckets costs one redraw per frame, not one per bucket.
  let raf = 0;
  const flush = () => { raf = 0; u.setData(data); };

  return {
    u,
    push(ts, vals) {
      data[0].push(ts);
      vals.forEach((v, i) => data[i + 1].push(v));
      if (!raf) raf = requestAnimationFrame(flush);
    },
    setAll(rows) {
      // rows: [[ts, v1, v2, …], …]
      if (raf) { cancelAnimationFrame(raf); raf = 0; }
      for (let i = 0; i < data.length; i++) data[i] = rows.map((r) => r[i] ?? null);
      u.setData(data);
    },
    destroy() {
      if (raf) cancelAnimationFrame(raf);
      ro.disconnect();
      u.destroy();
    },
  };
}

// sweepChart: results vs concurrency (log-2 x), for finished-run comparison.
// seriesData: [{label, color, points: [[concurrency, value], …]}].
export function sweepChart(el, { seriesData, unit = fmtNum, height = 260 }) {
  const xs = [...new Set(seriesData.flatMap((s) => s.points.map((p) => p[0])))].sort((a, b) => a - b);
  const data = [xs, ...seriesData.map((s) => {
    const byX = new Map(s.points);
    return xs.map((x) => byX.has(x) ? byX.get(x) : null);
  })];

  const opts = {
    width: el.clientWidth || 800,
    height,
    cursor: { points: { size: 8 } },
    scales: { x: { time: false, distr: 3, log: 2 } },
    axes: [
      { ...baseAxes(unit)[0], values: (u, splits) => splits.map((v) => (v == null ? '' : String(v))) },
      baseAxes(unit)[1],
    ],
    legend: { live: true },
    series: [
      { label: 'concurrency' },
      ...seriesData.map((s) => ({
        label: s.label,
        stroke: s.color,
        width: 2,
        spanGaps: true,
        points: { show: true, size: 7, fill: s.color },
        value: (u, v) => (v == null ? '–' : unit(v)),
      })),
    ],
  };

  const u = new uPlot(opts, data, el);
  const ro = new ResizeObserver(() => u.setSize({ width: el.clientWidth, height }));
  ro.observe(el);
  return { u, destroy() { ro.disconnect(); u.destroy(); } };
}
