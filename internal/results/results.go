// Package results reads and writes forgemark result documents. Two formats
// exist on disk in the same results/ directory:
//
//	format 1 (legacy, no "format" field): what the CLI has always written —
//	    one target, top-level "target"/"levels" fields. The CLI keeps writing
//	    this shape for strict compatibility with existing tooling.
//	format 2: what the server writes — multi-target, per-target level results
//	    plus the per-second live series so a finished run re-renders fully.
//
// Load normalizes format 1 into a one-target format 2 view, so consumers (the
// history UI) only ever see one shape.
package results

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/entireio/forgemark/internal/bench"
)

// Doc is a stored run. Exactly one of the two shapes is populated on disk;
// Load always returns the Targets shape.
type Doc struct {
	Format   int    `json:"format,omitempty"` // 2; absent/0 = legacy CLI doc
	RunID    string `json:"run_id"`
	Strategy string `json:"strategy"`
	Duration string `json:"duration"`
	Warmup   string `json:"warmup"`
	Commit   string `json:"commit"`

	// format 2
	State   string         `json:"state,omitempty"` // done | cancelled | failed
	Targets []TargetResult `json:"targets,omitempty"`

	// format 1 (legacy CLI)
	Target    string              `json:"target,omitempty"`
	RepoCount int                 `json:"repo_count,omitempty"`
	Levels    []bench.LevelResult `json:"levels,omitempty"`
}

// TargetResult is one target's outcome within a run.
type TargetResult struct {
	Name   string              `json:"name"`
	Label  string              `json:"label,omitempty"`
	Error  string              `json:"error,omitempty"` // fatal target error, if it died
	Levels []bench.LevelResult `json:"levels"`
	Series []SeriesPoint       `json:"series,omitempty"` // 1s live buckets, for re-rendering
	// SeriesTruncated marks a Series that hit the server's retention cap: the
	// run kept measuring (Levels is complete) but the stored timeline is not.
	SeriesTruncated bool `json:"series_truncated,omitempty"`
}

// SeriesPoint is one second of one target's live stats. Push and clone ops
// are kept separately, mirroring the live bucket event, so clone/session runs
// replay from history with the same fidelity they streamed with.
type SeriesPoint struct {
	T     int64   `json:"t"`
	DtMs  int64   `json:"dt_ms,omitempty"` // measured duration; writers emit >= 1 (zero-overlap ticks are skipped), so 0/absent occurs only in legacy docs → treat as ~1s
	Level int     `json:"level"`
	OK    int     `json:"ok"`
	CAS   int     `json:"cas"`
	Err   int     `json:"err"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`

	CloneOK  int     `json:"clone_ok,omitempty"`
	CloneErr int     `json:"clone_err,omitempty"`
	CloneP50 float64 `json:"clone_p50_ms,omitempty"`
	CloneP95 float64 `json:"clone_p95_ms,omitempty"`
	CloneP99 float64 `json:"clone_p99_ms,omitempty"`
}

// Summary is one history listing entry.
type Summary struct {
	File     string    `json:"file"`
	RunID    string    `json:"run_id"`
	Strategy string    `json:"strategy"`
	State    string    `json:"state,omitempty"`
	Targets  []string  `json:"targets"`
	Levels   int       `json:"levels"`
	ModTime  time.Time `json:"mtime"`
}

// ErrNotFound reports a missing result file.
var ErrNotFound = errors.New("result file not found")

// validFile is the only filename shape this package will touch. It doubles as
// the path-traversal guard for filenames arriving from HTTP.
var validFile = regexp.MustCompile(`^forgemark-[A-Za-z0-9._-]+\.json$`)

// Write stores a doc as <dir>/<file>, creating dir if needed. The filename
// must match the discoverable forgemark-*.json shape.
func Write(dir, file string, doc Doc) error {
	if !validFile.MatchString(file) {
		return fmt.Errorf("invalid result filename %q", file)
	}
	return Save(filepath.Join(dir, file), doc)
}

// Save stores a doc at an arbitrary path (the CLI's -out escape hatch),
// creating the parent directory if needed. Files outside the forgemark-*.json
// shape won't be discovered by List — that's the caller's tradeoff.
func Save(path string, doc Doc) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal results: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write results: %w", err)
	}
	return nil
}

// List summarizes every parseable result doc in dir, newest first. Unparseable
// files are skipped, not fatal — the directory is shared and hand-editable.
func List(dir string) ([]Summary, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Summary{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Summary{}
	for _, e := range entries {
		if e.IsDir() || !validFile.MatchString(e.Name()) {
			continue
		}
		doc, err := Load(dir, e.Name())
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		s := Summary{
			File: e.Name(), RunID: doc.RunID, Strategy: doc.Strategy,
			State: doc.State, ModTime: info.ModTime(),
		}
		for _, t := range doc.Targets {
			s.Targets = append(s.Targets, t.Name)
			s.Levels = max(s.Levels, len(t.Levels))
		}
		out = append(out, s)
	}
	// Newest first: the history view leads with the latest run.
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// Load reads one doc and normalizes legacy CLI docs into the Targets shape.
func Load(dir, file string) (Doc, error) {
	if !validFile.MatchString(file) {
		return Doc{}, fmt.Errorf("invalid result filename %q", file)
	}
	b, err := os.ReadFile(filepath.Join(dir, file))
	if errors.Is(err, os.ErrNotExist) {
		return Doc{}, ErrNotFound
	}
	if err != nil {
		return Doc{}, err
	}
	var doc Doc
	if err := json.Unmarshal(b, &doc); err != nil {
		return Doc{}, fmt.Errorf("parse %s: %w", file, err)
	}
	if doc.Format == 0 && len(doc.Targets) == 0 {
		// Legacy CLI doc: present it as one pseudo-target named by its label.
		doc.Targets = []TargetResult{{Name: doc.Target, Label: doc.Target, Levels: doc.Levels}}
		doc.State = "done"
	}
	doc.Target, doc.Levels = "", nil // consumers only see the normalized shape
	return doc, nil
}
