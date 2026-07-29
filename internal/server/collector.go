package server

import (
	"sort"
	"sync"
	"time"

	"github.com/entireio/forgemark/internal/bench"
)

// windowSecs is the rolling-percentile window: latency percentiles in a bucket
// event are computed over the OK samples of the last windowSecs seconds, so a
// single quiet second doesn't blank the lines while spikes still show quickly.
const windowSecs = 10

// collector is the live aggregator for one target of one run. It implements
// bench.Sink: agents call OnSample concurrently mid-measurement, so it does
// only counter increments and one append under its mutex. Once per second the
// run's ticker calls snapshot, which swaps the current bucket out and computes
// rolling percentiles (an exact sort over the window — a few thousand floats,
// ~100µs, nothing at benchmark sample rates).
type collector struct {
	mu        sync.Mutex
	warmup    time.Duration // samples before this offset are excluded (matches the level result)
	cur       bucket
	ring      [windowSecs][]float64 // push OK latencies, one slot per elapsed second
	cloneRing [windowSecs][]float64
	pos       int
}

// bucket accumulates one second of samples.
type bucket struct {
	ok, cas, errs     int
	cloneOK, cloneErr int
	lat               []float64 // OK push latencies (ms)
	cloneLat          []float64 // OK clone latencies (ms)
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
	ms := float64(s.Dur) / float64(time.Millisecond)
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
			c.cur.cloneLat = append(c.cur.cloneLat, ms)
		default:
			c.cur.cloneErr++
		}
		return
	}
	switch s.Res {
	case bench.OutcomeOK:
		c.cur.ok++
		c.cur.lat = append(c.cur.lat, ms)
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
	// Copy the ring's slice headers (cheap) and concatenate after unlocking:
	// each window slot is a completed second's slice, never appended to in
	// place, so holding the mutex across the O(window) concat would needlessly
	// block concurrent OnSample calls and depress measured throughput.
	ring, cloneRing := c.ring, c.cloneRing
	c.mu.Unlock()

	var lat, cloneLat []float64
	for i := range ring {
		lat = append(lat, ring[i]...)
		cloneLat = append(cloneLat, cloneRing[i]...)
	}

	st := BucketStats{
		OK: b.ok, CAS: b.cas, Err: b.errs,
		CloneOK: b.cloneOK, CloneErr: b.cloneErr,
	}
	if len(lat) > 0 {
		sort.Float64s(lat)
		st.P50 = bench.Percentile(lat, 50)
		st.P95 = bench.Percentile(lat, 95)
		st.P99 = bench.Percentile(lat, 99)
	}
	if len(cloneLat) > 0 {
		sort.Float64s(cloneLat)
		st.CloneP50 = bench.Percentile(cloneLat, 50)
		st.CloneP95 = bench.Percentile(cloneLat, 95)
		st.CloneP99 = bench.Percentile(cloneLat, 99)
	}
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
	c.ring = [windowSecs][]float64{}
	c.cloneRing = [windowSecs][]float64{}
	c.pos = 0
}
