package server

import (
	"math"
	"sync"
	"time"

	"github.com/entireio/forgemark/internal/bench"
)

// windowSecs is the rolling-percentile window: latency percentiles in a bucket
// event are computed over the OK samples of the last windowSecs seconds, so a
// single quiet second doesn't blank the lines while spikes still show quickly.
const windowSecs = 10

// The live path must stay bounded no matter what the workload does: the
// supported envelope (4096 agents, 1ms demo latency floor) reaches millions
// of samples per second, where retaining exact latencies and sorting the
// full 10s window every snapshot would burn hundreds of MB and whole cores —
// the observer throttling the load it measures. Latencies therefore go into
// fixed-size log-spaced histograms: O(1) insert with no allocation, ~1KB per
// second per op kind, and percentile extraction that walks 256 counters.
// Bucket resolution is ~6.7% relative, plenty for live charts; the
// authoritative LevelResult still computes exact percentiles from the
// engine's own samples.
const (
	latHistBuckets = 256
	latHistMinMs   = 0.05    // < 50µs collapses into bucket 0
	latHistMaxMs   = 600_000 // > 10min collapses into the last bucket
)

var latHistLnGrowth = math.Log(latHistMaxMs/latHistMinMs) / float64(latHistBuckets-2)

type latHist [latHistBuckets]uint32

// latHistIdx maps a latency to its bucket. Kept separate from the increment
// so OnSample can compute it (a math.Log) OUTSIDE the collector mutex,
// leaving only plain increments inside the critical section.
func latHistIdx(ms float64) int {
	if ms <= latHistMinMs {
		return 0
	}
	idx := 1 + int(math.Log(ms/latHistMinMs)/latHistLnGrowth)
	if idx > latHistBuckets-1 {
		return latHistBuckets - 1
	}
	return idx
}

func (h *latHist) add(ms float64) { h[latHistIdx(ms)]++ }

// latHistValue is the representative latency of bucket idx: the geometric
// midpoint of its bounds (the edge buckets return their clamp values).
func latHistValue(idx int) float64 {
	switch {
	case idx <= 0:
		return latHistMinMs
	case idx >= latHistBuckets-1:
		return latHistMaxMs
	default:
		return latHistMinMs * math.Exp((float64(idx)-0.5)*latHistLnGrowth)
	}
}

// histPercentiles computes nearest-rank p50/p95/p99 over the summed
// histograms. Returns zeros when the window holds no samples, matching the
// old exact-sample behavior.
func histPercentiles(hists ...*latHist) (p50, p95, p99 float64) {
	var merged [latHistBuckets]uint64
	total := uint64(0)
	for _, h := range hists {
		for i, n := range h {
			merged[i] += uint64(n)
			total += uint64(n)
		}
	}
	if total == 0 {
		return 0, 0, 0
	}
	at := func(p float64) float64 {
		rank := uint64(math.Ceil(p / 100 * float64(total)))
		if rank < 1 {
			rank = 1
		}
		cum := uint64(0)
		for i, n := range merged {
			cum += n
			if cum >= rank {
				return latHistValue(i)
			}
		}
		return latHistValue(latHistBuckets - 1)
	}
	return at(50), at(95), at(99)
}

// collector is the live aggregator for one target of one run. It implements
// bench.Sink: agents call OnSample concurrently mid-measurement, so it does
// only counter and histogram increments under its mutex — no allocation, no
// unbounded growth. Once per second the run's ticker calls snapshot, which
// rotates the current second into the ring and computes rolling percentiles
// from the windowed histograms.
type collector struct {
	mu        sync.Mutex
	warmup    time.Duration // samples before this offset are excluded (matches the level result)
	cur       bucket
	ring      [windowSecs]latHist // one closed second of OK push latencies per slot
	cloneRing [windowSecs]latHist
	pos       int
}

// bucket accumulates one second of samples.
type bucket struct {
	ok, cas, errs     int
	cloneOK, cloneErr int
	lat               latHist // OK push latencies (ms)
	cloneLat          latHist // OK clone latencies (ms)
}

// BucketStats is one target's slice of a per-second bucket event. Push and
// clone ops are reported separately, mirroring LevelResult; the UI picks the
// primary series from the workload strategy. Attempt totals are derivable
// (ok+cas+err, clone_ok+clone_err), so they aren't carried on the wire.
type BucketStats struct {
	OK  int `json:"ok"`
	CAS int `json:"cas"`
	Err int `json:"err"`
	// Rolling percentiles over the last windowSecs seconds of OK pushes.
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`

	CloneOK  int     `json:"clone_ok,omitempty"`
	CloneErr int     `json:"clone_err,omitempty"`
	CloneP50 float64 `json:"clone_p50_ms,omitempty"`
	CloneP95 float64 `json:"clone_p95_ms,omitempty"`
	CloneP99 float64 `json:"clone_p99_ms,omitempty"`
}

func (c *collector) OnSample(s bench.Sample) {
	// The bucket index (a math.Log) is computed before taking the lock, so the
	// critical section is nothing but integer increments — the sink must not
	// serialize the agents whose throughput it is observing.
	idx := latHistIdx(float64(s.Dur) / float64(time.Millisecond))
	c.mu.Lock()
	defer c.mu.Unlock()
	// The Sink receives warm-up samples too; drop them so the live counts,
	// rolling percentiles, and persisted series match the authoritative level
	// result, which excludes the warm-up window.
	if s.Offset < c.warmup {
		return
	}
	if s.Op == bench.OpClone {
		switch s.Res {
		case bench.OutcomeOK:
			c.cur.cloneOK++
			c.cur.cloneLat[idx]++
		default:
			c.cur.cloneErr++
		}
		return
	}
	switch s.Res {
	case bench.OutcomeOK:
		c.cur.ok++
		c.cur.lat[idx]++
	case bench.OutcomeCAS:
		c.cur.cas++
	default:
		c.cur.errs++
	}
}

// snapshot closes out the current second: it returns its counters and the
// rolling percentiles over the updated window.
func (c *collector) snapshot() BucketStats {
	c.mu.Lock()
	b := c.cur
	c.cur = bucket{}
	c.ring[c.pos] = b.lat
	c.cloneRing[c.pos] = b.cloneLat
	c.pos = (c.pos + 1) % windowSecs
	// Copy the window (a few KB of fixed arrays) and merge after unlocking, so
	// the percentile walk never blocks concurrent OnSample calls.
	ring, cloneRing := c.ring, c.cloneRing
	c.mu.Unlock()

	st := BucketStats{
		OK: b.ok, CAS: b.cas, Err: b.errs,
		CloneOK: b.cloneOK, CloneErr: b.cloneErr,
	}
	hists := make([]*latHist, windowSecs)
	for i := range ring {
		hists[i] = &ring[i]
	}
	st.P50, st.P95, st.P99 = histPercentiles(hists...)
	for i := range cloneRing {
		hists[i] = &cloneRing[i]
	}
	st.CloneP50, st.CloneP95, st.CloneP99 = histPercentiles(hists...)
	return st
}

// reset clears the bucket and the rolling window and sets the warm-up boundary
// for the level about to start, so percentiles never bleed across concurrency
// levels and warm-up samples are excluded.
func (c *collector) reset(warmup time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.warmup = warmup
	c.cur = bucket{}
	c.ring = [windowSecs]latHist{}
	c.cloneRing = [windowSecs]latHist{}
	c.pos = 0
}
