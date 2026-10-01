#!/usr/bin/env bash
# Pulls every THIRD-PARTY (non-`build:`) image referenced by the given compose
# files, with a bounded retry and exponential backoff.
#
# Why: three CI runs in one day died before any test ran with a Docker Hub
# connection reset while `compose up` pulled a base image (redis:7-alpine,
# postgres:18) inline, on the chat-stream shards of ci-web-e2e.yml. Pulling
# up front, with retries, turns a transient registry hiccup into a clearly
# named, retried step instead of an opaque `compose up` failure deep inside a
# stack script.
#
# `.github/actions/build-stack-images`'s own guard step asserts every image a
# compose file BUILDS is already loaded locally before `up` runs (issue
# #154) — that is deliberately NOT this script's job: pulling a build-tagged
# image here would silently paper over a guard failure with a stale registry
# tag instead of failing loud. `select(.build == null)` below is what keeps
# the two scripts from overlapping.
#
# Usage:
#   scripts/ci/pull-third-party-images.sh <compose-file> [<compose-file> ...]
#   scripts/ci/pull-third-party-images.sh --image <ref> [<ref> ...]
#
# Paths are resolved exactly as given (relative to the CALLER's cwd, or
# absolute) and handed to `compose -f` in the same order a caller would use
# them, so profile-scoped services follow the same COMPOSE_PROFILES /
# --profile rules as the real `up` — set them in the environment before
# calling this script if a caller needs a profile-gated image included.
#
# `--image` skips the compose lookup entirely and pulls the given ref(s)
# directly, under the same "already present locally / 5 attempts, 5s→80s
# backoff" loop below. This is what a caller with no compose file at hand
# uses — the Playwright container (`mcr.microsoft.com/playwright:...`), which
# every ci-web-e2e.yml job and its shared scripts (chat-stream-e2e.sh,
# index-stream-e2e.sh) run via `docker run`, not `compose up`. A background
# `docker pull` before the build step can save wall time, but it must never
# be the LAST word on whether the image is present: calling this script again
# right before the `docker run` that needs it is what turns "the background
# pull silently failed or is still in flight" into a bounded, retried wait —
# `docker image inspect` below returns instantly if the background pull
# already won, and a concurrent `docker pull` of the same ref the daemon
# already has in flight simply attaches to it rather than restarting it.
set -euo pipefail

CONTAINER_BIN="${CONTAINER_BIN:-docker}"

if [ "${1:-}" = "--image" ]; then
  shift
  if [ "$#" -eq 0 ]; then
    echo "usage: $0 --image <ref> [<ref> ...]" >&2
    exit 1
  fi
  images="$(printf '%s\n' "$@" | sort -u)"
else
  REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
  # shellcheck source=../../apps/elitea-web/scripts/lib/compose-detect.sh
  . "${REPO_ROOT}/apps/elitea-web/scripts/lib/compose-detect.sh"
  detect_compose_bin

  if [ "$#" -eq 0 ]; then
    echo "usage: $0 <compose-file> [<compose-file> ...]" >&2
    exit 1
  fi

  args=()
  for f in "$@"; do
    args+=(-f "$f")
  done

  # shellcheck disable=SC2086
  images="$($COMPOSE_BIN "${args[@]}" config --format json | jq -r '.services[] | select(.build == null) | .image' | sort -u)"
fi

if [ -z "$images" ]; then
  echo "no third-party images to pull"
  exit 0
fi

echo "third-party images to pull:"
echo "$images" | sed 's/^/  /'

max_attempts=5
base_delay=5
overall_status=0
while IFS= read -r img; do
  [ -n "$img" ] || continue
  # Already loaded locally — e.g. an overlay that points a service at an
  # image THIS job built a step earlier (deepwiki-real-engine.yml's
  # `local-engine` tag, never published to any registry) has no `build:` key
  # of its own by the time compose sees it, so it reads exactly like a
  # third-party pull from this script's side. Skip it: a `docker pull` of a
  # tag that only ever existed locally fails every attempt for no benefit.
  if "$CONTAINER_BIN" image inspect "$img" >/dev/null 2>&1; then
    echo "already present locally: $img"
    continue
  fi
  attempt=1
  delay="$base_delay"
  while true; do
    if "$CONTAINER_BIN" pull -q "$img"; then
      break
    fi
    if [ "$attempt" -ge "$max_attempts" ]; then
      echo "::error::failed to pull $img after $attempt attempts" >&2
      overall_status=1
      break
    fi
    echo "pull of $img failed (attempt $attempt/$max_attempts); retrying in ${delay}s" >&2
    sleep "$delay"
    attempt=$((attempt + 1))
    delay=$((delay * 2))
  done
done <<< "$images"

exit "$overall_status"
