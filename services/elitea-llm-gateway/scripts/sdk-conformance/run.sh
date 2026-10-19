#!/usr/bin/env bash
# run.sh — tier 2 of the elitea-sdk compatibility gates.
#
# It builds and starts cmd/sdk-conformance-harness (the gateway's real /llm
# router over the real llmproxy handler, with no provider and no database), then
# runs conformance.py against it with the INSTALLED elitea_sdk.
#
#   ELITEA_SDK_SOURCE_ROOT=/path/to/elitea-sdk \
#     services/elitea-llm-gateway/scripts/sdk-conformance/run.sh
#
# ── What this measures that nothing else does ────────────────────────────────
#
# Tier 1 (scripts/contract/test_sdk_budget_contract.py) compares the SDK source
# with the gateway source. It cannot see a defect that both sources agree on and
# the wire does not: a route that 404s, a body the client library reshapes, a
# stream whose usage trailer is dropped. Tier 3 (deploy/scripts/sdk-client-check
# .sh) drives a whole stack, but that stack has no budget enforcement, so its
# refusal arm is a negative control only. This tier is the one that produces a
# REAL 402 from the REAL gateway code and hands it to the REAL SDK reader.
#
# ── The pin is checked before anything runs ──────────────────────────────────
#
# internal/sdkpin/sdk-pin.json states the SDK revision the gateway's
# compatibility gates were verified against. This script refuses to run against
# any other revision: a green result about the wrong revision is worse than no
# result, and "the checkout happened to be something else" is exactly the shape
# this repository keeps mistaking for a pass.
#
# IDENTITY AND CONTENT ARE TWO CHECKS (#567). The revision below answers WHICH
# commit is checked out. It does not answer WHAT the working tree holds: a dirty
# tree keeps the pinned HEAD, and `git rev-parse` walks UP to an enclosing
# repository, so a hand-authored directory inside a pinned clone reports the
# pinned revision too. verify_pinned_content.py then compares the bytes of every
# file the gates read against the digests the pin records.
set -euo pipefail

GATEWAY_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PIN_FILE="${GATEWAY_ROOT}/internal/sdkpin/sdk-pin.json"

abort() { echo "ERROR: $1" >&2; exit 1; }

command -v python3 >/dev/null || abort "python3 is not on PATH"
command -v go >/dev/null || abort "go is not on PATH"
[ -f "$PIN_FILE" ] || abort "the gateway SDK pin ${PIN_FILE} is missing. Without it this
       script cannot tell which SDK revision it is allowed to measure."

SDK_SOURCE_ROOT="${ELITEA_SDK_SOURCE_ROOT:-}"
[ -n "$SDK_SOURCE_ROOT" ] || abort "set ELITEA_SDK_SOURCE_ROOT to a checkout of the pinned
       elitea-sdk revision. This gate drives the REAL SDK and verifies that the
       installed package is the pinned one; it has nothing to compare against
       otherwise."
[ -d "$SDK_SOURCE_ROOT" ] || abort "ELITEA_SDK_SOURCE_ROOT=${SDK_SOURCE_ROOT} is not a directory"

PINNED_REVISION="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["verified_against"]["revision"])' "$PIN_FILE")"
[ -n "$PINNED_REVISION" ] || abort "${PIN_FILE} carries no verified_against.revision"

# The checkout must BE the pinned revision. A worktree with the two pinned
# cherry-picks applied with --no-commit keeps HEAD at the base revision, which
# is what .github/workflows/ci-python.yml produces, so this comparison holds
# there too.
ACTUAL_REVISION="$(git -C "$SDK_SOURCE_ROOT" rev-parse HEAD 2>/dev/null || true)"
[ -n "$ACTUAL_REVISION" ] || abort "cannot read the git revision of ${SDK_SOURCE_ROOT}.
       This gate must know WHICH SDK it measured; an unidentified tree is not a
       pinned one."
[ "$ACTUAL_REVISION" = "$PINNED_REVISION" ] || abort "the SDK checkout is not the pinned revision.
         ${SDK_SOURCE_ROOT} is at ${ACTUAL_REVISION}
         ${PIN_FILE} names   ${PINNED_REVISION}
       Point ELITEA_SDK_SOURCE_ROOT at the pinned revision, or — if the SDK moved
       on purpose — update internal/sdkpin/elitea-sdk.lock.json and re-verify both gates before
       you touch the pin."

# The checkout must also HOLD the pinned content. The revision check above
# cannot see an uncommitted edit, and it reports the pinned revision for a
# hand-authored directory nested inside a pinned clone (#567). The digests come
# from the same pin file, so this script and tier 1 cannot measure two different
# trees. The check is SCOPED to the pinned files: CI applies two cherry-picks
# with --no-commit, so the CI tree is dirty on purpose.
CONTENT_CHECK="${GATEWAY_ROOT}/scripts/sdk-conformance/verify_pinned_content.py"
[ -f "$CONTENT_CHECK" ] || abort "${CONTENT_CHECK} is missing. Without it this script
       verifies WHICH commit is checked out and not WHAT the tree holds."
echo "-> verifying the pinned SDK content"
python3 "$CONTENT_CHECK" \
  --pin "$PIN_FILE" \
  --sdk-source-root "$SDK_SOURCE_ROOT" \
  --gate "${GATEWAY_ROOT}/scripts/sdk-conformance/conformance.py" \
  || abort "the SDK checkout does not hold the pinned content; see the difference above"

# The gateway is a standalone module on its own Go toolchain; the parent
# workspace must never be consulted.
export GOWORK=off

BUILD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/sdk-conformance.XXXXXX")"
HARNESS_BIN="${BUILD_DIR}/sdk-conformance-harness"
HARNESS_LOG="${BUILD_DIR}/harness.log"
HARNESS_PID=""

cleanup() {
  if [ -n "$HARNESS_PID" ] && kill -0 "$HARNESS_PID" 2>/dev/null; then
    kill "$HARNESS_PID" 2>/dev/null || true
    wait "$HARNESS_PID" 2>/dev/null || true
  fi
  rm -rf "$BUILD_DIR"
}
trap cleanup EXIT

echo "-> building the tier 2 harness"
(cd "$GATEWAY_ROOT" && go build -o "$HARNESS_BIN" ./cmd/sdk-conformance-harness)

echo "-> starting the tier 2 harness"
"$HARNESS_BIN" --project-id 4242 --user-id 77 > "$HARNESS_LOG" 2>&1 &
HARNESS_PID=$!

# Wait for the address LINE, never for a fixed delay. The harness prints it
# before it serves, so a slow start is a slow start rather than a connection
# refused that reads as a routing failure.
BASE_URL=""
for _ in $(seq 1 100); do
  if ! kill -0 "$HARNESS_PID" 2>/dev/null; then
    echo "--- harness output ---" >&2; cat "$HARNESS_LOG" >&2
    abort "the harness exited before it printed its address"
  fi
  BASE_URL="$(sed -n 's/^SDK_HARNESS_URL=//p' "$HARNESS_LOG" | head -1)"
  [ -n "$BASE_URL" ] && break
  sleep 0.1
done
[ -n "$BASE_URL" ] || { cat "$HARNESS_LOG" >&2; abort "the harness never printed SDK_HARNESS_URL"; }
echo "-> harness at ${BASE_URL}"

set +e
python3 "${GATEWAY_ROOT}/scripts/sdk-conformance/conformance.py" \
  --base-url "$BASE_URL" \
  --project 4242 \
  --sdk-source-root "$SDK_SOURCE_ROOT"
STATUS=$?
set -e

if [ "$STATUS" -ne 0 ]; then
  echo "--- harness output ---" >&2
  cat "$HARNESS_LOG" >&2
fi
exit "$STATUS"
