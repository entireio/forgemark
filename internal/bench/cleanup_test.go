package bench

import "testing"

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
