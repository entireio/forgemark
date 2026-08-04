package bench

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
)

// The sweep deletes whatever this regex matches, so its precision is the
// entire safety story: it must catch every shape forgemark pushes (CLI runs,
// server runs with -t<target>, session -s<n> refs) and NOTHING a human would
// name — deleting a user's branch is permanent remote data loss.
func TestStaleBenchRefRe(t *testing.T) {
	re := staleBenchRefRe("bench/")
	for _, ref := range []string{
		"refs/heads/bench/fmk3x9q2ab4z-c1-a0",        // CLI run
		"refs/heads/bench/fmk3x9q2ab4z-t0-c16-a3",    // server run, target 0
		"refs/heads/bench/fmk3x9q2ab4z-t1-c4-a0-s12", // session ephemeral ref
		"refs/heads/bench/fm0123456789ab-c128-a4095", // digits-only run id
	} {
		if !re.MatchString(ref) {
			t.Errorf("forgemark ref %q not matched; stale refs would accumulate", ref)
		}
	}
	for _, ref := range []string{
		"refs/heads/bench/my-feature",                 // plain user branch
		"refs/heads/bench/fmt-c1-a1",                  // human branch shaped close (fmt < 8 chars)
		"refs/heads/bench/fmk3x9q2ab4z-t0-c16-a3-wip", // suffix junk
		"refs/heads/bench/prefix-fmk3x9q2ab4z-c1-a0",  // wrong position
		"refs/heads/main",                             // outside the prefix
		"refs/heads/fmk3x9q2ab4z-c1-a0",               // right shape, wrong prefix
		"refs/tags/bench/fmk3x9q2ab4z-c1-a0",          // not a branch
	} {
		if re.MatchString(ref) {
			t.Errorf("non-forgemark ref %q matched; the sweep would delete user data", ref)
		}
	}
	// An empty prefix (the CLI default) anchors directly under refs/heads/.
	if !staleBenchRefRe("").MatchString("refs/heads/fmk3x9q2ab4z-c1-a0") {
		t.Error("empty-prefix forgemark ref not matched")
	}
	if staleBenchRefRe("").MatchString("refs/heads/bench/fmk3x9q2ab4z-c1-a0") {
		t.Error("empty-prefix regex must not reach into other prefixes")
	}
}

// The age guard protects runs the active-run lock can't see (another process
// or machine sharing the repo): a ref is swept only when its embedded
// run-start time is older than the cutoff. Shape alone must never suffice.
func TestStaleRefAgeGuard(t *testing.T) {
	re := staleBenchRefRe("bench/")
	cutoff := time.Now().Add(-staleAfter)
	id := func(at time.Time) string { return strconv.FormatInt(at.UnixNano(), 36) }

	old := "refs/heads/bench/fm" + id(time.Now().Add(-2*staleAfter)) + "-t0-c16-a3"
	if !staleRef(re, old, cutoff) {
		t.Errorf("ref from %v ago not swept; stale refs would accumulate", 2*staleAfter)
	}
	recent := "refs/heads/bench/fm" + id(time.Now().Add(-time.Hour)) + "-c4-a0"
	if staleRef(re, recent, cutoff) {
		t.Error("hour-old ref swept; it could belong to a concurrently running benchmark")
	}
	if staleRef(re, "refs/heads/bench/my-feature", cutoff) {
		t.Error("non-forgemark ref treated as stale")
	}
	// A shape match whose timestamp doesn't parse as base36 int64 (overflow)
	// must be left alone — when in doubt, never delete.
	if staleRef(re, "refs/heads/bench/fmzzzzzzzzzzzzzzzzzzzz-c1-a0", cutoff) {
		t.Error("unparseable run-id timestamp treated as stale")
	}
}

// The first push to an empty repo promotes that bench ref to the remote's
// default branch, which forges refuse to delete — the sweep must leave it
// alone instead of failing on every subsequent run, while still deleting the
// other stale refs. Only a symbolic HEAD pins a branch: go-git normalizes a
// bare-hash HEAD to a symref against the advertisement in every transport
// path, so a hash HEAD reaching sweepSpecs is detached-at-a-commit and spares
// nothing.
func TestSweepSpecsSpareDefaultBranch(t *testing.T) {
	re := staleBenchRefRe("bench/")
	cutoff := time.Now() // everything below is "old enough"
	oldID := "fm" + strconv.FormatInt(time.Now().Add(-3*staleAfter).UnixNano(), 36)
	pinned := plumbing.ReferenceName("refs/heads/bench/" + oldID + "-c1-a0")
	other := plumbing.ReferenceName("refs/heads/bench/" + oldID + "-c1-a1")
	tip := plumbing.NewHash("0102030405060708090a0b0c0d0e0f1011121314")
	otherTip := plumbing.NewHash("a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4")

	cases := []struct {
		name string
		refs []*plumbing.Reference
		want []string // deletion targets, in advertisement order
	}{
		{
			name: "symref HEAD spares the default branch",
			refs: []*plumbing.Reference{
				plumbing.NewSymbolicReference(plumbing.HEAD, pinned),
				plumbing.NewHashReference(pinned, tip),
				plumbing.NewHashReference(other, otherTip),
			},
			want: []string{other.String()},
		},
		{
			name: "detached hash HEAD spares nothing",
			refs: []*plumbing.Reference{
				plumbing.NewHashReference(plumbing.HEAD, tip),
				plumbing.NewHashReference(pinned, tip),
				plumbing.NewHashReference(other, otherTip),
			},
			want: []string{pinned.String(), other.String()},
		},
		{
			name: "no HEAD advertised sweeps everything stale",
			refs: []*plumbing.Reference{
				plumbing.NewHashReference(pinned, tip),
				plumbing.NewHashReference(other, otherTip),
			},
			want: []string{pinned.String(), other.String()},
		},
	}
	for _, tc := range cases {
		specs := sweepSpecs(tc.refs, re, cutoff)
		got := make([]string, len(specs))
		for i, s := range specs {
			got[i] = strings.TrimPrefix(string(s), ":")
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: sweep would delete %v, want %v", tc.name, got, tc.want)
		}
	}
}
