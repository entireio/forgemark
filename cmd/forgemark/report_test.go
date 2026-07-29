package main

import (
	"strings"
	"testing"

	"github.com/entireio/forgemark/internal/bench"
	"github.com/entireio/forgemark/internal/results"
)

func TestMonitorFor(t *testing.T) {
	push := results.TargetResult{Name: "gl", Levels: []bench.LevelResult{
		{Concurrency: 1, Strategy: "branch", OK: 8, OpsPerSec: 8.1, P95ms: 120},
		{Concurrency: 8, Strategy: "branch", OK: 28, OpsPerSec: 28.44, P95ms: 180, CASFailures: 1},
		{Concurrency: 32, Strategy: "branch", OK: 42, OpsPerSec: 42.7, P95ms: 210, OtherErrors: 2},
	}}
	m := monitorFor(push)
	if m.Value != 42.7 {
		t.Errorf("peak value = %v, want 42.7", m.Value)
	}
	// The peak is the max ops/s level, not the last one, and errors sum across
	// the sweep (1 CAS + 2 other).
	for _, want := range []string{"peak push 42.7 ops/s @ c=32", "p95 210ms", "3 errors", "c=8 28.4"} {
		if !strings.Contains(m.Rationale, want) {
			t.Errorf("rationale %q missing %q", m.Rationale, want)
		}
	}

	// A non-monotonic sweep (throughput falls at the top level) still reports the
	// true peak, not the last level.
	dip := results.TargetResult{Levels: []bench.LevelResult{
		{Concurrency: 8, OpsPerSec: 50, P95ms: 100},
		{Concurrency: 32, OpsPerSec: 20, P95ms: 400},
	}}
	if got := monitorFor(dip); got.Value != 50 || !strings.Contains(got.Rationale, "@ c=8") {
		t.Errorf("dip peak = %v (%q), want 50 @ c=8", got.Value, got.Rationale)
	}

	// Clone strategy labels the op and uses the clone p95 / clone errors.
	clone := results.TargetResult{Levels: []bench.LevelResult{
		{Concurrency: 4, Strategy: "clone", OK: 30, OpsPerSec: 30.2, CloneP95ms: 250, CloneErrors: 3},
	}}
	cm := monitorFor(clone)
	if cm.Value != 30.2 || !strings.Contains(cm.Rationale, "peak clone") || !strings.Contains(cm.Rationale, "p95 250ms") || !strings.Contains(cm.Rationale, "3 errors") {
		t.Errorf("clone monitor = %v %q", cm.Value, cm.Rationale)
	}

	// A dead or empty target reports 0 so a gate reads it as a regression.
	dead := monitorFor(results.TargetResult{Name: "x", Error: "auth 401"})
	if dead.Value != 0 || !strings.Contains(dead.Rationale, "auth 401") {
		t.Errorf("dead monitor = %v %q, want 0 with error", dead.Value, dead.Rationale)
	}
	empty := monitorFor(results.TargetResult{Name: "y"})
	if empty.Value != 0 || !strings.Contains(empty.Rationale, "no measured levels") {
		t.Errorf("empty monitor = %v %q, want 0 no-levels", empty.Value, empty.Rationale)
	}
}

func TestSelectTarget(t *testing.T) {
	ts := []results.TargetResult{{Name: "a", Label: "alpha"}, {Name: "b", Label: "beta"}}
	if got, err := selectTarget(ts, ""); err != nil || got.Name != "a" {
		t.Errorf("empty selector = %v %v, want first (a)", got.Name, err)
	}
	if got, err := selectTarget(ts, "b"); err != nil || got.Name != "b" {
		t.Errorf("by name b = %v %v", got.Name, err)
	}
	if got, err := selectTarget(ts, "beta"); err != nil || got.Name != "b" {
		t.Errorf("by label beta = %v %v", got.Name, err)
	}
	if _, err := selectTarget(ts, "ghost"); err == nil {
		t.Error("unknown target should error")
	}
}

func TestRound1(t *testing.T) {
	cases := map[float64]string{42.70: "42.7", 8: "8", 28.44: "28.4", 0: "0", 18.34: "18.3"}
	for in, want := range cases {
		if got := round1(in); got != want {
			t.Errorf("round1(%v) = %q, want %q", in, got, want)
		}
	}
}
