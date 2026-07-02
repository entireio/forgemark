# ForgeMark

**A concurrent git-push throughput benchmark for any smart-HTTP git forge.**

ForgeMark measures **sustained push throughput and per-push latency** the way an
agent fleet hits a forge: many concurrent writers, small commits. It drives
**real pushes** through [`go-git`](https://github.com/go-git/go-git) (packfile
build + ref update), so it exercises the full `git-receive-pack` path — not a
synthetic HTTP approximation.

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

## What it measures

Per concurrency level it reports successful pushes/sec, p50/p95/p99/p99.9/max
push latency, and CAS-failure / error counts. Results print as a table and are
written to `results/forgemark-<id>.json` for charting.

### Strategies

| `-strategy` | shape | what it tells you |
|---|---|---|
| `branch` (default) | N agents, **one repo**, each pushes its own branch | **Headline single-repo number.** No client-side contention (each agent owns its ref), so it isolates the server's per-repo ref-update path. |
| `repo` | N agents spread across **M repos** (`-repo-count`), own branches | Horizontal-scale ceiling — distinct repos serialize independently, so this should scale where `branch` saturates. |
| `session` | N agents, each loops: **shallow-clone** the base branch → commit+push `-session-commits` checkpoints to a fresh ephemeral branch → abandon → repeat | Realistic agent lifecycle: interleaves **read load (clone) with write load (push)** on one repo. Reports clone and push latencies separately. |

## Usage

Two steps: **set up the target repos**, then **run**.

### 1. Set up targets

ForgeMark does not create repos — it pushes to ones you provide. Create them on
your forge and make sure your credential can push:

- **`branch`** needs **one empty repo**.
- **`repo`** needs **M empty repos**.
- **`session`** needs a repo with a **base branch that has content** to clone.
  An empty repo degrades to a no-read orphan, so a session run against an
  unseeded repo isn't measuring what you think — push a base branch first.

Per forge, roughly:

```bash
# GitHub (needs the gh CLI): a throwaway private repo
gh repo create you/forgemark-target --private
# for session runs, give it content:
#   git clone … && git commit --allow-empty -m base && git push

# GitLab / Gitea / self-hosted: create an empty repo in the UI or via the
# forge's API, ensure your token/user can push, and (for session) push a base
# branch with at least one commit.
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

## Key flags

| flag | default | notes |
|---|---|---|
| `-remote` | — | base URL of the forge (e.g. `https://gitlab.com`); the cluster base URL for Entire |
| `-token-file` | — | file holding the credential secret; `-` reads stdin (else `$ACCESS_TOKEN`) |
| `-user` | `x-access-token` | basic-auth username (token forges ignore it) |
| `-repos` / `-repo-pattern`+`-repo-count` | — | target repo path(s), appended verbatim; `-repo-pattern` expands `{n}` to `1..N` |
| `-strategy` | `branch` | `branch` \| `repo` \| `session` |
| `-concurrency` | `1,8,32,128` | swept sequentially, one row each |
| `-duration` / `-warmup` | `60s` / `10s` | measured window / discarded ramp |
| `-files-min`/`-files-max`/`-file-size` | `1`/`10`/`2048` | commit shape |
| `-object-format` | `auto` | `sha1` \| `sha256`; `auto` probes the Entire advertisement (generic/github default sha1) |
| `-session-commits`/`-clone-depth`/`-base-ref` | `5`/`1`/default | session strategy knobs |
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
- **Stats**: every push attempt is timed; warm-up samples are dropped; exact
  percentiles are computed from the sorted OK-latency set.

## Caveats

- Run the generator **close to the target** (same region). Push latency from a
  distant machine is dominated by round-trip time, so you'd be measuring the
  network path, not the forge. Watch the generator's CPU stays below 100% at the
  top concurrency level, or it — not the server — is your bottleneck.
- `branch`/`repo` leave one per-agent branch each on the target (no cleanup);
  `session` deletes each ephemeral branch as the agent abandons it. Use
  throwaway repos regardless.
- `branch`/`repo` use force-push on agent-owned refs so numbers aren't polluted
  by spurious non-fast-forwards; the server still does the full receive-pack, so
  throughput is unaffected.

## License

MIT — see [LICENSE](LICENSE).
