package bench

import (
	"context"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gitHTTPRepo serves one bare repo over smart HTTP through the system git's
// http-backend, so the sweep is exercised against a real receive-pack rather
// than a canned advertisement: ref deletion, the default-branch refusal and
// the advertisement order all come from git itself. The repo is seeded with a
// root commit on main; every other ref the test wants is pointed at it.
type gitHTTPRepo struct {
	t      *testing.T
	dir    string // the bare repo
	url    string // the server base URL; the repo is <url>/repo.git
	commit string
}

func newGitHTTPRepo(t *testing.T) *gitHTTPRepo {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	execPath := strings.TrimSpace(runGit(t, "", "--exec-path"))
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not found at %s", backend)
	}
	root := t.TempDir()
	r := &gitHTTPRepo{t: t, dir: filepath.Join(root, "repo.git")}
	runGit(t, "", "init", "--quiet", "--bare", r.dir)
	runGit(t, r.dir, "symbolic-ref", "HEAD", "refs/heads/main")
	// Anonymous pushes are refused by default; this repo exists to be swept.
	runGit(t, r.dir, "config", "http.receivepack", "true")
	tree := strings.TrimSpace(runGit(t, r.dir, "mktree"))
	r.commit = strings.TrimSpace(runGit(t, r.dir, "commit-tree", tree, "-m", "base"))
	r.set("refs/heads/main")

	srv := httptest.NewServer(&cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "PATH=" + filepath.Dir(gitBin)},
	})
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

// set points ref at the root commit.
func (r *gitHTTPRepo) set(ref string) { runGit(r.t, r.dir, "update-ref", ref, r.commit) }

// refs lists the repo's refs in git's own (sorted) order.
func (r *gitHTTPRepo) refs() []string {
	out := strings.TrimSpace(runGit(r.t, r.dir, "for-each-ref", "--format=%(refname)"))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=forgemark", "GIT_AUTHOR_EMAIL=forgemark@test.invalid",
		"GIT_COMMITTER_NAME=forgemark", "GIT_COMMITTER_EMAIL=forgemark@test.invalid",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func runIDAt(at time.Time) string { return "fm" + strconv.FormatInt(at.UnixNano(), 36) }

// The standalone sweep and the in-run sweep share one selection: exact
// forgemark-shaped refs under the namespace + prefix (legacy refs/heads/
// included), nothing else. They differ only in the age cutoff and in whether
// anything is deleted — a dry run must list the same refs a real sweep would
// remove and leave the remote untouched, and a zero min-age must reach the
// refs the week-long in-run guard spares.
func TestSweepBenchRefsEndToEnd(t *testing.T) {
	repo := newGitHTTPRepo(t)
	oldStale := "refs/forgemark/bench/" + runIDAt(time.Now().Add(-2*staleAfter)) + "-c1-a0"
	fresh := "refs/forgemark/bench/" + runIDAt(time.Now().Add(-time.Hour)) + "-c1-a1"
	// Pushed by a generator whose clock runs ahead of this one: min-age 0
	// must still reach it.
	skewed := "refs/forgemark/bench/" + runIDAt(time.Now().Add(10*time.Minute)) + "-c1-a2"
	legacy := "refs/heads/bench/" + runIDAt(time.Now().Add(-2*staleAfter)) + "-c1-a0"
	otherPrefix := "refs/forgemark/other/" + runIDAt(time.Now().Add(-2*staleAfter)) + "-c1-a0"
	human := "refs/forgemark/bench/keep-me"
	for _, ref := range []string{oldStale, fresh, skewed, legacy, otherPrefix, human} {
		repo.set(ref)
	}
	before := repo.refs()

	ctx := context.Background()
	r, err := NewRunner(ctx, Target{Remote: repo.url, Repos: []string{"repo.git"}, Secret: "token"},
		SweepWorkload("", "bench/"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	want := func(got []RepoSweep, refs []string, deleted int) {
		t.Helper()
		if len(got) != 1 || got[0].Repo != "repo.git" {
			t.Fatalf("sweeps = %+v, want one for repo.git", got)
		}
		gotRefs := slices.Clone(got[0].Refs)
		slices.Sort(gotRefs)
		slices.Sort(refs)
		if !slices.Equal(gotRefs, refs) {
			t.Errorf("selected refs = %v, want %v", gotRefs, refs)
		}
		if got[0].Deleted != deleted {
			t.Errorf("deleted = %d, want %d", got[0].Deleted, deleted)
		}
	}

	// Dry run at min-age 0: every bench ref under the prefix is selected,
	// the human ref and the other prefix are not, and nothing changes.
	got, err := r.SweepBenchRefs(ctx, SweepOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	want(got, []string{oldStale, fresh, skewed, legacy}, 0)
	if after := repo.refs(); !slices.Equal(after, before) {
		t.Fatalf("dry run changed the remote: before %v, after %v", before, after)
	}

	// The in-run sweep applies the week-long guard: the hour-old and
	// future-stamped refs stay.
	n, err := r.CleanStaleBenchRefs(ctx)
	if err != nil {
		t.Fatalf("CleanStaleBenchRefs: %v", err)
	}
	if n != 2 {
		t.Errorf("CleanStaleBenchRefs deleted %d, want 2", n)
	}
	if after := repo.refs(); slices.Contains(after, oldStale) || slices.Contains(after, legacy) || !slices.Contains(after, fresh) || !slices.Contains(after, skewed) {
		t.Fatalf("in-run sweep left %v", after)
	}

	// The standalone sweep at min-age 0 reaches what the guard spared.
	got, err = r.SweepBenchRefs(ctx, SweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	want(got, []string{fresh, skewed}, 2)
	if after := repo.refs(); !slices.Equal(after, []string{human, otherPrefix, "refs/heads/main"}) {
		t.Fatalf("sweep left %v, want only the human ref, the other prefix and main", after)
	}

	// Nothing left to select: a clean report, no push.
	got, err = r.SweepBenchRefs(ctx, SweepOptions{})
	if err != nil {
		t.Fatalf("empty sweep: %v", err)
	}
	want(got, nil, 0)
}

// -min-age is an operator-set guard, so it must honor a real window too, and
// a negative value is a mistake to refuse rather than a cutoff in the future
// that would select every ref including ones from runs not yet started.
func TestSweepBenchRefsMinAge(t *testing.T) {
	repo := newGitHTTPRepo(t)
	old := "refs/forgemark/" + runIDAt(time.Now().Add(-48*time.Hour)) + "-c4-a2"
	recent := "refs/forgemark/" + runIDAt(time.Now().Add(-time.Minute)) + "-c4-a3"
	repo.set(old)
	repo.set(recent)

	ctx := context.Background()
	r, err := NewRunner(ctx, Target{Remote: repo.url, Repos: []string{"repo.git"}, Secret: "token"},
		SweepWorkload("", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, err := r.SweepBenchRefs(ctx, SweepOptions{MinAge: -time.Second}); err == nil {
		t.Fatal("negative min age accepted")
	}
	got, err := r.SweepBenchRefs(ctx, SweepOptions{MinAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !slices.Equal(got[0].Refs, []string{old}) || got[0].Deleted != 1 {
		t.Fatalf("min-age 24h sweep = %+v, want only %s deleted", got, old)
	}
	if after := repo.refs(); !slices.Equal(after, []string{recent, "refs/heads/main"}) {
		t.Fatalf("remote after min-age sweep = %v", after)
	}
}
