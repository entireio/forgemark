#!/usr/bin/env bash
# pushrace.sh — the demo: one flight of agents pushing checkpoint-sized
# commits at every forge simultaneously for one minute, rendered as a live
# head-to-head race (the GUI's #race view — big counters, lanes, a podium).
#
# All the auth/provisioning plumbing comes from compare-local.sh; this just
# presets the workload. Override any FM_* variable it documents, e.g.:
#
#   scripts/pushrace.sh                       # 16 agents, 60s
#   FM_CONCURRENCY=32 scripts/pushrace.sh     # careful on github.com — abuse limits
set -euo pipefail

export FM_CONCURRENCY="${FM_CONCURRENCY:-16}"
export FM_DURATION_SEC="${FM_DURATION_SEC:-60}"
export FM_WARMUP_SEC="${FM_WARMUP_SEC:-0}"    # a race counts from the gun
export FM_FILES_MIN="${FM_FILES_MIN:-1}"      # checkpoint-sized commits:
export FM_FILES_MAX="${FM_FILES_MAX:-3}"      # 1-3 files x 1KB
export FM_FILE_SIZE="${FM_FILE_SIZE:-1024}"
export FM_ENTIRE_NATIVE="${FM_ENTIRE_NATIVE:-1}"  # race EntireDB itself, not the
                                                  # GitHub write-through mirror
export FM_VIEW=race

exec "$(cd "$(dirname "$0")" && pwd)/compare-local.sh"
