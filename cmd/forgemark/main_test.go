package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/forgemark/internal/bench"
)

func TestExpandRepos(t *testing.T) {
	tests := []struct {
		name     string
		reposCSV string
		pattern  string
		count    int
		want     []string
		wantErr  bool
	}{
		{name: "repos passthrough", reposCSV: "a/b,c/d", want: []string{"a/b", "c/d"}},
		{name: "pattern expands 1..N", pattern: "org/bench-{n}", count: 3,
			want: []string{"org/bench-1", "org/bench-2", "org/bench-3"}},
		{name: "pattern repeats {n}", pattern: "grp-{n}/bench-{n}", count: 2,
			want: []string{"grp-1/bench-1", "grp-2/bench-2"}},
		{name: "pattern keeps verbatim suffix", pattern: "you/bench-{n}.git", count: 2,
			want: []string{"you/bench-1.git", "you/bench-2.git"}},
		{name: "missing {n}", pattern: "bench-", count: 3, wantErr: true},
		{name: "pattern without count", pattern: "bench-{n}", count: 0, wantErr: true},
		{name: "count without pattern", count: 3, wantErr: true},
		{name: "repos plus count", reposCSV: "a/b", count: 3, wantErr: true},
		{name: "both repos and pattern", reposCSV: "a/b", pattern: "c-{n}", count: 2, wantErr: true},
		{name: "neither", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandRepos(tt.reposCSV, tt.pattern, tt.count)
			if (err != nil) != tt.wantErr {
				t.Fatalf("expandRepos(%q,%q,%d) err = %v, wantErr %v", tt.reposCSV, tt.pattern, tt.count, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("expandRepos(%q,%q,%d) = %v, want %v", tt.reposCSV, tt.pattern, tt.count, got, tt.want)
			}
		})
	}
}

func TestParseFlagsCloneStrategyIgnoresCommitShape(t *testing.T) {
	cfg, err := parseFlagsForTest(t,
		"-strategy", "clone",
		"-repos", "org/repo",
		"-clone-depth", "3",
		"-base-ref", "main",
		"-files-min", "10",
		"-files-max", "1",
		"-file-size", "0",
	)
	if err != nil {
		t.Fatalf("parseFlags clone = %v, want commit-shape flags ignored", err)
	}
	if cfg.workload.Strategy != "clone" {
		t.Fatalf("strategy = %q, want clone", cfg.workload.Strategy)
	}
	if cfg.workload.CloneDepth != 3 || cfg.workload.BaseRef != "main" {
		t.Fatalf("clone knobs = depth %d base %q, want 3/main", cfg.workload.CloneDepth, cfg.workload.BaseRef)
	}
	if !reflect.DeepEqual(cfg.target.Repos, []string{"org/repo"}) {
		t.Fatalf("repos = %v, want [org/repo]", cfg.target.Repos)
	}
}

func TestParseFlagsCloneStrategyNeedsRepos(t *testing.T) {
	_, err := parseFlagsForTest(t, "-strategy", "clone")
	if err == nil || !strings.Contains(err.Error(), "no repos") {
		t.Fatalf("parseFlags clone without repos err = %v, want no repos error", err)
	}
}

func TestParseFlagsCommitShapeStillValidatedForPushStrategies(t *testing.T) {
	_, err := parseFlagsForTest(t, "-strategy", "branch", "-repos", "org/repo", "-files-min", "0")
	if err == nil || !strings.Contains(err.Error(), "-files-min") {
		t.Fatalf("parseFlags branch with -files-min 0 err = %v, want -files-min validation", err)
	}
}

// The CLI must keep writing the legacy format-1 document (no "format" field,
// a single "target" plus "repo_count" and "levels") so existing tooling and
// internal/results.Load keep reading its output after the engine refactor.
func TestWriteResultsEmitsLegacyFormat(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.json")
	cfg := &cliConfig{out: out}
	cfg.workload.RunID = "fmtest"
	cfg.workload.Strategy = "branch"
	cfg.workload.Duration = 60 * time.Second
	cfg.workload.Warmup = 10 * time.Second
	cfg.workload.Commit = bench.CommitConfig{FilesMin: 1, FilesMax: 10, FileSize: 2048}
	cfg.target.Repos = []string{"org/repo"}
	levels := []bench.LevelResult{{Concurrency: 4, OK: 12, OpsPerSec: 2}}

	if err := writeResults(cfg, "https://git.example", levels); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["format"]; ok {
		t.Errorf("legacy doc must not carry a format field, got %v", m["format"])
	}
	for _, k := range []string{"run_id", "target", "strategy", "duration", "warmup", "repo_count", "commit", "levels"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing legacy field %q", k)
		}
	}
	if m["target"] != "https://git.example" || m["run_id"] != "fmtest" {
		t.Errorf("target/run_id = %v/%v, want https://git.example/fmtest", m["target"], m["run_id"])
	}
}

func parseFlagsForTest(t *testing.T, args ...string) (*cliConfig, error) {
	t.Helper()
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine
	fs := flag.NewFlagSet("forgemark", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flag.CommandLine = fs
	os.Args = append([]string{"forgemark"}, args...)
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
	})
	return parseFlags()
}
