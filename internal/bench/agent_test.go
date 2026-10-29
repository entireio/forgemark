package bench

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/backend"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/memory"
)

type storerLoader struct{ st storage.Storer }

func (l storerLoader) Load(*url.URL) (storage.Storer, error) { return l.st, nil }

// receivePackBodies serves one in-memory repo over smart HTTP and keeps the
// body of every receive-pack request, where push options travel.
func receivePackBodies(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies []string
	)
	h := backend.New(storerLoader{memory.NewStorage()})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			bodies = append(bodies, string(b))
			mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return bodies
	}
}

func TestAgentPushSendsPushOptions(t *testing.T) {
	t.Parallel()

	srv, bodies := receivePackBodies(t)
	opts := []string{"ci.skip", "merge_request.label=a,b"}
	a, err := newAgent(0, "repo.git", srv.URL, "refs/forgemark/fmX-c1-a0", formatcfg.SHA1,
		&CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 16},
		staticCreds{username: "u", password: "p"}, srv.Client(), opts, nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.commit(rand.New(rand.NewSource(1)), 0); err != nil {
		t.Fatal(err)
	}
	if err := a.push(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.deleteRef(a.ref)

	got := bodies()
	if len(got) != 2 {
		t.Fatalf("server saw %d receive-pack requests, want a push and a delete", len(got))
	}
	for i, body := range got {
		for _, o := range opts {
			if !strings.Contains(body, o) {
				t.Errorf("receive-pack request %d is missing push option %q", i, o)
			}
		}
	}
}

// receivePackAdvertisement is a smart-HTTP receive-pack info/refs reply for an
// empty repo advertising caps.
func receivePackAdvertisement(caps string) string {
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	return pkt("# service=git-receive-pack\n") + "0000" +
		pkt("0000000000000000000000000000000000000000 capabilities^{}\x00"+caps+"\n") + "0000"
}

func TestAdvertisesPushOptions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		caps string
		want bool
	}{
		{"report-status delete-refs push-options", true},
		{"report-status delete-refs", false},
		{"report-status push-optionsx", false},
	} {
		got, err := advertisesPushOptions([]byte(receivePackAdvertisement(tc.caps)))
		if err != nil {
			t.Fatalf("caps %q: %v", tc.caps, err)
		}
		if got != tc.want {
			t.Errorf("caps %q: advertisesPushOptions = %v, want %v", tc.caps, got, tc.want)
		}
	}
	if _, err := advertisesPushOptions([]byte("<html>not git</html>")); err == nil {
		t.Error("a non-git reply must be an error, not a missing capability")
	}
}

// go-git drops push options the server doesn't advertise and the push still
// succeeds, so the run must refuse to start rather than measure pushes that
// differ from the ones requested.
func TestNewRunnerRequiresPushOptionsCapability(t *testing.T) {
	t.Parallel()

	noCaps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		_, _ = io.WriteString(w, receivePackAdvertisement("report-status delete-refs"))
	}))
	defer noCaps.Close()
	withCaps, _ := receivePackBodies(t)

	w := Workload{
		Strategy: "branch", Concurrency: []int{1}, Duration: time.Second,
		Commit: CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1},
	}
	target := func(remote string) Target {
		return Target{Remote: remote, Repos: []string{"repo.git"}, Secret: "s"}
	}

	if r, err := NewRunner(t.Context(), target(noCaps.URL), w, nil); err != nil {
		t.Fatalf("without push options the capability must not be probed: %v", err)
	} else {
		r.Close()
	}

	w.PushOptions = []string{"ci.skip"}
	_, err := NewRunner(t.Context(), target(noCaps.URL), w, nil)
	if err == nil || !strings.Contains(err.Error(), "push-options") {
		t.Fatalf("NewRunner against a server without push-options = %v, want a push-options error", err)
	}

	r, err := NewRunner(t.Context(), target(withCaps.URL), w, nil)
	if err != nil {
		t.Fatalf("NewRunner against a server advertising push-options: %v", err)
	}
	r.Close()
}

func TestAgentFitsUsesObservedLatency(t *testing.T) {
	t.Parallel()

	a := &agent{}
	now := time.Now()
	if !a.fits(OpPush, now.Add(time.Millisecond)) {
		t.Fatal("an agent with no observed latency must start its first op")
	}
	a.record(Sample{Op: OpPush, Dur: 2 * time.Second})
	if a.fits(OpPush, now.Add(time.Second)) {
		t.Fatal("a 2s push must not start with 1s left in the window")
	}
	if !a.fits(OpPush, now.Add(3*time.Second)) {
		t.Fatal("a 2s push must start with 3s left in the window")
	}
	if !a.fits(OpClone, now.Add(time.Second)) {
		t.Fatal("push latency must not gate clones")
	}
}

func TestAgentLatencyEstimateRisesFastAndDecaysSlowly(t *testing.T) {
	t.Parallel()

	a := &agent{}
	a.record(Sample{Op: OpPush, Dur: time.Second})
	a.record(Sample{Op: OpPush, Dur: 3 * time.Second})
	if got := a.est[OpPush]; got != 3*time.Second {
		t.Fatalf("estimate after a slower push = %s, want 3s", got)
	}
	a.record(Sample{Op: OpPush, Dur: time.Second})
	if got := a.est[OpPush]; got != 2500*time.Millisecond {
		t.Fatalf("estimate after a faster push = %s, want 2.5s", got)
	}
	a.record(Sample{Op: OpPush, Res: OutcomeErr})
	if got := a.est[OpPush]; got != 2500*time.Millisecond {
		t.Fatalf("a sample without a duration moved the estimate to %s", got)
	}
}

// A slow server must not see its in-flight requests cancelled when the
// window closes: the agent stops starting pushes it cannot finish in time.
func TestAgentStopsBeforeWindowEnd(t *testing.T) {
	t.Parallel()

	const latency = 200 * time.Millisecond
	var requests, cancelled atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-time.After(latency):
			http.Error(w, "slow", http.StatusInternalServerError)
		case <-r.Context().Done():
			cancelled.Add(1)
		}
	}))

	a, err := newAgent(0, "repo.git", srv.URL, "refs/forgemark/fmX-c1-a0", formatcfg.SHA1,
		&CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 16},
		staticCreds{username: "u", password: "p"}, srv.Client(), nil, nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	end := start.Add(5*latency + latency/2)
	ctx, cancel := context.WithDeadline(context.Background(), end.Add(10*latency))
	defer cancel()
	a.run(ctx, start, end)
	srv.Close()

	if requests.Load() < 3 {
		t.Fatalf("server saw %d requests, want the window to keep the agent busy", requests.Load())
	}
	if n := cancelled.Load(); n != 0 {
		t.Fatalf("%d requests were cancelled at the window end", n)
	}
	for _, s := range a.samples {
		if s.Offset+s.Dur > end.Sub(start)+latency {
			t.Fatalf("sample at %s took %s, running well past the %s window", s.Offset, s.Dur, end.Sub(start))
		}
	}
}

// An op that overruns the window finishes during the drain instead of being
// cancelled, and is recorded: it started inside the window, and dropping it
// would hide real work and tail latency just when the server slows down.
func TestAgentRecordsOpsFinishingDuringDrain(t *testing.T) {
	t.Parallel()

	const latency = 300 * time.Millisecond
	var cancelled atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(latency):
			http.Error(w, "slow", http.StatusInternalServerError)
		case <-r.Context().Done():
			cancelled.Add(1)
		}
	}))

	a, err := newAgent(0, "repo.git", srv.URL, "refs/forgemark/fmX-c1-a0", formatcfg.SHA1,
		&CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 16},
		staticCreds{username: "u", password: "p"}, srv.Client(), nil, nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	end := start.Add(latency / 3)
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(10*latency))
	defer cancel()
	a.run(ctx, start, end)
	srv.Close()

	if n := cancelled.Load(); n != 0 {
		t.Fatalf("%d requests were cancelled", n)
	}
	if len(a.samples) != 1 {
		t.Fatalf("recorded %d samples, want the one op that finished during the drain", len(a.samples))
	}
	if s := a.samples[0]; s.Offset+s.Dur < end.Sub(start) {
		t.Fatalf("sample finished at %s, want it to overrun the %s window", s.Offset+s.Dur, end.Sub(start))
	}
}

// An op cut off by the hard deadline is a cancellation the server sees. It
// is not a sample, but the level reports how many there were, so the effect
// of the drain on server-side cancellations is visible in the results.
func TestAgentCountsCancelledOps(t *testing.T) {
	t.Parallel()

	const latency = 300 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(latency):
			http.Error(w, "slow", http.StatusInternalServerError)
		case <-r.Context().Done():
		}
	}))

	a, err := newAgent(0, "repo.git", srv.URL, "refs/forgemark/fmX-c1-a0", formatcfg.SHA1,
		&CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 16},
		staticCreds{username: "u", password: "p"}, srv.Client(), nil, nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(latency/3))
	defer cancel()
	a.run(ctx, start, start.Add(10*latency))
	srv.Close()

	if a.cancelled != 1 {
		t.Fatalf("cancelled = %d, want the one op cut off by the deadline", a.cancelled)
	}
	if len(a.samples) != 0 {
		t.Fatalf("recorded %d samples for a cancelled op", len(a.samples))
	}
}

// Go forwards Authorization on a redirect to the same host on another port,
// so the probe must refuse a redirect that leaves the configured origin.
func TestPushOptionsProbeRefusesCrossOriginRedirects(t *testing.T) {
	t.Parallel()

	var leaked atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		_, _ = io.WriteString(w, receivePackAdvertisement("report-status push-options"))
	}))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer redirector.Close()

	w := Workload{
		Strategy: "branch", Concurrency: []int{1}, Duration: time.Second,
		Commit:      CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1},
		PushOptions: []string{"ci.skip"},
	}
	_, err := NewRunner(t.Context(), Target{Remote: redirector.URL, Repos: []string{"repo.git"}, Secret: "s"}, w, nil)
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("NewRunner against a redirecting forge = %v, want a redirect refusal", err)
	}
	if leaked.Load() {
		t.Fatal("the probe forwarded the credential across a redirect")
	}
}

// Go forwards Authorization to any port of the same hostname and to
// subdomains, so every request on the generic path, not only the probe, must
// keep the credential on the forge's own origin.
func TestGenericPathKeepsCredentialOnOrigin(t *testing.T) {
	t.Parallel()

	var leaked atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		_, _ = io.WriteString(w, receivePackAdvertisement("report-status delete-refs"))
	}))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	defer redirector.Close()

	w := Workload{
		Strategy: "branch", Concurrency: []int{1}, Duration: time.Second,
		Commit: CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1},
	}
	r, err := NewRunner(t.Context(), Target{Remote: redirector.URL, Repos: []string{"repo.git"}, Secret: "s"}, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.CleanStaleBenchRefs(t.Context()); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("CleanStaleBenchRefs against a cross-origin redirect = %v, want a redirect refusal", err)
	}
	if leaked.Load() {
		t.Fatal("the credential followed a redirect to another origin")
	}
}

// Forges redirect within their own origin (GitLab: repo -> repo.git), and git
// follows that on the initial info/refs request. The probe must accept it too,
// or -push-options fails where `git push -o` works.
func TestPushOptionsProbeFollowsSameOriginRedirect(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repo/info/refs" {
			http.Redirect(w, r, "/repo.git/info/refs?"+r.URL.RawQuery, http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
		_, _ = io.WriteString(w, receivePackAdvertisement("report-status push-options"))
	}))
	defer srv.Close()

	w := Workload{
		Strategy: "branch", Concurrency: []int{1}, Duration: time.Second,
		Commit:      CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1},
		PushOptions: []string{"ci.skip"},
	}
	r, err := NewRunner(t.Context(), Target{Remote: srv.URL, Repos: []string{"repo"}, Secret: "s"}, w, nil)
	if err != nil {
		t.Fatalf("NewRunner against a same-origin redirect: %v", err)
	}
	r.Close()
}
