#!/usr/bin/env bash
# check-gateway-toolchain.sh — the gateway is TESTED on the toolchain it SHIPS.
#
# ISSUE #506. `.github/workflows/ci-gateway.yml` pinned `go-version: "1.26.4"`
# in three jobs. `services/elitea-llm-gateway/Containerfile` had already moved
# from `golang:1.26.4-alpine` to `golang:1.26-alpine`, because an exact patch
# pin freezes the standard library and the standard library gets CVEs: the scan
# in ci-image-scan.yml reported 9 HIGH findings against stdlib v1.26.4, all
# fixed in 1.26.6. The gateway was then tested on one toolchain and released on
# another, so a standard-library change that alters behaviour reached the
# release and never reached the tests.
#
# Nothing reported that. Both files were readable, both were correct on their
# own, and no gate compared them. This script is that comparison.
#
# WHAT IT ASSERTS
#
#   1. The Containerfile names exactly one builder image, and its version is
#      readable. An unreadable tag FAILS: a comparison against nothing passes.
#   2. Every `go-version:` in the workflows below equals that version, string
#      for string. `1.26` and `1.26.4` are NOT the same request —
#      actions/setup-go reads `1.26` as "the newest 1.26 patch", which is what
#      a `golang:1.26-trixie` image does, and `1.26.4` as that one patch.
#   3. The `go` directive in go.mod shares the image's major.minor series, and
#      never asks for a patch newer than a pinned image can supply. That line
#      is a FLOOR, not a maximum, so it may sit below the image version.
#   4. That directive is at or above the standard-library security floor
#      (stdlibFloorGo126 in services/elitea-main/tests/buildcontext/
#      toolchain_floor_test.go, the single place the floor is written). The Go
#      test holds the same rule, but it runs in ci-go, whose paths do not cover
#      this module, so a gateway-only change is checked here.
#
# Every read that comes back empty FAILS. "Found no pin, so nothing disagreed"
# is the shape this repository keeps mistaking for a pass.
#
# It needs no network, no toolchain and no cluster. Run it directly:
#   bash scripts/ci/check-gateway-toolchain.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CONTAINERFILE="${REPO_ROOT}/services/elitea-llm-gateway/Containerfile"
GO_MOD="${REPO_ROOT}/services/elitea-llm-gateway/go.mod"

# The workflows that build or test THIS module.
#
# `.github/workflows/ci-python.yml` also sets up Go, to build the tier 2
# conformance harness from this module. Its pin moved to the series in the same
# change that moved ci-gateway.yml, so it is held here too.
# services/elitea-worker-python/tests/parity/test_sdk_lock.py asserts that file
# line by line, but it asserts the SDK revisions, the archive hash and the
# conformance driver path — not the `go-version:` line. Keep it that way: a
# toolchain pin belongs to this check, not to the SDK lock test.
WORKFLOWS=(
  "${REPO_ROOT}/.github/workflows/ci-gateway.yml"
  "${REPO_ROOT}/.github/workflows/ci-python.yml"
)

failures=0
fail() { echo "ERROR: $1" >&2; failures=$((failures + 1)); }

for file in "$CONTAINERFILE" "$GO_MOD" "${WORKFLOWS[@]}"; do
  [ -f "$file" ] || { echo "ERROR: ${file} is missing; this check has nothing to compare" >&2; exit 1; }
done

# ── 1. What the image builds with ────────────────────────────────────────────
#
# Matches `FROM golang:<version>[-<variant>]`. The version is captured on its
# own so `golang:1.26-trixie` and `golang:1.26.4-trixie` are told apart.
BUILDER_TAGS="$(sed -nE 's/^[[:space:]]*FROM[[:space:]]+golang:([0-9][0-9.]*)([-][^[:space:]]*)?.*/\1/p' "$CONTAINERFILE")"
builder_count="$(printf '%s' "$BUILDER_TAGS" | grep -c . || true)"
if [ "$builder_count" -eq 0 ]; then
  echo "ERROR: read no 'FROM golang:<version>' line from ${CONTAINERFILE}." >&2
  echo "       Every comparison below would then hold between two empty strings." >&2
  exit 1
fi
if [ "$builder_count" -gt 1 ]; then
  echo "ERROR: ${CONTAINERFILE} names ${builder_count} golang builder images: $(echo "$BUILDER_TAGS" | tr '\n' ' ')." >&2
  echo "       This check compares the workflow pin with ONE shipped toolchain." >&2
  exit 1
fi
IMAGE_VERSION="$BUILDER_TAGS"
echo "Containerfile builds with golang ${IMAGE_VERSION}"

# ── 2. What CI tests with ────────────────────────────────────────────────────
total_pins=0
for workflow in "${WORKFLOWS[@]}"; do
  relative="${workflow#"${REPO_ROOT}/"}"
  pins="$(sed -nE 's/^[[:space:]]*go-version:[[:space:]]*"?([0-9][0-9.]*)"?[[:space:]]*$/\1/p' "$workflow")"
  if [ -z "$pins" ]; then
    fail "read no go-version from ${relative}. Either the key was renamed or the
       jobs lost their toolchain pin; both make this comparison vacuous."
    continue
  fi
  echo "${relative} pins go-version $(echo "$pins" | tr '\n' ' ')"
  while IFS= read -r pin; do
    [ -n "$pin" ] || continue
    total_pins=$((total_pins + 1))
    if [ "$pin" != "$IMAGE_VERSION" ]; then
      fail "${relative} pins go-version ${pin}, the image builds with ${IMAGE_VERSION}.
       actions/setup-go and the golang image read these as DIFFERENT requests, so the
       gateway would be tested on one standard library and released on another.
       Set both to the same string."
    fi
  done <<EOF
$pins
EOF
done
[ "$total_pins" -gt 0 ] || fail "no workflow stated a Go version at all"

# ── 3. What the module needs ─────────────────────────────────────────────────
#
# The `go` directive is the MINIMUM the module compiles with, not a maximum, so
# it may sit below the image version. It must not sit above it, and it must stay in
# the same series: a floor of 1.27 with a 1.26 image does not build.
GO_DIRECTIVE="$(sed -nE 's/^go[[:space:]]+([0-9][0-9.]*)[[:space:]]*$/\1/p' "$GO_MOD" | head -1)"
if [ -z "$GO_DIRECTIVE" ]; then
  fail "read no 'go <version>' directive from ${GO_MOD#"${REPO_ROOT}/"}"
else
  echo "go.mod needs at least go ${GO_DIRECTIVE}"
  series() { printf '%s' "$1" | cut -d. -f1-2; }
  if [ "$(series "$GO_DIRECTIVE")" != "$(series "$IMAGE_VERSION")" ]; then
    fail "go.mod needs go ${GO_DIRECTIVE}, the image builds with golang ${IMAGE_VERSION}.
       A module floor outside the image series cannot build in that image."
  else
    # Compare patch levels only when the image states one. An image pinned to a
    # series supplies the newest patch, which is never below the floor.
    image_patch="$(printf '%s' "$IMAGE_VERSION" | cut -d. -f3)"
    mod_patch="$(printf '%s' "$GO_DIRECTIVE" | cut -d. -f3)"
    if [ -n "$image_patch" ] && [ -n "$mod_patch" ] && [ "$image_patch" -lt "$mod_patch" ]; then
      fail "go.mod needs go ${GO_DIRECTIVE}, the image pins golang ${IMAGE_VERSION}, which is older."
    fi
  fi
fi

# ── 4. The standard-library security floor ───────────────────────────────────
FLOOR_FILE="${REPO_ROOT}/services/elitea-main/tests/buildcontext/toolchain_floor_test.go"
[ -f "$FLOOR_FILE" ] || { echo "ERROR: ${FLOOR_FILE#"${REPO_ROOT}/"} is missing; the security floor has no source" >&2; exit 1; }
FLOOR="$(sed -nE 's/^[[:space:]]*stdlibFloorGo126[[:space:]]*=[[:space:]]*"([0-9]+\.[0-9]+\.[0-9]+)".*/\1/p' "$FLOOR_FILE" | head -1)"
if [ -z "$FLOOR" ]; then
  fail "read no stdlibFloorGo126 from ${FLOOR_FILE#"${REPO_ROOT}/"}. A floor that reads as nothing passes everything."
elif [ -n "$GO_DIRECTIVE" ]; then
  echo "the standard-library security floor is go ${FLOOR}"
  floor_patch="$(printf '%s' "$FLOOR" | cut -d. -f3)"
  directive_patch="$(printf '%s' "$GO_DIRECTIVE" | cut -d. -f3)"
  if [ "$(printf '%s' "$GO_DIRECTIVE" | cut -d. -f1-2)" != "$(printf '%s' "$FLOOR" | cut -d. -f1-2)" ]; then
    fail "go.mod needs go ${GO_DIRECTIVE}, outside the series of the security floor go ${FLOOR}.
       Move the floor constant with the series."
  elif [ -z "$directive_patch" ] || [ "$directive_patch" -lt "$floor_patch" ]; then
    fail "go.mod needs go ${GO_DIRECTIVE}, below the standard-library security floor go ${FLOOR}.
       A builder older than the floor would compile a vulnerable standard library."
  fi
fi

if [ "$failures" -ne 0 ]; then
  echo "" >&2
  echo "${failures} toolchain disagreement(s). The gateway must be tested on the toolchain it ships." >&2
  exit 1
fi

echo "The gateway is tested on the toolchain it ships (go ${IMAGE_VERSION})."
