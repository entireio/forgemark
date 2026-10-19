package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/forgemark/internal/bench"
)

// A subcommand is a bare leading word; a leading flag is the benchmark CLI,
// so adding subcommands can never steal an existing invocation.
func TestSubcommand(t *testing.T) {
	for argv, want := range map[*[]string]string{
		{"forgemark"}:                         "",
		{"forgemark", "-remote", "https://x"}: "",
		{"forgemark", "serve", "-addr", ":1"}: "serve",
		{"forgemark", "sweep", "-dry-run"}:    "sweep",
	} {
		if got := subcommand(*argv); got != want {
			t.Errorf("subcommand(%v) = %q, want %q", *argv, got, want)
		}
	}
}

// The sweep is aimed with the benchmark's own target and ref flags, so a
// user re-runs the flags that left the refs behind plus -dry-run; the parsed
// workload must be one NewRunner accepts, under the chosen namespace+prefix.
func TestParseSweepFlags(t *testing.T) {
	cfg, err := parseSweepFlags([]string{
		"-remote", "https://git.example", "-token-file", "tok",
		"-repo-pattern", "org/bench-{n}", "-repo-count", "2",
		"-ref-namespace", "refs/heads", "-branch-prefix", "bench/",
		"-dry-run", "-min-age", "24h",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.target.Remote != "https://git.example" || cfg.tokenFile != "tok" {
		t.Errorf("target = %+v tokenFile = %q", cfg.target, cfg.tokenFile)
	}
	if want := []string{"org/bench-1", "org/bench-2"}; !reflect.DeepEqual(cfg.target.Repos, want) {
		t.Errorf("repos = %v, want %v", cfg.target.Repos, want)
	}
	if !cfg.opts.DryRun || cfg.opts.MinAge != 24*time.Hour {
		t.Errorf("opts = %+v, want dry run at 24h", cfg.opts)
	}
	if cfg.workload.Namespace() != "refs/heads/" || cfg.workload.BranchPrefix != "bench/" {
		t.Errorf("workload refs = %s %q, want refs/heads/ bench/", cfg.workload.Namespace(), cfg.workload.BranchPrefix)
	}
	if err := cfg.workload.Validate(); err != nil {
		t.Errorf("sweep workload must validate for NewRunner: %v", err)
	}

	defaults, err := parseSweepFlags([]string{"-remote", "https://git.example", "-repos", "a/b"})
	if err != nil {
		t.Fatal(err)
	}
	if defaults.opts.DryRun || defaults.opts.MinAge != 0 || defaults.workload.Namespace() != bench.DefaultRefNamespace {
		t.Errorf("defaults = %+v / %s, want immediate real sweep under %s", defaults.opts, defaults.workload.Namespace(), bench.DefaultRefNamespace)
	}
}

func TestParseSweepFlagsRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no repos":          {[]string{"-remote", "https://x"}, "no repos"},
		"positional":        {[]string{"-remote", "https://x", "-repos", "a/b", "a/b"}, "unexpected argument"},
		"negative min-age":  {[]string{"-remote", "https://x", "-repos", "a/b", "-min-age", "-1h"}, "-min-age"},
		"bad namespace":     {[]string{"-remote", "https://x", "-repos", "a/b", "-ref-namespace", "heads/"}, "-ref-namespace"},
		"bad branch prefix": {[]string{"-remote", "https://x", "-repos", "a/b", "-branch-prefix", "a..b/"}, "-branch-prefix"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseSweepFlags(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseSweepFlags(%v) err = %v, want %q", tc.args, err, tc.want)
			}
		})
	}
}

// The report names every selected ref — the operator checks the selection
// against their expectations before a real sweep — and says plainly whether
// anything was deleted.
func TestSweepReport(t *testing.T) {
	sweeps := []bench.RepoSweep{
		{Repo: "org/a", Refs: []string{"refs/forgemark/fm1-c1-a0", "refs/forgemark/fm1-c1-a1"}, Deleted: 1},
		{Repo: "org/b"},
	}
	dry := sweepReport(sweeps, true)
	for _, want := range []string{
		"org/a: 2 bench refs", "    refs/forgemark/fm1-c1-a0", "    refs/forgemark/fm1-c1-a1",
		"org/b: no bench refs to sweep", "dry run: 2 bench refs would be deleted",
	} {
		if !strings.Contains(dry, want) {
			t.Errorf("dry-run report missing %q:\n%s", want, dry)
		}
	}
	if strings.Contains(dry, "deleted 1") {
		t.Errorf("dry-run report claims a deletion:\n%s", dry)
	}

	real := sweepReport(sweeps, false)
	for _, want := range []string{"org/a: deleted 1 of 2", "deleted 1 of 2 bench refs"} {
		if !strings.Contains(real, want) {
			t.Errorf("report missing %q:\n%s", want, real)
		}
	}
}
