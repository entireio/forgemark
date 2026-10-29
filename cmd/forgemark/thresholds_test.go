package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/entireio/forgemark/internal/bench"
)

func TestCheckThresholds(t *testing.T) {
	levels := []bench.LevelResult{
		{Concurrency: 1, OpsPerSec: 6.2, P95ms: 900, OtherErrors: 0},
		{Concurrency: 4, OpsPerSec: 3.1, P95ms: 2200, OtherErrors: 2},
	}
	tests := []struct {
		name  string
		gates thresholds
		want  []string
	}{
		{
			name:  "breach",
			gates: thresholds{minPushRate: 5, maxP95: 2 * time.Second, maxErrors: 0},
			want: []string{
				"c=4 push/s=3.1 < min 5.0",
				"c=4 p95=2200.0ms > max 2000.0ms",
				"c=4 errors=2 > max 0",
			},
		},
		{
			name:  "no breach",
			gates: thresholds{minPushRate: 3, maxP95: 3 * time.Second, maxErrors: 2},
		},
		{
			name:  "unset flags",
			gates: thresholds{maxErrors: -1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.gates.check(levels)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("check() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseFlagsThresholds(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want thresholds
	}{
		{
			name: "unset",
			args: []string{"-repos", "org/repo"},
			want: thresholds{maxErrors: -1},
		},
		{
			name: "configured",
			args: []string{"-repos", "org/repo", "-min-push-rate", "5.5", "-max-p95", "1500ms", "-max-errors", "0"},
			want: thresholds{minPushRate: 5.5, maxP95: 1500 * time.Millisecond, maxErrors: 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseFlagsForTest(t, tt.args...)
			if err != nil {
				t.Fatalf("parseFlags() error = %v", err)
			}
			if cfg.gates != tt.want {
				t.Errorf("gates = %+v, want %+v", cfg.gates, tt.want)
			}
		})
	}
}

func TestParseFlagsRejectsNegativeThresholds(t *testing.T) {
	for _, args := range [][]string{
		{"-repos", "org/repo", "-min-push-rate", "-1"},
		{"-repos", "org/repo", "-max-p95", "-1s"},
		{"-repos", "org/repo", "-max-errors", "-2"},
	} {
		if _, err := parseFlagsForTest(t, args...); err == nil {
			t.Errorf("parseFlags(%v) = nil error, want rejection", args)
		}
	}
}

func writeResultsForTest(t *testing.T, gates thresholds, levels []bench.LevelResult) map[string]any {
	t.Helper()
	out := filepath.Join(t.TempDir(), "results.json")
	cfg := &cliConfig{out: out, gates: gates}
	cfg.workload.RunID = "fmtest"
	cfg.workload.Strategy = "branch"
	cfg.target.Repos = []string{"org/repo"}
	if err := writeResults(cfg, "test", levels); err != nil {
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
	return doc
}

func TestWriteResultsIncludesThresholdsWhenConfigured(t *testing.T) {
	doc := writeResultsForTest(t,
		thresholds{minPushRate: 5, maxP95: 2 * time.Second, maxErrors: 0},
		[]bench.LevelResult{{Concurrency: 4, OpsPerSec: 3.1, P95ms: 2200, OtherErrors: 2}},
	)
	ok, exists := doc["thresholds_ok"].(bool)
	if !exists || ok {
		t.Fatalf("thresholds_ok = %v (exists %v), want false", doc["thresholds_ok"], exists)
	}
	gates, exists := doc["thresholds"].(map[string]any)
	if !exists {
		t.Fatalf("thresholds missing from result: %v", doc)
	}
	if got := gates["min_push_rate"]; got != float64(5) {
		t.Errorf("min_push_rate = %v, want 5", got)
	}
	if got := gates["max_p95"]; got != "2s" {
		t.Errorf("max_p95 = %v, want 2s", got)
	}
	if got := gates["max_errors"]; got != float64(0) {
		t.Errorf("max_errors = %v, want 0", got)
	}
	breaches, exists := gates["breaches"].([]any)
	if !exists || len(breaches) != 3 {
		t.Fatalf("breaches = %v (exists %v), want 3 entries", gates["breaches"], exists)
	}
	if breaches[0] != "c=4 push/s=3.1 < min 5.0" {
		t.Errorf("first breach = %v, want push-rate breach", breaches[0])
	}
}

func TestWriteResultsOmitsThresholdsWhenUnset(t *testing.T) {
	doc := writeResultsForTest(t, thresholds{maxErrors: -1}, []bench.LevelResult{{Concurrency: 1}})
	if _, exists := doc["thresholds"]; exists {
		t.Fatalf("thresholds present when unset: %v", doc["thresholds"])
	}
	if _, exists := doc["thresholds_ok"]; exists {
		t.Fatalf("thresholds_ok present when unset: %v", doc["thresholds_ok"])
	}
}
