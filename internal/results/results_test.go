package results

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/forgemark/internal/bench"
)

func TestWriteLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	doc := Doc{
		Format: 2, RunID: "fmtest", Strategy: "branch",
		Duration: "10s", Warmup: "1s", Commit: "1-10 x 2048B", State: "done",
		Targets: []TargetResult{{
			Name: "a", Label: "https://a.example",
			Levels: []bench.LevelResult{{Concurrency: 4, OK: 10, OpsPerSec: 2.5}},
			Series: []SeriesPoint{{T: 100, Level: 0, OK: 3, P95: 42.5}},
		}},
	}
	if err := Write(dir, "forgemark-fmtest.json", doc); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "forgemark-fmtest.json")
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != 2 || len(got.Targets) != 1 || got.Targets[0].Series[0].P95 != 42.5 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestLoadNormalizesLegacyDoc(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"run_id":"fmold","target":"https://old.example","strategy":"branch",
		"levels":[{"concurrency":8,"ok":50,"ops_per_sec":5}]}`
	if err := os.WriteFile(filepath.Join(dir, "forgemark-fmold.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := Load(dir, "forgemark-fmold.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Targets) != 1 || doc.Targets[0].Name != "https://old.example" {
		t.Fatalf("legacy not normalized to one pseudo-target: %+v", doc)
	}
	if doc.Targets[0].Levels[0].Concurrency != 8 {
		t.Fatalf("legacy levels lost: %+v", doc.Targets[0])
	}
	if doc.Target != "" || doc.Levels != nil {
		t.Fatal("legacy fields should be cleared after normalization")
	}
}

func TestFilenameValidation(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../evil.json", "forgemark-x.txt", "other.json", "forgemark-a/b.json"} {
		if _, err := Load(dir, bad); err == nil {
			t.Fatalf("Load(%q) succeeded, want rejection", bad)
		}
		if err := Write(dir, bad, Doc{}); err == nil {
			t.Fatalf("Write(%q) succeeded, want rejection", bad)
		}
	}
}

func TestListSkipsUnparseableAndSortsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "forgemark-good.json"), []byte(`{"run_id":"good","target":"x","levels":[]}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "forgemark-bad.json"), []byte(`{{{`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte(`x`), 0o600)

	list, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].RunID != "good" {
		t.Fatalf("List = %+v, want just the parseable doc", list)
	}

	if empty, err := List(filepath.Join(dir, "missing")); err != nil || len(empty) != 0 {
		t.Fatalf("List(missing dir) = %v, %v — want empty, nil", empty, err)
	}
}
