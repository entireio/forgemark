package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
)

func TestPercentileNearestRank(t *testing.T) {
	xs := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	cases := []struct {
		p    float64
		want float64
	}{
		{50, 50}, // ceil(0.5*10)=5 -> idx 4 -> 50
		{95, 100},
		{99, 100},
		{100, 100},
		{10, 10},
	}
	for _, c := range cases {
		if got := percentile(xs, c.p); got != c.want {
			t.Errorf("percentile(%v) = %v, want %v", c.p, got, c.want)
		}
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}
}

func TestSummarizeDropsWarmupAndCountsOutcomes(t *testing.T) {
	warmup := 10 * time.Second
	window := 10 * time.Second
	samples := []sample{
		{offset: 5 * time.Second, dur: 1 * time.Second, res: outcomeOK}, // warm-up: dropped
		{offset: 11 * time.Second, dur: 100 * time.Millisecond, res: outcomeOK},
		{offset: 12 * time.Second, dur: 300 * time.Millisecond, res: outcomeOK},
		{offset: 13 * time.Second, res: outcomeCAS},
		{offset: 14 * time.Second, res: outcomeErr},
	}
	r := summarize(samples, 8, "branch", 1, 3, warmup, window, "1-10 x 2048B")

	if r.Pushes != 4 {
		t.Errorf("Pushes = %d, want 4 (warm-up sample excluded)", r.Pushes)
	}
	if r.OK != 2 || r.CASFailures != 1 || r.OtherErrors != 1 {
		t.Errorf("outcomes ok=%d cas=%d err=%d, want 2/1/1", r.OK, r.CASFailures, r.OtherErrors)
	}
	if want := float64(2) / window.Seconds(); r.OpsPerSec != want {
		t.Errorf("OpsPerSec = %v, want %v", r.OpsPerSec, want)
	}
	// Percentiles computed over OK durations only (100ms, 300ms).
	if r.P50ms != 100 || r.Maxms != 300 {
		t.Errorf("p50=%v max=%v, want 100/300", r.P50ms, r.Maxms)
	}
	if r.Concurrency != 8 || r.Nodes != 3 {
		t.Errorf("metadata not carried through: %+v", r)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want outcome
	}{
		{"nil", nil, outcomeOK},
		{"up-to-date", git.NoErrAlreadyUpToDate, outcomeOK},
		{"non-ff sentinel", git.ErrNonFastForwardUpdate, outcomeCAS},
		{"ref changed text", errors.New("rpc: reference has changed"), outcomeCAS},
		{"fetch first text", errors.New("failed to push: fetch first"), outcomeCAS},
		{"transport", errors.New("dial tcp: connection refused"), outcomeErr},
	}
	for _, c := range cases {
		if got := classify(c.err); got != c.want {
			t.Errorf("%s: classify = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNormalizeErr(t *testing.T) {
	// The same server failure against different nodes/repos must collapse to one
	// key: the embedded receive-pack URL varies, the status code (the signal) does not.
	a := normalizeErr(`push: unexpected requesting "https://node3.cluster/repo-7/git-receive-pack" status code: 503`)
	b := normalizeErr(`push: unexpected requesting "https://node1.cluster/repo-42/git-receive-pack" status code: 503`)
	if a != b {
		t.Errorf("URL variance not collapsed:\n a=%q\n b=%q", a, b)
	}
	if !strings.Contains(a, "<url>") || !strings.Contains(a, "status code: 503") {
		t.Errorf("normalized dropped the signal: %q", a)
	}

	// Per-agent refs collapse too.
	r1 := normalizeErr("command error on refs/heads/run-a4: rejected")
	r2 := normalizeErr("command error on refs/heads/run-a17: rejected")
	if r1 != r2 || !strings.Contains(r1, "<ref>") {
		t.Errorf("ref variance not collapsed: %q vs %q", r1, r2)
	}

	// Truncation runs AFTER normalization: a long URL must not push the status
	// code past the cutoff.
	longURL := "https://node." + strings.Repeat("x", 400) + "/repo/git-receive-pack"
	got := normalizeErr(fmt.Sprintf(`push: unexpected requesting "%s" status code: 500`, longURL))
	if !strings.Contains(got, "status code: 500") {
		t.Errorf("status code cut off by truncation: %q", got)
	}
	if len(got) > maxErrMsgLen {
		t.Errorf("normalized message not truncated: len=%d", len(got))
	}

	if got := normalizeErr(""); got != "(no message)" {
		t.Errorf("empty message = %q, want (no message)", got)
	}

	// A message with no volatile parts is truncated but otherwise untouched.
	long := strings.Repeat("z", maxErrMsgLen+50)
	if got := normalizeErr(long); len(got) != maxErrMsgLen {
		t.Errorf("plain message not truncated to %d: got len=%d", maxErrMsgLen, len(got))
	}
}

func TestSummarizeAggregatesErrorMessages(t *testing.T) {
	window := 10 * time.Second
	samples := []sample{
		// Two push errors that normalize to the same 503 group (different URLs).
		{offset: 1 * time.Second, res: outcomeErr, msg: `push: unexpected requesting "https://n1/r1/git-receive-pack" status code: 503`},
		{offset: 2 * time.Second, res: outcomeErr, msg: `push: unexpected requesting "https://n2/r9/git-receive-pack" status code: 503`},
		// One local harness-bug error, distinct message (no volatile parts).
		{offset: 3 * time.Second, res: outcomeErr, msg: "commit: worktree: disk full"},
		// A successful push (no message) and a CAS (no message) must not appear.
		{offset: 4 * time.Second, dur: 10 * time.Millisecond, res: outcomeOK},
		{offset: 5 * time.Second, res: outcomeCAS},
		// A clone error goes to the clone bucket only.
		{offset: 6 * time.Second, res: outcomeErr, op: opClone, msg: `clone: unexpected requesting "https://n1/r1/git-upload-pack" status code: 500`},
	}
	r := summarize(samples, 8, "session", 1, 3, 0, window, "1-10 x 2048B")

	if len(r.ErrorMessages) != 2 {
		t.Fatalf("ErrorMessages = %+v, want 2 groups", r.ErrorMessages)
	}
	// Count-desc order: the 503 group (2) before the commit failure (1).
	if r.ErrorMessages[0].Count != 2 ||
		!strings.Contains(r.ErrorMessages[0].Message, "status code: 503") ||
		!strings.Contains(r.ErrorMessages[0].Message, "<url>") {
		t.Errorf("top push error = %+v, want count 2 / normalized 503", r.ErrorMessages[0])
	}
	if r.ErrorMessages[1].Count != 1 || r.ErrorMessages[1].Message != "commit: worktree: disk full" {
		t.Errorf("second push error = %+v, want the commit failure verbatim", r.ErrorMessages[1])
	}

	if len(r.CloneErrorMessages) != 1 || r.CloneErrorMessages[0].Count != 1 ||
		!strings.Contains(r.CloneErrorMessages[0].Message, "status code: 500") {
		t.Errorf("CloneErrorMessages = %+v, want one 500 group", r.CloneErrorMessages)
	}

	// A clean run must emit no lists (so omitempty fires in the JSON).
	clean := summarize([]sample{{offset: time.Second, dur: time.Millisecond, res: outcomeOK}}, 1, "branch", 1, 1, 0, window, "")
	if clean.ErrorMessages != nil || clean.CloneErrorMessages != nil {
		t.Errorf("clean run should have nil error lists, got %+v / %+v", clean.ErrorMessages, clean.CloneErrorMessages)
	}
}

func TestSummarizeCapsDistinctErrors(t *testing.T) {
	var samples []sample
	const distinct = 25
	for i := range distinct {
		// No volatile substrings, so each stays a distinct normalized key.
		samples = append(samples, sample{offset: time.Second, res: outcomeErr, msg: fmt.Sprintf("error variant %d", i)})
	}
	r := summarize(samples, 1, "branch", 1, 1, 0, 10*time.Second, "")

	// maxDistinctErrors real messages + one "(other errors)" overflow bucket.
	if len(r.ErrorMessages) != maxDistinctErrors+1 {
		t.Fatalf("groups = %d, want %d", len(r.ErrorMessages), maxDistinctErrors+1)
	}
	var overflow *errGroup
	for i := range r.ErrorMessages {
		if r.ErrorMessages[i].Message == "(other errors)" {
			overflow = &r.ErrorMessages[i]
		}
	}
	if overflow == nil {
		t.Fatal("no (other errors) overflow bucket")
	}
	if want := distinct - maxDistinctErrors; overflow.Count != want {
		t.Errorf("overflow count = %d, want %d", overflow.Count, want)
	}
}

// session strategy records clone (op=clone) and push (op=push) samples in one
// slice; summarize must report them as separate metrics, not conflate them.
func TestSummarizeSplitsCloneAndPush(t *testing.T) {
	warmup := 0 * time.Second
	window := 10 * time.Second
	samples := []sample{
		{offset: 1 * time.Second, dur: 200 * time.Millisecond, res: outcomeOK, op: opClone},
		{offset: 2 * time.Second, dur: 400 * time.Millisecond, res: outcomeOK, op: opClone},
		{offset: 3 * time.Second, res: outcomeErr, op: opClone}, // failed clone
		{offset: 4 * time.Second, dur: 30 * time.Millisecond, res: outcomeOK, op: opPush},
		{offset: 5 * time.Second, dur: 50 * time.Millisecond, res: outcomeOK, op: opPush},
	}
	r := summarize(samples, 8, "session", 1, 3, warmup, window, "1-10 x 2048B")

	if r.Pushes != 2 || r.OK != 2 {
		t.Fatalf("push side: Pushes=%d OK=%d, want 2/2 (clones excluded)", r.Pushes, r.OK)
	}
	if r.P50ms != 30 {
		t.Fatalf("push p50=%v, want 30 (clone durations must not leak in)", r.P50ms)
	}
	if r.Clones != 3 || r.CloneOK != 2 || r.CloneErrors != 1 {
		t.Fatalf("clone side: clones=%d ok=%d err=%d, want 3/2/1", r.Clones, r.CloneOK, r.CloneErrors)
	}
	if r.CloneP50ms != 200 {
		t.Fatalf("clone p50=%v, want 200", r.CloneP50ms)
	}
}
