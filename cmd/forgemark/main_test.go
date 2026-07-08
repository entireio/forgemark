package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
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

func TestDestRef(t *testing.T) {
	// The prefix is prepended verbatim before the run ID; the assembled ref is what
	// parseFlags validates. Readable prefixes pass; typo shapes git rejects fail fast
	// (before any push pollutes the measured window).
	tests := []struct {
		name      string
		prefix    string
		c, i      int
		want      string
		wantValid bool
	}{
		{name: "no prefix", prefix: "", c: 32, i: 5, want: "refs/heads/fmX-c32-a5", wantValid: true},
		{name: "slash namespace", prefix: "bench/", c: 32, i: 5, want: "refs/heads/bench/fmX-c32-a5", wantValid: true},
		{name: "dash separator", prefix: "bench-", c: 8, i: 0, want: "refs/heads/bench-fmX-c8-a0", wantValid: true},
		{name: "no separator", prefix: "bench", c: 1, i: 0, want: "refs/heads/benchfmX-c1-a0", wantValid: true}, // ugly-but-valid
		{name: "space", prefix: "my bench/", c: 1, i: 0, want: "refs/heads/my bench/fmX-c1-a0", wantValid: false},
		{name: "tilde", prefix: "bench~1", c: 1, i: 0, want: "refs/heads/bench~1fmX-c1-a0", wantValid: false},
		{name: "leading slash", prefix: "/bench", c: 1, i: 0, want: "refs/heads//benchfmX-c1-a0", wantValid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := destRef(tt.prefix, "fmX", tt.c, tt.i)
			if got != tt.want {
				t.Errorf("destRef(%q, ...) = %q, want %q", tt.prefix, got, tt.want)
			}
			if valid := plumbing.ReferenceName(got).Validate() == nil; valid != tt.wantValid {
				t.Errorf("Validate(%q) valid = %v, want %v", got, valid, tt.wantValid)
			}
		})
	}
}

func TestParseFlagsThresholds(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantMinRate   float64
		wantMaxP95    time.Duration
		wantMaxErrors int
	}{
		{
			name:          "unset",
			args:          []string{"-repos", "org/repo"},
			wantMaxErrors: -1,
		},
		{
			name:          "configured",
			args:          []string{"-repos", "org/repo", "-min-push-rate", "5.5", "-max-p95", "1500ms", "-max-errors", "0"},
			wantMinRate:   5.5,
			wantMaxP95:    1500 * time.Millisecond,
			wantMaxErrors: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseFlagsForTest(t, tt.args...)
			if err != nil {
				t.Fatalf("parseFlags() error = %v", err)
			}
			if cfg.minPushRate != tt.wantMinRate {
				t.Errorf("minPushRate = %v, want %v", cfg.minPushRate, tt.wantMinRate)
			}
			if cfg.maxP95 != tt.wantMaxP95 {
				t.Errorf("maxP95 = %v, want %v", cfg.maxP95, tt.wantMaxP95)
			}
			if cfg.maxErrors != tt.wantMaxErrors {
				t.Errorf("maxErrors = %v, want %v", cfg.maxErrors, tt.wantMaxErrors)
			}
		})
	}
}

func TestWriteResultsIncludesThresholdsWhenConfigured(t *testing.T) {
	out := filepath.Join(t.TempDir(), "results.json")
	cfg := &runConfig{
		out:         out,
		runID:       "fmtest",
		strategy:    "branch",
		duration:    30 * time.Second,
		warmup:      5 * time.Second,
		repos:       []string{"org/repo"},
		minPushRate: 5,
		maxP95:      2 * time.Second,
		maxErrors:   0,
	}
	results := []levelResult{{
		Concurrency: 4,
		OpsPerSec:   3.1,
		P95ms:       2200,
		OtherErrors: 2,
	}}

	if err := writeResults(cfg, &endpoint{label: "test"}, results); err != nil {
		t.Fatalf("writeResults() error = %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", out, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("Unmarshal results error = %v", err)
	}
	ok, exists := doc["thresholds_ok"].(bool)
	if !exists || ok {
		t.Fatalf("thresholds_ok = %v (exists %v), want false", doc["thresholds_ok"], exists)
	}
	thresholds, exists := doc["thresholds"].(map[string]any)
	if !exists {
		t.Fatalf("thresholds missing from result: %v", doc)
	}
	if got := thresholds["min_push_rate"]; got != float64(5) {
		t.Errorf("min_push_rate = %v, want 5", got)
	}
	if got := thresholds["max_p95"]; got != "2s" {
		t.Errorf("max_p95 = %v, want 2s", got)
	}
	if got := thresholds["max_errors"]; got != float64(0) {
		t.Errorf("max_errors = %v, want 0", got)
	}
	breaches, exists := thresholds["breaches"].([]any)
	if !exists || len(breaches) != 3 {
		t.Fatalf("breaches = %v (exists %v), want 3 entries", thresholds["breaches"], exists)
	}
	if breaches[0] != "c=4 push/s=3.1 < min 5.0" {
		t.Errorf("first breach = %v, want push-rate breach", breaches[0])
	}
}

func TestWriteResultsOmitsThresholdsWhenUnset(t *testing.T) {
	out := filepath.Join(t.TempDir(), "results.json")
	cfg := &runConfig{
		out:       out,
		runID:     "fmtest",
		strategy:  "branch",
		repos:     []string{"org/repo"},
		maxErrors: -1,
	}

	if err := writeResults(cfg, &endpoint{label: "test"}, []levelResult{{Concurrency: 1}}); err != nil {
		t.Fatalf("writeResults() error = %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", out, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("Unmarshal results error = %v", err)
	}
	if _, exists := doc["thresholds"]; exists {
		t.Fatalf("thresholds present when unset: %v", doc["thresholds"])
	}
	if _, exists := doc["thresholds_ok"]; exists {
		t.Fatalf("thresholds_ok present when unset: %v", doc["thresholds_ok"])
	}
}

func parseFlagsForTest(t *testing.T, args ...string) (*runConfig, error) {
	t.Helper()
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
	})

	fs := flag.NewFlagSet("forgemark", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	flag.CommandLine = fs
	os.Args = append([]string{"forgemark"}, args...)
	return parseFlags()
}
