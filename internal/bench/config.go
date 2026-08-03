// Package bench is the forgemark benchmark engine: it drives real git
// push/clone operations against one target forge and reports per-level
// results. Frontends (the CLI, and any interactive UI) are thin layers over
// Runner; they observe a run live through the exported Sink.
package bench

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
)

// Target is one forge to benchmark: where to connect and how to authenticate.
// Supplying TokenURL or Jurisdiction selects the entiredb path (jurisdiction
// token exchange + node discovery); they mean nothing to any other forge, so
// their presence is the signal — no separate target-kind field.
type Target struct {
	Name      string   // display label; empty defaults to the remote base URL
	Remote    string   // base URL of the forge, e.g. https://gitlab.com
	Repos     []string // repo paths appended to Remote verbatim
	User      string   // basic-auth username (default x-access-token; token forges ignore it)
	Secret    string   `json:"-"` // credential secret (forge token, or the entiredb subject token); json:"-" so no marshal path can ever leak it
	Insecure  bool     // skip TLS verification
	ObjectFmt string   // "auto" | "sha1" | "sha256"

	// entiredb
	TokenURL     string // core /oauth/token endpoint (selects entiredb)
	Jurisdiction string // jurisdiction audience host, bare origin (selects entiredb)
	ClientID     string // public OAuth client id; empty defaults to entire-cli
}

// Hard safety caps, not tuning suggestions: a single host can't generate a
// meaningful benchmark anywhere near them, and unbounded values from the
// server API would otherwise reach make([]*agent, c), c goroutine spawns,
// and per-sample retention for the whole window — panic/OOM territory.
const (
	maxConcurrency = 4096          // agents per level
	maxLevels      = 16            // levels per sweep
	maxDuration    = 6 * time.Hour // measured window per level
	maxWarmup      = time.Hour     // warm-up per level

	// Commit content is the same class of API-reachable allocation: FileSize
	// goes straight into make([]byte, FileSize) in every agent (a near-MaxInt
	// value is a runtime panic that kills the whole process, not just the
	// run), and FilesMax × FileSize is the working set every agent keeps in
	// its in-memory worktree.
	maxFilesMax    = 1024     // files per commit
	maxFileSize    = 16 << 20 // bytes per file
	maxCommitBytes = 64 << 20 // files-max × file-size, one commit's payload
)

// Workload is the target-independent shape of a run: strategy, sweep, and
// commit content. One Workload is shared by every target of a comparison run.
type Workload struct {
	RunID        string
	Strategy     string // branch | repo | clone | session
	BranchPrefix string
	Concurrency  []int
	Duration     time.Duration
	Warmup       time.Duration
	Commit       CommitConfig

	// session/clone strategies
	SessionCommits int
	CloneDepth     int
	BaseRef        string
}

// Validate applies the target-independent checks that parseFlags used to do,
// so the server rejects a bad workload with the same errors the CLI reports.
func (w Workload) Validate() error {
	switch w.Strategy {
	case "branch", "repo", "clone", "session":
	default:
		return fmt.Errorf("invalid -strategy %q (branch|repo|clone|session)", w.Strategy)
	}
	if w.Strategy == "session" && w.SessionCommits < 1 {
		return errors.New("-session-commits must be >= 1")
	}
	if w.CloneDepth < 0 {
		return errors.New("-clone-depth must be >= 0 (0 = full history)")
	}
	if len(w.Concurrency) == 0 {
		return errors.New("no concurrency levels")
	}
	if len(w.Concurrency) > maxLevels {
		return fmt.Errorf("too many concurrency levels: %d (max %d)", len(w.Concurrency), maxLevels)
	}
	for _, n := range w.Concurrency {
		if n < 1 || n > maxConcurrency {
			return fmt.Errorf("invalid concurrency %d (must be 1..%d)", n, maxConcurrency)
		}
	}
	if w.Duration <= 0 || w.Duration > maxDuration {
		return fmt.Errorf("-duration must be > 0 and <= %s", maxDuration)
	}
	if w.Warmup < 0 || w.Warmup > maxWarmup {
		return fmt.Errorf("-warmup must be >= 0 and <= %s", maxWarmup)
	}
	if w.Strategy != "clone" {
		if w.Commit.FilesMin < 1 {
			return errors.New("-files-min must be >= 1 (a commit needs a change to push)")
		}
		if w.Commit.FilesMax < w.Commit.FilesMin {
			return errors.New("-files-max < -files-min")
		}
		if w.Commit.FilesMax > maxFilesMax {
			return fmt.Errorf("-files-max must be <= %d", maxFilesMax)
		}
		if w.Commit.FileSize < 1 {
			return errors.New("-file-size must be >= 1 (empty blobs reproduce → ErrEmptyCommit)")
		}
		if w.Commit.FileSize > maxFileSize {
			return fmt.Errorf("-file-size must be <= %d (%dMiB)", maxFileSize, maxFileSize>>20)
		}
		// Both factors already fit comfortably in int64, so the product can't
		// overflow this aggregate check.
		if int64(w.Commit.FilesMax)*int64(w.Commit.FileSize) > maxCommitBytes {
			return fmt.Errorf("-files-max × -file-size must be <= %dMiB per commit", maxCommitBytes>>20)
		}
	}
	// Validate the assembled ref, not the prefix alone: validity is context-dependent
	// (a trailing "/" or bare word is fine mid-ref, invalid standalone). c/a are arbitrary —
	// only the prefix can invalidate it — so this one parse-time check covers the whole sweep.
	runID := w.RunID
	if runID == "" {
		runID = "fmX"
	}
	if err := plumbing.ReferenceName(DestRef(w.BranchPrefix, runID, 1, 0)).Validate(); err != nil {
		return fmt.Errorf("invalid -branch-prefix %q: %w", w.BranchPrefix, err)
	}
	return nil
}

// CommitDesc describes the commit content shape, e.g. "1-10 x 2048B".
func (w Workload) CommitDesc() string {
	return fmt.Sprintf("%d-%d x %dB", w.Commit.FilesMin, w.Commit.FilesMax, w.Commit.FileSize)
}

// NewRunID returns a fresh run identifier. Nanosecond resolution so two runs
// started back-to-back (a server restarting a sweep) can't collide.
func NewRunID() string {
	return "fm" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// DestRef builds an agent's destination branch ref, prepending branchPrefix
// verbatim before the run ID. Empty prefix reproduces the default name.
func DestRef(branchPrefix, runID string, c, i int) string {
	return fmt.Sprintf("refs/heads/%s%s-c%d-a%d", branchPrefix, runID, c, i)
}

// IsGitHubDotCom reports whether remote points at github.com (so the abuse
// warning + sha1 constraint apply). Any other host — GHES, GitLab, Gitea — is
// just a generic forge.
func IsGitHubDotCom(remote string) bool {
	u, err := url.Parse(remote)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "github.com")
}
