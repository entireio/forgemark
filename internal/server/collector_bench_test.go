package server

import (
	"testing"
	"time"

	"github.com/entireio/forgemark/internal/bench"
)

// BenchmarkCollectorOnSample measures the per-call cost of the collector sink
// under contention. Run with -cpu=1,8,32,128 to see how much the single mutex
// serializes concurrent agents. Compare ns/op to a real git operation
// (tens-to-hundreds of ms): that ratio is the perturbation the sink adds to the
// measured throughput. A snapshot every ~1000 samples mimics the once-per-second
// rotation so the latency slice doesn't grow unbounded (unrealistic reallocation).
func BenchmarkCollectorOnSample(b *testing.B) {
	c := &collector{}
	c.reset(0)
	s := bench.Sample{Op: bench.OpPush, Res: bench.OutcomeOK, Dur: 30 * time.Millisecond, Offset: time.Second}
	var n int64
	b.RunParallel(func(pb *testing.PB) {
		local := 0
		for pb.Next() {
			c.OnSample(s)
			if local++; local%1000 == 0 {
				c.snapshot() // rotate like the per-second ticker does
			}
		}
	})
	_ = n
}
