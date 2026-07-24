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
			if err != nil || f < 0 {
				return nil, fmt.Errorf("demo: invalid %s %q", name, v)
			}
			*dst = f
		}
	}
	if spread < 1 {
		return nil, fmt.Errorf("demo: spread must be >= 1")
	}
	// For a log-normal around the median, p99 = median * exp(sigma * z99).
	d.sigma = math.Log(spread) / 2.326
	return d, nil
}

func (d *demoRunner) Label() string        { return d.label }
func (d *demoRunner) Nodes() int           { return 1 }
func (d *demoRunner) ObjectFormat() string { return "sha1" }

// opFor mirrors the real strategies' op mix so demo buckets exercise the same
// consumer paths: clone strategy emits only clones, and session emits one
// clone followed by SessionCommits pushes per simulated session (n is the
// agent's op counter).
func (d *demoRunner) opFor(n int) bench.OpKind {
	switch d.w.Strategy {
	case "clone":
		return bench.OpClone
	case "session":
		if d.w.SessionCommits > 0 && n%(d.w.SessionCommits+1) == 0 {
			return bench.OpClone
		}
	}
	return bench.OpPush
}

func (d *demoRunner) RunLevel(ctx context.Context, c int) (bench.LevelResult, error) {
	total := d.w.Warmup + d.w.Duration
	lctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	// Saturation: offered load approaches cap → queueing inflates latency.
	inflate := 1.0
	if d.cap > 0 {
		offered := float64(c) / d.p50.Seconds()
		util := math.Min(offered/d.cap, 0.95)
		inflate = 1 / (1 - util)
	}

	start := time.Now()
	var mu sync.Mutex
	var all []bench.Sample
	var wg sync.WaitGroup
	for i := range c {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Deterministic per agent, like the engine's commit content rng.
			rng := rand.New(rand.NewSource(int64(id)*7919 + 42)) //nolint:gosec // synthetic demo data
			for n := 0; lctx.Err() == nil; n++ {
				lat := time.Duration(float64(d.p50) * inflate * math.Exp(d.sigma*rng.NormFloat64()))
				t0 := time.Now()
				select {
				case <-time.After(lat):
				case <-lctx.Done():
					return // drop the truncated op, like the engine does
				}
				s := bench.Sample{Offset: t0.Sub(start), Dur: time.Since(t0), Op: d.opFor(n)}
				switch roll := rng.Float64(); {
				case roll < d.errRate:
					s.Res = bench.OutcomeErr
					s.Msg = "demo: synthetic error"
				case roll < d.errRate+d.casRate && s.Op == bench.OpPush:
					s.Res = bench.OutcomeCAS // CAS is a ref-update outcome; clones can't CAS
				}
				if d.sink != nil {
					d.sink.OnSample(s)
				}
				mu.Lock()
				all = append(all, s)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	return bench.SummarizeSamples(all, c, d.w.Strategy, 1, 1, d.w.Warmup, d.w.Duration, d.w.CommitDesc()), nil
}
