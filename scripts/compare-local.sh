#!/usr/bin/env bash
# compare-local.sh — start a ForgeMark comparison run (GitHub vs Entire) on the
# local `forgemark serve` GUI without any token handling at all: targets are
# posted with a secret_source ("gh" | "entire") and the SERVER pulls each
# credential from your already-authenticated CLI at run start. No token ever
# passes through this script, the request body, or the browser.
#
# One-time setup: scripts/login.sh (authenticates both CLIs). Everything else
# is automatic — the throwaway GitHub repo, its EntireDB mirror (or a native
# EntireDB repo), and the local server itself are created or started on first
# use. Entire endpoints come from the server's own discovery endpoint
# (GET /api/local/suggest), so that logic lives in exactly one place.
#
# Usage: scripts/compare-local.sh
#
# Overridable via environment variables:
#   FM_ADDR                 server address            (default 127.0.0.1:8377)
#   FM_GH_REPO              GitHub throwaway repo     (default <you>/forgemark-target, auto-created private)
#   FM_GITHUB_ONLY          set to 1 to skip the Entire target
#   FM_ENTIRE_NATIVE        set to 1 to benchmark a NATIVE EntireDB repo
#                           (et/<project>/<repo>, auto-created) instead of a
#                           GitHub mirror. A mirror write-throughs to GitHub on
#                           every push, so mirror mode measures the sync flow;
#                           native mode measures EntireDB itself.
#   FM_ENTIRE_PROJECT       native mode: owning project (default forgemark-<you>, auto-created)
#   FM_ENTIRE_REPO_NAME     native mode: repo name (default forgemark-target, auto-created)
#   FM_ENTIRE_UPSTREAM      mirror mode: GitHub repo behind the mirror (default
#                           <you>/forgemark-target-entire, auto-created private).
#                           Deliberately NOT the same repo as FM_GH_REPO: the
#                           mirror writes through to its GitHub upstream, so
#                           sharing one repo makes the two targets race each
#                           other's refs ("mirror is stale" on every push).
#   FM_ENTIRE_CLUSTER       EntireDB cluster host     (default: from /api/local/suggest)
#   FM_ENTIRE_REPOS         Entire repo path(s)       (default: the auto-created mirror/native repo)
#   FM_ENTIRE_NAME          chart label               (default: from /api/local/suggest, e.g. entire-in)
#   FM_ENTIRE_TOKEN_URL     core /oauth/token         (default: from /api/local/suggest)
#   FM_ENTIRE_JURISDICTION  jurisdiction audience     (default: from /api/local/suggest)
#   FM_CONCURRENCY          sweep, comma-separated    (default 1,4 — github.com abuse limits)
#   FM_DURATION_SEC         seconds per level         (default 30)
#   FM_WARMUP_SEC           warm-up seconds           (default 5)
#   FM_FILES_MIN/FM_FILES_MAX/FM_FILE_SIZE  commit shape (defaults 1/10/2048)
#   FM_VIEW                 UI view to open           (run | race, default run)
set -euo pipefail

ADDR="${FM_ADDR:-127.0.0.1:8377}"
CONCURRENCY="${FM_CONCURRENCY:-1,4}"
DURATION_SEC="${FM_DURATION_SEC:-30}"
WARMUP_SEC="${FM_WARMUP_SEC:-5}"

# Pin every gh command to github.com. The server resolves `gh auth token
# --hostname github.com` and benchmarks github.com, so a user's GH_HOST (e.g. a
# GHES host) must not make this script validate/provision a different forge.
export GH_HOST=github.com
command -v gh >/dev/null || { echo "compare-local: needs the gh CLI — run scripts/login.sh" >&2; exit 1; }
command -v python3 >/dev/null || { echo "compare-local: needs python3" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "compare-local: gh is not authenticated — run scripts/login.sh" >&2; exit 1; }

# --- server: start one if nothing is listening (needed before discovery) ---
if ! curl -sf "http://$ADDR/api/runs" >/dev/null 2>&1; then
  echo "compare-local: no server on $ADDR — starting one (logs: /tmp/forgemark-serve.log)"
  if command -v forgemark >/dev/null; then
    nohup forgemark serve -addr "$ADDR" >/tmp/forgemark-serve.log 2>&1 &
  else
    nohup go run ./cmd/forgemark serve -addr "$ADDR" >/tmp/forgemark-serve.log 2>&1 &
  fi
  for _ in $(seq 1 30); do
    curl -sf "http://$ADDR/api/runs" >/dev/null 2>&1 && break
    sleep 1
  done
  curl -sf "http://$ADDR/api/runs" >/dev/null 2>&1 || { echo "compare-local: server did not come up; see /tmp/forgemark-serve.log" >&2; exit 1; }
fi

# ensure_repo <owner/repo>: throwaway private repo with an initial commit
# (the EntireDB mirror's initial clone needs one), created on first use.
ensure_repo() {
  local repo="$1"
  if ! gh repo view "$repo" >/dev/null 2>&1; then
    echo "compare-local: creating throwaway private repo $repo"
    gh repo create "$repo" --private --add-readme
  elif ! gh api "repos/$repo/commits?per_page=1" --jq '.[0].sha' >/dev/null 2>&1; then
    echo "compare-local: seeding empty repo $repo with an initial commit"
    gh api -X PUT "repos/$repo/contents/README.md" \
      -f message="forgemark bench target" \
      -f content="$(printf 'forgemark bench target\n' | base64)" >/dev/null
  fi
}

# --- GitHub target ---
GH_USER="$(gh api user --jq .login)"
GH_REPO="${FM_GH_REPO:-$GH_USER/forgemark-target}"
ensure_repo "$GH_REPO"
export GH_REPO

# --- Entire target: endpoints from the server's discovery, repo provisioned here ---
ENTIRE_ENABLED=0
if [ "${FM_GITHUB_ONLY:-0}" != "1" ]; then
  command -v entire >/dev/null || { echo "compare-local: entire CLI not found — run scripts/login.sh (or FM_GITHUB_ONLY=1)" >&2; exit 1; }
  entire auth status >/dev/null 2>&1 || { echo "compare-local: entire is not authenticated — run scripts/login.sh (or FM_GITHUB_ONLY=1)" >&2; exit 1; }

  # One discovery source: the same /api/local/suggest the GUI's "Add from CLI
  # logins" button uses. It runs gh+entire discovery server-side in parallel.
  SUGGEST="$(curl -sf "http://$ADDR/api/local/suggest")" \
    || { echo "compare-local: GET /api/local/suggest failed" >&2; exit 1; }
  export SUGGEST
  eval "$(python3 - <<'PY'
import json, os, shlex, sys

entry = json.loads(os.environ["SUGGEST"]).get("entire") or {}
if entry.get("error") or not entry.get("target"):
    # shlex.quote the whole echo argument as one token. Embedding it INSIDE a
    # double-quoted string would leave any command substitution in the error to
    # be evaluated when eval runs this generated line.
    msg = "compare-local: entire discovery failed: " + entry.get("error", "no target")
    print("echo " + shlex.quote(msg) + " >&2; exit 1")
    sys.exit(0)
t = entry["target"]
repo = (t.get("repos") or [""])[0]          # et/forgemark-<handle>/forgemark-target
jur = t.get("jurisdiction", "")             # https://<slug>.entire.io
slug = jur.removeprefix("https://").split(".")[0]
print(f'SUG_NAME={shlex.quote(t.get("name", ""))}')
print(f'SUG_REMOTE={shlex.quote(t.get("remote", ""))}')
print(f'SUG_TOKEN_URL={shlex.quote(t.get("token_url", ""))}')
print(f'SUG_JURISDICTION={shlex.quote(jur)}')
print(f'SUG_SLUG={shlex.quote(slug)}')
print(f'SUG_NATIVE_REPO={shlex.quote(repo)}')
PY
)"

  # /api/local/suggest is unauthenticated and only as trustworthy as whatever is
  # bound to $ADDR. Before provisioning or pushing with the AUTHENTICATED entire
  # CLI, cross-check the discovered destination against authenticated local CLI
  # state, so a rogue process squatting the port can't redirect us to a hostile
  # cluster or a repo we don't own.
  AUTH_STATUS="$(entire auth status 2>/dev/null || true)"
  AUTH_HANDLE="$(awk '/User:/{sub(/^@/,"",$2); print $2; exit}' <<<"$AUTH_STATUS")"
  AUTH_JUR="$(awk '/Jurisdiction:/{print $2; exit}' <<<"$AUTH_STATUS")"
  AUTH_CTX="$(awk '/Context:/{print $2; exit}' <<<"$AUTH_STATUS")"
  # Registrable domain of the authenticated context host (in.auth.entire.io ->
  # entire.io); every discovered endpoint must live under it.
  AUTH_DOMAIN="$(awk -F. 'NF>=2{print $(NF-1)"."$NF}' <<<"$AUTH_CTX")"
  if [ -z "$AUTH_HANDLE" ] || [ -z "$AUTH_JUR" ] || [ -z "$AUTH_DOMAIN" ]; then
    echo "compare-local: could not read authenticated entire identity to validate discovery — refusing" >&2; exit 1
  fi
  for u in "$SUG_REMOTE" "$SUG_JURISDICTION" "$SUG_TOKEN_URL"; do
    [ -n "$u" ] || continue
    host="${u#*://}"; host="${host%%/*}"; host="${host%%:*}"
    case "$host" in
      "$AUTH_DOMAIN" | *".$AUTH_DOMAIN") ;;
      *) echo "compare-local: discovered host '$host' is not under '$AUTH_DOMAIN' — the suggest endpoint may be spoofed; refusing" >&2; exit 1 ;;
    esac
  done
  if [ "$SUG_SLUG" != "$AUTH_JUR" ]; then
    echo "compare-local: discovered jurisdiction '$SUG_SLUG' != authenticated '$AUTH_JUR' — refusing" >&2; exit 1
  fi
  # The native repo path is et/forgemark-<handle>/... — the owner segment must be
  # our authenticated handle, never one an attacker chose.
  case "$SUG_NATIVE_REPO" in
    "et/forgemark-$AUTH_HANDLE/"*) ;;
    "") ;; # only used in native mode; emptiness is caught there
    *) echo "compare-local: discovered native repo '$SUG_NATIVE_REPO' is not owned by @$AUTH_HANDLE — refusing" >&2; exit 1 ;;
  esac

  export FM_ENTIRE_NAME="${FM_ENTIRE_NAME:-$SUG_NAME}"
  export FM_ENTIRE_TOKEN_URL="${FM_ENTIRE_TOKEN_URL:-$SUG_TOKEN_URL}"
  export FM_ENTIRE_JURISDICTION="${FM_ENTIRE_JURISDICTION:-$SUG_JURISDICTION}"
  # The cluster and the benchmarked remote must move together: FM_ENTIRE_CLUSTER
  # (else an explicit FM_ENTIRE_REMOTE, else the suggest default) sets both the
  # provisioning host and the remote posted to /api/runs. Otherwise setting only
  # FM_ENTIRE_CLUSTER would provision on one cluster but benchmark another.
  ENTIRE_CLUSTER="${FM_ENTIRE_CLUSTER:-${FM_ENTIRE_REMOTE:-$SUG_REMOTE}}"
  ENTIRE_CLUSTER="${ENTIRE_CLUSTER#https://}"
  export FM_ENTIRE_REMOTE="${FM_ENTIRE_REMOTE:-https://$ENTIRE_CLUSTER}"

  if [ "${FM_ENTIRE_NATIVE:-0}" = "1" ]; then
    # Native EntireDB repo: no GitHub in the write path. The suggest endpoint
    # already names the per-user default (project names are deployment-global).
    ENTIRE_REPO_PATH="${FM_ENTIRE_REPOS:-$SUG_NATIVE_REPO}"
    if [ -z "${FM_ENTIRE_PROJECT:-}" ] && [ -z "${FM_ENTIRE_REPO_NAME:-}" ]; then
      ENTIRE_PROJECT="$(cut -d/ -f2 <<<"$ENTIRE_REPO_PATH")"
      ENTIRE_REPO_NAME="$(cut -d/ -f3 <<<"$ENTIRE_REPO_PATH")"
    else
      ENTIRE_PROJECT="${FM_ENTIRE_PROJECT:-forgemark-$GH_USER}"
      ENTIRE_REPO_NAME="${FM_ENTIRE_REPO_NAME:-forgemark-target}"
      ENTIRE_REPO_PATH="et/$ENTIRE_PROJECT/$ENTIRE_REPO_NAME"
    fi
    if ! entire repo list "$ENTIRE_PROJECT" >/dev/null 2>&1; then
      ENTIRE_HANDLE="$(entire auth status | awk '/User:/{sub(/^@/, "", $2); print $2}')"
      echo "compare-local: creating project $ENTIRE_PROJECT (region $SUG_SLUG)"
      entire project create "$ENTIRE_PROJECT" --owner "github:$ENTIRE_HANDLE" --owner-type account --region "$SUG_SLUG"
    fi
    if ! entire repo get "$ENTIRE_REPO_NAME" --project "$ENTIRE_PROJECT" >/dev/null 2>&1; then
      echo "compare-local: creating native repo $ENTIRE_PROJECT/$ENTIRE_REPO_NAME on $ENTIRE_CLUSTER"
      entire repo create "$ENTIRE_REPO_NAME" --project "$ENTIRE_PROJECT" --cluster-host "$ENTIRE_CLUSTER"
    fi
  else
    # Mirror mode: own upstream per target (see FM_ENTIRE_UPSTREAM above).
    # Mirror create is idempotent on (upstream, cluster) and waits for the
    # initial GitHub→EntireDB clone so the first push has a repo to land in.
    ENTIRE_UPSTREAM="${FM_ENTIRE_UPSTREAM:-$GH_USER/forgemark-target-entire}"
    ensure_repo "$ENTIRE_UPSTREAM"
    echo "compare-local: ensuring mirror of $ENTIRE_UPSTREAM on $ENTIRE_CLUSTER"
    entire repo mirror create "https://github.com/$ENTIRE_UPSTREAM" "$ENTIRE_CLUSTER"
    ENTIRE_REPO_PATH="gh/$ENTIRE_UPSTREAM"
  fi
  export FM_ENTIRE_REPOS="${FM_ENTIRE_REPOS:-$ENTIRE_REPO_PATH}"
  ENTIRE_ENABLED=1
fi

# Targets carry secret_source, never a secret: the server execs `gh auth
# token` / `entire auth token` itself at run start.
export ENTIRE_ENABLED CONCURRENCY DURATION_SEC WARMUP_SEC
BODY="$(python3 - <<'PY'
import json, os

def opt_int(workload, key, env):
    v = os.environ.get(env)
    if v:
        workload[key] = int(v)

targets = [{
    "name": "github",
    "remote": "https://github.com",
    "repos": [os.environ["GH_REPO"]],
    "secret_source": "gh",
}]
if os.environ["ENTIRE_ENABLED"] == "1":
    targets.append({
        "name": os.environ["FM_ENTIRE_NAME"],
        "remote": os.environ["FM_ENTIRE_REMOTE"],
        "repos": [r for r in os.environ["FM_ENTIRE_REPOS"].split(",") if r],
        "secret_source": "entire",
        "token_url": os.environ["FM_ENTIRE_TOKEN_URL"],
        "jurisdiction": os.environ["FM_ENTIRE_JURISDICTION"],
    })

workload = {
    "strategy": "branch",
    "branch_prefix": "bench/",
    "concurrency": [int(c) for c in os.environ["CONCURRENCY"].split(",")],
    "duration_sec": float(os.environ["DURATION_SEC"]),
    "warmup_sec": float(os.environ["WARMUP_SEC"]),
}
opt_int(workload, "files_min", "FM_FILES_MIN")
opt_int(workload, "files_max", "FM_FILES_MAX")
opt_int(workload, "file_size", "FM_FILE_SIZE")

print(json.dumps({
    "confirm_authorized": True,  # your repos on both sides — see the README warning
    "workload": workload,
    "targets": targets,
}))
PY
)"

# One POST, body and status captured together: a retry-to-print-the-error
# would itself start a benchmark run.
HTTP_RESP="$(curl -s -w $'\n%{http_code}' -X POST "http://$ADDR/api/runs" -H 'Content-Type: application/json' --data-binary @- <<<"$BODY")"
CODE="${HTTP_RESP##*$'\n'}"
RESP="${HTTP_RESP%$'\n'*}"
if [ "$CODE" != "201" ]; then
  echo "compare-local: starting the run failed (HTTP $CODE): $RESP" >&2
  exit 1
fi

RUN_ID="$(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); print(d["id"])' "$RESP")"
python3 -c 'import json,sys; [print("compare-local: warning:", w) for w in json.loads(sys.argv[1]).get("warnings") or []]' "$RESP"

URL="http://$ADDR/#${FM_VIEW:-run}/$RUN_ID"
echo "compare-local: run $RUN_ID started — $URL"
command -v open >/dev/null && open "$URL" || true
