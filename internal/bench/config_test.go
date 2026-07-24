package bench

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

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
