#!/usr/bin/env bash
# login.sh — authenticate the forge CLIs the GUI can pull credentials from:
# gh (GitHub, required by compare-local.sh) and entire (EntireDB, required).
# glab (GitLab) is optional: if installed, it's logged in too so GitLab shows
# up in the GUI's "Add from CLI logins". Idempotent — valid logins are left
# alone, missing ones launch the CLI's own interactive login flow.
set -euo pipefail

# Pin gh to github.com so this login (and the token forgemark later reads with
# --hostname github.com) target the same forge, regardless of any GH_HOST set.
export GH_HOST=github.com
command -v gh >/dev/null || { echo "login: gh CLI not found — install it from https://cli.github.com" >&2; exit 1; }
command -v entire >/dev/null || { echo "login: entire CLI not found — install it from https://docs.entire.io" >&2; exit 1; }

if gh auth status >/dev/null 2>&1; then
  echo "login: gh — already authenticated as $(gh api user --jq .login)"
else
  echo "login: gh — not authenticated, starting gh auth login"
  gh auth login
fi

if command -v glab >/dev/null; then
  if glab auth status >/dev/null 2>&1; then
    echo "login: glab — already authenticated"
  else
    echo "login: glab — not authenticated, starting glab auth login"
    glab auth login
  fi
else
  echo "login: glab CLI not found — skipping GitLab (install from https://gitlab.com/gitlab-org/cli to enable it)"
fi

if ENTIRE_STATUS="$(entire auth status 2>/dev/null)"; then
  echo "login: entire — already authenticated ($(awk '/User:/{print $2}' <<<"$ENTIRE_STATUS") on $(awk '/Context:/{print $2}' <<<"$ENTIRE_STATUS"))"
else
  echo "login: entire — not authenticated, starting entire login"
  entire login
fi

echo "login: CLIs ready — run scripts/compare-local.sh"
