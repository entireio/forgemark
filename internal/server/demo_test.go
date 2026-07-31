package server

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/entireio/forgemark/internal/bench"
)

func TestNewDemoRunnerValidation(t *testing.T) {
	w := bench.Workload{Strategy: "branch", Concurrency: []int{1}}
	ok := func(remote string) {
		if _, err := newDemoRunner(remote, w, nil); err != nil {
			t.Errorf("newDemoRunner(%q) = %v, want nil", remote, err)
		}
	}
	bad := func(remote string) {
		if _, err := newDemoRunner(remote, w, nil); err == nil {
			t.Errorf("newDemoRunner(%q) = nil, want error", remote)
		}
	}
	ok("demo://fast?p50=60ms&spread=3&err=0.01&cas=0.02&cap=400")
	// Non-finite floats pass strconv.ParseFloat and a bare `< 0` check; they must
	// be rejected before they poison sigma/durations into a tight loop.
	bad("demo://x?spread=NaN")
	bad("demo://x?spread=Inf")
	bad("demo://x?cap=+Inf")
	bad("demo://x?err=nan")
	// Probabilities are bounded to [0,1].
	bad("demo://x?err=2")
	bad("demo://x?cas=1.5")
	// err and cas share one probability roll, so their sum is bounded too:
	// err=0.8&cas=0.8 would silently yield only ~0.2 CAS, not the requested 0.8.
	bad("demo://x?err=0.8&cas=0.8")
	ok("demo://x?err=0.4&cas=0.6")
}

type captureSink struct {
	mu      sync.Mutex
	samples []bench.Sample
}

func (c *captureSink) OnSample(s bench.Sample) {
	c.mu.Lock()
	c.samples = append(c.samples, s)
	c.mu.Unlock()
}

// A real session agent retries the clone immediately after a clone failure —
// pushes only ever happen inside a successfully cloned session. The demo's
// synthetic op mix must obey the same state machine, or error-heavy demo runs
// publish an impossible clone/push blend.
func TestDemoSessionPushesOnlyAfterSuccessfulClone(t *testing.T) {
	w := bench.Workload{
		Strategy: "session", SessionCommits: 3, Concurrency: []int{1},
		Duration: 100 * time.Millisecond,
	}

	// Every clone fails: the agent must loop on clone retries, never pushing.
	sink := &captureSink{}
	d, err := newDemoRunner("demo://x?p50=1ms&spread=1&err=1", w, sink)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RunLevel(context.Background(), 1, nil); err != nil {
		t.Fatal(err)
	}
	if len(sink.samples) == 0 {
		t.Fatal("no samples generated")
	}
	for _, s := range sink.samples {
		if s.Op == bench.OpPush {
			t.Fatal("push emitted while every clone fails; a session needs a successful clone first")
		}
	}

	// Healthy path, single agent: samples must follow clone, push×3, clone, …
	sink = &captureSink{}
	d, err = newDemoRunner("demo://x?p50=1ms&spread=1&err=0", w, sink)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RunLevel(context.Background(), 1, nil); err != nil {
		t.Fatal(err)
	}
	left := 0
	for i, s := range sink.samples {
		if s.Op == bench.OpPush {
			if left == 0 {
				t.Fatalf("sample %d is a push outside an open session", i)
			}
			left--
		} else {
			left = w.SessionCommits // err=0: every clone succeeds and opens a session
		}
	}
	if len(sink.samples) == 0 {
		t.Fatal("no samples generated")
	}
}

// Extreme-but-finite parameters (spread=1e308 parses fine) overflow the
// latency product past float64/int64; an unclamped float→Duration conversion
// is implementation-defined and can go non-positive, turning time.After into
// an instant fire and the agent loop into an unbounded allocation spin. The
// sample must stay a positive Duration within [1ms, 1h] for any draw.
func TestDemoSampleLatencyClampedUnderExtremeSpread(t *testing.T) {
	w := bench.Workload{Strategy: "branch", Concurrency: []int{1}}
	for _, remote := range []string{
		"demo://x?spread=1e308",            // sigma ≈ 305: exp overflows to +Inf on the high tail, ~0 on the low
		"demo://x?p50=1ns",                 // legitimate-but-tiny p50 must not become a busy spin
		"demo://x?p50=100000h&spread=1000", // huge p50: product can exceed int64 nanoseconds
	} {
		d, err := newDemoRunner(remote, w, nil)
		if err != nil {
			t.Fatalf("newDemoRunner(%q): %v", remote, err)
		}
		rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test draws
		for i := 0; i < 10000; i++ {
			if lat := d.sampleLatency(rng, 1); lat < time.Millisecond || lat > time.Hour {
				t.Fatalf("%s: sampleLatency draw %d = %v, want within [1ms, 1h]", remote, i, lat)
			}
		}
	}
}
