// Push race: the demo view. Same SSE stream as the dashboard, rendered as a
// live head-to-head with three metrics to race on — throughput (cumulative
// successful ops), latency (rolling p50, lower wins), and reliability
// (success rate). Every number shown is measured, and a methodology caption
// built from the run's actual workload says exactly what is being watched.
// State derives entirely from the event stream, so a finished run replays
// straight to its final standings.

import { h } from '../dom.js';
import { openRunStream } from '../sse.js';
import { targetColor, fmtMs } from '../charts.js';
import { levelCountdown, resultsSavedBanner } from './shared.js';

const fmtInt = (n) => n.toLocaleString('en-US');

const METRICS = {
  pushes: { label: 'Throughput' },
  latency: { label: 'Latency' },
  reliability: { label: 'Reliability' },
};

export function renderRace(app, arg) {
  const [runId, argMetric] = String(arg).split('/');
  let metric = METRICS[argMetric] ? argMetric : 'pushes';

  const state = {
    hello: null,
    curLevel: null,
    done: null,
    isClone: false,
    isSession: false,
    lanes: {},       // target id → {ok: primary ops, good/errs: ALL ops incl. session clones, rates, lat, els, lane}
    finals: {},      // target id → last level_result row (measured window stats)
  };
  let es = null;
  let ticker = null;
  let raf = 0; // coalesces lane DOM updates: replay dispatches thousands of buckets

  const opsNoun = () => (state.isClone ? 'clones' : 'pushes');

  const badge = h('span', { class: 'badge running' }, 'connecting…');
  const clock = h('div', { class: 'race-clock' }, '–:––');
  const phase = h('div', { class: 'race-phase' }, '');
  const banners = h('div');
  const lanesBox = h('div', { class: 'race-lanes' });
  const finishBox = h('div');
  const science = h('p', { class: 'race-science' });

  const seg = h('div', { class: 'seg' }, Object.entries(METRICS).map(([key, m]) =>
    h('button', { type: 'button', class: key === metric ? 'on' : '', onclick: () => {
      metric = key;
      for (const b of seg.querySelectorAll('button')) b.classList.toggle('on', b.textContent === METRICS[metric].label);
      history.replaceState(null, '', `#race/${runId}/${metric}`);
      updateLanes();
      refinish();
    } }, m.label)));

  app.append(
    h('div', { class: 'race-head' },
      h('h1', {}, '⚡ Push race'),
      badge,
      seg,
      h('span', { class: 'spacer' }),
      h('a', { class: 'race-alt', href: `#run/${runId}` }, '📊 full dashboard')),
    h('div', { class: 'race-timer' }, clock, phase),
    banners, lanesBox, finishBox, science);

  function buildLanes(targets) {
    lanesBox.innerHTML = '';
    for (const t of targets) {
      const els = {
        big: h('div', { class: 'race-total' }, '–'),
        unit: h('span', { class: 'race-unit' }, ''),
        side: h('div', { class: 'race-rate' }, '–'),
        bar: h('div', { class: 'race-bar', style: { '--tcolor': targetColor(t.id), width: '2%' } }),
        crown: h('span', { class: 'race-crown' }, ''),
        note: h('div', { class: 'race-note' }, t.remote),
      };
      const lane = h('div', { class: 'race-lane', style: { '--tcolor': targetColor(t.id) } },
        h('div', { class: 'race-lane-head' },
          h('span', { class: 'dot', style: { '--tcolor': targetColor(t.id) } }),
          h('span', { class: 'race-name' }, t.name), els.crown,
          h('span', { class: 'spacer' }), els.side),
        h('div', { class: 'race-track' }, els.bar, h('span', { class: 'race-flag' }, '🏁')),
        h('div', { class: 'race-lane-foot' }, els.big, els.unit, els.note));
      state.lanes[t.id] = { ok: 0, good: 0, errs: 0, rates: [], lat: null, pop: false, els, lane };
      lanesBox.append(lane);
    }
  }

  // Every metric is maintained simultaneously from the buckets, so switching
  // mid-race just re-renders the same lane state through a different lens.
  function updateLanes() {
    const lanes = Object.values(state.lanes);
    if (!lanes.length) return;

    if (metric === 'pushes') {
      const leader = Math.max(1, ...lanes.map((l) => l.ok));
      for (const l of lanes) {
        const avg = l.rates.length ? l.rates.reduce((a, b) => a + b, 0) / l.rates.length : 0;
        l.els.big.textContent = fmtInt(l.ok);
        l.els.unit.textContent = opsNoun();
        l.els.side.textContent = `${avg >= 10 ? Math.round(avg) : avg.toFixed(1)} ${opsNoun()}/s`;
        l.els.bar.style.width = `${Math.max(2, (l.ok / leader) * 100)}%`;
        l.els.crown.textContent = lanes.length > 1 && l.ok === leader && l.ok > 0 ? '👑' : '';
      }
    } else if (metric === 'latency') {
      // Only positive p50s are meaningful; a sub-millisecond p50 can round to 0
      // and must not become the divisor (NaN width) or a shared crowned "best".
      const p50s = lanes.map((l) => l.lat && l.lat.p50 > 0 ? l.lat.p50 : null).filter((v) => v != null);
      const best = p50s.length ? Math.min(...p50s) : 0;
      for (const l of lanes) {
        l.els.big.textContent = l.lat ? fmtMs(l.lat.p50) : '–';
        l.els.unit.textContent = 'p50 · rolling 10s';
        l.els.side.textContent = l.lat ? `p95 ${fmtMs(l.lat.p95)}` : '–';
        // bar = relative speed: the fastest lane fills the track, a lane at
        // 2× its p50 reaches halfway. Lower latency literally looks faster.
        const p50 = l.lat && l.lat.p50 > 0 ? l.lat.p50 : 0;
        l.els.bar.style.width = `${Math.max(2, p50 > 0 ? (best / p50) * 100 : 2)}%`;
        l.els.crown.textContent = lanes.length > 1 && best > 0 && p50 === best ? '👑' : '';
      }
    } else { // reliability: ALL ops — for session, clones count alongside pushes
      const attempts = (l) => l.good + l.errs;
      const pcts = lanes.map((l) => (attempts(l) ? (l.good / attempts(l)) * 100 : null));
      const best = Math.max(...pcts.map((p) => (p == null ? -1 : p)));
      lanes.forEach((l, i) => {
        const pct = pcts[i];
        l.els.big.textContent = pct == null ? '–' : `${pct.toFixed(pct === 100 ? 0 : 1)}%`;
        l.els.unit.textContent = `of ${fmtInt(attempts(l))} attempts succeeded`;
        l.els.side.textContent = `${fmtInt(l.errs)} failures`;
        l.els.bar.style.width = `${Math.max(2, pct == null ? 2 : pct)}%`;
        l.els.crown.textContent = lanes.length > 1 && pct != null && pct === best ? '👑' : '';
      });
    }
  }

  function tickClock() {
    if (state.done) return;
    if (!state.curLevel) { clock.textContent = '–:––'; return; }
    const l = state.curLevel;
    const { remain, warming } = levelCountdown(l);
    clock.textContent = `${Math.floor(remain / 60)}:${String(remain % 60).padStart(2, '0')}`;
    phase.textContent = warming ? 'warming up' : `c=${l.concurrency} · racing`;
  }

  // The caption is built from the run's actual workload so it never overstates:
  // what the agents do, what the counters mean, and what's inside the numbers.
  function renderScience() {
    if (!state.hello) return;
    const w = state.hello.workload;
    const commit = `${w.files_min}–${w.files_max} files × ${fmtInt(w.file_size)} B`;
    const shape = {
      branch: `each commits ${commit} and force-pushes its own branch to one shared repo`,
      repo: `spread across multiple repos, each commits ${commit} and pushes its own branch`,
      clone: 'each shallow-clones the base branch, discards it, and repeats',
      session: `each shallow-clones, pushes ${w.session_commits} checkpoint commits (${commit}) to an ephemeral branch, abandons it, and repeats`,
    }[w.strategy] || '';
    const pack = w.strategy === 'clone' ? 'upload-pack' : 'packfile build + receive-pack';
    science.textContent =
      `What you're watching: ${state.hello.levels.join('/')} concurrent agents per forge — ${shape} — ` +
      `in a tight loop of real git over smart HTTP (${pack}). Identical workload on every lane, levels started simultaneously. ` +
      `Counters are successful ${opsNoun()}; rates are a 5 s rolling mean; latency is a rolling 10 s window percentile; ` +
      `reliability is successful ${w.strategy === 'session' ? 'operations (pushes and clones)' : opsNoun()} ÷ attempts ` +
      `(errors and CAS rejections count as failures)` +
      (w.warmup_sec > 0 ? `; the ${w.warmup_sec} s warm-up is excluded from final stats` : '') +
      `. The generator runs on this machine, so network round-trip to each forge is included in every number.`;
  }

  function refinish() {
    finishBox.innerHTML = '';
    if (state.done) finish(state.done);
  }

  function finish(ev) {
    clock.textContent = '0:00';
    phase.textContent = 'finished';
    const rows = (state.hello ? state.hello.targets : [])
      .map((t) => ({ t, lane: state.lanes[t.id], final: state.finals[t.id] }))
      .filter((r) => r.lane);
    if (!rows.length || !rows.some((r) => r.lane.ok > 0)) return;

    // Rank and phrase the verdict by the metric being watched. Final stats
    // come from the measured window (level_result), not the live counters.
    // An exact tie on the ranked value is reported as one — never a winner.
    let verdict = '';
    let tie = false;
    if (metric === 'latency') {
      rows.sort((a, b) => (a.final?.p50_ms ?? Infinity) - (b.final?.p50_ms ?? Infinity));
      const [win, next] = rows;
      if (win.final && next?.final) {
        tie = win.final.p50_ms === next.final.p50_ms;
        verdict = tie
          ? ` — ${fmtMs(win.final.p50_ms)} median on both`
          : ` — ${(next.final.p50_ms / win.final.p50_ms).toFixed(1)}× lower median latency`;
      }
    } else if (metric === 'reliability') {
      const pct = (r) => (r.lane.good + r.lane.errs ? r.lane.good / (r.lane.good + r.lane.errs) : -1);
      rows.sort((a, b) => pct(b) - pct(a) || b.lane.good - a.lane.good);
      const [win, next] = rows;
      if (next) {
        tie = pct(win) === pct(next);
        verdict = tie
          ? ` — ${(pct(win) * 100).toFixed(1)}% success across the board`
          : ` — ${(pct(win) * 100).toFixed(1)}% vs ${(pct(next) * 100).toFixed(1)}% success`;
      }
    } else {
      rows.sort((a, b) => b.lane.ok - a.lane.ok);
      const [win, next] = rows;
      if (next && next.lane.ok > 0) {
        tie = win.lane.ok === next.lane.ok;
        verdict = tie
          ? ` — ${fmtInt(win.lane.ok)} ${opsNoun()} each`
          : ` — ${(win.lane.ok / next.lane.ok).toFixed(1)}× more ${opsNoun()}`;
      }
    }
    const win = rows[0];

    finishBox.append(h('div', { class: 'race-podium card' },
      h('div', { class: 'race-winner' },
        tie
          ? ['🤝 dead heat', verdict]
          : ['🏆 ', h('b', { style: { color: targetColor(win.t.id) } }, win.t.name), ' wins', verdict]),
      h('table', { class: 'results' },
        h('thead', {}, h('tr', {}, h('th', {}, ''), h('th', {}, 'target'), h('th', {}, opsNoun()),
          h('th', {}, `${opsNoun()}/s`), h('th', {}, 'p50'), h('th', {}, 'p95'), h('th', {}, 'failures'))),
        h('tbody', {}, rows.map((r, i) => h('tr', {},
          h('td', {}, tie && i < 2 ? '🥇' : ['🥇', '🥈', '🥉'][i] || ''),
          h('td', { class: 'tname' }, h('span', { class: 'dot', style: { '--tcolor': targetColor(r.t.id), marginRight: '6px' } }), r.t.name),
          h('td', {}, fmtInt(r.lane.ok)),
          h('td', {}, r.final ? r.final.ops_per_sec.toFixed(1) : '–'),
          h('td', {}, r.final ? fmtMs(r.final.p50_ms) : '–'),
          h('td', {}, r.final ? fmtMs(r.final.p95_ms) : '–'),
          h('td', { class: r.lane.errs > 0 ? 'num-bad' : '' }, fmtInt(r.lane.errs))))))));
    const saved = resultsSavedBanner(ev);
    if (saved) finishBox.append(saved);
  }

  const handlers = {
    hello(ev) {
      state.hello = ev;
      state.isClone = ev.workload.strategy === 'clone';
      state.isSession = ev.workload.strategy === 'session';
      buildLanes(ev.targets);
      renderScience();
      badge.className = 'badge running';
      badge.textContent = 'running';
    },
    level_start(ev) { state.curLevel = ev; },
    bucket(ev) {
      if (!state.hello) return;
      for (const t of state.hello.targets) {
        const v = ev.targets[String(t.id)];
        const l = state.lanes[t.id];
        if (!v || !l) continue;
        const ok = state.isClone ? v.clone_ok || 0 : v.ok;
        let good = ok;
        let err = (state.isClone ? v.clone_err || 0 : v.err) + (v.cas || 0);
        if (state.isSession) {
          // Session agents clone AND push: both count toward reliability even
          // though throughput/latency lanes track the pushes.
          good += v.clone_ok || 0;
          err += v.clone_err || 0;
        }
        l.ok += ok;
        l.good += good;
        l.errs += err;
        l.rates.push(ok);
        if (l.rates.length > 5) l.rates.shift();
        l.lat = ok > 0
          ? (state.isClone
            ? { p50: v.clone_p50_ms, p95: v.clone_p95_ms }
            : { p50: v.p50_ms, p95: v.p95_ms })
          : l.lat;
        l.pop = l.pop || ok > 0;
      }
      // Coalesce DOM work: a finished-run replay delivers thousands of buckets
      // back-to-back, so lane updates (and the pop's forced reflow) run once
      // per animation frame, not once per bucket.
      if (!raf) {
        raf = requestAnimationFrame(() => {
          raf = 0;
          updateLanes();
          for (const l of Object.values(state.lanes)) {
            if (!l.pop) continue;
            l.pop = false;
            if (metric !== 'pushes') continue;
            l.els.big.classList.remove('pop');
            void l.els.big.offsetWidth; // restart the pop animation
            l.els.big.classList.add('pop');
          }
        });
      }
    },
    level_result(ev) {
      for (const [id, r] of Object.entries(ev.targets)) state.finals[id] = r;
    },
    target_error(ev) {
      const t = state.hello?.targets.find((x) => x.id === ev.target);
      banners.append(h('div', { class: 'banner error' },
        `✖ ${t ? t.name : `target ${ev.target}`}: ${ev.message}${ev.fatal ? ' — out of the race' : ''}`));
      if (t && ev.fatal) state.lanes[t.id]?.lane.classList.add('dead');
    },
    run_done(ev) {
      state.done = ev;
      badge.className = `badge ${ev.state}`;
      badge.textContent = ev.state;
      refinish();
      if (es) { es.close(); es = null; }
    },
  };

  es = openRunStream(runId, handlers);
  es.onerror = () => { if (!state.done) { badge.className = 'badge'; badge.textContent = 'reconnecting…'; } };
  ticker = setInterval(tickClock, 250);

  return () => {
    if (es) es.close();
    clearInterval(ticker);
    if (raf) cancelAnimationFrame(raf);
  };
}
