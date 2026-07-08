// forgemark — a concurrent git-push load generator for any smart-HTTP git forge.
//
// It answers the headline question: how many small pushes/sec can one repo
// sustain under many concurrent writers, and what's the per-push latency
// distribution? It drives REAL pushes through go-git (packfile build + ref
// update), so unlike a plain HTTP load tool it exercises the full receive-pack
// path.
//
// The forge is inferred from the flags, not an explicit -target:
//
//	any smart-HTTP host (GitLab, Gitea, Bitbucket, self-hosted, GHES, github.com)
//	    — push to <-remote>/<repo> (repo path appended verbatim; include a .git
//	    suffix in -repos if the forge needs it) with a static credential from the
//	    environment. A github.com remote additionally gets the abuse-detection
//	    warning above concurrency 16.
//	entiredb — selected by supplying -token-url and -jurisdiction: one
//	    jurisdiction identity token (RFC 8693 exchange, authorized live per push),
//	    direct-to-node push, node discovery via X-Entire-Replicas.
//
// Strategies (-strategy): branch (default; per-agent branches on one repo),
// repo (spread across N repos), session (clone+push loop per agent).
//
// Only ever run this against infrastructure you own or are explicitly
// authorized to load-test.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forgemark: "+err.Error())
		os.Exit(1)
	}
}

type runConfig struct {
	remote       string
	tokenFile    string // path to the credential secret ("-" = stdin); preferred over $ACCESS_TOKEN
	user         string // basic-auth username (default x-access-token; token forges ignore it)
	repos        []string
	strategy     string
	branchPrefix string
	concurrency  []int
	duration     time.Duration
	warmup       time.Duration
	commit       commitConfig
	objectFmt    string
	insecure     bool
	out          string
	runID        string

	// session strategy
	sessionCommits int
	cloneDepth     int
	baseRef        string

	// entiredb: supplying -jurisdiction/-token-url selects the entiredb path
	// (jurisdiction-token exchange + node discovery). Meaningless to any other
	// forge, so their presence is the signal — no separate -target flag.
	tokenURL     string
	jurisdiction string
	clientID     string
}

func run() error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}

	httpc := newHTTPClient(cfg.insecure, maxInt(cfg.concurrency)+8)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	creds, ep, err := setupTarget(ctx, cfg, httpc)
	if err != nil {
		return err
	}

	fmt.Printf("forgemark: target=%s strategy=%s repos=%d nodes=%d object-format=%s commit=%s\n",
		ep.label, cfg.strategy, len(cfg.repos), len(ep.nodes), ep.objFmt, cfg.commitDesc())
	fmt.Printf("           sweep=%v duration=%s warmup=%s\n", cfg.concurrency, cfg.duration, cfg.warmup)
	fmt.Println()

	var results []levelResult
	for _, c := range cfg.concurrency {
		if ctx.Err() != nil {
			break
		}
		res, err := runLevel(ctx, cfg, creds, ep, httpc, c)
		if err != nil {
			return fmt.Errorf("concurrency=%d: %w", c, err)
		}
		results = append(results, res)
		printRow(res)
	}

	return writeResults(cfg, ep, results)
}

// setupTarget builds the credential provider and resolved endpoint, inferring
// the forge from the flags rather than an explicit switch:
//
//   - Supplying -jurisdiction or -token-url selects entiredb (they mean nothing
//     to any other forge). A partial set is a hard error, not a silent fallback.
//   - Otherwise it's a plain smart-HTTP forge with a static credential. If the
//     remote host is github.com, the abuse-detection warning applies — that's
//     the only thing that ever distinguished "github".
func setupTarget(ctx context.Context, cfg *runConfig, httpc *http.Client) (credentialProvider, *endpoint, error) {
	if cfg.tokenURL != "" || cfg.jurisdiction != "" {
		return setupEntiredb(ctx, cfg, httpc)
	}
	return setupGeneric(cfg)
}

func setupGeneric(cfg *runConfig) (credentialProvider, *endpoint, error) {
	if cfg.remote == "" {
		return nil, nil, errors.New("-remote required (e.g. https://gitlab.com); for entiredb also pass -token-url and -jurisdiction")
	}
	github := isGitHubDotCom(cfg.remote)
	if github && cfg.objectFmt == "sha256" {
		return nil, nil, errors.New("github.com is sha1; drop -object-format sha256")
	}
	if github {
		if hi := maxInt(cfg.concurrency); hi > 16 {
			fmt.Fprintf(os.Stderr, "forgemark: warning: concurrency %d against github.com is likely to trip "+
				"secondary rate limits / abuse detection — keep it low (e.g. -concurrency 1,4)\n", hi)
		}
	}
	objFmt, err := parseObjectFormat(cfg.objectFmt)
	if err != nil {
		return nil, nil, err
	}
	secret, err := readSecret(cfg)
	if err != nil {
		return nil, nil, err
	}
	user := cfg.user
	if user == "" {
		user = "x-access-token" // token forges ignore the username; the token is the password
	}
	return staticCreds{username: user, password: secret}, newGenericEndpoint(cfg.remote, objFmt), nil
}

func setupEntiredb(ctx context.Context, cfg *runConfig, httpc *http.Client) (credentialProvider, *endpoint, error) {
	var missing []string
	if cfg.remote == "" {
		missing = append(missing, "-remote (cluster base URL)")
	}
	if cfg.tokenURL == "" {
		missing = append(missing, "-token-url")
	}
	if cfg.jurisdiction == "" {
		missing = append(missing, "-jurisdiction")
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("entire target (selected by -token-url/-jurisdiction) also needs: %s", strings.Join(missing, ", "))
	}
	subject, err := readSecret(cfg) // the subject token to exchange
	if err != nil {
		return nil, nil, err
	}
	creds := newJurisdictionCreds(httpc, cfg.tokenURL, cfg.jurisdiction, cfg.clientID, subject, "token")
	ep, err := newEntireEndpoint(ctx, cfg, creds, httpc)
	if err != nil {
		return nil, nil, err
	}
	return creds, ep, nil
}

// isGitHubDotCom reports whether remote points at github.com (so the abuse
// warning + sha1 constraint apply). Any other host — GHES, GitLab, Gitea — is
// just a generic forge.
func isGitHubDotCom(remote string) bool {
	u, err := url.Parse(remote)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "github.com")
}

// readSecret returns the credential secret from the most secure source
// available: -token-file (a file path, or "-" for stdin) if set, otherwise the
// ACCESS_TOKEN env var. A raw token on argv is intentionally unsupported — it
// would leak via ps(1) and shell history. The secret is the forge access token
// for a plain forge, or the subject token to exchange for Entire.
func readSecret(cfg *runConfig) (string, error) {
	if cfg.tokenFile != "" {
		var (
			b   []byte
			err error
		)
		if cfg.tokenFile == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(cfg.tokenFile)
		}
		if err != nil {
			return "", fmt.Errorf("read -token-file: %w", err)
		}
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
		return "", errors.New("-token-file is empty")
	}
	if t := os.Getenv("ACCESS_TOKEN"); t != "" {
		return t, nil
	}
	return "", errors.New("no credential: pass -token-file <path|-> (recommended) or set ACCESS_TOKEN")
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

// runLevel runs a single concurrency level: spin up c agents, push for
// warmup+duration, then fold their samples into one result.
func runLevel(ctx context.Context, cfg *runConfig, creds credentialProvider, ep *endpoint, httpc *http.Client, c int) (levelResult, error) {
	var sess *sessionConfig
	if cfg.strategy == "session" {
		sess = &sessionConfig{commits: cfg.sessionCommits, cloneDepth: cfg.cloneDepth, baseRef: cfg.baseRef}
	}
	agents := make([]*agent, c)
	for i := range c {
		repoPath := cfg.repos[0]
		if cfg.strategy == "repo" {
			repoPath = cfg.repos[i%len(cfg.repos)]
		}
		node := ep.nodes[i%len(ep.nodes)]
		ref := destRef(cfg.branchPrefix, cfg.runID, c, i)
		a, err := newAgent(i, repoPath, node, ref, ep.objFmt, &cfg.commit, creds, httpc, sess)
		if err != nil {
			return levelResult{}, fmt.Errorf("new agent %d: %w", i, err)
		}
		agents[i] = a
	}

	total := cfg.warmup + cfg.duration
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

	var all []sample
	for _, a := range agents {
		all = append(all, a.samples...)
	}
	reposUsed := 1
	if cfg.strategy == "repo" {
		reposUsed = min(c, len(cfg.repos))
	}
	return summarize(all, c, cfg.strategy, reposUsed, len(ep.nodes), cfg.warmup, cfg.duration, cfg.commitDesc()), nil
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

func (c *runConfig) commitDesc() string {
	return fmt.Sprintf("%d-%d x %dB", c.commit.filesMin, c.commit.filesMax, c.commit.fileSize)
}

func parseFlags() (*runConfig, error) {
	cfg := &runConfig{}
	var reposCSV, pattern, concCSV string
	var repoCount int

	flag.StringVar(&cfg.remote, "remote", "", "base URL of the forge, e.g. https://gitlab.com (repos are appended verbatim as <base>/<repo>); for Entire, the cluster base URL")
	flag.StringVar(&cfg.tokenFile, "token-file", "", "path to a file holding the credential secret (Entire: the subject token); \"-\" reads stdin. Preferred over $ACCESS_TOKEN — keeps the secret off argv")
	flag.StringVar(&cfg.user, "user", "", "basic-auth username for the credential (default x-access-token; token forges ignore it)")
	flag.StringVar(&reposCSV, "repos", "", "comma-separated repo paths, appended to -remote verbatim (e.g. you/bench.git)")
	flag.StringVar(&pattern, "repo-pattern", "", "repo path template; {n} is replaced by the index (1..N), used with -repo-count (e.g. you/bench-{n})")
	flag.IntVar(&repoCount, "repo-count", 0, "number of repos for -repo-pattern (expands {n} = 1..N)")
	flag.StringVar(&cfg.strategy, "strategy", "branch", "branch (one repo, per-agent branches) | repo (spread across repos) | session (clone+push loop per agent)")
	flag.StringVar(&cfg.branchPrefix, "branch-prefix", "", "prefix prepended verbatim to branch names, before the run ID (e.g. bench/ → refs/heads/bench/fm...-c1-a0); empty keeps the default")
	flag.StringVar(&concCSV, "concurrency", "1,8,32,128", "comma-separated writer counts to sweep")
	flag.DurationVar(&cfg.duration, "duration", 60*time.Second, "measured window per concurrency level")
	flag.DurationVar(&cfg.warmup, "warmup", 10*time.Second, "warm-up before measuring (excluded from stats)")
	flag.IntVar(&cfg.commit.filesMin, "files-min", 1, "min changed files per commit")
	flag.IntVar(&cfg.commit.filesMax, "files-max", 10, "max changed files per commit")
	flag.IntVar(&cfg.commit.fileSize, "file-size", 2048, "bytes per changed file")
	flag.StringVar(&cfg.objectFmt, "object-format", "auto", "auto | sha1 | sha256 (auto probes the entiredb advertisement; generic/github default sha1)")
	flag.BoolVar(&cfg.insecure, "insecure", false, "skip TLS verification (dev/self-signed hosts)")
	flag.StringVar(&cfg.out, "out", "", "write JSON results here (default: results/forgemark-<id>.json)")
	flag.IntVar(&cfg.sessionCommits, "session-commits", 5, "session strategy: commit+push checkpoints per cloned session")
	flag.IntVar(&cfg.cloneDepth, "clone-depth", 1, "session strategy: shallow clone depth (1=tip; 0=full history)")
	flag.StringVar(&cfg.baseRef, "base-ref", "", "session strategy: branch to clone — bare name (main) or full ref (refs/heads/main); default: remote default branch")

	// entiredb: presence of -token-url/-jurisdiction selects the entiredb path.
	flag.StringVar(&cfg.tokenURL, "token-url", "", "entiredb: core /oauth/token endpoint, e.g. https://<region>.auth.example.com/oauth/token (selects entiredb)")
	flag.StringVar(&cfg.jurisdiction, "jurisdiction", "", "entiredb: jurisdiction audience host (bare origin), e.g. https://<region>.example.com (selects entiredb)")
	flag.StringVar(&cfg.clientID, "client-id", "entire-cli", "entiredb: public OAuth client id for the exchange")
	flag.Parse()

	repos, err := expandRepos(reposCSV, pattern, repoCount)
	if err != nil {
		return nil, err
	}
	cfg.repos = repos
	if cfg.strategy != "branch" && cfg.strategy != "repo" && cfg.strategy != "session" {
		return nil, fmt.Errorf("invalid -strategy %q (branch|repo|session)", cfg.strategy)
	}
	if cfg.strategy == "repo" && len(cfg.repos) < 2 {
		return nil, errors.New("strategy=repo needs >= 2 repos")
	}
	if cfg.strategy == "session" && cfg.sessionCommits < 1 {
		return nil, errors.New("-session-commits must be >= 1")
	}
	for _, p := range splitCSV(concCSV) {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid concurrency %q", p)
		}
		cfg.concurrency = append(cfg.concurrency, n)
	}
	if len(cfg.concurrency) == 0 {
		return nil, errors.New("no concurrency levels")
	}
	if cfg.commit.filesMin < 1 {
		return nil, errors.New("-files-min must be >= 1 (a commit needs a change to push)")
	}
	if cfg.commit.filesMax < cfg.commit.filesMin {
		return nil, errors.New("-files-max < -files-min")
	}
	if cfg.commit.fileSize < 1 {
		return nil, errors.New("-file-size must be >= 1 (empty blobs reproduce → ErrEmptyCommit)")
	}
	cfg.runID = "fm" + strconv.FormatInt(time.Now().Unix(), 36)
	// Validate the assembled ref, not the prefix alone: validity is context-dependent
	// (a trailing "/" or bare word is fine mid-ref, invalid standalone). c/a are arbitrary —
	// only the prefix can invalidate it — so this one parse-time check covers the whole sweep.
	if err := plumbing.ReferenceName(destRef(cfg.branchPrefix, cfg.runID, 1, 0)).Validate(); err != nil {
		return nil, fmt.Errorf("invalid -branch-prefix %q: %w", cfg.branchPrefix, err)
	}
	return cfg, nil
}

// destRef builds an agent's destination branch ref, prepending branchPrefix
// verbatim before the run ID. Empty prefix reproduces the default name.
func destRef(branchPrefix, runID string, c, i int) string {
	return fmt.Sprintf("refs/heads/%s%s-c%d-a%d", branchPrefix, runID, c, i)
}

// expandRepos builds the target repo list from the mutually-exclusive
// -repos / -repo-pattern flags. Paths are used verbatim downstream (the
// endpoint appends them to the node with no rewriting), so this only validates
// and expands {n}; it never rewrites the path shape.
func expandRepos(reposCSV, pattern string, count int) ([]string, error) {
	repos := splitCSV(reposCSV)
	switch {
	case len(repos) > 0 && pattern != "":
		return nil, errors.New("pass -repos or -repo-pattern, not both")
	case len(repos) > 0:
		if count > 0 {
			return nil, errors.New("-repo-count applies to -repo-pattern, not -repos")
		}
		return repos, nil
	case pattern != "":
		if count <= 0 {
			return nil, errors.New("-repo-pattern requires -repo-count > 0")
		}
		if !strings.Contains(pattern, "{n}") {
			return nil, errors.New("-repo-pattern must contain the {n} index placeholder (e.g. you/bench-{n})")
		}
		for i := 1; i <= count; i++ {
			repos = append(repos, strings.ReplaceAll(pattern, "{n}", strconv.Itoa(i)))
		}
		return repos, nil
	case count > 0:
		return nil, errors.New("-repo-count requires -repo-pattern")
	default:
		return nil, errors.New("no repos: pass -repos or -repo-pattern + -repo-count")
	}
}

func printRow(r levelResult) {
	fmt.Printf("  c=%-4d push_ok=%-6d push/s=%-8.1f p50=%-7.1f p95=%-8.1f p99=%-8.1f max=%-8.1f cas=%d err=%d\n",
		r.Concurrency, r.OK, r.OpsPerSec, r.P50ms, r.P95ms, r.P99ms, r.Maxms, r.CASFailures, r.OtherErrors)
	if r.Clones > 0 {
		fmt.Printf("        clones=%-5d clone_ok=%-5d clone_p50=%-7.1f clone_p95=%-8.1f clone_p99=%-8.1f clone_err=%d\n",
			r.Clones, r.CloneOK, r.CloneP50ms, r.CloneP95ms, r.CloneP99ms, r.CloneErrors)
	}
}

func writeResults(cfg *runConfig, ep *endpoint, results []levelResult) error {
	out := cfg.out
	if out == "" {
		if err := os.MkdirAll("results", 0o750); err != nil {
			return fmt.Errorf("mkdir results: %w", err)
		}
		out = fmt.Sprintf("results/forgemark-%s.json", cfg.runID)
	}
	doc := map[string]any{
		"run_id":     cfg.runID,
		"target":     ep.label,
		"strategy":   cfg.strategy,
		"duration":   cfg.duration.String(),
		"warmup":     cfg.warmup.String(),
		"repo_count": len(cfg.repos),
		"commit":     cfg.commitDesc(),
		"levels":     results,
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal results: %w", err)
	}
	if err := os.WriteFile(out, b, 0o600); err != nil {
		return fmt.Errorf("write results: %w", err)
	}
	fmt.Printf("\nwrote %s\n", out)
	return nil
}

func maxInt(xs []int) int {
	m := 0
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}
