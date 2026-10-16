#!/usr/bin/env bash
# check-rust-toolchain-test.sh — the Rust toolchain gate is proved red.
#
# Each case copies the tracked files the gate reads into a throwaway git tree,
# breaks ONE thing, and requires check-rust-toolchain.sh to fail with the
# message that names it. A gate that cannot be shown to fail measures nothing.
# The first case is the unbroken copy, which must pass: otherwise every red
# below could be the copy, not the breakage.
#
#   bash scripts/ci/check-rust-toolchain-test.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GATE="${REPO_ROOT}/scripts/ci/check-rust-toolchain.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/rust-toolchain-gate.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

# The inputs the gate reads, from the real tree.
inputs="$(cd "$REPO_ROOT" && git ls-files --cached --others --exclude-standard \
  'rust-toolchain.toml' '*/rust-toolchain.toml' '*Containerfile*' '*Dockerfile*' '*.hcl' '*Cargo.toml')"
[ -n "$inputs" ] || { echo "ERROR: found no gate inputs to copy" >&2; exit 1; }

passed=0
failed=0

fresh() {
  rm -rf "$WORK/tree"
  mkdir -p "$WORK/tree"
  (cd "$REPO_ROOT" && printf '%s\n' "$inputs" | while IFS= read -r f; do
    mkdir -p "$WORK/tree/$(dirname "$f")"
    cp "$f" "$WORK/tree/$f"
  done)
  git -C "$WORK/tree" init -q
  git -C "$WORK/tree" add -A
}

# expect <name> <pass|fail> <message substring or ''>
expect() {
  local name="$1" want="$2" needle="$3" output status
  set +e
  output="$(RUST_TOOLCHAIN_ROOT="$WORK/tree" bash "$GATE" 2>&1)"
  status=$?
  set -e
  if [ "$want" = pass ] && [ "$status" -eq 0 ]; then
    echo "ok   ${name}"; passed=$((passed + 1)); return
  fi
  if [ "$want" = fail ] && [ "$status" -ne 0 ] && printf '%s' "$output" | grep -qF -- "$needle"; then
    echo "ok   ${name}"; passed=$((passed + 1)); return
  fi
  echo "FAIL ${name}: wanted ${want} (message: ${needle:-none}), got exit ${status}:" >&2
  printf '%s\n' "$output" | tail -8 | sed 's/^/       /' >&2
  failed=$((failed + 1))
}

edit() { # edit <file> <sed expression>
  sed -i.bak -E "$2" "$WORK/tree/$1" && rm -f "$WORK/tree/$1.bak"
  git -C "$WORK/tree" add -A
}

fresh; expect "the unbroken tree passes" pass ""

# (a) toolchain files
fresh; edit services/elitea-vector/rust-toolchain.toml 's/1\.97\.1/1.96.0/'
expect "(a) a subtree channel that differs" fail "services/elitea-vector/rust-toolchain.toml: channel 1.96.0"
fresh; edit libs/rust/rust-toolchain.toml 's/^channel.*$/# channel removed/'
expect "(a) a subtree file with no channel" fail "libs/rust/rust-toolchain.toml: read no"
fresh; git -C "$WORK/tree" rm -qf rust-toolchain.toml
expect "(a)(d) no root pin" fail "rust-toolchain.toml is missing at the repository root"
fresh; edit rust-toolchain.toml 's/1\.97\.1/stable/'
expect "(a)(d) a root pin that is not an exact version" fail "read no exact"
fresh; git -C "$WORK/tree" rm -qf services/elitea-vector/rust-toolchain.toml services/elitea-doc-extractor/rust-toolchain.toml
expect "(a)(d) toolchain files below the floor" fail "rust-toolchain file(s), expected at least"

# (b) images
fresh; edit services/elitea-vector/Containerfile 's/^FROM rust:1\.97\.1-trixie@sha256:[0-9a-f]+ AS builder/FROM rust:1.97.1-trixie AS builder/'
expect "(b) an image without the digest" fail "services/elitea-vector/Containerfile:36: rust:1.97.1-trixie, expected"
fresh; edit services/elitea-worker-rust/Containerfile 's/^FROM rust:1\.97\.1-trixie@/FROM rust:1.96.0-trixie@/'
expect "(b) an image on another version" fail "rust:1.96.0-trixie@"
fresh; edit services/elitea-doc-extractor/Containerfile 's/@sha256:b1b3c9c0[0-9a-f]+ AS runtime-libraries/@sha256:0000000000000000000000000000000000000000000000000000000000000000 AS runtime-libraries/'
expect "(b) a runtime-libraries stage with another digest" fail "services/elitea-doc-extractor/Containerfile:24"
fresh; edit services/elitea-code-runner/Containerfile 's/^FROM rust:[^ ]+ AS builder/FROM rust:1.97.1-bookworm AS builder/'
expect "(b) another Debian variant" fail "rust:1.97.1-bookworm"
fresh; for f in services/elitea-vector/Containerfile services/elitea-inventory-engine/Containerfile; do edit "$f" 's/^FROM rust:/FROM docker.io\/library\/rustlang:/'; done
expect "(b)(d) image references below the floor" fail "rust image reference(s), expected at least"

# (c) MSRV
fresh; edit services/elitea-vector/Cargo.toml 's/^rust-version = "1\.95"/rust-version = "1.96"/'
expect "(c) a crate with another MSRV" fail "services/elitea-vector/Cargo.toml: rust-version 1.96 (stated)"
fresh; edit libs/rust/Cargo.toml 's/^rust-version = "1\.95"/rust-version = "1.94"/'
expect "(c) the workspace MSRV moves under its members" fail "libs/rust/agent-runtime/Cargo.toml: rust-version 1.94 (inherited"
fresh; edit services/elitea-worker-rust/Cargo.toml '/^rust-version/d'
expect "(c)(d) a crate with no rust-version" fail "services/elitea-worker-rust/Cargo.toml: no readable rust-version"
fresh; edit libs/rust/vendor/leiden-rs/Cargo.toml 's/^rust-version = "[^"]*"/rust-version = "1.99"/'
expect "(c) a vendored crate above the pin" fail "libs/rust/vendor/leiden-rs/Cargo.toml: vendored crate needs rust 1.99"
fresh; edit libs/rust/vendor/zavora-slide/Cargo.toml '/^rust-version/d'
expect "(c)(d) a vendored crate with no rust-version" fail "libs/rust/vendor/zavora-slide/Cargo.toml: vendored crate with no readable"

# (d) nothing to read
fresh; git -C "$WORK/tree" rm -qf -r --cached .
expect "(d) an empty tree is refused" fail "there is nothing to check"

total=$((passed + failed))
expected_cases="$(grep -c '^fresh; ' "$0")"
echo "${passed}/${total} gate cases behave (${expected_cases} declared)"
if [ "$failed" -ne 0 ] || [ "$total" -ne "$expected_cases" ] || [ "$total" -lt 15 ]; then
  echo "ERROR: the Rust toolchain gate did not behave in every case" >&2
  exit 1
fi
