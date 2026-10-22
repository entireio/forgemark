# Agent notes for ForgeMark

Durable lessons for any agent (Claude Code, Codex, or otherwise) working in
this repository. Read this before changing the areas it names. Add a lesson
when a fix depends on a fact the code alone would not stop you from undoing.

## Sweep: `-min-age 0` must never compare timestamps against `time.Now()`

**Where:** `internal/bench/cleanup.go` — `SweepBenchRefs`, `sweepBenchRefs`,
`staleRef`; the `-min-age` flag in `cmd/forgemark/sweep.go`.

**The bug this guards against:** forgemark run IDs embed the run's start time
(`"fm"` + base36 UnixNano) as stamped by the *pushing* machine's clock. An
earlier version of `forgemark sweep` turned `-min-age 0` into
`cutoff = time.Now()` and swept a ref only if its embedded start time was
before that cutoff. A ref pushed from a machine whose clock ran ahead of the
sweeping machine decoded to a start time in the sweeper's future, failed the
comparison, and was silently skipped on every sweep until the local clock
caught up. The operator had asked for an immediate, complete sweep and got
an incomplete one with no indication why.

**The rule:**

- `MinAge == 0` means **no age guard at all**. It is represented as a zero
  `time.Time` cutoff, and `staleRef` returns true for every shaped ref when
  the cutoff is zero. Do not "simplify" this back to `time.Now().Add(-0)`.
- Only a **positive** `MinAge` compares timestamps, because a window of
  hours or days makes seconds or minutes of skew irrelevant. A ref stamped in
  the future is still spared under a positive window: its age is unknowable,
  and a window is where caution belongs.
- The in-run housekeeping sweep (`CleanStaleBenchRefs`) always passes
  `now - staleAfter` and is unaffected; keep it that way. It must stay
  conservative because it cannot know what else is pushing to the repo.
- The unparseable-timestamp guard in `staleRef` (overflowing base36) stays in
  force even with the zero cutoff: a shape match that is not a real run ID is
  not ours to delete.

**Tests that pin this:** `TestStaleRefZeroCutoffIgnoresClockSkew` and the
`skewed` ref in `TestSweepBenchRefsEndToEnd` / `TestSweepBenchRefsMinAge`
(`internal/bench/`). If a change makes any of them fail, the change is
reintroducing this bug.
