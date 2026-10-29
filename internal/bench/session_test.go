package bench

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol"
	"github.com/go-git/go-git/v6/storage/memory"
)

// A branch/repo run against an unseeded repo under the default namespace
// leaves refs/forgemark/* behind with HEAD still unborn. A later clone/session
// must treat that like an empty repo and degrade to an orphan, on every wire
// protocol: go-git reports it as "empty" over v2 (prefix-filtered ls-refs
// returns nothing) but as "couldn't find remote ref HEAD" over v0/v1 (the
// full advertisement is non-empty). Without the fallback every session clone
// fails forever on a v0/v1 forge. An explicit -base-ref that is missing stays
// an error — that is a typo the operator must see.
func TestOrphanFallbackUnbornHeadWithBenchRefs(t *testing.T) {
	bare := filepath.Join(t.TempDir(), "bare.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	// One commit, pushed only to a bench-shaped ref: HEAD on the bare stays unborn.
	seed, err := git.Init(memory.NewStorage(), git.WithWorkTree(memfs.New()))
	if err != nil {
		t.Fatal(err)
	}
	wt, err := seed.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "x", Email: "x@x", When: time.Now()}
	if _, err := wt.Commit("x", &git.CommitOptions{Author: sig, AllowEmptyCommits: true, Signer: noopSigner{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Push(&git.PushOptions{RemoteName: "origin",
		RefSpecs: []config.RefSpec{"+refs/heads/master:refs/forgemark/fmX-c1-a0"}}); err != nil {
		t.Fatal(err)
	}

	clone := func(v protocol.Version, ref plumbing.ReferenceName) error {
		t.Helper()
		st := memory.NewStorage()
		cfg, err := st.Config()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Protocol.Version = v
		if err := st.SetConfig(cfg); err != nil {
			t.Fatal(err)
		}
		opts := &git.CloneOptions{URL: bare, Depth: 1, SingleBranch: true, ReferenceName: ref}
		_, err = git.CloneContext(context.Background(), st, memfs.New(), opts)
		if err == nil {
			t.Fatalf("clone(v%d, %q) succeeded against an unborn HEAD", v, ref)
		}
		return err
	}
	for _, v := range []protocol.Version{protocol.V0, protocol.V1, protocol.V2} {
		if err := clone(v, ""); !orphanFallback(err, false) {
			t.Errorf("v%d, implicit default branch: %v not treated as nothing-to-clone", v, err)
		}
		if v == protocol.V2 {
			continue // v2's prefix filter hides a missing explicit ref as "empty" — pre-existing, not this fallback's call
		}
		if err := clone(v, "refs/heads/main"); orphanFallback(err, true) {
			t.Errorf("v%d, explicit -base-ref: %v silently degraded to an orphan", v, err)
		}
	}
	// The implicit branch must only degrade on the two nothing-to-clone errors;
	// anything else (auth, network, a bad pack) is still a clone failure.
	if orphanFallback(errors.New("401 Unauthorized"), false) {
		t.Error("unrelated clone error degraded to an orphan")
	}
	if orphanFallback(nil, false) {
		t.Error("nil error is not a fallback")
	}
}
