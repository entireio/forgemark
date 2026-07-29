# ForgeMark

**A concurrent git push/clone throughput benchmark for any smart-HTTP git forge.**

ForgeMark measures **sustained push or clone throughput and per-operation
latency** the way an agent fleet hits a forge: many concurrent writers, readers,
or mixed sessions. It drives **real git operations** through
[`go-git`](https://github.com/go-git/go-git) (packfile build + ref update for
pushes, upload-pack for clones), so it exercises the forge paths directly — not
a synthetic HTTP approximation.

It works against **any smart-HTTP git host** — GitLab, Gitea, Bitbucket,
self-hosted, GitHub Enterprise Server, github.com, and
[Entire](https://github.com/entireio). There's no target flag: the forge is
inferred from `-remote` (and, for Entire, from the auth flags you supply).

> **Only run ForgeMark against infrastructure you own or are explicitly
> authorized to load-test.** A tight push loop is abusive traffic to a host that
> hasn't agreed to it.

## Install

```bash
go install github.com/entireio/forgemark/cmd/forgemark@latest
```

## Web GUI

`forgemark serve` starts a local dashboard over the same engine: configure a
run in the browser, watch live per-second throughput / latency-percentile /
error charts while it executes, and benchmark **multiple forges side by side**
— every concurrency level starts simultaneously on all targets, so the overlay
is a fair comparison.

```bash
forgemark serve            # http://127.0.0.1:8377
```

- **Try it with no forge**: add a `demo://` target (e.g.
  `demo://fast?p50=60ms&spread=3&err=0.01&cap=400`) — a synthetic latency
  generator that exercises the whole pipeline offline. Add two with different
  profiles to see the comparison view.
- **Targets** take the same parameters as the CLI flags (remote, repos, object
  format, and the Entire fields). The credential is never typed into the
  browser: each target names a **source** — `gh CLI login` / `glab CLI login` /
  `entire CLI login` — and the server reads the token from that
  already-authenticated CLI at run start, so it never enters the page or the
  request body. (For GitLab the server uses glab's git-credential helper, which
  returns a repository-scoped token even when glab's stored API token isn't.)
  **Add from CLI logins** prefills complete GitHub / GitLab / Entire targets
  (endpoints, repos, credential source) from whichever CLIs you're logged into,
  and the **⚡ Push race preset** fills the race workload and opens the race
  view. A forge without a supported CLI login is benchmarked from the command
  line (`forgemark -token-file …`), not the web form.
- **History** lists every result doc in `results/` — CLI runs included — and
  overlays any selection as ops/s-vs-concurrency and p95-vs-concurrency curves.
  Server runs also store their 1-second live series, so finished runs replay.
- The server binds to loopback by default and refuses cross-origin requests.
  It is a load-generation control panel: the same authorization warning as the
  CLI applies, and starting a run requires confirming it in the form.

`-addr` changes the listen address (a warning prints if it isn't loopback);
`-results` points at a different results directory.

### Local comparison without pasting tokens

Two scripts make the GitHub-vs-Entire comparison fully local — nobody shares or
pastes a token, ever:

```bash
scripts/login.sh           # one-time: authenticate the gh and entire CLIs
scripts/compare-local.sh   # start (or reuse) the server and kick off a run
scripts/pushrace.sh        # demo mode: a 60s live head-to-head push race
```

`pushrace.sh` presets a race-shaped workload — one flight of 16 agents pushing
checkpoint-sized commits (1-3 files × 1KB) at every target simultaneously for
one minute — and opens the GUI's **race view** (`#race/<run-id>`): a countdown,
one lane per forge, and a podium with the verdict when the flag drops. Three
metrics to race on (switchable live, or deep-linkable as
`#race/<run-id>/<metric>`):

- **Throughput** — cumulative successful pushes; the leader fills the track.
- **Latency** — rolling 10s p50, lower wins; bar length is relative speed, so
  lower latency literally looks faster.
- **Reliability** — successful pushes ÷ attempts (errors and CAS rejections
  count as failures).

Every number is measured, an exact tie is called a dead heat, and a caption
under the lanes — generated from the run's actual workload — states precisely
what is being watched: the commit shape, the loop, what the counters and
windows mean, and that client-side network round-trip is included. Any run can
be watched either way; the dashboard and race views link to each other.

Mind what the Entire target measures: **mirror mode** (compare-local's
default) pushes through a GitHub mirror, which write-throughs to GitHub on
every push — that benchmarks the sync flow and can never beat GitHub itself.
**Native mode** (`FM_ENTIRE_NATIVE=1`, the pushrace default) auto-creates a
plain EntireDB repo (`et/forgemark-<you>/forgemark-target`) and benchmarks the
forge directly.

[`scripts/compare-local.sh`](scripts/compare-local.sh) posts its targets with
a credential **source** (`secret_source: gh | entire`) instead of a token: the
server pulls each credential from your already-authenticated CLI at run start,
so no token ever passes through the script, the request body, or the browser.
Everything else is provisioned on first use:

- a private throwaway GitHub repo (`<you>/forgemark-target`, seeded with an
  initial commit),
- its EntireDB mirror on your jurisdiction's default cluster
  (`entire repo mirror create` is idempotent, so re-runs are free),
- and `forgemark serve` itself, if nothing is listening.

The Entire endpoints (token URL, jurisdiction audience, cluster) come from the
server's own discovery endpoint (`GET /api/local/suggest`, derived from your
active `entire auth` login context) — the same source the GUI's "Add from CLI
logins" button uses, so that logic exists once. Every repo, endpoint, and
workload knob is an `FM_*` environment variable — see the header comment in
the script; `FM_GITHUB_ONLY=1` skips the Entire target. Defaults keep
concurrency at `1,4` because of github.com's abuse limits (see below).

## What it measures

Per concurrency level it reports successful operations/sec,
p50/p95/p99/p99.9/max latency, and CAS-failure / error counts where they apply.
Results print as a table and are written to `results/forgemark-<id>.json` for
charting.

### Strategies

| `-strategy` | shape | what it tells you |
|---|---|---|
| `branch` (default) | N agents, **one repo**, each pushes its own branch | **Headline single-repo number.** No client-side contention (each agent owns its ref), so it isolates the server's per-repo ref-update path. |
| `repo` | N agents spread across **M repos** (`-repo-count`), own branches | Horizontal-scale ceiling — distinct repos serialize independently, so this should scale where `branch` saturates. |
| `clone` | N agents, **one repo**, each loops: **shallow-clone** the base branch → discard → repeat | Read-side ceiling: how many concurrent shallow clones/sec the forge sustains, without write load. |
| `session` | N agents, each loops: **shallow-clone** the base branch → commit+push `-session-commits` checkpoints to a fresh ephemeral branch → abandon → repeat | Realistic agent lifecycle: interleaves **read load (clone) with write load (push)** on one repo. Reports clone and push latencies separately. |

## Usage

Two steps: **set up the target repos**, then **run**.

### 1. Set up targets

ForgeMark does not create repos — it pushes to ones you provide. Create them on
your forge and make sure your credential can push:

- **`branch`** needs **one empty repo**.
- **`repo`** needs **M empty repos**.
- **`clone`** and **`session`** need a repo with a **base branch that has
  content** to clone. An empty repo degrades to a no-read orphan, so a read-side
  run against an unseeded repo isn't measuring what you think — push a base
  branch first.

Per forge, roughly:

```bash
# GitHub (needs the gh CLI): a throwaway private repo
gh repo create you/forgemark-target --private
# for clone/session runs, give it content:
#   git clone … && git commit --allow-empty -m base && git push

# GitLab (needs the glab CLI): a throwaway private project
glab repo create forgemark-target --private

# Gitea / self-hosted: create an empty repo in the UI or via the forge's API,
# ensure your token/user can push, and (for clone/session) push a base branch
# with at least one commit.
```

### 2. Run

Most forges just need a token. Point `-remote` at the host and supply the token
via `-token-file` (a file, or `-` for stdin):

```bash
FORGE="-remote https://git.example.com"

# Headline: single-repo throughput, sweep concurrency
# (-token-file reads a file; use "-" to pipe the token in on stdin instead)
forgemark $FORGE -token-file ~/.forge-token \
  -repos org/repo -concurrency 1,8,32 -duration 2m

# Spread across 16 repos (horizontal scale)
forgemark $FORGE -token-file ~/.forge-token -strategy repo \
  -repo-pattern "org/bench-{n}" -repo-count 16 -concurrency 32,128 -duration 2m

# Clone: read-side throughput, discard each in-memory clone (needs a seeded base branch)
forgemark $FORGE -token-file ~/.forge-token -repos org/repo \
  -strategy clone -clone-depth 1 -concurrency 8,32 -duration 2m

# Session: clone + push 5 checkpoints, abandon, repeat (needs a seeded base branch)
forgemark $FORGE -token-file ~/.forge-token -repos org/repo \
  -strategy session -session-commits 5 -clone-depth 1 -concurrency 8,32
```

The push URL is `<-remote>/<repo>` — the `-repos` value is appended **verbatim**
(ForgeMark does no path rewriting), so include a `.git` suffix if your forge
needs it. The token is used as the basic-auth password with a conventional
username (`-user` to override); most forges accept a PAT this way. The secret
comes from `-token-file` or the `ACCESS_TOKEN` env var — never a CLI flag, so it
can't leak via `ps` or shell history. Entire needs a couple of extra flags — see
below.

## Entire

Entire needs two extra flags — `-token-url` and `-jurisdiction`. Auth is a
single short-lived **jurisdiction identity token**: ForgeMark exchanges your
subject token for it (RFC 8693) and refreshes it, so one token authorizes every
repo you can reach. Pass the subject token as `-token-file`/`ACCESS_TOKEN`:

```bash
forgemark -remote https://aws-us-east-2.entire.io \
  -token-url    https://us.auth.entire.io/oauth/token \
  -jurisdiction https://us.entire.io \
  -token-file   ~/.entire-subject-token \
  -repos your/repo -concurrency 1,8,32,128 -duration 2m
```

`-repos` is appended verbatim — ForgeMark does no path rewriting, so pass the
full repo path exactly as your Entire deployment expects it. The exact form is
deployment-specific — see the runbook.

Deployment-specific setup (obtaining the subject token, target provisioning) and
methodology notes live in Entire's own runbook.

## github.com

github.com is just a generic remote — point `-remote` at it with a PAT
(needs **contents: write**):

```bash
gh repo create you/forgemark-throwaway --private
gh auth token | forgemark -remote https://github.com -token-file - \
  -repos you/forgemark-throwaway -concurrency 1,4 -duration 30s
```

**Keep concurrency low.** github.com applies secondary rate limits and abuse
detection to high-volume content writes; a tight push loop at high concurrency
will get throttled or blocked, and high-volume automated load testing of
github.com isn't sanctioned by their acceptable-use policy. ForgeMark detects a
`github.com` remote and warns above concurrency 16. For a higher-volume
comparison, point `-remote` at a **GitHub Enterprise Server** you control.

## GitLab

GitLab is also a generic remote. The push path wants a **repository-scoped**
credential, which is not always what `glab auth token` returns — so pull the
credential from glab's git-credential helper (username `oauth2`, password a
repo-scoped token):

```bash
glab repo create forgemark-target --private
CRED=$(printf 'protocol=https\nhost=gitlab.com\n\n' | glab auth git-credential get)
printf '%s' "$CRED" | sed -n 's/^password=//p' \
  | forgemark -remote https://gitlab.com -token-file - -user oauth2 \
      -repos you/forgemark-target.git -concurrency 1,8 -duration 1m
```

The web GUI does this for you: pick **glab CLI login** as the credential source
(or **Add from CLI logins**) and the server runs the helper at run start. As
with github.com, gitlab.com rate-limits high-volume writes — keep concurrency
modest, or point `-remote` at a self-managed GitLab you control.

## Key flags

| flag | default | notes |
|---|---|---|
| `-remote` | — | base URL of the forge (e.g. `https://gitlab.com`); the cluster base URL for Entire |
| `-token-file` | — | file holding the credential secret; `-` reads stdin (else `$ACCESS_TOKEN`) |
| `-user` | `x-access-token` | basic-auth username (token forges ignore it) |
| `-repos` / `-repo-pattern`+`-repo-count` | — | target repo path(s), appended verbatim; `-repo-pattern` expands `{n}` to `1..N` |
| `-strategy` | `branch` | `branch` \| `repo` \| `clone` \| `session` |
| `-branch-prefix` | — | prepended verbatim to branch names, before the run ID (e.g. `bench/` → `refs/heads/bench/fm...`); groups branches for easy cleanup |
| `-concurrency` | `1,8,32,128` | swept sequentially, one row each |
| `-duration` / `-warmup` | `60s` / `10s` | measured window / discarded ramp |
| `-files-min`/`-files-max`/`-file-size` | `1`/`10`/`2048` | commit shape; ignored by `clone` |
| `-object-format` | `auto` | `sha1` \| `sha256`; `auto` probes the Entire advertisement (generic/github default sha1) |
| `-session-commits` | `5` | session strategy checkpoints per cloned session |
| `-clone-depth`/`-base-ref` | `1`/default | clone/session strategy knobs |
| `-insecure` | `false` | skip TLS verification (dev / self-signed hosts) |
| `-out` | — | JSON results path (default `results/forgemark-<id>.json`) |

### Entire (supplying `-token-url` + `-jurisdiction` selects it)

| flag | notes |
|---|---|
| `-token-url` | core `/oauth/token` endpoint (presence selects Entire) |
| `-jurisdiction` | jurisdiction audience host, bare origin (presence selects Entire) |
| `-client-id` | public OAuth client id (default `entire-cli`) |

In this mode `-token-file`/`ACCESS_TOKEN` carries the subject token to exchange,
not a forge PAT.

## Environment variables

| var | notes |
|---|---|
| `ACCESS_TOKEN` | the credential secret, if `-token-file` isn't given: the forge token, or Entire's subject token. `-token-file` is preferred (keeps the secret out of the environment too). |

## How it works

- **Each agent** keeps an in-memory go-git repo (object format matched to the
  remote), commits 1–10 small files per iteration, and pushes its own branch in
  a tight loop. Agents are pinned round-robin across the target's nodes.
- **Clone runs** create each shallow clone in memory and discard it after
  recording latency, so the client side stays off disk.
- **Stats**: every operation is timed; warm-up samples are dropped; exact
  percentiles are computed from the sorted OK-latency set.

## Caveats

- Run the generator **close to the target** (same region). Push latency from a
  distant machine is dominated by round-trip time, so you'd be measuring the
  network path, not the forge. Watch the generator's CPU stays below 100% at the
  top concurrency level, or it — not the server — is your bottleneck.
- `branch`/`repo` leave one per-agent branch each on the target (no cleanup);
  `session` deletes each ephemeral branch as the agent abandons it; `clone`
  does not write refs. Use throwaway repos regardless. Pass `-branch-prefix`
  (e.g. `bench/`) to namespace the branches so they're easy to find and delete
  on the target afterwards.
- `branch`/`repo` use force-push on agent-owned refs so numbers aren't polluted
  by spurious non-fast-forwards; the server still does the full receive-pack, so
  throughput is unaffected.

## License

MIT — see [LICENSE](LICENSE).
