#!/usr/bin/env bash
# Runs the index definition-of-done journey (#93 Surface A) against the FULL
# standalone stack — the only stack where an index run can happen at all.
#
#   apps/elitea-web/scripts/index-stream-e2e.sh            # up + seed + run
#   apps/elitea-web/scripts/index-stream-e2e.sh --keep     # leave the stack up
#   INDEX_STREAM_REPEAT=3 …/index-stream-e2e.sh            # flake check
#
# Why not the E2E stack: `docker-compose.e2e-standalone.yml` has no runtime
# plane and no agent worker, so the run would never leave the start route and
# the journey would fail there for a reason unrelated to the code under test.
#
# The stack runs under its own compose project so it can never disturb one
# somebody else started. Note that oidc-mock's port 9400 cannot be remapped
# (the issuer is derived from the Host header), so only one such stack can be
# up at a time.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
WEB_DIR="${REPO_ROOT}/apps/elitea-web"

PROJECT="${INDEX_STREAM_PROJECT:-elitea-indexstream}"
PORT="${INDEX_STREAM_PORT:-8087}"
# The mock provider's request journal (#470). The journey reads it to learn
# which model name the gateway put on the wire and which project's credential
# it used. 8091 rather than the compose default 8090, so this stack can sit
# beside a plain standalone one.
MOCK_PORT="${INDEX_STREAM_MOCK_PORT:-8091}"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

cleanup() {
  if [ "$KEEP" -eq 0 ]; then
    echo "→ Tearing down ${PROJECT}…"
    STANDALONE_PROJECT="$PROJECT" \
      "${REPO_ROOT}/deploy/scripts/standalone-stack.sh" down -v >/dev/null 2>&1 || true
  else
    echo "→ Leaving ${PROJECT} up (--keep). Tear down with:"
    echo "   STANDALONE_PROJECT=${PROJECT} deploy/scripts/standalone-stack.sh down -v"
  fi
}
trap cleanup EXIT

run_stack() {
  STANDALONE_PROJECT="$PROJECT" STANDALONE_PORT="$PORT" STANDALONE_MOCK_PORT="$MOCK_PORT" \
    "${REPO_ROOT}/deploy/scripts/standalone-stack.sh" "$@"
}

echo "→ Runtime PKI + secrets (idempotent)…"
run_stack certs

echo "→ Bringing up ${PROJECT} on :${PORT}…"
# `up` reuses an image that already exists, so a source change lands only if the
# build is asked for explicitly. Not a nicety here: the whole point of this
# journey is that the backend serves REAL argument schemas, and a stale
# elitea-main image serves the `{"type":"object"}` placeholder — which reads as
# "the form is broken" rather than "the harness ran last week's binary".
# STANDALONE_SKIP_BUILD=1: the continuous-integration job already built and
# loaded every image (.github/actions/build-stack-images) and asserted each is
# present; see the same guard in chat-stream-e2e.sh.
if [ "${STANDALONE_SKIP_BUILD:-0}" = "1" ]; then
  echo "→ STANDALONE_SKIP_BUILD=1: images were built and asserted by the caller; not rebuilding"
else
  run_stack build
fi
# `up` exits non-zero when a service declares no healthcheck even though the
# stack is fine, so readiness is asserted below rather than taken from it.
run_stack up || true

echo "→ Waiting for the app to answer on :${PORT}…"
for _ in $(seq 1 60); do
  if curl -sf -o /dev/null "http://localhost:${PORT}/app/"; then break; fi
  sleep 2
done
curl -sf -o /dev/null "http://localhost:${PORT}/app/" || {
  echo "ERROR: the stack never served /app/ on :${PORT}" >&2
  exit 1
}

run_stack seed
run_stack seed-runtime
run_stack seed-llm
# The three rows the start route cannot resolve a toolkit without: a vector
# store, an embedding model and an indexable toolkit. Skipping this leaves the
# Indexes tab present but every run failing at resolution.
run_stack seed-index

# The gateway-facing half of the embedding path (#470): the negative direction
# and the public-project scope, neither of which a browser journey can reach.
# Read its assertion lines — it reports how many assertions ran, and a run that
# skips one exits non-zero.
echo "→ Embedding path assertions…"
STANDALONE_PROJECT="$PROJECT" \
  "${REPO_ROOT}/deploy/scripts/embedding-path-check.sh"

echo "→ Running the index-stream journey…"
cd "$WEB_DIR"
# Built as a flat string, not an array: macOS ships bash 3.2, where expanding an
# EMPTY array under `set -u` is an "unbound variable" error rather than nothing.
REPEAT_ARGS=""
[ -n "${INDEX_STREAM_REPEAT:-}" ] && REPEAT_ARGS="--repeat-each ${INDEX_STREAM_REPEAT}"

# E2E_REUSE_STACK=1 stops Playwright's own `webServer` from trying to bring up
# the E2E stack: the stack this journey needs is already up, and that hook
# points at a different compose file entirely.
#
# `--workers 1` is not a flake workaround, it is the journey's scope. Both tests
# drive the SAME toolkit through the SAME single-consumer execution plane, so
# running copies against each other measures the stack's concurrency rather than
# the feature. Measured: spread over three workers, elitea-main's pool saturated
# (`list project configurations` timed out) and the create-index form failed to
# render inside 20s — which reads exactly like the defect this journey exists to
# detect. The project's `fullyParallel: false` covers tests within the file;
# this covers `--repeat-each` copies, which Playwright otherwise spreads.
#
# When PLAYWRIGHT_CONTAINER_IMAGE is set (ci-web-e2e.yml sets it), the tests run
# inside the pinned Playwright container instead of on the host — see the
# identical block in chat-stream-e2e.sh for the full reasoning. In short: the
# image replaces `npx playwright install --with-deps`; `--network host` because
# the stack (and the mock journal on :${MOCK_PORT}) live on the HOST; and the
# image TAG stays in the workflow because check-playwright-image-tag.mjs only
# scans .github/workflows/, so a tag written here would escape the version gate.
# shellcheck disable=SC2086 -- REPEAT_ARGS is deliberately word-split
if [ -n "${PLAYWRIGHT_CONTAINER_IMAGE:-}" ]; then
  # See chat-stream-e2e.sh's identical block: the workflow backgrounds a
  # `docker pull` of this image earlier in the job but never waits on it or
  # checks whether it won, so a stalled or failed pull would otherwise fall
  # through to this `docker run` pulling inline once, unbounded and
  # unretried — the failure mode behind PR #1001. This call waits: it returns
  # instantly if the background pull already finished, and retries with a
  # bounded backoff if it did not.
  CONTAINER_BIN="${CONTAINER_BIN:-docker}" \
    "${REPO_ROOT}/scripts/ci/pull-third-party-images.sh" --image "$PLAYWRIGHT_CONTAINER_IMAGE"
  "${CONTAINER_BIN:-docker}" run --rm --network host \
    -v "$WEB_DIR":/work -w /work \
    -e CI="${CI:-}" \
    -e E2E_REUSE_STACK=1 \
    -e PLAYWRIGHT_BASE_URL="http://localhost:${PORT}" \
    -e STANDALONE_MOCK_PORT="${MOCK_PORT}" \
    "$PLAYWRIGHT_CONTAINER_IMAGE" \
    npx playwright test --project=index-stream --workers 1 $REPEAT_ARGS
else
  PLAYWRIGHT_BASE_URL="http://localhost:${PORT}" \
  STANDALONE_MOCK_PORT="${MOCK_PORT}" \
  E2E_REUSE_STACK=1 \
    npx playwright test --project=index-stream --workers 1 $REPEAT_ARGS
fi
