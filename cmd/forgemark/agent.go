package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

// noopSigner skips commit signing so the host's commit.gpgsign config never
// interferes.
type noopSigner struct{}

func (noopSigner) Sign(io.Reader) ([]byte, error) { return nil, nil }

// agent is one synthetic writer. It keeps an in-memory git repo and pushes a
// stream of small commits to its assigned ref on its assigned repo+node. Each
// agent owns its own ref (per-agent branch), so in branch/repo strategies its
// pushes never lose a CAS race — what we measure is the server's per-repo
// serialized ref-commit throughput, not client retry behaviour.
type agent struct {
	id       int
	repoPath string // repo path, appended to node verbatim (see verbatimURLFor)
	node     string // node base URL, e.g. https://node1.cluster:443
	ref      string // destination ref, e.g. refs/heads/<run>-a7
	objFmt   formatcfg.ObjectFormat

	cfg   *commitConfig
	creds credentialProvider
	httpc *http.Client
	sess  *sessionConfig // non-nil → "session" strategy (clone+push loop)

	repo    *git.Repository
	samples []sample
}

// commitConfig is the subset of run config an agent needs.
type commitConfig struct {
	filesMin int
	filesMax int
	fileSize int
}

// authorEmail stamps the synthetic load commits. .invalid is the RFC 2606
// reserved TLD, so it can never collide with a real address.
const authorEmail = "agent@forgemark.invalid"

// resetEvery re-initialises an agent's in-memory repo after this many commits.
// memory.NewStorage keeps every object it ever wrote, so a long sweep would
// grow unbounded (each commit adds fresh random blobs); re-init caps retained
// history. Each agent force-pushes its own ref, so a fresh orphan line is fine.
const resetEvery = 128

// newAgent builds an agent. In branch/repo mode it initialises its in-memory
// repo up front (object format matched to the remote). In session mode (sess
// != nil) the repo is (re)created per session by the clone loop, so init is
// skipped.
func newAgent(id int, repoPath, node, ref string, objFmt formatcfg.ObjectFormat, cfg *commitConfig, creds credentialProvider, httpc *http.Client, sess *sessionConfig) (*agent, error) {
	a := &agent{
		id: id, repoPath: repoPath, node: node, ref: ref, objFmt: objFmt,
		cfg: cfg, creds: creds, httpc: httpc, sess: sess,
	}
	if sess == nil {
		if err := a.initRepo(); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// initRepo (re)creates the agent's in-memory repo and origin remote, dropping
// any accumulated objects from a previous generation.
func (a *agent) initRepo() error {
	repo, err := git.Init(memory.NewStorage(),
		git.WithWorkTree(memfs.New()),
		git.WithObjectFormat(a.objFmt),
	)
	if err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{verbatimURLFor(a.node, a.repoPath)},
	}); err != nil {
		return fmt.Errorf("create remote: %w", err)
	}
	a.repo = repo
	return nil
}

// run pushes commits in a tight loop until ctx is cancelled, recording one
// sample per attempt. start anchors sample offsets so the caller can drop the
// warm-up window afterwards.
func (a *agent) run(ctx context.Context, start time.Time) {
	if a.sess != nil {
		a.runSessions(ctx, start)
		return
	}
	rng := rand.New(rand.NewSource(int64(a.id)*1_000_003 + 1)) //nolint:gosec // synthetic load content, not security-sensitive
	iter := 0
	for ctx.Err() == nil {
		if iter > 0 && iter%resetEvery == 0 {
			if err := a.initRepo(); err != nil {
				a.samples = append(a.samples, sample{offset: time.Since(start), res: outcomeErr})
				iter++
				continue
			}
		}
		if err := a.commit(rng, iter); err != nil {
			// A local commit failure is a harness bug, not a server signal —
			// record it as an error and keep going so one bad agent doesn't
			// silently drop out of the concurrency level.
			a.samples = append(a.samples, sample{offset: time.Since(start), res: outcomeErr})
			iter++
			continue
		}
		t0 := time.Now()
		err := a.push(ctx)
		dur := time.Since(t0)
		// Don't record the attempt that lost the ctx-cancellation race; it's a
		// truncated measurement, not real latency.
		if ctx.Err() != nil {
			break
		}
		a.samples = append(a.samples, sample{offset: t0.Sub(start), dur: dur, res: classify(err)})
		iter++
	}
}

// commit writes filesMin..filesMax small files and records a commit on the
// local default branch (refs/heads/master). Stable filenames mean later commits
// are modifications, the realistic shape of an agent editing a working set.
func (a *agent) commit(rng *rand.Rand, iter int) error {
	wt, err := a.repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	n := a.cfg.filesMin
	if a.cfg.filesMax > a.cfg.filesMin {
		n += rng.Intn(a.cfg.filesMax - a.cfg.filesMin + 1)
	}
	buf := make([]byte, a.cfg.fileSize)
	for i := range n {
		name := fmt.Sprintf("file-%d.txt", i)
		f, err := wt.Filesystem().Create(name)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		_, _ = rng.Read(buf) // math/rand never errors; random bytes → distinct blob
		if _, err := f.Write(buf); err != nil {
			_ = f.Close()
			return fmt.Errorf("write %s: %w", name, err)
		}
		_ = f.Close()
		if _, err := wt.Add(name); err != nil {
			return fmt.Errorf("add %s: %w", name, err)
		}
	}
	sig := &object.Signature{
		Name:  fmt.Sprintf("agent-%d", a.id),
		Email: authorEmail,
		When:  time.Now(),
	}
	if _, err := wt.Commit(fmt.Sprintf("agent %d commit %d", a.id, iter),
		&git.CommitOptions{Author: sig, Signer: noopSigner{}}); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (a *agent) push(ctx context.Context) error {
	auth, err := a.creds.basicAuth(ctx, a.repoPath)
	if err != nil {
		return err
	}
	err = a.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("+refs/heads/master:" + a.ref)},
		Force:      true, // each agent owns its ref; force keeps numbers clean of spurious non-ff
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

// classify maps a push result to an outcome. A nil error or "already
// up-to-date" is success; a CAS / non-fast-forward rejection is contention;
// everything else is an error.
func classify(err error) outcome {
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
		return outcomeOK
	case errors.Is(err, git.ErrNonFastForwardUpdate):
		return outcomeCAS
	default:
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "non-fast-forward") ||
			strings.Contains(msg, "reference has changed") ||
			strings.Contains(msg, "fetch first") ||
			strings.Contains(msg, "stale info") {
			return outcomeCAS
		}
		return outcomeErr
	}
}
