package bench

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
)

// Runner drives one target through concurrency levels of one workload. The
// caller owns the sweep loop: it calls RunLevel once per level, so level
// lifecycle (start/end, the returned LevelResult) is the caller's to observe —
// the engine only reports per-operation samples, via the optional Sink.
type Runner struct {
	target Target
	w      Workload
	creds  credentialProvider
	ep     *endpoint
	httpc  *http.Client
	sink   Sink
}

// NewRunner resolves a target (credentials + endpoint; for entiredb this
// probes the cluster) and validates the workload against it. sink may be nil.
func NewRunner(ctx context.Context, t Target, w Workload, sink Sink) (*Runner, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	if len(t.Repos) == 0 {
		return nil, errors.New("no repos: pass -repos or -repo-pattern + -repo-count")
	}
	if w.Strategy == "repo" && len(t.Repos) < 2 {
		return nil, errors.New("strategy=repo needs >= 2 repos")
	}
	if t.Secret == "" {
		return nil, errors.New("no credential secret provided")
	}
	httpc := newHTTPClient(t.Insecure, slices.Max(w.Concurrency)+8)

	var (
		creds credentialProvider
		ep    *endpoint
		err   error
	)
	if t.TokenURL != "" || t.Jurisdiction != "" {
		creds, ep, err = setupEntiredb(ctx, t, httpc)
	} else {
		creds, ep, err = setupGeneric(t)
	}
	if err != nil {
		return nil, err
	}
	return &Runner{target: t, w: w, creds: creds, ep: ep, httpc: httpc, sink: sink}, nil
}

// Label is the resolved endpoint label (the remote base URL), for banners and
// results.
func (r *Runner) Label() string { return r.ep.label }

// Nodes is how many push destinations the endpoint fans out across (1 for any
// plain forge; the discovered replica count for entiredb).
func (r *Runner) Nodes() int { return len(r.ep.nodes) }

// ObjectFormat is the resolved wire object format ("sha1" or "sha256").
func (r *Runner) ObjectFormat() string { return string(r.ep.objFmt) }

// Close releases the runner's resources once its run is over: it drops idle
// keep-alive connections and severs the runner's reference to the credential
// provider, so the credential (a PAT, or an Entire subject/access token) held
// for the run becomes eligible for garbage collection instead of living on in a
// finished run kept resident in memory. Go strings can't be wiped in place, so
// dropping the reference is the strongest release available.
func (r *Runner) Close() {
	// Stop any background token refresh before dropping the reference, or a
	// detached exchange could keep running (and holding the token) after the run.
	if c, ok := r.creds.(credentialCloser); ok {
		c.close()
	}
	if r.httpc != nil {
		r.httpc.CloseIdleConnections()
	}
	r.creds = nil
	r.target.Secret = "" // drop the runner's own copy of the token
}

// credentialCloser is an optional credentialProvider capability: release any
// background refresh machinery and the credentials it holds. staticCreds needs
// nothing; jurisdictionCreds cancels and waits for its refresh goroutine.
type credentialCloser interface{ close() }

// StartBarrier synchronizes the start of one concurrency level across several
// runners: each Arrives once its agents are built, and the coordinator
// releases them together, so target-dependent setup time can't skew the
// measured windows of a comparison run. It tolerates a runner that fails
// before it is ready — that runner still Arrives, so Await can't hang. A nil
// barrier means "run alone, no synchronization" (the CLI's single-target path).
type StartBarrier struct {
	arrived chan struct{}
	release chan struct{}
	n       int
	firedAt time.Time // set before release closes; the shared start instant
}

// NewStartBarrier makes a barrier expecting n participants.
func NewStartBarrier(n int) *StartBarrier {
	return &StartBarrier{arrived: make(chan struct{}, n), release: make(chan struct{}), n: n}
}

// Arrive reports that this participant has finished preparing (or has given up
// and won't Hold). The buffered channel makes it non-blocking.
func (b *StartBarrier) Arrive() { b.arrived <- struct{}{} }

// Hold blocks until the coordinator fires the shared start signal.
func (b *StartBarrier) Hold() { <-b.release }

// Await blocks until every participant has Arrived.
func (b *StartBarrier) Await() {
	for i := 0; i < b.n; i++ {
		<-b.arrived
	}
}

// Fire releases every held participant to begin its timed window together. It
// stamps firedAt before closing release, so every participant and the
// coordinator share one start instant (via FiredAt) rather than each reading
// its own time.Now after being scheduled — which would skew the sample clock
// against the window/dt_ms timestamps. The close is a happens-before edge, so
// the write is visible to every Hold() that returns.
func (b *StartBarrier) Fire() {
	b.firedAt = time.Now()
	close(b.release)
}

// FiredAt is the shared start instant, valid after Fire (for the coordinator) or
// after Hold returns (for a participant).
func (b *StartBarrier) FiredAt() time.Time { return b.firedAt }

// RunLevel runs a single concurrency level: build c agents, wait at the
// barrier so all targets start together, run for warmup+duration, then fold
// the samples into one result. It blocks for the full level window (or until
// ctx is cancelled). barrier may be nil to run without synchronization.
func (r *Runner) RunLevel(ctx context.Context, c int, barrier *StartBarrier) (LevelResult, error) {
	var clone *cloneConfig
	var sess *sessionConfig
	switch r.w.Strategy {
	case "clone":
		clone = &cloneConfig{cloneDepth: r.w.CloneDepth, baseRef: r.w.BaseRef}
	case "session":
		sess = &sessionConfig{
			cloneConfig: cloneConfig{cloneDepth: r.w.CloneDepth, baseRef: r.w.BaseRef},
			commits:     r.w.SessionCommits,
		}
	}
	agents := make([]*agent, c)
	for i := range c {
		repoPath := r.target.Repos[0]
		if r.w.Strategy == "repo" {
			repoPath = r.target.Repos[i%len(r.target.Repos)]
		}
		node := r.ep.nodes[i%len(r.ep.nodes)]
		ref := DestRef(r.w.BranchPrefix, r.w.RunID, c, i)
		a, err := newAgent(i, repoPath, node, ref, r.ep.objFmt, &r.w.Commit, r.creds, r.httpc, clone, sess, r.sink)
		if err != nil {
			if barrier != nil {
				barrier.Arrive() // don't leave the coordinator's Await hanging
			}
			return LevelResult{}, fmt.Errorf("new agent %d: %w", i, err)
		}
		agents[i] = a
	}

	// Agents are built; wait for every target before starting the clock, so the
	// measured window begins at the same instant on each and the comparison is
	// fair regardless of per-target setup time.
	if barrier != nil {
		barrier.Arrive()
		barrier.Hold()
	}

	total := r.w.Warmup + r.w.Duration
	// Origin the sample clock AND the context deadline on the same instant — the
	// barrier's shared fire time when there is one. Using FiredAt for the offset
	// origin but a scheduled-now timeout for the deadline let a target released
	// late run a full `total` from its late start, recording samples past the
	// shared window end that summarize then miscounts against the fixed duration.
	// Deriving the deadline from FiredAt()+total stops every target together.
	// Single-target CLI runs have no barrier and just start now.
	start := time.Now()
	if barrier != nil {
		start = barrier.FiredAt()
	}
	lvlCtx, cancel := context.WithDeadline(ctx, start.Add(total))
	defer cancel()
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func(a *agent) {
			defer wg.Done()
			a.run(lvlCtx, start)
		}(a)
	}
	wg.Wait()

	var all []Sample
	for _, a := range agents {
		all = append(all, a.samples...)
	}
	reposUsed := 1
	if r.w.Strategy == "repo" {
		reposUsed = min(c, len(r.target.Repos))
	}
	return summarize(all, c, r.w.Strategy, reposUsed, len(r.ep.nodes), r.w.Warmup, r.w.Duration, r.w.CommitDesc()), nil
}

func setupGeneric(t Target) (credentialProvider, *endpoint, error) {
	if t.Remote == "" {
		return nil, nil, errors.New("-remote required (e.g. https://gitlab.com); for entiredb also pass -token-url and -jurisdiction")
	}
	if IsGitHubDotCom(t.Remote) && t.ObjectFmt == "sha256" {
		return nil, nil, errors.New("github.com is sha1; drop -object-format sha256")
	}
	objFmt, err := parseObjectFormat(t.ObjectFmt)
	if err != nil {
		return nil, nil, err
	}
	user := t.User
	if user == "" {
		user = "x-access-token" // token forges ignore the username; the token is the password
	}
	return staticCreds{username: user, password: t.Secret}, newGenericEndpoint(t.Remote, objFmt), nil
}

func setupEntiredb(ctx context.Context, t Target, httpc *http.Client) (credentialProvider, *endpoint, error) {
	var missing []string
	if t.Remote == "" {
		missing = append(missing, "-remote (cluster base URL)")
	}
	if t.TokenURL == "" {
		missing = append(missing, "-token-url")
	}
	if t.Jurisdiction == "" {
		missing = append(missing, "-jurisdiction")
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("entire target (selected by -token-url/-jurisdiction) also needs: %s", strings.Join(missing, ", "))
	}
	clientID := t.ClientID
	if clientID == "" {
		clientID = "entire-cli"
	}
	creds := newJurisdictionCreds(httpc, t.TokenURL, t.Jurisdiction, clientID, t.Secret, "token")
	ep, err := newEntireEndpoint(ctx, t.Remote, t.ObjectFmt, t.Repos[0], creds, httpc)
	if err != nil {
		return nil, nil, err
	}
	return creds, ep, nil
}

func parseObjectFormat(s string) (formatcfg.ObjectFormat, error) {
	switch s {
	case "sha256":
		return formatcfg.SHA256, nil
	case "sha1", "auto", "": // generic doesn't probe; auto degrades to sha1
		return formatcfg.SHA1, nil
	default:
		return "", fmt.Errorf("invalid -object-format %q (sha1|sha256|auto)", s)
	}
}

func newHTTPClient(insecure bool, maxConns int) *http.Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        maxConns * 2,
		MaxIdleConnsPerHost: maxConns,
		MaxConnsPerHost:     0, // unbounded: don't queue pushes behind a conn cap
		IdleConnTimeout:     90 * time.Second,
	}
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // load-test opt-in
	}
	return &http.Client{Transport: &uaTransport{base: tr}, Timeout: 120 * time.Second}
}

// uaTransport appends "forgemark" to every request's User-Agent so server logs
// can attribute load-test traffic. Appending keeps go-git's agent string first
// (servers may key protocol behavior on it); requests with no User-Agent get
// plain "forgemark" instead of Go's stdlib default.
type uaTransport struct {
	base *http.Transport
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Per the RoundTripper contract, don't mutate the caller's request.
	req = req.Clone(req.Context())
	if ua := req.Header.Get("User-Agent"); ua != "" {
		req.Header.Set("User-Agent", ua+" forgemark")
	} else {
		req.Header.Set("User-Agent", "forgemark")
	}
	return t.base.RoundTrip(req)
}

// CloseIdleConnections forwards to the underlying transport; without it,
// http.Client.CloseIdleConnections (used by run cleanup) would be a no-op.
func (t *uaTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }
