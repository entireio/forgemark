package bench

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/memory"
)

// staleAfter is how old a run must be before its refs are swept. The run ID
// embeds its start time (base36 UnixNano), so this is a positive record of
// when the owning run began — refs younger than this belong to a run that
// could plausibly still be executing SOMEWHERE (another process or machine
// sharing the repo; within one server the active-run lock already
// serializes), and deleting a live run's refs would corrupt its
// measurements. "Plausibly still executing" must cover the longest run
// Validate itself admits — maxLevels × (maxDuration + maxWarmup) ≈ 112h —
// or a second forgemark process starting past the cutoff would sweep a
// still-running long run's refs mid-measurement; seven days covers that
// envelope with margin. Two overlapping generators on one repo+prefix
// corrupt each other's numbers regardless, so this guard is belt-and-braces
// for an unsupported setup; the cost is that refs from runs within the
// window linger until a later sweep, a few dozen refs at typical sweep
// sizes.
const staleAfter = 7 * 24 * time.Hour

// staleBenchRefRe matches exactly the refs forgemark itself pushes under a
// namespace + branch prefix, and nothing else: DestRef is
// <namespace><prefix><runID>-c<level>-a<agent>, run IDs are "fm"+base36
// UnixNano (12+ chars — the {8,} floor keeps a human branch like
// <prefix>fmt-c1-a1 out of range), the server inserts -t<target> into the run
// ID, and the session strategy appends -s<n> to the agent ref. Anything that
// doesn't match this shape is someone else's ref and must never be deleted.
// The namespace is matched verbatim — plus legacyRefNamespace, always: every
// forgemark before -ref-namespace existed pushed to refs/heads/, and those
// leftovers inflate the advertisement exactly like this run's own would, so
// a run under the new default must still sweep them or the history-dependence
// this sweep exists to remove would persist until someone ran with
// -ref-namespace refs/heads/ by hand. The shape and age guards apply to the
// legacy namespace unchanged, and sweepSpecs still spares the default branch
// a legacy run may have promoted. Any OTHER namespace's leftovers stay put;
// they belong to a run that chose it on purpose. The capture group is the
// run ID's base36 timestamp, for the age guard.
func staleBenchRefRe(namespace, branchPrefix string) *regexp.Regexp {
	ns := regexp.QuoteMeta(namespace)
	if namespace != legacyRefNamespace {
		ns = `(?:` + ns + `|` + regexp.QuoteMeta(legacyRefNamespace) + `)`
	}
	return regexp.MustCompile(`^` + ns + regexp.QuoteMeta(branchPrefix) +
		`fm([0-9a-z]{8,})(-t[0-9]+)?-c[0-9]+-a[0-9]+(-s[0-9]+)?$`)
}

// legacyRefNamespace is where every forgemark run pushed before the namespace
// became configurable; the sweep keeps covering it (see staleBenchRefRe).
const legacyRefNamespace = "refs/heads/"

// staleRef reports whether ref is a forgemark bench ref whose owning run
// started before cutoff. A ref that matches the shape but carries an
// unparseable timestamp is NOT stale — when in doubt, never delete.
//
// A zero cutoff means "no age guard": every well-formed bench ref is stale.
// The run timestamp comes from the clock of whichever host pushed the ref, so
// a standalone sweep at -min-age 0 compared against time.Now() would silently
// skip refs from a generator whose clock runs ahead of the sweeper's — the
// exact refs an operator asking for "everything" expects gone. Shape and a
// parseable run ID still gate deletion; only the time comparison is dropped.
func staleRef(re *regexp.Regexp, ref string, cutoff time.Time) bool {
	m := re.FindStringSubmatch(ref)
	if m == nil {
		return false
	}
	nanos, err := strconv.ParseInt(m[1], 36, 64)
	if err != nil {
		return false
	}
	if cutoff.IsZero() {
		return true
	}
	return time.Unix(0, nanos).Before(cutoff)
}

// CleanStaleBenchRefs deletes refs left behind by earlier forgemark runs from
// every repo this runner pushes to, and returns how many it deleted. Each
// run's refs embed its run ID, so they accumulate across runs; a growing ref
// advertisement inflates receive-pack negotiation and makes later
// measurements history-dependent. Running this inside the run — under the
// server's one-active-run rule — makes the deletion atomic with run
// registration on this server, and the staleAfter age guard (decoded from
// each ref's embedded run-start time) protects runs owned by OTHER processes
// or machines that this lock can't see. Only exact forgemark-shaped refs
// under this workload's branch prefix, from runs older than staleAfter, are
// touched.
//
// The sweep deliberately runs for EVERY strategy, clone included, even though
// a clone workload is otherwise read-only: stale forgemark refs inflate the
// upload-pack advertisement and ride along in full clones, so skipping the
// sweep for clone would make clone measurements depend on how many push
// benchmarks ran before — the exact history-dependence this function exists
// to remove. The write is conditional anyway (no matching stale refs → no
// push), and a read-only credential degrades to the callers' best-effort
// warning, not a failed run.
func (r *Runner) CleanStaleBenchRefs(ctx context.Context) (int, error) {
	// The sweep's ls-remote/push run over the same HTTP client the measured
	// agents use, which would leave warm TCP/TLS keep-alives in the pool and
	// quietly subtract cold-connection cost from a warmup=0 run's first
	// samples. Drop them when done (even on error — the ListContext alone
	// warms a connection) so level one dials as cold as it would have without
	// housekeeping. OS-level DNS caching remains either way.
	defer func() {
		if r.httpc != nil {
			r.httpc.CloseIdleConnections()
		}
	}()
	sweeps, err := r.sweepBenchRefs(ctx, time.Now().Add(-staleAfter), false)
	deleted := 0
	for _, s := range sweeps {
		deleted += s.Deleted
	}
	return deleted, err
}

// SweepOptions tunes a standalone sweep (`forgemark sweep`), as opposed to
// the housekeeping sweep every run performs on itself before measuring.
type SweepOptions struct {
	// DryRun lists the refs the sweep would delete and deletes nothing.
	DryRun bool
	// MinAge spares refs whose owning run started less than this long ago.
	// Zero sweeps every matching ref right away. The in-run sweep always
	// waits staleAfter because it cannot know what else is pushing to the
	// repo; a standalone sweep is the operator asserting that nothing is.
	MinAge time.Duration
}

// RepoSweep is one repo's share of a sweep.
type RepoSweep struct {
	Repo    string
	Refs    []string // bench refs selected for deletion, in advertisement order
	Deleted int      // how many of Refs were deleted; 0 on a dry run
}

// SweepBenchRefs is the standalone sweep behind `forgemark sweep`. It selects
// refs exactly like CleanStaleBenchRefs — forgemark-shaped refs under this
// workload's namespace and branch prefix, plus the legacy refs/heads/
// leftovers, never the remote's default branch — but with the age cutoff set
// by the caller and an optional dry run, and it reports what it found in
// every repo so the operator can see the refs before deleting them. Like the
// in-run sweep it keeps going past a repo that refuses and returns the joined
// refusals alongside the per-repo outcomes.
func (r *Runner) SweepBenchRefs(ctx context.Context, o SweepOptions) ([]RepoSweep, error) {
	if o.MinAge < 0 {
		return nil, fmt.Errorf("sweep min age %s must be >= 0", o.MinAge)
	}
	// Zero age means no guard at all, not "older than now": see staleRef.
	var cutoff time.Time
	if o.MinAge > 0 {
		cutoff = time.Now().Add(-o.MinAge)
	}
	return r.sweepBenchRefs(ctx, cutoff, o.DryRun)
}

func (r *Runner) sweepBenchRefs(ctx context.Context, cutoff time.Time, dryRun bool) ([]RepoSweep, error) {
	re := staleBenchRefRe(r.w.Namespace(), r.w.BranchPrefix)
	var sweeps []RepoSweep
	var errs []error
	seen := map[string]bool{}
	for _, repoPath := range r.target.Repos {
		if seen[repoPath] {
			continue
		}
		seen[repoPath] = true
		s, err := r.sweepRepoRefs(ctx, repoPath, re, cutoff, dryRun)
		sweeps = append(sweeps, s)
		if err != nil {
			// One repo's refusal (protected ref, permissions) must not leave the
			// REST of a multi-repo target unswept — their advertisements would
			// keep growing behind a repeating warning. Collect and keep going;
			// a dead context is different, every further repo would just add
			// the same cancellation error.
			errs = append(errs, fmt.Errorf("sweep bench refs in %s: %w", repoPath, err))
			if ctx.Err() != nil {
				break
			}
		}
	}
	return sweeps, errors.Join(errs...)
}

// sweepSpecs selects the deletion refspecs for one repo's advertisement:
// every forgemark-shaped ref older than cutoff, EXCEPT the remote's default
// branch. The default branch can itself be a stale bench ref — under a
// refs/heads/ namespace the FIRST push to an empty repo promotes that branch
// to HEAD, and forges then refuse to delete it (GitHub: "refusing to delete
// the current branch", GitLab: a pre-receive decline) — so asking would fail
// on every run, forever; one permanently pinned ref is a constant, not the
// unbounded growth this sweep exists to bound. (Under the default
// refs/forgemark/ namespace no bench ref can become HEAD, and this check is
// simply never hit.) A symbolic HEAD is the only signal needed: go-git already
// normalizes a bare-hash HEAD against the advertisement in every transport
// path (transport.NewRemoteRefs → packp.ResolveHeadFromHashHeuristic), so a
// hash HEAD reaching this code means detached-at-a-commit with no matching
// branch — nothing to spare.
func sweepSpecs(refs []*plumbing.Reference, re *regexp.Regexp, cutoff time.Time) []config.RefSpec {
	defaultBranch := ""
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference {
			defaultBranch = ref.Target().String()
		}
	}
	var specs []config.RefSpec
	for _, ref := range refs {
		if name := ref.Name().String(); name != defaultBranch && staleRef(re, name, cutoff) {
			specs = append(specs, config.RefSpec(":"+name))
		}
	}
	return specs
}

// sweepRepoRefs lists one repo's advertisement, selects the bench refs older
// than cutoff, and — unless dryRun — deletes them. The returned RepoSweep
// always carries the selection, even when the deletion was refused, so a
// caller can show what the sweep tried.
func (r *Runner) sweepRepoRefs(ctx context.Context, repoPath string, re *regexp.Regexp, cutoff time.Time, dryRun bool) (RepoSweep, error) {
	s := RepoSweep{Repo: repoPath}
	auth, err := r.creds.basicAuth(ctx, repoPath)
	if err != nil {
		return s, err
	}
	opts := []gitclient.Option{gitclient.WithHTTPClient(r.httpc), gitclient.WithHTTPAuth(auth)}
	// nodes[0] serves the full advertisement and accepts ref deletes like any
	// push; per-node fan-out exists to spread load, not for correctness.
	remote := git.NewRemote(memory.NewStorage(memory.WithObjectFormat(r.ep.objFmt)), &config.RemoteConfig{
		Name: "origin", URLs: []string{verbatimURLFor(r.ep.nodes[0], repoPath)},
	})
	refs, err := remote.ListContext(ctx, &git.ListOptions{ClientOptions: opts})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return s, nil // nothing to clean in an unborn repo
	}
	if err != nil {
		return s, fmt.Errorf("list refs: %w", err)
	}
	specs := sweepSpecs(refs, re, cutoff)
	for _, spec := range specs {
		s.Refs = append(s.Refs, strings.TrimPrefix(string(spec), ":"))
	}
	if len(specs) == 0 || dryRun {
		return s, nil
	}
	// One push carries every deletion; an empty-source refspec needs no local
	// objects, so a bare remote over an empty storer suffices. NoErrAlreadyUpToDate
	// means nothing matching the spec(s) remained — already gone counts as done.
	push := func(specs []config.RefSpec) error {
		err := remote.PushContext(ctx, &git.PushOptions{RemoteName: "origin", RefSpecs: specs, ClientOptions: opts})
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			return nil
		}
		return err
	}
	if err := push(specs); err == nil {
		s.Deleted = len(specs)
		return s, nil
	} else if ctx.Err() != nil {
		return s, err
	}
	// The batch was refused. receive-pack applies each deletion independently,
	// but go-git reports ONE error for the whole push — a single protected ref
	// (a rule this sweep can't predict) would otherwise read as "0 deleted,
	// sweep failed" on every run while the other deletions had in fact
	// happened. Retry per ref: each push re-lists the advertisement, so refs
	// the batch DID delete resolve to already-up-to-date successes, and the
	// error confines itself to the refs the forge actually refused. A run of
	// consecutive refusals means a blanket rule (every retry would fail the
	// same way) — stop burning a round-trip per ref on it.
	var errs []error
	consecutive := 0
	for i, spec := range specs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := push([]config.RefSpec{spec}); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", strings.TrimPrefix(string(spec), ":"), err))
			if consecutive++; consecutive >= 8 {
				errs = append(errs, fmt.Errorf("%d refusals in a row looks like a blanket rule; %d refs left untried", consecutive, len(specs)-i-1))
				break
			}
			continue
		}
		consecutive = 0
		s.Deleted++
	}
	return s, errors.Join(errs...)
}
