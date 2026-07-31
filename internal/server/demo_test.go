package server

import (
	"testing"

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
