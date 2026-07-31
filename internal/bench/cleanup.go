package bench

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/memory"
)

// staleBenchRefRe matches exactly the refs forgemark itself pushes under a
// branch prefix, and nothing else: DestRef is
// refs/heads/<prefix><runID>-c<level>-a<agent>, run IDs are "fm"+base36
// UnixNano (12+ chars — the {8,} floor keeps a human branch like
// <prefix>fmt-c1-a1 out of range), the server inserts -t<target> into the run
// ID, and the session strategy appends -s<n> to the agent ref. Anything that
// doesn't match this shape is someone else's branch and must never be deleted.
func staleBenchRefRe(branchPrefix string) *regexp.Regexp {
	return regexp.MustCompile(`^refs/heads/` + regexp.QuoteMeta(branchPrefix) +
		`fm[0-9a-z]{8,}(-t[0-9]+)?-c[0-9]+-a[0-9]+(-s[0-9]+)?$`)
}

// CleanStaleBenchRefs deletes refs left behind by earlier forgemark runs from
// every repo this runner pushes to, and returns how many it deleted. Each
// run's refs embed its run ID, so they accumulate across runs; a growing ref
// advertisement inflates receive-pack negotiation and makes later
// measurements history-dependent. Running this inside the run — under the
// server's one-active-run rule — makes the deletion atomic with run
// registration: no concurrently started benchmark can have its refs swept.
// Only exact forgemark-shaped refs under this workload's branch prefix are
// touched.
func (r *Runner) CleanStaleBenchRefs(ctx context.Context) (int, error) {
	re := staleBenchRefRe(r.w.BranchPrefix)
	deleted := 0
	seen := map[string]bool{}
	for _, repoPath := range r.target.Repos {
		if seen[repoPath] {
			continue
		}
		seen[repoPath] = true
		n, err := r.cleanRepoRefs(ctx, repoPath, re)
		deleted += n
		if err != nil {
			return deleted, fmt.Errorf("clean stale bench refs in %s: %w", repoPath, err)
		}
	}
	return deleted, nil
}

func (r *Runner) cleanRepoRefs(ctx context.Context, repoPath string, re *regexp.Regexp) (int, error) {
	auth, err := r.creds.basicAuth(ctx, repoPath)
	if err != nil {
		return 0, err
	}
	opts := []gitclient.Option{gitclient.WithHTTPClient(r.httpc), gitclient.WithHTTPAuth(auth)}
	// nodes[0] serves the full advertisement and accepts ref deletes like any
	// push; per-node fan-out exists to spread load, not for correctness.
	remote := git.NewRemote(memory.NewStorage(memory.WithObjectFormat(r.ep.objFmt)), &config.RemoteConfig{
		Name: "origin", URLs: []string{verbatimURLFor(r.ep.nodes[0], repoPath)},
	})
	refs, err := remote.ListContext(ctx, &git.ListOptions{ClientOptions: opts})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return 0, nil // nothing to clean in an unborn repo
	}
	if err != nil {
		return 0, fmt.Errorf("list refs: %w", err)
	}
	var specs []config.RefSpec
	for _, ref := range refs {
		if name := ref.Name().String(); re.MatchString(name) {
			specs = append(specs, config.RefSpec(":"+name))
		}
	}
	if len(specs) == 0 {
		return 0, nil
	}
	// One push carries every deletion; an empty-source refspec needs no local
	// objects, so a bare remote over an empty storer suffices.
	err = remote.PushContext(ctx, &git.PushOptions{RemoteName: "origin", RefSpecs: specs, ClientOptions: opts})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return 0, fmt.Errorf("delete %d refs: %w", len(specs), err)
	}
	return len(specs), nil
}
