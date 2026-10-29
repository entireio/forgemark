package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-billy/v6/memfs"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

// noopSigner skips commit signing so the host's commit.gpgsign config never
// interferes.
type noopSigner struct{}

func (noopSigner) Sign(context.Context, io.Reader) ([]byte, error) { return nil, nil }

// agent is one synthetic writer. It keeps an in-memory git repo and pushes a
// stream of small commits to its assigned ref on its assigned repo+node. Each
// agent owns its own ref (per-agent branch), so in branch/repo strategies its
// pushes never lose a CAS race — what we measure is the server's per-repo
// serialized ref-commit throughput, not client retry behaviour. The
// contention strategy inverts that on purpose: every agent shares one ref and
// CAS rejections are the measurement.
type agent struct {
	id       int
	repoPath string // repo path, appended to node verbatim (see verbatimURLFor)
	node     string // node base URL, e.g. https://node1.cluster:443
	ref      string // destination ref, e.g. refs/forgemark/<run>-c8-a7 (see DestRef)
	objFmt   formatcfg.ObjectFormat

	cfg      *CommitConfig
	creds    credentialProvider
	httpc    *http.Client
	pushOpts []string       // push options sent with every push (git push -o)
	clone    *cloneConfig   // non-nil → "clone" strategy (clone-only loop)
	sess     *sessionConfig // non-nil → "session" strategy (clone+push loop)
	contend  bool           // "contention" strategy: every agent races the same ref, non-force
	sink     Sink           // optional live observer; nil on the CLI path

	repo      *git.Repository
	samples   []Sample
	cancelled int                        // ops cut off by ctx, which the server saw as cancelled
	est       [OpClone + 1]time.Duration // smoothed latency per OpKind (see observe)
}

// record appends a sample to the agent's authoritative post-hoc log and, when
// a sink is attached, forwards it live. Every sample the agent takes goes
// through here.
func (a *agent) record(s Sample) {
	a.samples = append(a.samples, s)
	if s.Dur > 0 {
		a.observe(s.Op, s.Dur)
	}
	if a.sink != nil {
		a.sink.OnSample(s)
	}
}

// observe folds one measured duration into the op's latency estimate. The
// estimate rises at once to a slower sample but decays slowly towards faster
// ones. Latency growing because requests are queueing on the server is then
// reflected immediately, and the agent errs towards stopping early rather than
// starting an op the window end would cancel.
func (a *agent) observe(op OpKind, d time.Duration) {
	if e := a.est[op]; e > 0 {
		d = max(d, e+(d-e)/4)
	}
	a.est[op] = d
}

// fits reports whether an op of kind op started now is expected to finish
// before end. With nothing observed yet it always fits.
func (a *agent) fits(op OpKind, end time.Time) bool {
	return time.Now().Add(a.est[op]).Before(end)
}

// CommitConfig is the subset of run config an agent needs.
type CommitConfig struct {
	FilesMin int
	FilesMax int
	FileSize int
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
// repo up front (object format matched to the remote). In clone/session mode
// the repo is created per clone by the clone loop, so init is skipped.
func newAgent(id int, repoPath, node, ref string, objFmt formatcfg.ObjectFormat, cfg *CommitConfig, creds credentialProvider, httpc *http.Client, pushOpts []string, clone *cloneConfig, sess *sessionConfig, contend bool, sink Sink) (*agent, error) {
	a := &agent{
		id: id, repoPath: repoPath, node: node, ref: ref, objFmt: objFmt,
		cfg: cfg, creds: creds, httpc: httpc, pushOpts: pushOpts, clone: clone, sess: sess, contend: contend, sink: sink,
	}
	if clone == nil && sess == nil {
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

// run dispatches the agent's configured strategy until the window ends or ctx
// is cancelled. start anchors sample offsets so the caller can drop the
// warm-up window afterwards. No op starts unless it is expected to finish by
// end, so the window closing doesn't cancel a burst of in-flight requests.
// ctx is the hard stop for ops that overrun their estimate.
func (a *agent) run(ctx context.Context, start, end time.Time) {
	if a.clone != nil {
		a.runClones(ctx, start, end)
		return
	}
	if a.sess != nil {
		a.runSessions(ctx, start, end)
		return
	}
	rng := rand.New(rand.NewSource(int64(a.id)*1_000_003 + 1)) //nolint:gosec // synthetic load content, not security-sensitive
	iter := 0
	for ctx.Err() == nil && a.fits(OpPush, end) {
		if iter > 0 && iter%resetEvery == 0 {
			if err := a.initRepo(); err != nil {
				a.record(Sample{Offset: time.Since(start), Res: OutcomeErr, Msg: err.Error()})
				iter++
				continue
			}
		}
		if err := a.commit(rng, iter); err != nil {
			// A local commit failure is a harness bug, not a server signal —
			// record it as an error and keep going so one bad agent doesn't
			// silently drop out of the concurrency level.
			a.record(Sample{Offset: time.Since(start), Res: OutcomeErr, Msg: err.Error()})
			iter++
			continue
		}
		// Building the commit spent window time. Recheck, or a slow commit
		// could start the push after end, outside the measured window.
		if !a.fits(OpPush, end) {
			break
		}
		t0 := time.Now()
		err := a.push(ctx)
		dur := time.Since(t0)
		// Don't record the attempt that lost the ctx-cancellation race; it's a
		// truncated measurement, not real latency. Count it instead.
		if ctx.Err() != nil {
			a.cancelled++
			break
		}
		o := classify(err)
		a.record(Sample{Offset: t0.Sub(start), Dur: dur, Res: o, Msg: errMsg(o, err)})
		iter++
		// A contention agent that lost the ref race fetches the shared tip and
		// resets onto it so the next commit is its child — the push → reject →
		// fetch → rebase → push loop a real agent runs against a busy ref. The
		// converge is not a measured op; if it fails the next push just
		// CAS-rejects again, so a converge failure can't hot-spin the loop.
		if a.contend && o == OutcomeCAS {
			_ = a.converge(ctx)
		}
	}
}

// commit writes FilesMin..FilesMax small files and records a commit on the
// local default branch (refs/heads/master). Stable filenames mean later commits
// are modifications, the realistic shape of an agent editing a working set.
func (a *agent) commit(rng *rand.Rand, iter int) error {
	wt, err := a.repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	n := a.cfg.FilesMin
	if a.cfg.FilesMax > a.cfg.FilesMin {
		n += rng.Intn(a.cfg.FilesMax - a.cfg.FilesMin + 1)
	}
	buf := make([]byte, a.cfg.FileSize)
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
	// Owned refs force-push ('+' refspec) so numbers stay clean of spurious
	// non-ff. Contention agents race the shared ref with NEITHER '+' nor
	// Force — the CAS / non-fast-forward rejection is the measured signal,
	// and forcing would silently clobber every race.
	refspec := config.RefSpec("+refs/heads/master:" + a.ref)
	if a.contend {
		refspec = config.RefSpec("refs/heads/master:" + a.ref)
	}
	err = a.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{refspec},
		Force:      !a.contend,
		Options:    a.pushOpts,
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

// contentionTipRef is the local scratch ref a contention agent fetches the
// shared ref's current tip into before resetting onto it. It lives outside
// refs/heads/ so it can never collide with the push refspec
// (refs/heads/master:<shared>) or be swept up as a bench ref on the remote —
// it never leaves the in-memory storer.
const contentionTipRef = "refs/forgemark-contention/tip"

// converge moves a contention agent's local branch onto the shared ref's
// current tip after a CAS rejection: a depth-1 fetch of just that ref, then a
// hard reset. Depth 1 is load-shaping, not just an optimization — the shared
// ref's history grows by every accepted push, so a full fetch would get
// heavier with every converged op until the retry path dominated the measured
// window. The shared ref is guaranteed to exist when converge runs (a CAS
// rejection implies it does), so a "ref not found" here is a real error.
func (a *agent) converge(ctx context.Context) error {
	auth, err := a.creds.basicAuth(ctx, a.repoPath)
	if err != nil {
		return err
	}
	err = a.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("+" + a.ref + ":" + contentionTipRef)},
		Depth:      1,
		ClientOptions: []gitclient.Option{
			gitclient.WithHTTPClient(a.httpc),
			gitclient.WithHTTPAuth(auth),
		},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("fetch shared tip: %w", err)
	}
	tip, err := a.repo.Reference(plumbing.ReferenceName(contentionTipRef), true)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", contentionTipRef, err)
	}
	wt, err := a.repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if err := wt.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: tip.Hash()}); err != nil {
		return fmt.Errorf("reset onto shared tip: %w", err)
	}
	return nil
}

// classify maps a push result to an Outcome. A nil error or "already
// up-to-date" is success; a CAS / non-fast-forward rejection is contention;
// everything else is an error.
func classify(err error) Outcome {
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
		return OutcomeOK
	case errors.Is(err, git.ErrNonFastForwardUpdate):
		return OutcomeCAS
	default:
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "non-fast-forward") ||
			strings.Contains(msg, "reference has changed") ||
			strings.Contains(msg, "fetch first") ||
			strings.Contains(msg, "stale info") ||
			// Real git receive-pack rejects the optimistic-update side of a
			// ref race with its own phrasing — none of it contains
			// "non-fast-forward":
			//   "command error on <ref> incorrect old value provided" — the
			//     advertised tip the command's old= was built on moved before
			//     the update landed (the TOCTOU half of the race);
			//   "…reference already exists" — the create half of the race;
			//   "cannot lock ref" — the ref backend's own update spool
			//     refusing a second writer, contention by another name.
			// Bucketing these as errors would make real forges report CAS
			// storms as failures, so the contention strategy depends on them.
			strings.Contains(msg, "incorrect old value provided") ||
			strings.Contains(msg, "reference already exists") ||
			strings.Contains(msg, "cannot lock ref") {
			return OutcomeCAS
		}
		return OutcomeErr
	}
}

// maxErrMsgLen caps a retained error message. Longer messages are truncated
// after normalization so a long URL can't crowd the signal out of the window.
const maxErrMsgLen = 200

// reURL/reRef match the volatile substrings go-git embeds in error text — the
// receive-pack URL (varies by node/repo) and the destination ref (varies by
// agent/session, and by namespace: refs/forgemark/ by default, refs/heads/ or
// anything else via -ref-namespace). Collapsing them lets identical failures
// group in the results. Compiled once; the message-shape sibling of classify.
var (
	reURL = regexp.MustCompile(`https?://\S+`)
	reRef = regexp.MustCompile(`refs/\S+`)
)

// normalizeErr turns a raw go-git error message into a stable grouping key:
// collapse this run's volatile URLs/refs, then truncate. Order matters —
// truncating first could cut the status code that follows a long URL.
func normalizeErr(msg string) string {
	if msg == "" {
		return "(no message)"
	}
	msg = reURL.ReplaceAllString(msg, "<url>")
	msg = reRef.ReplaceAllString(msg, "<ref>")
	if len(msg) > maxErrMsgLen {
		msg = msg[:maxErrMsgLen]
	}
	return msg
}
