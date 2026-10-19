package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/signal"
	"strings"
	"syscall"

	"github.com/entireio/forgemark/internal/bench"
)

// sweepConfig is the parsed `forgemark sweep` flag set: the target whose
// repos to sweep, where its bench refs live, and how the sweep behaves.
type sweepConfig struct {
	target    bench.Target
	workload  bench.Workload // only Namespace() and BranchPrefix matter; see bench.SweepWorkload
	tokenFile string
	opts      bench.SweepOptions
}

// runSweep is `forgemark sweep`: delete the per-agent refs that branch and
// repo runs leave on the target, on demand. A run sweeps its own leftovers
// before measuring, but only once they are a week old (bench.staleAfter) —
// it can't tell a fresh ref from another generator's live one. The operator
// of a throwaway repo can, so this command sweeps immediately by default,
// lists first with -dry-run, and is aimed with the same target, -ref-namespace
// and -branch-prefix flags as the run that pushed the refs.
func runSweep(args []string) error {
	cfg, err := parseSweepFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil // -h/-help already printed usage; a help request isn't a failure
	}
	if err != nil {
		return err
	}
	secret, err := readSecret(cfg.tokenFile)
	if err != nil {
		return err
	}
	cfg.target.Secret = secret

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	r, err := bench.NewRunner(ctx, cfg.target, cfg.workload, nil)
	if err != nil {
		return err
	}
	defer r.Close()

	mode := "deleting"
	if cfg.opts.DryRun {
		mode = "dry run"
	}
	fmt.Printf("forgemark: sweep target=%s refs=%s prefix=%q repos=%d min-age=%s mode=%s\n\n",
		r.Label(), cfg.workload.Namespace(), cfg.workload.BranchPrefix, len(cfg.target.Repos), cfg.opts.MinAge, mode)

	// Best-effort per ref and per repo, like the in-run sweep: print every
	// repo's outcome, then fail on whatever was refused so a script notices.
	sweeps, err := r.SweepBenchRefs(ctx, cfg.opts)
	fmt.Print(sweepReport(sweeps, cfg.opts.DryRun))
	return err
}

func parseSweepFlags(args []string) (*sweepConfig, error) {
	fs := flag.NewFlagSet("forgemark sweep", flag.ContinueOnError)
	cfg := &sweepConfig{}
	var ta targetArgs
	ta.register(fs, &cfg.target, &cfg.tokenFile)
	registerRefFlags(fs, &cfg.workload)
	fs.BoolVar(&cfg.opts.DryRun, "dry-run", false, "list the bench refs the sweep would delete and delete nothing")
	fs.DurationVar(&cfg.opts.MinAge, "min-age", 0, "spare refs from runs that started less than this long ago (0 sweeps every matching ref, regardless of the pushing host's clock; a benchmark's own pre-run sweep waits 168h)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q (forgemark sweep takes flags only)", fs.Arg(0))
	}
	if cfg.opts.MinAge < 0 {
		return nil, errors.New("-min-age must be >= 0")
	}
	repos, err := ta.repos()
	if err != nil {
		return nil, err
	}
	cfg.target.Repos = repos
	cfg.workload = bench.SweepWorkload(cfg.workload.RefNamespace, cfg.workload.BranchPrefix)
	if err := cfg.workload.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// sweepReport renders each repo's selected refs and, unless this was a dry
// run, how many of them were deleted. Refs are listed by name so the operator
// can check the selection against what they expect before trusting a real
// sweep.
func sweepReport(sweeps []bench.RepoSweep, dryRun bool) string {
	var b strings.Builder
	selected, deleted := 0, 0
	for _, s := range sweeps {
		if len(s.Refs) == 0 {
			fmt.Fprintf(&b, "  %s: no bench refs to sweep\n", s.Repo)
			continue
		}
		fmt.Fprintf(&b, "  %s: %d bench refs\n", s.Repo, len(s.Refs))
		for _, ref := range s.Refs {
			fmt.Fprintf(&b, "    %s\n", ref)
		}
		if !dryRun {
			fmt.Fprintf(&b, "  %s: deleted %d of %d\n", s.Repo, s.Deleted, len(s.Refs))
		}
		selected += len(s.Refs)
		deleted += s.Deleted
	}
	if dryRun {
		fmt.Fprintf(&b, "\ndry run: %d bench refs would be deleted\n", selected)
	} else {
		fmt.Fprintf(&b, "\ndeleted %d of %d bench refs\n", deleted, selected)
	}
	return b.String()
}
