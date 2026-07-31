// Push race: the demo view. Same SSE stream as the dashboard, rendered as a
// live head-to-head with three metrics to race on — throughput (cumulative
// successful ops), latency (rolling p50, lower wins), and reliability
// (success rate). Every number shown is measured, and a methodology caption
// built from the run's actual workload says exactly what is being watched.
// State derives entirely from the event stream, so a finished run replays
// straight to its final standings.

import { h } from '../dom.js';
import { openRunStream } from '../sse.js';
import { targetColor, fmtMs, bucketSecs } from '../charts.js';
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
    finals: {},      // target id → last level_result row (measured window stats), stamped with _level
    finalLevel: -1,  // highest level_index seen; a final counts only if it's from this level
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
        // Rate = total ops / total covered time over the rolling window, not a
        // mean of per-bucket rates: buckets are duration-weighted, so a
        // milliseconds-wide edge bucket can't swing the displayed rate the way
        // it would if its instantaneous rate were averaged in equally.
        const win = l.rates.reduce((a, b) => ({ ok: a.ok + b.ok, dt: a.dt + b.dt }), { ok: 0, dt: 0 });
        const avg = win.dt > 0 ? win.ok / win.dt : 0;
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
    phase.textContent = ev.state === 'done' ? 'finished' : ev.state;
    // A podium is only meaningful for a run that completed its full sweep: a
    // cancelled or failed run stopped mid-window, and crowning a winner from
    // those partial samples would present an unfinished race as a result.
    if (ev.state !== 'done') {
      finishBox.append(h('div', { class: 'banner' },
        `run ${ev.state} — no podium, the race didn't finish`));
      return;
    }
    // finalOf returns a target's result only when it's from the final level: a
    // target that died at a lower concurrency keeps that lower-level result in
    // state.finals, and comparing it against other targets' final-level numbers
    // would award an invalid winner.
    const finalOf = (r) => (r.final && r.final._level === state.finalLevel ? r.final : null);
    const rows = (state.hello ? state.hello.targets : [])
      .map((t) => ({ t, lane: state.lanes[t.id], final: state.finals[t.id] }))
      .filter((r) => r.lane);
    // No success gate here: a completed run where every operation failed is
    // still a measured outcome — each metric branch below renders its own
    // "no successful …" verdict, and the table shows the failure counts.
    if (!rows.length) return;

    // Rank and phrase the verdict by the metric being watched. Final stats
    // come from the measured window (level_result), not the live counters.
    // An exact tie on the ranked value is reported as one — never a winner.
    let verdict = '';
    let tie = false;
    if (metric === 'latency') {
      // A target with no final-level result, no successes, or a non-positive
      // p50 (an all-failed level publishes p50_ms: 0) has no comparable latency;
      // rank it as Infinity so it sorts last and can never win.
      const latOf = (r) => { const f = finalOf(r); return f && f.ok > 0 && f.p50_ms > 0 ? f.p50_ms : Infinity; };
      rows.sort((a, b) => latOf(a) - latOf(b));
      const [win, next] = rows;
      if (latOf(win) === Infinity) {
        tie = true; // nobody has a comparable latency — suppress a lone winner
        verdict = ' — no comparable latency at the final level';
      } else {
        tie = next && latOf(next) === latOf(win);
        verdict = tie
          ? ` — ${fmtMs(finalOf(win).p50_ms)} median on both`
          : next && latOf(next) !== Infinity
            ? ` — ${(finalOf(next).p50_ms / finalOf(win).p50_ms).toFixed(1)}× lower median latency`
            : ' — only target with a comparable median latency';
      }
    } else if (metric === 'reliability') {
      // Rank from the final level's measured result, like latency — never the
      // cumulative live counters: those mix every level, including partial
      // levels from a target that died early, so a lane's cumulative rate can
      // out-rank the survivors and contradict the authoritative table below.
      // A target without a final-level result gets -1 and can never win.
      const pct = (r) => {
        const f = finalOf(r);
        if (!f) return -1;
        let good = f.ok;
        let bad = f.cas_failures + f.other_errors;
        if (state.isSession) { good += f.clone_ok || 0; bad += f.clone_errors || 0; }
        return good + bad > 0 ? good / (good + bad) : -1;
      };
      rows.sort((a, b) => pct(b) - pct(a) || (finalOf(b)?.ok || 0) - (finalOf(a)?.ok || 0));
      const [win, next] = rows;
      if (pct(win) < 0) {
        tie = true; // nobody finished the final level — suppress a lone winner
        verdict = ' — no final-level result to compare';
      } else if (next) {
        tie = pct(win) === pct(next);
        verdict = tie
          ? ` — ${(pct(win) * 100).toFixed(1)}% success across the board`
          : pct(next) >= 0
            ? ` — ${(pct(win) * 100).toFixed(1)}% vs ${(pct(next) * 100).toFixed(1)}% success at the final level`
            : ' — only target with a final-level result';
      }
    } else {
      // Throughput ranks the final level's measured rate (the table's ops/s
      // column), not the cumulative lane counter, for the same reason as
      // reliability: cumulative counts include partial levels from dead
      // targets and would let the verdict contradict the table.
      const rateOf = (r) => { const f = finalOf(r); return f ? f.ops_per_sec : -1; };
      rows.sort((a, b) => rateOf(b) - rateOf(a) || b.lane.ok - a.lane.ok);
      const [win, next] = rows;
      if (rateOf(win) <= 0) {
        tie = true; // no successful final-level ops anywhere — nothing to crown
        verdict = ` — no successful ${opsNoun()} at the final level`;
      } else if (next) {
        tie = rateOf(win) === rateOf(next);
        verdict = tie
          ? ` — ${rateOf(win).toFixed(1)} ${opsNoun()}/s each at the final level`
          : rateOf(next) > 0
            ? ` — ${(rateOf(win) / rateOf(next)).toFixed(1)}× higher throughput at the final level`
            : ' — only target with a final-level result';
      }
    }
    const win = rows[0];

    // Every table column comes from the final level's measured window — one
    // denominator per row. Mixing the lanes' whole-run cumulative counters
    // with final-level rates/latencies here would put incompatible windows
    // side by side in one row of a multi-level run; the note says which
    // window is being scored, and the lanes above keep the run totals.
    // Push and clone are separate op streams for the session strategy, so
    // they get separate columns rather than a blended failure count that
    // would make successful pushes look unreliable when clones failed.
    const failsOf = (f) => f.cas_failures + f.other_errors;
    const finalConc = state.hello && state.finalLevel >= 0 ? state.hello.levels[state.finalLevel] : null;
    const sess = state.isSession;
    finishBox.append(h('div', { class: 'race-podium card' },
      h('div', { class: 'race-winner' },
        tie
          ? ['🤝 dead heat', verdict]
          : ['🏆 ', h('b', { style: { color: targetColor(win.t.id) } }, win.t.name), ' wins', verdict]),
      h('div', { class: 'u-note' },
        `scored on the final level's measured window${finalConc != null ? ` (c=${finalConc})` : ''} — the lane counters above are whole-run totals`),
      h('table', { class: 'results' },
        h('thead', {}, h('tr', {}, h('th', {}, ''), h('th', {}, 'target'), h('th', {}, opsNoun()),
          h('th', {}, `${opsNoun()}/s`), h('th', {}, 'p50'), h('th', {}, 'p95'), h('th', {}, 'failures'),
          sess ? [h('th', {}, 'clones'), h('th', {}, 'clone err')] : null)),
        h('tbody', {}, rows.map((r, i) => {
          const f = finalOf(r); // final-level result only; a stale earlier level shows '–'
          return h('tr', {},
            h('td', {}, tie && i < 2 ? '🥇' : ['🥇', '🥈', '🥉'][i] || ''),
            h('td', { class: 'tname' }, h('span', { class: 'dot', style: { '--tcolor': targetColor(r.t.id), marginRight: '6px' } }), r.t.name),
            h('td', {}, f ? fmtInt(f.ok) : '–'),
            h('td', {}, f ? f.ops_per_sec.toFixed(1) : '–'),
            h('td', {}, f ? fmtMs(f.p50_ms) : '–'),
            h('td', {}, f ? fmtMs(f.p95_ms) : '–'),
            h('td', { class: f && failsOf(f) > 0 ? 'num-bad' : '' }, f ? fmtInt(failsOf(f)) : '–'),
            sess ? [
              h('td', {}, f ? fmtInt(f.clone_ok || 0) : '–'),
              h('td', { class: f && f.clone_errors > 0 ? 'num-bad' : '' }, f ? fmtInt(f.clone_errors || 0) : '–'),
            ] : null);
        })))));
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
      // Normalize this bucket's counts to a per-second rate: the first
      // post-warmup bucket and the tail flush cover only a fraction of a second
      // (ev.dt_ms), so a raw count would over/understate the rolling rate.
      const dt = bucketSecs(ev);
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
        if (dt > 0) {
          // Keep count and duration, not a precomputed rate: the rolling rate
          // is sum(ok)/sum(dt) so fractional edge buckets weigh by their size.
          l.rates.push({ ok, dt });
          if (l.rates.length > 5) l.rates.shift();
        }
        // Latency is the collector's rolling 10s-window percentile, reported
        // every bucket regardless of this second's completions. Track it whenever
        // the window has data (p95 > 0) and clear it to null when the window
        // empties, so a stalled target stops displaying — and winning with — a
        // stale latency.
        const lp = state.isClone
          ? { p50: v.clone_p50_ms, p95: v.clone_p95_ms }
          : { p50: v.p50_ms, p95: v.p95_ms };
        l.lat = lp.p95 > 0 ? lp : null;
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
      // Stamp each result with its level so the podium can tell a target's
      // final-level result from a stale earlier one it kept after dying.
      for (const [id, r] of Object.entries(ev.targets)) state.finals[id] = { ...r, _level: ev.level_index };
      state.finalLevel = Math.max(state.finalLevel, ev.level_index);
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
