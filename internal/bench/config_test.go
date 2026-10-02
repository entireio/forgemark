package bench

import (
	"strings"
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

// Commit-content fields arrive from the server API just as unbounded as
// concurrency does, and an unchecked FileSize flows into make([]byte, FileSize)
// in every agent — near MaxInt that's a runtime panic ("len out of range")
// that kills the whole server process, not just the run. FilesMax × FileSize
// is bounded too: it's the working set every agent keeps in its worktree.
func TestValidateBoundsCommitContent(t *testing.T) {
	check := func(c CommitConfig, wantOK bool) {
		t.Helper()
		w := Workload{Strategy: "branch", Concurrency: []int{1}, Duration: time.Second, Commit: c}
		if err := w.Validate(); (err == nil) != wantOK {
			t.Errorf("Validate(%+v) = %v, want ok=%v", c, err, wantOK)
		}
	}
	check(CommitConfig{FilesMin: 1, FilesMax: maxFilesMax, FileSize: 1}, true)
	check(CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: maxFileSize}, true)
	check(CommitConfig{FilesMin: 1, FilesMax: maxFilesMax + 1, FileSize: 1}, false)
	check(CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: maxFileSize + 1}, false)
	check(CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1<<63 - 1}, false)
	// Each factor within its own cap can still multiply past the per-commit
	// aggregate.
	check(CommitConfig{FilesMin: 1, FilesMax: maxFilesMax, FileSize: maxFileSize}, false)

	// clone never commits, so its commit config is inert and stays unvalidated
	// — a huge FileSize on a clone run must not start failing.
	w := Workload{Strategy: "clone", Concurrency: []int{1}, Duration: time.Second,
		Commit: CommitConfig{FileSize: 1 << 40}}
	if err := w.Validate(); err != nil {
		t.Errorf("Validate(clone, huge inert FileSize) = %v, want nil", err)
	}
}

func TestDestRef(t *testing.T) {
	// The prefix is prepended verbatim inside the namespace, before the run ID; the
	// assembled ref is what Workload.Validate checks. Readable prefixes pass; typo
	// shapes git rejects fail fast (before any push pollutes the measured window).
	tests := []struct {
		name      string
		ns        string
		prefix    string
		c, i      int
		want      string
		wantValid bool
	}{
		{name: "no prefix", ns: DefaultRefNamespace, prefix: "", c: 32, i: 5, want: "refs/forgemark/fmX-c32-a5", wantValid: true},
		{name: "slash prefix", ns: DefaultRefNamespace, prefix: "bench/", c: 32, i: 5, want: "refs/forgemark/bench/fmX-c32-a5", wantValid: true},
		{name: "dash separator", ns: DefaultRefNamespace, prefix: "bench-", c: 8, i: 0, want: "refs/forgemark/bench-fmX-c8-a0", wantValid: true},
		{name: "no separator", ns: DefaultRefNamespace, prefix: "bench", c: 1, i: 0, want: "refs/forgemark/benchfmX-c1-a0", wantValid: true}, // ugly-but-valid
		{name: "heads override", ns: "refs/heads/", prefix: "bench/", c: 32, i: 5, want: "refs/heads/bench/fmX-c32-a5", wantValid: true},
		{name: "space", ns: DefaultRefNamespace, prefix: "my bench/", c: 1, i: 0, want: "refs/forgemark/my bench/fmX-c1-a0", wantValid: false},
		{name: "tilde", ns: DefaultRefNamespace, prefix: "bench~1", c: 1, i: 0, want: "refs/forgemark/bench~1fmX-c1-a0", wantValid: false},
		{name: "leading slash", ns: DefaultRefNamespace, prefix: "/bench", c: 1, i: 0, want: "refs/forgemark//benchfmX-c1-a0", wantValid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DestRef(tt.ns, tt.prefix, "fmX", tt.c, tt.i)
			if got != tt.want {
				t.Errorf("DestRef(%q, %q, ...) = %q, want %q", tt.ns, tt.prefix, got, tt.want)
			}
			if valid := plumbing.ReferenceName(got).Validate() == nil; valid != tt.wantValid {
				t.Errorf("Validate(%q) valid = %v, want %v", got, valid, tt.wantValid)
			}
		})
	}
}

// The namespace defaults to refs/forgemark/ — deliberately off refs/heads/, so
// bench pushes never trigger a forge's post-receive pipeline — and is
// normalized to exactly one trailing slash so DestRef can concatenate. The
// override must be a real refs/<name>/ namespace: a bare "refs/" or anything
// outside refs/ would never be advertised back, so the sweep could never find
// what it pushed.
func TestNamespace(t *testing.T) {
	for in, want := range map[string]string{
		"":                 DefaultRefNamespace,
		"refs/forgemark":   "refs/forgemark/",
		"refs/forgemark//": "refs/forgemark/",
		"refs/heads/":      "refs/heads/",
		"refs/bench/x":     "refs/bench/x/",
	} {
		if got := (Workload{RefNamespace: in}).Namespace(); got != want {
			t.Errorf("Namespace(%q) = %q, want %q", in, got, want)
		}
	}
	base := Workload{Strategy: "branch", Concurrency: []int{1}, Duration: time.Second,
		Commit: CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1}}
	for _, ns := range []string{"", "refs/forgemark/", "refs/heads/", "refs/bench"} {
		w := base
		w.RefNamespace = ns
		if err := w.Validate(); err != nil {
			t.Errorf("Validate(namespace=%q) = %v, want nil", ns, err)
		}
	}
	for _, ns := range []string{"refs/", "heads/", "forgemark", "refs/my space/", "refs/a~b/"} {
		w := base
		w.RefNamespace = ns
		err := w.Validate()
		if err == nil {
			t.Errorf("Validate(namespace=%q) = nil, want error", ns)
		} else if !strings.Contains(err.Error(), "-ref-namespace") {
			t.Errorf("Validate(namespace=%q) blamed the wrong flag: %v", ns, err)
		}
	}
	// A bad prefix under a good namespace is still reported as the prefix's fault.
	w := base
	w.BranchPrefix = "bad~prefix"
	if err := w.Validate(); err == nil || !strings.Contains(err.Error(), "-branch-prefix") {
		t.Errorf("Validate(bad prefix) = %v, want -branch-prefix error", err)
	}
}
