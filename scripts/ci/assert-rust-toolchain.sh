#!/usr/bin/env bash
# assert-rust-toolchain.sh — the compiler this job will use IS the pinned one.
#
# Every Rust workflow used to "Show pinned toolchain": print `rustc --version`
# and move on. Printing is not checking. rustup picks the toolchain from the
# rust-toolchain.toml nearest the CURRENT DIRECTORY, so a step that runs cargo
# from a directory with no toolchain file gets the runner's default stable
# compiler, silently. That happened (PR #1297): scripts/runtime/
# test_sandbox_ledger.py ran cargo from the repository root, compiled every
# dependency again with the runner's stable rustc, and produced a different
# test binary. The log printed both versions and nothing compared them.
#
# This script compares. It reads the pin from the repository-root
# rust-toolchain.toml (scripts/ci/check-rust-toolchain.sh holds every other
# toolchain file to the same channel) and fails unless `rustc --version`, run
# in the CURRENT directory, reports exactly that version. Run it in the
# working directory the job's cargo steps use.
#
# An unreadable pin or an unreadable compiler version FAILS: a comparison
# against nothing must not pass.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ROOT_TOOLCHAIN="${REPO_ROOT}/rust-toolchain.toml"

[ -f "$ROOT_TOOLCHAIN" ] || { echo "ERROR: ${ROOT_TOOLCHAIN} is missing; there is no pin to assert" >&2; exit 1; }
PIN="$(sed -nE 's/^[[:space:]]*channel[[:space:]]*=[[:space:]]*"([0-9]+\.[0-9]+\.[0-9]+)"[[:space:]]*$/\1/p' "$ROOT_TOOLCHAIN")"
if [ -z "$PIN" ] || [ "$(printf '%s\n' "$PIN" | wc -l | tr -d ' ')" -ne 1 ]; then
  echo "ERROR: read no single exact 'channel = \"X.Y.Z\"' from ${ROOT_TOOLCHAIN}." >&2
  exit 1
fi

# rustup may install the pinned toolchain on first use; that output is not the
# version line, so read only the line that starts with `rustc `.
RUSTC_LINE="$(rustc --version 2>/dev/null | grep -E '^rustc ' || true)"
ACTUAL="$(printf '%s' "$RUSTC_LINE" | sed -nE 's/^rustc ([0-9]+\.[0-9]+\.[0-9]+).*/\1/p')"
if [ -z "$ACTUAL" ]; then
  echo "ERROR: could not read a version from 'rustc --version' in $(pwd) (got: '${RUSTC_LINE}')." >&2
  exit 1
fi

# cargo ships in the same toolchain and carries the same release number. A
# cargo from elsewhere on PATH (a system package next to rustup's rustc)
# resolves dependencies and features differently even with the right rustc.
CARGO_LINE="$(cargo --version 2>/dev/null | grep -E '^cargo ' || true)"
CARGO_ACTUAL="$(printf '%s' "$CARGO_LINE" | sed -nE 's/^cargo ([0-9]+\.[0-9]+\.[0-9]+).*/\1/p')"
if [ -z "$CARGO_ACTUAL" ]; then
  echo "ERROR: could not read a version from 'cargo --version' in $(pwd) (got: '${CARGO_LINE}')." >&2
  exit 1
fi

echo "pinned toolchain: ${PIN} (${ROOT_TOOLCHAIN#"${REPO_ROOT}/"})"
echo "${RUSTC_LINE}"
echo "${CARGO_LINE}"

status=0
if [ "$ACTUAL" != "$PIN" ]; then
  echo "::error::rustc in $(pwd) is ${ACTUAL}, the pin is ${PIN}. rustup chose another" \
    "toolchain here (no rust-toolchain.toml on this path, a RUSTUP_TOOLCHAIN override," \
    "or a rustup quirk); every build below would use the wrong compiler." >&2
  status=1
fi
if [ "$CARGO_ACTUAL" != "$PIN" ]; then
  echo "::error::cargo in $(pwd) is ${CARGO_ACTUAL}, the pin is ${PIN}; a cargo outside the" \
    "pinned toolchain is first on PATH." >&2
  status=1
fi
[ "$status" -eq 0 ] || exit 1
echo "rustc and cargo match the pin."
