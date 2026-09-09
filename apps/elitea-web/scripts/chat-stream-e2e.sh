#!/usr/bin/env bash
# Runs the chat definition-of-done journey (#284) against the FULL standalone
# stack — the only stack where an agent turn can happen at all.
#
#   apps/elitea-web/scripts/chat-stream-e2e.sh            # up + seed + run
#   apps/elitea-web/scripts/chat-stream-e2e.sh --keep     # leave the stack up
#   CHAT_STREAM_REPEAT=3 …/chat-stream-e2e.sh            # flake check
#   STANDALONE_WORKER=rust …/chat-stream-e2e.sh          # native Rust runtime
#   COMPOSE_PROFILES=real-llm LLM_PROVIDER=vllm LLM_API_BASE=http://llm-real:8090 \
#     LLM_MODEL=qwen3-0.6b LLM_EMBEDDING_MODEL=vllm/nomic-embed-text-v1.5 \
#     PLAYWRIGHT_PROJECT=chat-stream-real E2E_CHAT_MODEL=vllm/qwen3-0.6b \
#     …/chat-stream-e2e.sh                               # real-model lane
#
# The real-model lane changes nothing in this script's lifecycle: the LLM_*
# variables reach `seed-llm` through the environment every `run_stack` call
# inherits, COMPOSE_PROFILES reaches compose the same way, and the mock stays
# up beside the real model because `check` still asserts against it. What the
# lane DOES change is which Playwright project runs and which model name the
# specs expect (E2E_CHAT_MODEL), both forwarded below. See the `llm-real`
# service in deploy/docker-compose.standalone-full.yml for what this lane can
# and cannot assert.
#
# STANDALONE_WORKER is read by deploy/scripts/standalone-stack.sh, not by this
# script, and reaches it through the environment every `run_stack` call
# inherits. It is written down here because the journeys are the only thing
# that can tell the two runtimes apart: the same UI drives both, and a change
# that admits one and refuses the other passes every unit suite. Use a distinct
# CHAT_STREAM_PROJECT and CHAT_STREAM_PORT when running both on one host — the
# oidc-mock port is fixed at 9400 and cannot be shared, so they cannot be up at
# the same time.
#
# Why not the E2E stack: `docker-compose.e2e-standalone.yml` has no runtime
# plane, no worker and no model backend, so the journey would fail there for a
# reason that has nothing to do with the code under test.
#
# The stack runs under its own compose project so it can never disturb one
# somebody else started, and the mock LLM is slowed to a human-visible token
# rate so "a partial answer was painted" is a deterministic observation rather
# than a race.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
WEB_DIR="${REPO_ROOT}/apps/elitea-web"

PROJECT="${CHAT_STREAM_PROJECT:-elitea-chatstream}"
PORT="${CHAT_STREAM_PORT:-8084}"
# Per-chunk delay for the mock. 400ms x the mock's word-per-chunk reply keeps
# the whole turn well under the journey's timeouts while making each token a
# separate paint — comfortably longer than a frame, so "a partial answer was
# rendered" is an observation rather than a race.
DELAY_MS="${MOCK_LLM_CHUNK_DELAY_MS:-400}"
KEEP=0
[ "${1:-}" = "--keep" ] && KEEP=1

cleanup() {
  # The exit status of whatever is unwinding, read FIRST: every command below
  # overwrites it.
  status=$?
  # `${CHECK_LOG:-}` because this function is installed before that variable
  # exists, and `set -u` would abort the trap itself on an early failure.
  rm -f "${CHECK_LOG:-}" 2>/dev/null || true
  # THE STACK'S OWN ACCOUNT OF A FAILURE, BEFORE THE TEARDOWN TAKES IT AWAY.
  #
  # This script owns the whole lifecycle, so a workflow step that runs after it
  # has no stack left to ask — and the browser artefacts cannot answer the
  # questions these journeys fail on. A turn that offered the model no tool
  # looks, in the report, exactly like a turn whose tool was never called: only
  # the worker says which, and only elitea-main says why it refused the call the
  # worker made. Measured cost of not having it: a whole investigation for one
  # refused sub-agent read (#850).
  #
  # `|| true` because a diagnostic that fails the run it is diagnosing replaces
  # one unexplained failure with another.
  if [ "$status" -ne 0 ]; then
    echo "→ Stack logs (${PROJECT}) — the run exited ${status}:"
    STANDALONE_PROJECT="$PROJECT" MOCK_LLM_CHUNK_DELAY_MS="$DELAY_MS" \
      "${REPO_ROOT}/deploy/scripts/standalone-stack.sh" logs 2>&1 || true
  fi
  if [ "$KEEP" -eq 0 ]; then
    echo "→ Tearing down ${PROJECT}…"
    STANDALONE_PROJECT="$PROJECT" MOCK_LLM_CHUNK_DELAY_MS="$DELAY_MS" \
      "${REPO_ROOT}/deploy/scripts/standalone-stack.sh" down -v >/dev/null 2>&1 || true
  else
    echo "→ Leaving ${PROJECT} up (--keep). Tear down with:"
    echo "   STANDALONE_PROJECT=${PROJECT} deploy/scripts/standalone-stack.sh down -v"
  fi
}
trap cleanup EXIT

run_stack() {
  STANDALONE_PROJECT="$PROJECT" STANDALONE_PORT="$PORT" MOCK_LLM_CHUNK_DELAY_MS="$DELAY_MS" \
    "${REPO_ROOT}/deploy/scripts/standalone-stack.sh" "$@"
}

echo "→ Runtime PKI + secrets (idempotent)…"
run_stack certs

echo "→ Bringing up ${PROJECT} on :${PORT} (worker=${STANDALONE_WORKER:-python}, profiles=${COMPOSE_PROFILES:-none}, mock chunk delay ${DELAY_MS}ms)…"
# `up` reuses an image that already exists, so a source change lands only if
# the build is asked for explicitly. That is not a nicety here: the whole
# incremental-render assertion depends on the mock's per-chunk delay actually
# running, and a stale mock streams the reply in one burst — which reads as
# "the UI does not stream" rather than "the harness did not slow anything
# down". Measured: chunk frames 33ms apart with the delay set to 600.
#
# STANDALONE_SKIP_BUILD=1 is set by the continuous-integration jobs, which
# have already built and loaded every image through
# .github/actions/build-stack-images (buildx bake, one shared layer cache per
# image, the cache mounts carried across runs) and asserted each one is in the
# local store. Running `compose build` again here would rebuild them from
# scratch on the daemon's own builder, cache-less — the 5-to-10-minute cost
# that action exists to remove. Locally the variable is unset and the build
# runs, for the reason above.
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

# ── Assert the stack before driving it (#368) ────────────────────────────────
#
# `check` holds the repository's only runtime proof of the #326 edge
# identity-header strip: it forges an X-Auth-ID and requires 401 or 403, it
# requires a real personal access token on the SAME route to answer 200 and to
# echo its own user, and it requires a forged header not to override a genuine
# bearer. Those assertions existed and no continuous-integration job ran them,
# so deleting the middleware from an edge file passed every gate.
#
# This script runs `check` rather than a new workflow job. The full standalone
# stack is the only stack the assertions can run against, this script is the
# only thing in continuous integration that stands one up, and building it takes
# about six minutes. A second job would pay that cost again for the same
# assertions.
#
# KNOW WHAT THAT BUYS AND WHAT IT DOES NOT. The check now starts exactly where
# the `chat-stream` job in .github/workflows/ci-web-e2e.yml starts: on every
# pull request that touches deploy/**, services/elitea-main/**,
# apps/elitea-web/**, tools/uictl/**, docker-bake.hcl or that workflow file.
# Both edge files live under deploy/, so an edit to either one starts it. A pull
# request that touches none of those paths does not start it — a workflow-only
# or docs-only change, for instance. The static gates in
# services/elitea-main/tests/deployedge/ cover that gap: the No Binaries
# workflow is not path-filtered and runs them on every pull request.
#
# It runs BEFORE the journey on purpose. Every assertion in `check` is a
# precondition of an agent turn — the mTLS listeners, the worker's consumer
# registration, the active personal access token, the model hop. When one of
# them is broken, the journey fails too, with a message about a button.
echo "→ Asserting the stack (gateway mTLS, runtime plane, #326 edge strip)…"
# An explicit template, not `mktemp -t <prefix>`: BSD mktemp reads that form as
# a prefix and GNU mktemp reads it as a template that needs its own X's, so the
# short form works on one platform and fails on the other. This repository has
# already been bitten by that difference.
CHECK_LOG="$(mktemp "${TMPDIR:-/tmp}/chat-stream-check.XXXXXX")"
# `--allow-skips` is a STATED choice, and it is stated here rather than assumed
# inside `check` (#429). This journey seeds the chat plane and not the index
# plane, so `check`'s index-toolkit assertion cannot run. On the real-model
# lane (LLM_PROVIDER=vllm at llm-real) two more are given up, and `check`
# names each: the embedding-path check and the elitea-sdk client check both
# assert against llm-mock's request journal, nonce echo and 1536-wide
# vectors, which a real upstream does not produce; the hops themselves are
# still asserted (completion, vector width, model list). Since #429 an
# unmeasured assertion exits non-zero by default, which is right for an operator
# and wrong for this caller alone.
#
# The flag accepts a SKIP. It does not accept a FAILURE: `check` still exits
# non-zero on any failed assertion, and the loop below still requires the three
# #326 lines by name. Do not add `--allow-skips` anywhere the caller has not
# written down which assertion it gives up, and why.
#
# It does not accept a MISSING assertion either (#422). `check` states how many
# assertions it makes, and a run that reports fewer results than that exits
# non-zero whatever this flag says. That rule matters most on this path: one
# guard inside `check` stands for eleven assertions and raises a single skip, so
# without the count this caller would accept eleven unmeasured assertions as
# one stated skip.
# CHAT_STREAM_SKIP_CHECK: a caller that REPLACES the LLM mock (the DeepWiki
# real-engine runner puts a deterministic engine stub at llm-mock:8090) cannot
# run `check` — its completion, journal and SDK assertions are written against
# the mock's own protocol and fail against any other upstream. That caller
# says so by name, here, and gets the journey alone; every other caller keeps
# the full assertion set.
if [ -n "${CHAT_STREAM_SKIP_CHECK:-}" ]; then
  echo "→ Skipping the stack check (CHAT_STREAM_SKIP_CHECK=${CHAT_STREAM_SKIP_CHECK}: the LLM mock is replaced on this stack)."
else
  if ! run_stack check --allow-skips 2>&1 | tee "$CHECK_LOG"; then
    echo "ERROR: the standalone stack failed its own check; see the output above" >&2
    exit 1
  fi

  # A passing `check` is NOT proof that the header-strip case ran. Every one of
  # its three assertions sits behind `if [ -z "$spoof_uuid" ] … SKIPPED`, and a
  # seeding change that leaves no personal access token turns all three into
  # printed skips while `check` still exits 0. That is this repository's recurring
  # defect: a gate that stops gating and reports success. Require the three
  # success lines by name.
  for marker in \
    "forged X-Auth-ID alone is rejected" \
    "a real PAT still authenticates through the middleware" \
    "a forged X-Auth-ID cannot override a genuine bearer"
  do
    # Reads a FILE and not a pipe, so grep -q cannot make the producer die of
    # SIGPIPE under `set -o pipefail` — the inversion documented in
    # standalone-stack.sh.
    if ! grep -qF "$marker" "$CHECK_LOG"; then
      echo "ERROR: the #326 edge header-strip assertion did not run." >&2
      echo "       Expected this line in the check output: ${marker}" >&2
      echo "       A skipped assertion proves nothing. Fix the seeding, or fix" >&2
      echo "       the check — do not delete this guard." >&2
      exit 1
    fi
  done
  echo "→ The #326 edge header-strip assertions all ran and passed."
fi

echo "→ Running the chat-stream journey…"
cd "$WEB_DIR"
# Built as a flat string, not an array: macOS ships bash 3.2, where expanding
# an EMPTY array under `set -u` is an "unbound variable" error rather than
# nothing — the failure this script hit on its first real run.
REPEAT_ARGS=""
[ -n "${CHAT_STREAM_REPEAT:-}" ] && REPEAT_ARGS="--repeat-each ${CHAT_STREAM_REPEAT}"

# E2E_REUSE_STACK=1 stops Playwright's own `webServer` from trying to bring up
# the E2E stack: the stack this journey needs is already up, and that hook
# points at a different compose file entirely.
#
# When PLAYWRIGHT_CONTAINER_IMAGE is set (ci-web-e2e.yml sets it), the tests run
# inside the pinned Playwright container instead of on the host — the same
# mechanism and reasons as that workflow's `e2e` job: the image ships the
# browsers AND their system libraries, so `npx playwright install --with-deps`
# (whose cost is mostly apt fetching ~114 MB the runner then throws away)
# disappears entirely. `--network host` because the stack is up on the HOST; a
# container on the default bridge could not reach localhost:${PORT}. No
# `--user`: playwright-core adds `--no-sandbox` itself when it detects uid 0.
#
# The image TAG is deliberately not written here: it must live in the workflow,
# because scripts/check-playwright-image-tag.mjs only scans .github/workflows/
# when it asserts every container tag matches the pinned @playwright/test — a
# tag hardcoded in this script would escape that gate. Locally the variable is
# normally unset and the host npx path runs; set CONTAINER_BIN=podman alongside
# the image to use the container path off-runner.
#
# ── `--workers=1` is the thing that makes this project serial ────────────────
#
# elitea-main admits FOUR concurrent replay streams per principal
# (services/elitea-main/internal/api/v2/executions/events_admission.go), and
# every chat-stream spec signs in as the same chat persona. Each holds an
# execution stream for the length of a turn while the app also holds its
# notifications stream, so running the specs against each other sits on that
# budget and answers 429 — a failure that reads as "the browser cannot read the
# stream", i.e. as a statement about the feature rather than about the harness.
#
# `fullyParallel: false` on the project in playwright.config.ts does NOT
# achieve this and must not be mistaken for it: it orders tests WITHIN a file,
# and each chat-stream spec holds exactly one test in a file of its own, so the
# config's `workers: 4` under CI still started all three files at once. The
# worker count is the only lever that crosses files, it cannot be set
# per-project in the config, and this script is the single entry point every
# run goes through — both ci-web-e2e.yml jobs (`chat-stream` and
# `chat-stream-rust`) invoke it with no Playwright flags of their own. So the
# pin lives here, once.
# E2E_WORKER tells the specs WHICH runtime answers the turns. The two legs
# pin different contracts on purpose — the pipeline/HITL journeys author the
# native runtime's shapes — so a spec must know its leg, and this script is
# the one place that knows it authoritatively.
#
# `chat.variables.spec.ts` USED to be on that list and no longer is: it proved
# substitution on the native leg alone while Main's version projection built an
# empty `variables` list (the SDK reads `meta.variables` only when it is a
# dict, so the same agent answered on the python leg with its placeholder
# intact). The projection now carries the version's real list, so that spec
# runs one assertion on BOTH legs.
E2E_WORKER="${STANDALONE_WORKER:-python}"
# Which Playwright project to run against the stack. `chat-stream` is the
# original; `deepwiki-stack` (scripts/deepwiki-e2e.sh) reuses everything
# above it — the same stack, seeds and assertions — for the provider-backed
# DeepWiki journeys.
PLAYWRIGHT_PROJECT="${PLAYWRIGHT_PROJECT:-chat-stream}"
# E2E_CHAT_MODEL names the model the specs expect the picker to offer. Unset
# or empty, the specs default to the mock's name (they read it with `||`, so
# '' is "unset" — the first version passed '' through and the specs' `??`
# reads took it for a configured real model); the real-model lane sets it to
# the seeded `vllm/<model>` row. Forwarded explicitly because the container
# run below starts from an EMPTY environment — a variable exported on the
# host and not listed here reaches the host path and silently not the
# container path.
# THE LIVE LANES' VARIABLES, forwarded by NAME.
#
# `e2e/live/liveEnv.ts` decides whether the `toolkits-live` and `image-live`
# projects list anything at all, and it decides it from the environment the
# Playwright process sees. The container run below starts from an EMPTY
# environment, so a variable exported on the host reaches the host path and
# silently not the container path — which would show up as a live project that
# listed zero tests inside the container while the operator had just set the
# secret. `-e NAME` with no `=` forwards the host's value, so the list is built
# from whatever `E2E_LIVE_*` names are actually present rather than from a
# hard-coded roster that a new provider would fall off.
LIVE_ENV_ARGS=()
while IFS= read -r live_name; do
  [ -n "$live_name" ] && LIVE_ENV_ARGS+=("-e" "$live_name")
done <<EOF
$(env | sed -n 's/^\(E2E_LIVE_[A-Za-z0-9_]*\)=.*/\1/p')
EOF

# shellcheck disable=SC2086 -- REPEAT_ARGS is deliberately word-split
if [ -n "${PLAYWRIGHT_CONTAINER_IMAGE:-}" ]; then
  "${CONTAINER_BIN:-docker}" run --rm --network host \
    -v "$WEB_DIR":/work -w /work \
    -e CI="${CI:-}" \
    -e E2E_REUSE_STACK=1 \
    -e E2E_WORKER="$E2E_WORKER" \
    -e E2E_CHAT_MODEL="${E2E_CHAT_MODEL:-}" \
    "${LIVE_ENV_ARGS[@]+"${LIVE_ENV_ARGS[@]}"}" \
    -e PLAYWRIGHT_BASE_URL="http://localhost:${PORT}" \
    "$PLAYWRIGHT_CONTAINER_IMAGE" \
    npx playwright test --project="$PLAYWRIGHT_PROJECT" --workers=1 $REPEAT_ARGS
else
  PLAYWRIGHT_BASE_URL="http://localhost:${PORT}" \
  E2E_REUSE_STACK=1 \
  E2E_WORKER="$E2E_WORKER" \
  E2E_CHAT_MODEL="${E2E_CHAT_MODEL:-}" \
    npx playwright test --project="$PLAYWRIGHT_PROJECT" --workers=1 $REPEAT_ARGS
fi
