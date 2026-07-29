package server

import (
	"testing"
	"time"

	"github.com/entireio/forgemark/internal/bench"
)

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

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
	if st.P50 != 20 || st.P99 != 40 {
		t.Fatalf("percentiles p50=%v p99=%v, want 20/40 (nearest-rank over 10,20,30,40)", st.P50, st.P99)
	}

	// Second 2: no new samples — rolling window still holds second 1's latencies.
	st = c.snapshot()
	if st.OK != 0 && st.CAS != 0 && st.Err != 0 {
		t.Fatalf("second bucket counters = %+v, want empty", st)
	}
	if st.P50 != 20 {
		t.Fatalf("rolling p50 after empty second = %v, want 20 (window retains prior seconds)", st.P50)
	}

	// After windowSecs empty seconds the window drains entirely.
	for range windowSecs {
		st = c.snapshot()
	}
	if st.P50 != 0 {
		t.Fatalf("p50 after window drained = %v, want 0", st.P50)
	}
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
