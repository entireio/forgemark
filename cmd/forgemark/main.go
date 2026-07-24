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
// repo (spread across N repos), clone (clone-only read loop), session
// (clone+push loop per agent).
//
// The benchmark engine itself lives in internal/bench; this package is the
// flag-driven CLI over it, and `forgemark serve` is the web demo GUI over the
// same engine.
//
// Only ever run this against infrastructure you own or are explicitly
// authorized to load-test.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/entireio/forgemark/internal/bench"
	"github.com/entireio/forgemark/internal/results"
)

func main() {
	// `forgemark serve` starts the web demo GUI; anything else is the classic
	// flag-driven CLI (which takes no positional args, so this can't collide).
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "forgemark: "+err.Error())
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "forgemark: "+err.Error())
		os.Exit(1)
	}
}

// cliConfig is the parsed flag set: the single benchmark target, the shared
// workload shape, and CLI-only concerns (where the secret comes from, where
// results go).
type cliConfig struct {
	target    bench.Target
	workload  bench.Workload
	tokenFile string // path to the credential secret ("-" = stdin); preferred over $ACCESS_TOKEN
	out       string
}

func run() error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}

	secret, err := readSecret(cfg.tokenFile)
	if err != nil {
		return err
	}
	cfg.target.Secret = secret

	// github.com throttles high-volume writes; warn on stderr before a sweep
	// that's likely to trip its abuse detection.
	if cfg.target.TokenURL == "" && cfg.target.Jurisdiction == "" && bench.IsGitHubDotCom(cfg.target.Remote) {
		if hi := slices.Max(cfg.workload.Concurrency); hi > 16 {
			fmt.Fprintf(os.Stderr, "forgemark: warning: concurrency %d against github.com is likely to trip "+
				"secondary rate limits / abuse detection — keep it low (e.g. -concurrency 1,4)\n", hi)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	r, err := bench.NewRunner(ctx, cfg.target, cfg.workload, nil)
	if err != nil {
		return err
	}

	fmt.Printf("forgemark: target=%s strategy=%s repos=%d nodes=%d object-format=%s commit=%s\n",
		r.Label(), cfg.workload.Strategy, len(cfg.target.Repos), r.Nodes(), r.ObjectFormat(), cfg.workload.CommitDesc())
	fmt.Printf("           sweep=%v duration=%s warmup=%s\n", cfg.workload.Concurrency, cfg.workload.Duration, cfg.workload.Warmup)
	fmt.Println()

	var results []bench.LevelResult
	for _, c := range cfg.workload.Concurrency {
		if ctx.Err() != nil {
			break
		}
		res, err := r.RunLevel(ctx, c)
		if err != nil {
			return fmt.Errorf("concurrency=%d: %w", c, err)
		}
		results = append(results, res)
		printRow(res)
	}

	return writeResults(cfg, r.Label(), results)
}

// readSecret returns the credential secret from the most secure source
// available: -token-file (a file path, or "-" for stdin) if set, otherwise the
// ACCESS_TOKEN env var. A raw token on argv is intentionally unsupported — it
// would leak via ps(1) and shell history. The secret is the forge access token
// for a plain forge, or the subject token to exchange for Entire.
func readSecret(tokenFile string) (string, error) {
	if tokenFile != "" {
		var (
			b   []byte
			err error
		)
		if tokenFile == "-" {
			b, err = io.ReadAll(os.Stdin)
		} else {
			b, err = os.ReadFile(tokenFile)
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

func parseFlags() (*cliConfig, error) {
	cfg := &cliConfig{}
	var reposCSV, pattern, concCSV string
	var repoCount int
	var durationFlag, warmupFlag time.Duration

	flag.StringVar(&cfg.target.Remote, "remote", "", "base URL of the forge, e.g. https://gitlab.com (repos are appended verbatim as <base>/<repo>); for Entire, the cluster base URL")
	flag.StringVar(&cfg.tokenFile, "token-file", "", "path to a file holding the credential secret (Entire: the subject token); \"-\" reads stdin. Preferred over $ACCESS_TOKEN — keeps the secret off argv")
	flag.StringVar(&cfg.target.User, "user", "", "basic-auth username for the credential (default x-access-token; token forges ignore it)")
	flag.StringVar(&reposCSV, "repos", "", "comma-separated repo paths, appended to -remote verbatim (e.g. you/bench.git)")
	flag.StringVar(&pattern, "repo-pattern", "", "repo path template; {n} is replaced by the index (1..N), used with -repo-count (e.g. you/bench-{n})")
	flag.IntVar(&repoCount, "repo-count", 0, "number of repos for -repo-pattern (expands {n} = 1..N)")
	flag.StringVar(&cfg.workload.Strategy, "strategy", "branch", "branch (one repo, per-agent branches) | repo (spread across repos) | clone (clone-only read loop) | session (clone+push loop per agent)")
	flag.StringVar(&cfg.workload.BranchPrefix, "branch-prefix", "", "prefix prepended verbatim to branch names, before the run ID (e.g. bench/ → refs/heads/bench/fm...-c1-a0); empty keeps the default")
	flag.StringVar(&concCSV, "concurrency", "1,8,32,128", "comma-separated writer counts to sweep")
	flag.DurationVar(&durationFlag, "duration", 60*time.Second, "measured window per concurrency level")
	flag.DurationVar(&warmupFlag, "warmup", 10*time.Second, "warm-up before measuring (excluded from stats)")
	flag.IntVar(&cfg.workload.Commit.FilesMin, "files-min", 1, "min changed files per commit")
	flag.IntVar(&cfg.workload.Commit.FilesMax, "files-max", 10, "max changed files per commit")
	flag.IntVar(&cfg.workload.Commit.FileSize, "file-size", 2048, "bytes per changed file")
	flag.StringVar(&cfg.target.ObjectFmt, "object-format", "auto", "auto | sha1 | sha256 (auto probes the entiredb advertisement; generic/github default sha1)")
	flag.BoolVar(&cfg.target.Insecure, "insecure", false, "skip TLS verification (dev/self-signed hosts)")
	flag.StringVar(&cfg.out, "out", "", "write JSON results here (default: results/forgemark-<id>.json)")
	flag.IntVar(&cfg.workload.SessionCommits, "session-commits", 5, "session strategy: commit+push checkpoints per cloned session")
	flag.IntVar(&cfg.workload.CloneDepth, "clone-depth", 1, "clone/session strategy: shallow clone depth (1=tip; 0=full history)")
	flag.StringVar(&cfg.workload.BaseRef, "base-ref", "", "clone/session strategy: branch to clone — bare name (main) or full ref (refs/heads/main); default: remote default branch")

	// entiredb: presence of -token-url/-jurisdiction selects the entiredb path.
	flag.StringVar(&cfg.target.TokenURL, "token-url", "", "entiredb: core /oauth/token endpoint, e.g. https://<region>.auth.example.com/oauth/token (selects entiredb)")
	flag.StringVar(&cfg.target.Jurisdiction, "jurisdiction", "", "entiredb: jurisdiction audience host (bare origin), e.g. https://<region>.example.com (selects entiredb)")
	flag.StringVar(&cfg.target.ClientID, "client-id", "entire-cli", "entiredb: public OAuth client id for the exchange")
	flag.Parse()

	cfg.workload.Duration = durationFlag
	cfg.workload.Warmup = warmupFlag

	repos, err := expandRepos(reposCSV, pattern, repoCount)
	if err != nil {
		return nil, err
	}
	cfg.target.Repos = repos
	if cfg.workload.Strategy == "repo" && len(repos) < 2 {
		return nil, errors.New("strategy=repo needs >= 2 repos")
	}
	for _, p := range bench.SplitCSV(concCSV) {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("invalid concurrency %q", p)
		}
		cfg.workload.Concurrency = append(cfg.workload.Concurrency, n)
	}
	cfg.workload.RunID = bench.NewRunID()
	if err := cfg.workload.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// expandRepos builds the target repo list from the mutually-exclusive
// -repos / -repo-pattern flags. Paths are used verbatim downstream (the
// endpoint appends them to the node with no rewriting), so this only validates
// and expands {n}; it never rewrites the path shape.
func expandRepos(reposCSV, pattern string, count int) ([]string, error) {
	repos := bench.SplitCSV(reposCSV)
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

func printRow(r bench.LevelResult) {
	if r.Strategy == "clone" {
		fmt.Printf("  c=%-4d clone_ok=%-6d clone/s=%-8.1f p50=%-7.1f p95=%-8.1f p99=%-8.1f max=%-8.1f clone_err=%d\n",
			r.Concurrency, r.OK, r.OpsPerSec, r.P50ms, r.P95ms, r.P99ms, r.Maxms, r.OtherErrors)
		return
	}
	fmt.Printf("  c=%-4d push_ok=%-6d push/s=%-8.1f p50=%-7.1f p95=%-8.1f p99=%-8.1f max=%-8.1f cas=%d err=%d\n",
		r.Concurrency, r.OK, r.OpsPerSec, r.P50ms, r.P95ms, r.P99ms, r.Maxms, r.CASFailures, r.OtherErrors)
	if r.Clones > 0 {
		fmt.Printf("        clones=%-5d clone_ok=%-5d clone_p50=%-7.1f clone_p95=%-8.1f clone_p99=%-8.1f clone_err=%d\n",
			r.Clones, r.CloneOK, r.CloneP50ms, r.CloneP95ms, r.CloneP99ms, r.CloneErrors)
	}
}

// writeResults persists the run as a legacy format-1 doc (the shape the CLI
// has always written) through the results package, so the document schema and
// the parser that history reads it back with live in one place.
func writeResults(cfg *cliConfig, label string, levels []bench.LevelResult) error {
	out := cfg.out
	if out == "" {
		out = fmt.Sprintf("results/forgemark-%s.json", cfg.workload.RunID)
	}
	doc := results.Doc{
		RunID:     cfg.workload.RunID,
		Strategy:  cfg.workload.Strategy,
		Duration:  cfg.workload.Duration.String(),
		Warmup:    cfg.workload.Warmup.String(),
		Commit:    cfg.workload.CommitDesc(),
		Target:    label,
		RepoCount: len(cfg.target.Repos),
		Levels:    levels,
	}
	if err := results.Save(out, doc); err != nil {
		return err
	}
	fmt.Printf("\nwrote %s\n", out)
	return nil
}
