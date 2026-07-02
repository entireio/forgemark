package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/memory"
)

// sessionConfig drives the "session" strategy: each agent repeatedly
// shallow-clones the base branch, commits + pushes `commits` checkpoints to a
// fresh ephemeral branch, then abandons it and starts a new session. This
// interleaves read load (clone) with write load (push) on the same repo — the
// realistic agent shape — and exercises read/write contention (bitmap/cache
// invalidation under push churn) that the pure-push strategies can't.
type sessionConfig struct {
	commits    int    // commit+push checkpoints per cloned session
	cloneDepth int    // shallow clone depth (1 = tip only; 0 = full history)
	baseRef    string // branch to clone; "" = remote default branch
}

// runSessions is the agent loop for the session strategy. Clone latency is
// recorded as op==clone, push latency as op==push, so they're summarized
// separately. Ephemeral session branches (…-sN) model an agent abandoning a
// working branch each session.
func (a *agent) runSessions(ctx context.Context, start time.Time) {
	rng := rand.New(rand.NewSource(int64(a.id)*1_000_003 + 1)) //nolint:gosec // synthetic load, not security-sensitive
	for session := 0; ctx.Err() == nil; session++ {
		t0 := time.Now()
		repo, branch, err := a.cloneSession(ctx)
		if ctx.Err() != nil {
			return
		}
		res := readOutcome(err)
		a.samples = append(a.samples, sample{offset: t0.Sub(start), dur: time.Since(t0), res: res, op: opClone, msg: errMsg(res, err)})
		if err != nil {
			continue // clone failed — next session retries
		}
		a.repo = repo

		sessRef := fmt.Sprintf("%s-s%d", a.ref, session)
		pushed := false
		for k := 0; k < a.sess.commits && ctx.Err() == nil; k++ {
			if err := a.commit(rng, k); err != nil {
				a.samples = append(a.samples, sample{offset: time.Since(start), res: outcomeErr, op: opPush, msg: err.Error()})
				continue
			}
			t1 := time.Now()
			perr := a.pushRef(ctx, branch, sessRef)
			if perr == nil {
				pushed = true // the branch now exists on the remote
			}
			if ctx.Err() != nil {
				break // window closed mid-push: stop, but still clean up sessRef
			}
			o := classify(perr)
			a.samples = append(a.samples, sample{offset: t1.Sub(start), dur: time.Since(t1), res: o, op: opPush, msg: errMsg(o, perr)})
		}
		// Abandon the session branch by deleting it, so ephemeral …-sN refs don't
		// accumulate on the target repo across the sweep (a growing receive-pack
		// advertisement would inflate later push/clone latencies and bias the
		// numbers). Only delete a branch we actually created, and let deleteRef
		// run on its own context so the final session per level — reached as the
		// window closes — is still cleaned up rather than leaked.
		if pushed {
			a.deleteRef(sessRef)
		}
	}
}

// cloneSession shallow-clones the base branch into a fresh in-memory repo and
// returns the local branch to commit on. An empty remote degrades to an orphan
// (no read load) so the mode still runs against an unseeded repo.
func (a *agent) cloneSession(ctx context.Context) (*git.Repository, plumbing.ReferenceName, error) {
	auth, err := a.creds.basicAuth(ctx, a.repoPath)
	if err != nil {
		return nil, "", err
	}
	// Seed the storer's object format (matches initRepo for branch/repo). go-git
	// only auto-negotiates SHA256 on the plain-clone path (HEAD=refs/heads/.invalid);
	// a CloneContext into a default storer silently falls back to SHA1, so a
	// SHA256 cluster would break here. a.objFmt comes from the same advertisement
	// probe, so this also fixes the empty-remote (git.Open) degraded path below.
	storer := memory.NewStorage(memory.WithObjectFormat(a.objFmt))
	wt := memfs.New()
	opts := &git.CloneOptions{
		URL:          verbatimURLFor(a.node, a.repoPath),
		Depth:        a.sess.cloneDepth,
		SingleBranch: true,
		ClientOptions: []gitclient.Option{
			gitclient.WithHTTPClient(a.httpc),
			gitclient.WithHTTPAuth(auth),
		},
	}
	if a.sess.baseRef != "" {
		opts.ReferenceName = normalizeBaseRef(a.sess.baseRef)
	}
	repo, err := git.CloneContext(ctx, storer, wt, opts)
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		// CloneContext still initialised the storer + origin remote; open it and
		// commit on an unborn master so the session runs without a real base.
		repo, err = git.Open(storer, wt)
		if err != nil {
			return nil, "", fmt.Errorf("open empty clone: %w", err)
		}
		return repo, plumbing.ReferenceName("refs/heads/master"), nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("clone: %w", err)
	}
	head, err := repo.Head()
	if err != nil {
		return nil, "", fmt.Errorf("resolve head: %w", err)
	}
	return repo, head.Name(), nil
}

// pushRef force-pushes the local working branch to a session-scoped remote ref.
func (a *agent) pushRef(ctx context.Context, src plumbing.ReferenceName, dst string) error {
	auth, err := a.creds.basicAuth(ctx, a.repoPath)
	if err != nil {
		return err
	}
	err = a.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("+" + src.String() + ":" + dst)},
		Force:      true,
		ClientOptions: []gitclient.Option{
			gitclient.WithHTTPClient(a.httpc),
			gitclient.WithHTTPAuth(auth),
		},
	})
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// deleteRef best-effort deletes an ephemeral session branch via an empty-source
// refspec, so refs don't accumulate on the target repo across a sweep. It runs
// on its OWN short-lived context, not the caller's per-level ctx: the final
// session of each level is cleaned up as the measurement window closes, when the
// level ctx is already done — using that dead ctx would no-op the delete and
// leak the ref. It is cleanup, not a measured operation.
func (a *agent) deleteRef(dst string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	auth, err := a.creds.basicAuth(ctx, a.repoPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forgemark: delete session ref %s: auth: %v\n", dst, err)
		return
	}
	if err := a.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec(":" + dst)},
		ClientOptions: []gitclient.Option{
			gitclient.WithHTTPClient(a.httpc),
			gitclient.WithHTTPAuth(auth),
		},
	}); err != nil {
		// Best-effort: a failed delete just leaves one ref behind.
		fmt.Fprintf(os.Stderr, "forgemark: delete session ref %s failed: %v\n", dst, err)
	}
}

// normalizeBaseRef accepts either a fully-qualified ref ("refs/heads/main") or a
// bare branch name ("main") and returns the fully-qualified ReferenceName that
// go-git's clone requires, so `-base-ref main` works as the help text implies.
func normalizeBaseRef(ref string) plumbing.ReferenceName {
	if strings.HasPrefix(ref, "refs/") {
		return plumbing.ReferenceName(ref)
	}
	return plumbing.NewBranchReferenceName(ref)
}

// readOutcome maps a clone/fetch result to an outcome (reads have no CAS).
func readOutcome(err error) outcome {
	if err == nil {
		return outcomeOK
	}
	return outcomeErr
}
