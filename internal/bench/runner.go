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

// Fire releases every held participant to begin its timed window together.
func (b *StartBarrier) Fire() { close(b.release) }

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
	lvlCtx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	start := time.Now()
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
	return &http.Client{Transport: tr, Timeout: 120 * time.Second}
}
