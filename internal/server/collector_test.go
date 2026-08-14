package server

import (
	"testing"
	"time"

	"github.com/entireio/forgemark/internal/bench"
)

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

// The collector's percentiles come from log-spaced histograms (~6.7% bucket
// width), so latency assertions allow that resolution rather than exact
// equality. Counters stay exact.
func near(t *testing.T, label string, got, want float64) {
	t.Helper()
	if want == 0 {
		if got != 0 {
			t.Fatalf("%s = %v, want exactly 0", label, got)
		}
		return
	}
	if got < want*0.92 || got > want*1.08 {
		t.Fatalf("%s = %v, want %v ±8%% (one histogram bucket)", label, got, want)
	}
}

func TestCollectorBucketsAndRollingPercentiles(t *testing.T) {
	c := &collector{}

	// Second 1: 4 OK pushes (10/20/30/40ms), 1 CAS, 1 err, plus a clone.
	for _, d := range []float64{10, 20, 30, 40} {
		c.OnSample(bench.Sample{Dur: ms(d), Res: bench.OutcomeOK})
	}
	c.OnSample(bench.Sample{Res: bench.OutcomeCAS})
	c.OnSample(bench.Sample{Dur: ms(5), Res: bench.OutcomeErr, Msg: "boom"})
	c.OnSample(bench.Sample{Op: bench.OpClone, Dur: ms(100), Res: bench.OutcomeOK})

	st := c.snapshot()
	if st.OK != 4 || st.CAS != 1 || st.Err != 1 {
		t.Fatalf("bucket counters = %+v, want ok=4 cas=1 err=1", st)
	}
	if st.CloneOK != 1 || st.CloneErr != 0 {
		t.Fatalf("clone counters = %+v, want clone_ok=1 clone_err=0", st)
	}
	near(t, "p50", st.P50, 20) // nearest-rank over 10,20,30,40
	near(t, "p99", st.P99, 40)
	near(t, "clone p50", st.CloneP50, 100)

	// Second 2: no new samples — rolling window still holds second 1's latencies.
	st = c.snapshot()
	if st.OK != 0 && st.CAS != 0 && st.Err != 0 {
		t.Fatalf("second bucket counters = %+v, want empty", st)
	}
	near(t, "rolling p50 after empty second", st.P50, 20)

	// After windowSecs empty seconds the window drains entirely.
	for range windowSecs {
		st = c.snapshot()
	}
	near(t, "p50 after window drained", st.P50, 0)
}

func TestCollectorResetClearsWindow(t *testing.T) {
	c := &collector{}
	c.OnSample(bench.Sample{Dur: ms(50), Res: bench.OutcomeOK})
	c.snapshot()
	c.reset(0)
	st := c.snapshot()
	if st.OK != 0 || st.P50 != 0 {
		t.Fatalf("after reset snapshot = %+v, want empty", st)
	}
}

// The histogram must clamp, not index out of range, at both extremes, and
// stay within its documented resolution across the whole span.
func TestLatHistBoundsAndResolution(t *testing.T) {
	var h latHist
	h.add(0)    // below the floor
	h.add(1e12) // absurdly above the ceiling
	if h[0] != 1 || h[latHistBuckets-1] != 1 {
		t.Fatalf("edge samples landed in wrong buckets: h[0]=%d h[last]=%d", h[0], h[latHistBuckets-1])
	}
	for _, want := range []float64{0.1, 1, 15, 250, 4000, 90_000} {
		var g latHist
		g.add(want)
		p50, _, _ := histPercentiles(&g)
		if p50 < want*0.92 || p50 > want*1.08 {
			t.Errorf("round-trip of %vms = %vms, want within one bucket (±8%%)", want, p50)
		}
	}
}

// demoAgg accumulates a WHOLE LEVEL into one histogram, and the envelope's
// worst case (4096 agents at the 1ms floor with spread=1, 6h level) puts
// ~8.8e10 increments in a single bucket. The counters must hold level-scale
// counts without wrapping — as uint32 they wrapped after ~18 minutes and the
// published percentiles were garbage for the rest of the level.
func TestLatHistHoldsLevelScaleCounts(t *testing.T) {
	var h latHist
	h[10] = 100_000_000_000 // > MaxUint32: a uint32 counter would have wrapped
	if got := h.quantile(50); got != latHistValue(10) {
		t.Fatalf("p50 over a level-scale bucket = %v, want %v", got, latHistValue(10))
	}
	if got := h.maxValue(); got != latHistValue(10) {
		t.Fatalf("max over a level-scale bucket = %v, want %v", got, latHistValue(10))
	}
}
