package bench

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
)

// Concurrency is bounded on both sides: a non-positive level is meaningless,
// and an unbounded one (the API accepts arbitrary ints) would reach
// make([]*agent, c) and c goroutine spawns — a remotely triggerable
// panic/OOM on an exposed bind with a demo:// target.
func TestValidateBoundsConcurrency(t *testing.T) {
	w := Workload{
		Strategy: "branch", Duration: time.Second,
		Commit: CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1},
	}
	for _, levels := range [][]int{{1}, {maxConcurrency}, {1, 4, 128}} {
		w.Concurrency = levels
		if err := w.Validate(); err != nil {
			t.Errorf("Validate(concurrency=%v) = %v, want nil", levels, err)
		}
	}
	for _, levels := range [][]int{{0}, {-1}, {maxConcurrency + 1}, {1<<63 - 1}, {1, 1 << 40}} {
		w.Concurrency = levels
		if err := w.Validate(); err == nil {
			t.Errorf("Validate(concurrency=%v) = nil, want error", levels)
		}
	}
	// Sweep length and window durations are bounded too: many levels × long
	// windows is the other axis of the same unauthenticated-work problem.
	w.Concurrency = make([]int, maxLevels+1)
	for i := range w.Concurrency {
		w.Concurrency[i] = 1
	}
	if err := w.Validate(); err == nil {
		t.Errorf("Validate(%d levels) = nil, want error", maxLevels+1)
	}
	w.Concurrency = []int{1}
	w.Duration = maxDuration + time.Second
	if err := w.Validate(); err == nil {
		t.Error("Validate(over-long duration) = nil, want error")
	}
	w.Duration = time.Second
	w.Warmup = maxWarmup + time.Second
	if err := w.Validate(); err == nil {
		t.Error("Validate(over-long warmup) = nil, want error")
	}
}

func TestDestRef(t *testing.T) {
	// The prefix is prepended verbatim before the run ID; the assembled ref is what
	// Workload.Validate checks. Readable prefixes pass; typo shapes git rejects fail fast
	// (before any push pollutes the measured window).
	tests := []struct {
		name      string
		prefix    string
		c, i      int
		want      string
		wantValid bool
	}{
		{name: "no prefix", prefix: "", c: 32, i: 5, want: "refs/heads/fmX-c32-a5", wantValid: true},
		{name: "slash namespace", prefix: "bench/", c: 32, i: 5, want: "refs/heads/bench/fmX-c32-a5", wantValid: true},
		{name: "dash separator", prefix: "bench-", c: 8, i: 0, want: "refs/heads/bench-fmX-c8-a0", wantValid: true},
		{name: "no separator", prefix: "bench", c: 1, i: 0, want: "refs/heads/benchfmX-c1-a0", wantValid: true}, // ugly-but-valid
		{name: "space", prefix: "my bench/", c: 1, i: 0, want: "refs/heads/my bench/fmX-c1-a0", wantValid: false},
		{name: "tilde", prefix: "bench~1", c: 1, i: 0, want: "refs/heads/bench~1fmX-c1-a0", wantValid: false},
		{name: "leading slash", prefix: "/bench", c: 1, i: 0, want: "refs/heads//benchfmX-c1-a0", wantValid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DestRef(tt.prefix, "fmX", tt.c, tt.i)
			if got != tt.want {
				t.Errorf("DestRef(%q, ...) = %q, want %q", tt.prefix, got, tt.want)
			}
			if valid := plumbing.ReferenceName(got).Validate() == nil; valid != tt.wantValid {
				t.Errorf("Validate(%q) valid = %v, want %v", got, valid, tt.wantValid)
			}
		})
	}
}
