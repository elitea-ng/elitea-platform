#!/usr/bin/env bash
# build-native-rust.sh <out-dir> — the standalone-full stack's Rust services
# and the LLM gateway for a native E2E stack (E2E_NATIVE_BIN; layout in
# deploy/native/standalone-full.mjs), built ON the machine that runs them.
#
#   elitea-worker-rust        services/elitea-worker-rust/Containerfile
#   elitea-vector             services/elitea-vector/Containerfile
#   elitea-doc-extractor      services/elitea-doc-extractor/Containerfile
#   elitea-deepwiki-engine    services/elitea-deepwiki-engine/Containerfile
#   elitea-inventory-engine   services/elitea-inventory-engine/Containerfile
#   elitea-llm-gateway        services/elitea-llm-gateway/Containerfile (Go, GOWORK=off)
#
# NEVER LIFTED FROM AN IMAGE. The images link against Debian 13's glibc as
# their builder has it; a binary is only proven on the libc it was linked
# against, and the pool's differs from the hosted runners' (2.41 vs 2.39). So
# each binary is built here, by the toolchain its rust-toolchain.toml pins (the
# caller runs scripts/ci/assert-rust-toolchain.sh in each crate first).
#
# Each service is its own Cargo workspace. `cargo build --locked --release`,
# as the images run it (they add `cargo auditable`, which only embeds the
# dependency list for the image scanner). One CARGO_TARGET_DIR for all five
# (NATIVE_RUST_TARGET_DIR, default <out-dir>/../native-rust-target), so a
# dependency the workspaces share at one version and feature set is compiled
# once. The gateway is outside go.work and needs its own Go
# (services/elitea-llm-gateway/go.mod): the caller puts that go on PATH.
#
# Used by ci-web-e2e-native.yml's `native-build-rust` job; runnable locally.
set -euo pipefail

out="${1:?usage: build-native-rust.sh <out-dir>}"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
export CARGO_TARGET_DIR="${NATIVE_RUST_TARGET_DIR:-$(dirname "$out")/native-rust-target}"
export CARGO_NET_RETRY=10 CARGO_NET_GIT_FETCH_WITH_CLI=true

CRATES=(elitea-worker-rust elitea-vector elitea-doc-extractor elitea-deepwiki-engine elitea-inventory-engine)
for crate in "${CRATES[@]}"; do
  echo "→ ${crate}"
  (
    cd "$REPO_ROOT/services/${crate}"
    for attempt in 1 2 3; do
      cargo fetch --locked && break
      [ "$attempt" = 3 ] && { echo "cargo fetch failed after 3 attempts (${crate})" >&2; exit 1; }
      sleep $((attempt * 10))
    done
    cargo build --locked --release --bin "${crate}"
  )
  install -m 0755 "${CARGO_TARGET_DIR}/release/${crate}" "$out/${crate}"
done

echo "→ elitea-llm-gateway (GOWORK=off, $(go version))"
(
  cd "$REPO_ROOT/services/elitea-llm-gateway"
  export CGO_ENABLED=0 GOWORK=off
  for attempt in 1 2 3 4 5; do
    go mod download && break
    [ "$attempt" = 5 ] && { echo "go mod download failed after 5 attempts" >&2; exit 1; }
    sleep $((attempt * 5))
  done
  go build -trimpath -ldflags="-s -w" -o "$out/elitea-llm-gateway" ./cmd/elitea-llm-gateway
)

for f in "${CRATES[@]}" elitea-llm-gateway; do
  [ -x "$out/$f" ] || { echo "ERROR: $out/$f was not built" >&2; exit 1; }
done
echo "✓ native Rust and gateway build directory: $out"
