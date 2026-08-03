package server

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/entireio/forgemark/internal/bench"
)

// demoRunner synthesizes push samples so the whole pipeline — level barriers,
// collector, SSE, charts, persistence — can be exercised and demoed with no
// forge and no network. It is selected by a demo:// remote:
//
//	demo://fast?p50=80ms&spread=3&err=0.01&cas=0.02&cap=200
//
//	p50     median latency (Go duration; default 80ms)
//	spread  p99/p50 shape multiplier of the log-normal tail (default 3)
//	err     probability an op fails (default 0.01)
//	cas     probability an op is a CAS rejection (default 0)
//	cap     ops/sec saturation ceiling; approaching it inflates latency the
//	        way a real forge queues (default 0 = unbounded)
//
// Each concurrency level runs c goroutines that sleep a sampled latency per
// op, so throughput scales with concurrency until cap bends the curve — the
// same shapes a real sweep produces.
type demoRunner struct {
	label   string
	w       bench.Workload
	sink    bench.Sink
	p50     time.Duration
	sigma   float64 // log-normal shape derived from spread
	errRate float64
	casRate float64
	cap     float64
}

func isDemoRemote(remote string) bool {
	u, err := url.Parse(remote)
	return err == nil && u.Scheme == "demo"
}

func newDemoRunner(remote string, w bench.Workload, sink bench.Sink) (*demoRunner, error) {
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "demo" {
		return nil, fmt.Errorf("invalid demo remote %q", remote)
	}
	q := u.Query()
	d := &demoRunner{label: remote, w: w, sink: sink, p50: 80 * time.Millisecond, errRate: 0.01}
	spread := 3.0
	if v := q.Get("p50"); v != "" {
		if d.p50, err = time.ParseDuration(v); err != nil || d.p50 <= 0 {
			return nil, fmt.Errorf("demo: invalid p50 %q", v)
		}
	}
	for name, dst := range map[string]*float64{"spread": &spread, "err": &d.errRate, "cas": &d.casRate, "cap": &d.cap} {
		if v := q.Get(name); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			// Reject NaN/±Inf explicitly: ParseFloat accepts them, and they pass a
			// bare `f < 0` check, then poison sigma/durations and can spin a tight
			// synthetic loop.
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
				return nil, fmt.Errorf("demo: invalid %s %q", name, v)
			}
			*dst = f
		}
	}
	if spread < 1 {
		return nil, fmt.Errorf("demo: spread must be >= 1")
	}
	// err and cas are probabilities per operation.
	if d.errRate > 1 || d.casRate > 1 {
		return nil, fmt.Errorf("demo: err and cas must be probabilities in [0,1]")
	}
	// Both outcomes resolve from one shared roll (bands [0,err) and
	// [err,err+cas)), so a sum over 1 would silently clip the CAS band — e.g.
	// err=0.8&cas=0.8 would yield only ~0.2 CAS instead of the requested 0.8.
	if d.errRate+d.casRate > 1 {
		return nil, fmt.Errorf("demo: err + cas must not exceed 1 (each op resolves one shared probability roll)")
	}
	// For a log-normal around the median, p99 = median * exp(sigma * z99).
	d.sigma = math.Log(spread) / 2.326
	return d, nil
}

func (d *demoRunner) Label() string        { return d.label }
func (d *demoRunner) Nodes() int           { return 1 }
func (d *demoRunner) ObjectFormat() string { return "sha1" }

// nextOp mirrors the real strategies' op mix so demo buckets exercise the
// same consumer paths: clone strategy emits only clones; session emits one
// clone and then, ONLY if that clone succeeded, SessionCommits pushes before
// the next clone — a real session agent retries the clone immediately after
// a clone failure (runSessions' `continue`), so an error-heavy demo must not
// publish pushes against a session that never cloned. pushesLeft is the
// agent's session state: how many pushes remain in its current session.
func (d *demoRunner) nextOp(pushesLeft int) bench.OpKind {
	switch d.w.Strategy {
	case "clone":
		return bench.OpClone
	case "session":
		if pushesLeft == 0 {
			return bench.OpClone
		}
	}
	return bench.OpPush
}

// demoAgg folds a level's synthetic samples into bounded state as they are
// produced, instead of retaining them for an end-of-level summarize: at the
// supported envelope (4096 agents × the 1ms latency floor × a long level,
// times up to 8 targets) retained samples reach millions per second and OOM
// the server. Counters plus two fixed-size histograms (~2KB) carry everything
// LevelResult needs; percentiles come out with the histogram's ~6.7% bucket
// resolution, which is indistinguishable on a chart of synthetic data.
type demoAgg struct {
	mu              sync.Mutex
	warmup          time.Duration
	pushes, ok, cas int
	errs            int
	clones, cloneOK int
	cloneErrs       int
	lat, cloneLat   latHist
}

func (a *demoAgg) add(s bench.Sample) {
	if s.Offset < a.warmup {
		return // warm-up: excluded from every statistic, like summarize
	}
	idx := latHistIdx(float64(s.Dur) / float64(time.Millisecond))
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.Op == bench.OpClone {
		a.clones++
		if s.Res == bench.OutcomeOK {
			a.cloneOK++
			a.cloneLat[idx]++
		} else {
			a.cloneErrs++
		}
		return
	}
	a.pushes++
	switch s.Res {
	case bench.OutcomeOK:
		a.ok++
		a.lat[idx]++
	case bench.OutcomeCAS:
		a.cas++
	default:
		a.errs++
	}
}

// result assembles the LevelResult the way bench.summarize would have,
// including the clone-strategy remap of clone stats into the primary fields.
func (a *demoAgg) result(c int, strategy string, window time.Duration, commitDesc string) bench.LevelResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := bench.LevelResult{
		Concurrency: c, Strategy: strategy, Repos: 1, Nodes: 1,
		WindowSec: window.Seconds(), CommitFiles: commitDesc,
		Pushes: a.pushes, OK: a.ok, CASFailures: a.cas, OtherErrors: a.errs,
		Clones: a.clones, CloneOK: a.cloneOK, CloneErrors: a.cloneErrs,
		P50ms: a.lat.quantile(50), P95ms: a.lat.quantile(95),
		P99ms: a.lat.quantile(99), P999ms: a.lat.quantile(99.9), Maxms: a.lat.maxValue(),
		CloneP50ms: a.cloneLat.quantile(50), CloneP95ms: a.cloneLat.quantile(95), CloneP99ms: a.cloneLat.quantile(99),
	}
	if a.errs > 0 {
		r.ErrorMessages = []bench.ErrGroup{{Message: "demo: synthetic error", Count: a.errs}}
	}
	if a.cloneErrs > 0 {
		r.CloneErrorMessages = []bench.ErrGroup{{Message: "demo: synthetic error", Count: a.cloneErrs}}
	}
	if strategy == "clone" {
		r.Pushes, r.OK, r.OtherErrors, r.CASFailures = r.Clones, r.CloneOK, r.CloneErrors, 0
		r.ErrorMessages = r.CloneErrorMessages
		r.P50ms, r.P95ms, r.P99ms = r.CloneP50ms, r.CloneP95ms, r.CloneP99ms
		r.P999ms, r.Maxms = a.cloneLat.quantile(99.9), a.cloneLat.maxValue()
	}
	if window > 0 {
		r.OpsPerSec = float64(r.OK) / window.Seconds()
	}
	return r
}

// sampleLatency draws one op's synthetic latency, clamped into [1ms, 1h]
// BEFORE the float→Duration conversion. The clamp is a hard safety bound, not
// shaping: with an extreme-but-finite spread (say 1e308, which parses fine)
// the product overflows float64 or int64, and Go's float→int conversion on
// overflow is implementation-defined — a zero/negative duration would make
// time.After fire instantly and turn every agent into an unbounded
// sample-allocating spin. The floor also bounds the op rate (≤1000/s per
// agent) for legitimately tiny p50s; real git ops are never sub-millisecond.
func (d *demoRunner) sampleLatency(rng *rand.Rand, inflate float64) time.Duration {
	f := float64(d.p50) * inflate * math.Exp(d.sigma*rng.NormFloat64())
	return time.Duration(math.Min(math.Max(f, float64(time.Millisecond)), float64(time.Hour)))
}

// saturationInflate models a closed loop hitting an ops/sec ceiling. Each demo
// agent completes ops serially, so a level's throughput is offered/inflate with
// offered = c/p50; inflate therefore fully determines the throughput curve.
// (1 + u⁴)^¼ is a smooth max(1, u) of utilization u = offered/cap: well below
// cap it's ~1 (throughput ≈ offered, latency ≈ p50), around cap latency bends
// up (~19% at u=1), and past cap inflate ≈ u, so throughput plateaus at ~cap
// (a shade under: the log-normal mean sits above p50) while latency keeps
// growing linearly with overload — a real queue's closed-loop shape. The
// open-loop 1/(1-util) formula this replaces behaved unphysically in a closed
// loop: throughput FELL as offered load approached cap, and once util hit its
// 0.95 clamp it grew linearly again with no ceiling at all.
func (d *demoRunner) saturationInflate(c int) float64 {
	if d.cap <= 0 {
		return 1
	}
	u := float64(c) / d.p50.Seconds() / d.cap
	return math.Sqrt(math.Sqrt(1 + u*u*u*u))
}

func (d *demoRunner) RunLevel(ctx context.Context, c int, barrier *bench.StartBarrier) (bench.LevelResult, error) {
	inflate := d.saturationInflate(c)

	// Wait for the shared start so the demo target's window aligns with the
	// real targets it's compared against.
	if barrier != nil {
		barrier.Arrive()
		barrier.Hold()
	}

	// Origin both the deadline and the sample clock on the barrier's shared fire
	// instant, exactly like bench.Runner, so scheduler delay after release can't
	// shift or extend the demo's window past the real targets it races against
	// (which would give it extra ops and make live buckets disagree with the
	// final result).
	total := d.w.Warmup + d.w.Duration
	start := time.Now()
	if barrier != nil {
		start = barrier.FiredAt()
	}
	lctx, cancel := context.WithDeadline(ctx, start.Add(total))
	defer cancel()
	agg := &demoAgg{warmup: d.w.Warmup}
	var wg sync.WaitGroup
	for i := range c {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Deterministic per agent, like the engine's commit content rng.
			rng := rand.New(rand.NewSource(int64(id)*7919 + 42)) //nolint:gosec // synthetic demo data
			pushesLeft := 0                                      // session state: pushes remaining before the next clone
			for lctx.Err() == nil {
				lat := d.sampleLatency(rng, inflate)
				t0 := time.Now()
				select {
				case <-time.After(lat):
				case <-lctx.Done():
					return // drop the truncated op, like the engine does
				}
				s := bench.Sample{Offset: t0.Sub(start), Dur: time.Since(t0), Op: d.nextOp(pushesLeft)}
				switch roll := rng.Float64(); {
				case roll < d.errRate:
					s.Res = bench.OutcomeErr
					s.Msg = "demo: synthetic error"
				case roll < d.errRate+d.casRate && s.Op == bench.OpPush:
					s.Res = bench.OutcomeCAS // CAS is a ref-update outcome; clones can't CAS
				}
				// Session bookkeeping mirrors runSessions: a successful clone opens a
				// session of SessionCommits pushes; a failed clone retries (pushesLeft
				// stays 0); each push consumes an attempt whatever its outcome.
				if d.w.Strategy == "session" {
					if s.Op == bench.OpClone {
						if s.Res == bench.OutcomeOK {
							pushesLeft = d.w.SessionCommits
						}
					} else {
						pushesLeft--
					}
				}
				if d.sink != nil {
					d.sink.OnSample(s)
				}
				agg.add(s)
			}
		}(i)
	}
	wg.Wait()
	return agg.result(c, d.w.Strategy, d.w.Duration, d.w.CommitDesc()), nil
}
