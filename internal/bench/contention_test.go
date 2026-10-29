package bench

import (
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-git/go-git/v6/backend"
	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/memory"
)

// serveRepo runs one in-memory repo over smart HTTP and returns the server,
// its client, and the storer so a test can inspect server-side refs after
// pushes land.
func serveRepo(t *testing.T) (*httptest.Server, *http.Client, storage.Storer) {
	t.Helper()
	st := memory.NewStorage()
	h := backend.New(storerLoader{st})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, srv.Client(), st
}

func newContendAgent(t *testing.T, id int, node, ref string, httpClient *http.Client) *agent {
	t.Helper()
	a, err := newAgent(id, "repo.git", node, ref, formatcfg.SHA1,
		&CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 16},
		staticCreds{username: "u", password: "p"}, httpClient, nil, nil, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustHead(t *testing.T, a *agent) plumbing.Hash {
	t.Helper()
	head, err := a.repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head.Hash()
}

// Two contention agents race one ref: the first creates it, the second is
// CAS-rejected, fetches the shared tip, resets onto it, and lands its retry.
// The whole loop is the workload the contention strategy measures.
func TestContentionAgentConvergesAfterCAS(t *testing.T) {
	t.Parallel()

	srv, httpClient, st := serveRepo(t)
	const shared = "refs/forgemark/fmX-c2-a0"
	first := newContendAgent(t, 0, srv.URL, shared, httpClient)
	loser := newContendAgent(t, 1, srv.URL, shared, httpClient)
	rngA := rand.New(rand.NewSource(1))
	rngB := rand.New(rand.NewSource(2))

	// First pusher creates the shared ref.
	if err := first.commit(rngA, 0); err != nil {
		t.Fatal(err)
	}
	if err := first.push(t.Context()); err != nil {
		t.Fatalf("creating the shared ref: %v", err)
	}
	wantTip := mustHead(t, first)

	// The loser's non-forced push onto unrelated history must CAS-reject —
	// that rejection is the signal this strategy exists to measure.
	if err := loser.commit(rngB, 0); err != nil {
		t.Fatal(err)
	}
	casErr := loser.push(t.Context())
	if got := classify(casErr); got != OutcomeCAS {
		t.Fatalf("racing push outcome = %v (err %v), want OutcomeCAS", got, casErr)
	}

	// Converge puts the loser's branch on the shared tip, so its next commit
	// is a child and pushes fast-forward.
	if err := loser.converge(t.Context()); err != nil {
		t.Fatalf("converge after CAS: %v", err)
	}
	if got := mustHead(t, loser); got != wantTip {
		t.Fatalf("local head after converge = %s, want shared tip %s", got, wantTip)
	}
	if err := loser.commit(rngB, 1); err != nil {
		t.Fatal(err)
	}
	if err := loser.push(t.Context()); err != nil {
		t.Fatalf("push after converge must land: %v", err)
	}

	// Server-side the shared ref advanced to the loser's converged commit.
	ref, err := st.Reference(plumbing.ReferenceName(shared))
	if err != nil {
		t.Fatalf("resolve server-side %s: %v", shared, err)
	}
	if got, want := ref.Hash(), mustHead(t, loser); got != want {
		t.Fatalf("server tip = %s, want %s", got, want)
	}
}

// A contention level points every agent at one shared destination ref, built
// as DestRef(ns, prefix, runID, c, 0). That a0 shape must stay inside the
// stale-bench-ref sweep, or every contention level leaks a ref per run.
func TestContentionSharedRefMatchesSweep(t *testing.T) {
	t.Parallel()

	ns := Workload{}.Namespace()
	re := staleBenchRefRe(ns, "")
	if !re.MatchString(DestRef(ns, "", "fm12345678", 4, 0)) {
		t.Fatal("shared contention ref must match the stale-ref sweep pattern")
	}
}
