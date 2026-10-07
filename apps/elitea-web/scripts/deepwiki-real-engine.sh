#!/usr/bin/env bash
# DWIKI-014 — the REAL DeepWiki engine through the product (Playwright
# project `deepwiki-real-engine`) against the standalone stack, with the
# deterministic LLM stub in the mock's place. The engine is the Rust-native
# one (ADR-0026; deploy/docker-compose.deepwiki-native-real-engine.yml over
# the standalone stack, which runs the native sidecar already). Its clone
# speaks HTTPS only, so the repository under analysis is a real public one
# on github.com: kharkevich-engineering-lab/floe (its owner agreed to this
# use). Needs internet access.
#
#   apps/elitea-web/scripts/deepwiki-real-engine.sh            # up + seed + run
#   apps/elitea-web/scripts/deepwiki-real-engine.sh --keep     # leave the stack up
#
# THE NATIVE PIN. The product clones a branch or a tag head (depth 1), never
# a commit, so the pin is CHECKED, not requested: before anything is built,
# `git ls-remote` resolves DEEPWIKI_NATIVE_REF (default `main`) on
# DEEPWIKI_NATIVE_REPOSITORY_URL, and the run stops when the head is not
# DEEPWIKI_NATIVE_COMMIT (default the pinned commit below). The journey then
# requires the published wiki to name that commit. Set
# DEEPWIKI_NATIVE_COMMIT= (empty, or `none`) to analyse whatever the head is; the
# journey still requires the wiki to name the head resolved here.
#
# The engine image is built here once, from
# services/elitea-deepwiki-engine/Containerfile, when DEEPWIKI_ENGINE_IMAGE
# is not present locally, and never by compose — the overlay resets the
# services' `build:` so the stack's own `compose build` leaves them alone.
# Rebuild after an engine change with
#   DEEPWIKI_ENGINE_REBUILD=1 apps/elitea-web/scripts/deepwiki-real-engine.sh
#
# CI: .github/workflows/deepwiki-real-engine.yml, manual + weekly (Sundays
# 04:23 UTC) — never on pull_request; the fixture-engine job in
# ci-web-e2e.yml is the per-change gate.
#
# Everything else is chat-stream-e2e.sh (stack, certificates, seeds). Its own
# compose project and port keep its state apart from the fixture stack
# (deepwiki-e2e.sh), but not UP beside it: oidc-mock's port is fixed at 9400,
# so a kept standalone stack of any flavour must come down first.
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
export PLAYWRIGHT_PROJECT=deepwiki-real-engine
# The stub replaces the LLM mock, so the stack's own `check` (written against
# the mock's journal and canned answers) does not apply here; deepwiki-e2e.sh
# runs it on the same stack shape with the mock in place.
export CHAT_STREAM_SKIP_CHECK=1
# The engine's model calls bill the wiki toolkit's project (the E2E seed's
# 90200), and the gateway resolves a model per project: seed the mock chat
# and embedding rows there too (standalone-stack.sh seed-llm / seed-index).
export SEED_EXTRA_PROJECTS="${SEED_EXTRA_PROJECTS:-90200}"
export CHAT_STREAM_PROJECT="${DEEPWIKI_REAL_PROJECT:-elitea-deepwiki-real}"
export CHAT_STREAM_PORT="${DEEPWIKI_REAL_PORT:-8087}"
# The runtime that holds the images is the one compose runs on: CI sets
# COMPOSE_BIN="docker compose" and bakes the engine into docker's store,
# while the runner ALSO has podman, which would not see that image and would
# build it again (with a builder that lacks Dockerfile heredocs).
case "${COMPOSE_BIN:-}" in
  docker*) CONTAINER_BIN="${CONTAINER_BIN:-docker}" ;;
  podman*) CONTAINER_BIN="${CONTAINER_BIN:-podman}" ;;
  *) CONTAINER_BIN="${CONTAINER_BIN:-$(command -v podman || command -v docker)}" ;;
esac

# The legacy (Python) engine is retired; refuse a caller that still asks for it.
case "${DEEPWIKI_REAL_ENGINE:-native}" in
  native) ;;
  *)
    echo "ERROR: DEEPWIKI_REAL_ENGINE must be native (got '${DEEPWIKI_REAL_ENGINE}'); the Python engine is retired." >&2
    exit 1
    ;;
esac
export DEEPWIKI_ENGINE_IMAGE="${DEEPWIKI_ENGINE_IMAGE:-ghcr.io/eliteaai/elitea-deepwiki-engine-native:local}"
ENGINE_CONTAINERFILE="${REPO_ROOT}/services/elitea-deepwiki-engine/Containerfile"
ENGINE_OVERLAY="${REPO_ROOT}/deploy/docker-compose.deepwiki-native-real-engine.yml"
NATIVE_OWNER_REPO="${DEEPWIKI_NATIVE_REPOSITORY:-kharkevich-engineering-lab/floe}"
NATIVE_URL="https://github.com/${NATIVE_OWNER_REPO}"
NATIVE_REF="${DEEPWIKI_NATIVE_REF:-main}"
# `-` and not `:-`: an EMPTY value is the stated "no pin" choice.
# `none` says the same (a workflow input cannot be dispatched empty).
NATIVE_COMMIT="${DEEPWIKI_NATIVE_COMMIT-89c2197fa88a910c9b8344ae3c4dd06618ed28ae}"
[ "$NATIVE_COMMIT" = "none" ] && NATIVE_COMMIT=""
echo "→ Resolving ${NATIVE_URL} ${NATIVE_REF}…"
# Branch first, then the tag of that name: the order the engine's own
# ls-remote uses. `^{}` peels an annotated tag to its commit.
REMOTE_REFS="$(git ls-remote "$NATIVE_URL" "refs/heads/${NATIVE_REF}" "refs/tags/${NATIVE_REF}" "refs/tags/${NATIVE_REF}^{}")"
HEAD_SHA="$(printf '%s\n' "$REMOTE_REFS" | awk -v r="refs/heads/${NATIVE_REF}" '$2 == r { print $1 }')"
if [ -z "$HEAD_SHA" ]; then
  HEAD_SHA="$(printf '%s\n' "$REMOTE_REFS" | awk -v r="refs/tags/${NATIVE_REF}^{}" '$2 == r { print $1 }')"
fi
if [ -z "$HEAD_SHA" ]; then
  HEAD_SHA="$(printf '%s\n' "$REMOTE_REFS" | awk -v r="refs/tags/${NATIVE_REF}" '$2 == r { print $1 }')"
fi
if [ -z "$HEAD_SHA" ]; then
  echo "ERROR: ${NATIVE_URL} has no branch or tag '${NATIVE_REF}'." >&2
  exit 1
fi
if [ -n "$NATIVE_COMMIT" ] && [ "$HEAD_SHA" != "$NATIVE_COMMIT" ]; then
  echo "ERROR: ${NATIVE_URL} ${NATIVE_REF} is at ${HEAD_SHA}, not the pinned ${NATIVE_COMMIT}." >&2
  echo "       The product clones a branch or tag head, never a commit, so the pin" >&2
  echo "       can only be checked. Re-run with DEEPWIKI_NATIVE_REF set to a tag at" >&2
  echo "       the pin, move the pin (DEEPWIKI_NATIVE_COMMIT, and the workflow's" >&2
  echo "       default), or set DEEPWIKI_NATIVE_COMMIT= to analyse the head." >&2
  exit 1
fi
echo "   ${NATIVE_REF} is at ${HEAD_SHA}${NATIVE_COMMIT:+ (the pin)}."
# What the journey drives and asserts (forwarded into the Playwright
# container by name, see chat-stream-e2e.sh).
export E2E_REAL_ENGINE_REPOSITORY="$NATIVE_OWNER_REPO"
export E2E_REAL_ENGINE_BRANCH="$NATIVE_REF"
export E2E_REAL_ENGINE_COMMIT="$HEAD_SHA"
# The anonymous repository toolkit `seed-deepwiki-public` writes: the
# seed's 9010 carries a literal token GitHub refuses with 401.
export E2E_REAL_ENGINE_CODE_TOOLKIT=9011
export CHAT_STREAM_EXTRA_SEEDS="seed-deepwiki-public ${CHAT_STREAM_EXTRA_SEEDS:-}"

# `image inspect`, not podman's `image exists`: `docker image exists` is not a
# docker subcommand, so under docker the probe always failed and the image was
# rebuilt from scratch every run — including in CI, where the workflow has
# already baked the tag. `image inspect` is present in both runtimes.
if [ -n "${DEEPWIKI_ENGINE_REBUILD:-}" ] || ! "$CONTAINER_BIN" image inspect "$DEEPWIKI_ENGINE_IMAGE" >/dev/null 2>&1; then
  echo "→ Building the native engine image ${DEEPWIKI_ENGINE_IMAGE} (once; minutes)…"
  "$CONTAINER_BIN" build -f "$ENGINE_CONTAINERFILE" -t "$DEEPWIKI_ENGINE_IMAGE" "$REPO_ROOT"
fi
export STANDALONE_OVERLAY="${ENGINE_OVERLAY} ${STANDALONE_OVERLAY:-}"
exec "$(dirname "$0")/chat-stream-e2e.sh" "$@"
