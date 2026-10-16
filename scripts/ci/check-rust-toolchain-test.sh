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
  'rust-toolchain.toml' '*/rust-toolchain.toml' '*Containerfile*' '*Dockerfile*' '*.hcl' '*Cargo.toml'
  # Assertion (e) scans the tracked tree for release literals; copy the files
  # that carry them (and so would carry a new one in the same place).
  git grep -lE 'rust_revision|Profile\.Rust|RUST_RELEASE|release: [0-9]|rustc [0-9]' -- ':!*.md' ':!*.lock' ':!*/vendor/*' ':!scripts/ci/*')"
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
fresh; edit services/elitea-vector/Containerfile 's/^FROM rust:[^ ]+ AS builder/FROM docker.io\/library\/rust:1.96-trixie AS builder/'
expect "(b) a docker.io/library/ prefixed image" fail "docker.io/library/rust:1.96-trixie, expected exactly"
fresh; edit services/elitea-vector/Containerfile 's/^FROM rust:[^ ]+ AS builder/FROM index.docker.io\/library\/rust:1.97.1-trixie@sha256:b1b3c9c0d921d7fa0a6d1f9ec7e4eab87f8c8ec97644c3d791450f131dec813f AS builder/'
expect "(b) an index.docker.io/ prefix, even with the right tag and digest" fail "index.docker.io/library/rust:1.97.1-trixie@"
fresh; edit services/elitea-vector/Containerfile 's/^FROM rust:[^ ]+ AS builder/FROM library\/rust:1.97.1-trixie AS builder/'
expect "(b) a library/ namespace without a registry" fail "library/rust:1.97.1-trixie, expected exactly"
fresh; edit services/elitea-vector/Containerfile 's/^FROM rust:[^ ]+ AS builder/FROM rust AS builder/'
expect "(b) an untagged image" fail "services/elitea-vector/Containerfile:36: rust, expected exactly"
fresh; edit services/elitea-vector/Containerfile 's/^FROM rust:[^ ]+ AS builder/FROM rust@sha256:b1b3c9c0d921d7fa0a6d1f9ec7e4eab87f8c8ec97644c3d791450f131dec813f AS builder/'
expect "(b) a digest-only image" fail "services/elitea-vector/Containerfile:36: rust@sha256:b1b3c9c0"
fresh; edit services/elitea-vector/Containerfile 's#^FROM rust:[^ ]+ AS builder#FROM --platform=linux/amd64 rust:1.96-trixie AS builder#'
expect "(b) FROM --platform= before the image" fail "rust:1.96-trixie, expected exactly"
fresh; edit services/elitea-code-runner/Containerfile 's#^COPY adapters/rust ./adapters/rust#COPY --from=rust:1.96-trixie /usr/local/cargo/bin/cargo /tmp/cargo\nCOPY adapters/rust ./adapters/rust#'
expect "(b) a COPY --from= image" fail "rust:1.96-trixie, expected exactly"
fresh; printf 'target "x" {\n  contexts = { toolchain = "docker-image://rust:1.96-trixie" }\n}\n' > "$WORK/tree/deploy/ci/extra.hcl"; git -C "$WORK/tree" add -A
expect "(b) a bake docker-image:// context" fail "deploy/ci/extra.hcl:2: rust:1.96-trixie"
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

# (e) release literals
fresh; edit services/elitea-worker-rust/src/sandbox/cargo_dependency_bundle.rs 's/rust_revision != "1\.97\.1"/rust_revision != "1.96.0"/'
expect "(e) the worker validator states another release" fail "cargo_dependency_bundle.rs:97: toolchain release 1.96.0"
fresh; edit services/elitea-main/internal/infra/storage/sandbox_native_bundle.go 's/Profile\.Rust != "1\.97\.1"/Profile.Rust != "1.98.0"/'
expect "(e) the Main validator states another release" fail "sandbox_native_bundle.go:219: toolchain release 1.98.0"
fresh; edit scripts/runtime/rust_compiled_release_manifest.py 's/^RUST_RELEASE = "1\.97\.1"/RUST_RELEASE = "1.96.0"/'
expect "(e) the release audit states another release" fail "rust_compiled_release_manifest.py:22: toolchain release 1.96.0"
fresh; edit services/elitea-code-runner/fixtures/cargo-native-v2.json 's/"rust_revision":"1\.97\.1"/"rust_revision":"1.96.0"/'
expect "(e) a native bundle fixture records another release" fail "cargo-native-v2.json:1: toolchain release 1.96.0"
fresh; printf 'fn probe() { let _ = "rustc 1.98.0 (abc 2026-09-01)"; }\n' > "$WORK/tree/services/elitea-code-runner/new_probe.rs"; git -C "$WORK/tree" add -A
expect "(e) a NEW file with an off-pin rustc line" fail "services/elitea-code-runner/new_probe.rs:1: toolchain release 1.98.0"
fresh; edit services/elitea-main/internal/infra/storage/sandbox_native_bundle.go 's/Profile\.Rust != "1\.97\.1"/Profile.Rust != wantRust/'
expect "(e) a validator whose literal moved out of sight" fail "sandbox_native_bundle.go: carries no toolchain release literal"
fresh; for f in services/elitea-worker-rust/src/sandbox/cargo_dependency_bundle.rs scripts/runtime/test_rust_compiled_release_manifest.py services/elitea-code-runner/fixtures/cargo-native-v2.json services/elitea-main/internal/infra/storage/testdata/native-cargo-v2.json services/elitea-worker-rust/src/sandbox/native-cargo-v2.json; do git -C "$WORK/tree" rm -qf "$f"; done
expect "(e)(d) release literals below the floor" fail "toolchain release literal(s), expected at least 6"

# (d) nothing to read
fresh; git -C "$WORK/tree" rm -qf -r --cached .
expect "(d) an empty tree is refused" fail "there is nothing to check"

total=$((passed + failed))
expected_cases="$(grep -c '^fresh; ' "$0")"
echo "${passed}/${total} gate cases behave (${expected_cases} declared)"
if [ "$failed" -ne 0 ] || [ "$total" -ne "$expected_cases" ] || [ "$total" -lt 30 ]; then
  echo "ERROR: the Rust toolchain gate did not behave in every case" >&2
  exit 1
fi
