// LAYER CACHE: read-only, from the registry. Each target reads two refs of
// the public package ghcr.io/elitea-ng/ci-cache-<target>: `release-<platform>`,
// which publish.yml's build of the same Containerfile writes (mode=max, on a
// push to main only), and `linux-amd64`, which ci-web-e2e.yml's build-once
// jobs write for the compose stacks (same rule; deploy/ci/bake-cache.hcl).
// NOTHING here declares a cache-to: a local `docker buildx bake`, the helm
// smoke and the DeepWiki real-engine lane read and never write, so no build
// from a pull request or a laptop can replace a layer a release reads. (Until
// 2026-10-20 every target exported `type=gha,mode=max` into the Actions cache,
// whose 10 GB cap evicted it.) These are layer caches, not images: nothing
// here, or in the chart or a release, runs a ci-* image
// (scripts/ci/check-ci-image-references.mjs).

variable "TAG" {
  default = "dev"
}

variable "REGISTRY" {
  default = "ghcr.io/eliteaai"
}

group "default" {
  targets = ["elitea-main", "elitea-ui", "elitea-scheduler", "elitea-llm-gateway"]
}

group "go" {
  targets = ["elitea-main", "elitea-scheduler", "elitea-subapp-host", "elitea-llm-gateway"]
}

group "scheduler" {
  targets = ["elitea-scheduler"]
}

group "ui" {
  targets = ["elitea-ui"]
}

target "elitea-main" {
  context    = "."
  dockerfile = "services/elitea-main/Containerfile"
  # Named explicitly rather than relying on "last stage wins": the Containerfile
  # also defines `hybrid` and `e2e`, and which one ships must not depend on
  # stage order.
  target     = "final"
  tags       = ["${REGISTRY}/elitea-main:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-main:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-main:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

target "elitea-ui" {
  context    = "./apps/elitea-ui"
  dockerfile = "../deploy/docker/Containerfile.elitea-ui"
  tags       = ["${REGISTRY}/elitea-ui:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-ui:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-ui:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# New UI (spec §7.3): context is the repo root and the Containerfile lives with
# the app, so bake, publish.yml and Taskfile build with identical contexts —
# fixing defect D1 by construction. Built in parallel with elitea-ui for the
# whole cutover; rollback is an edge routing change, never a rebuild.
target "elitea-web" {
  context    = "."
  dockerfile = "apps/elitea-web/Containerfile"
  tags       = ["${REGISTRY}/elitea-web:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-web:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-web:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}


# The agent worker (native Rust). Repo-root context, because the Containerfile
# COPYs libs/proto and libs/rust from outside its own directory.
#
# Deliberately NOT in `group "default"`: it compiles 418 crates from source, so
# a bare `docker buildx bake` would go from minutes to tens of minutes. Name it
# to build it.
target "elitea-worker-rust" {
  context    = "."
  dockerfile = "services/elitea-worker-rust/Containerfile"
  tags       = ["${REGISTRY}/elitea-worker-rust:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-worker-rust:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-worker-rust:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

target "elitea-scheduler" {
  # Repo root, not ./services/elitea-scheduler: the Containerfile COPYs
  # libs/go/observability (issue #250's local replace target) from there.
  context    = "."
  dockerfile = "services/elitea-scheduler/Containerfile"
  tags       = ["${REGISTRY}/elitea-scheduler:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-scheduler:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-scheduler:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# The sub-application host (ADR-0023 H1): the provider SPI once, in Go, for
# DeepWiki and every sub-application after it. Repo-root context like the
# other Go services; the module is standard-library only.
target "elitea-subapp-host" {
  context    = "."
  dockerfile = "services/elitea-subapp-host/Containerfile"
  tags       = ["${REGISTRY}/elitea-subapp-host:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-subapp-host:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-subapp-host:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# The egress proxy (ADR-0034 decision 9): the host-allowlist boundary for
# cloud-executor sandboxes. Repo-root context, because the module replaces
# libs/go/egresslib; standard library otherwise.
target "elitea-egress-proxy" {
  context    = "."
  dockerfile = "services/elitea-egress-proxy/Containerfile"
  target     = "final"
  tags       = ["${REGISTRY}/elitea-egress-proxy:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-egress-proxy:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-egress-proxy:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# The cloud executor's sandbox supervisor (ADR-0034): the `supervisor` stage of
# the worker Containerfile, built with `--features sandbox-supervisor`. It runs
# the workspace sandboxes (Kubernetes pods under a gVisor/Kata RuntimeClass)
# and is the only process that may create them. The same distroless
# base-nossl-debian13 runtime and cargo-auditable build as the worker; the
# default worker image keeps no sandbox feature.
#
# Not in `group "default"`, like the worker: it compiles from source.
target "elitea-sandbox-supervisor" {
  context    = "."
  dockerfile = "services/elitea-worker-rust/Containerfile"
  target     = "supervisor"
  tags       = ["${REGISTRY}/elitea-sandbox-supervisor:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-sandbox-supervisor:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-sandbox-supervisor:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# The Rust-native DeepWiki engine (ADR-0026), the only engine sidecar the
# chart runs (`deepwiki.engine.runner: native` or `fixture`). One binary on
# distroless base-nossl-debian13 plus libgcc_s, built with cargo-auditable.
# It replaced the Python elitea-deepwiki images, which are no longer built.
#
# Repo-root context, as the Containerfile COPYs services/elitea-deepwiki-engine
# and the shared engine crates in libs/rust
# from there (the crate embeds its own migrations).
#
# Out of `group "default"`, like the worker: it compiles ~420 crates from
# source. Name it to build it.
target "elitea-deepwiki-engine-native" {
  context    = "."
  dockerfile = "services/elitea-deepwiki-engine/Containerfile"
  tags       = ["${REGISTRY}/elitea-deepwiki-engine-native:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-deepwiki-engine-native:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-deepwiki-engine-native:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# The Rust-native Inventory engine (ADR-0027): the only Inventory engine
# sidecar, replacing the Python elitea-inventory image. Context is the
# repository root because the Containerfile COPYs services/elitea-inventory-engine
# and the shared engine crates in libs/rust from there (the crate embeds its own
# migrations). No Python, no ML closure; distroless runtime.
#
# Deliberately NOT in `group "default"`, like the DeepWiki engine and the
# two workers: it compiles a few hundred crates from source.
target "elitea-inventory-engine" {
  context    = "."
  dockerfile = "services/elitea-inventory-engine/Containerfile"
  tags       = ["${REGISTRY}/elitea-inventory-engine:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-inventory-engine:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-inventory-engine:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# elitea-vector (ADR-0031): the stateless gRPC facade that is the only Qdrant
# client. One binary on distroless base-nossl-debian13 plus libgcc_s, built
# with cargo-auditable. Repo-root context: the Containerfile COPYs
# services/elitea-vector, libs/proto (its build script compiles
# elitea.vector.v1) and libs/rust (a path dependency).
#
# Not in `group "default"`, like the engines: it compiles from source.
target "elitea-vector" {
  context    = "."
  dockerfile = "services/elitea-vector/Containerfile"
  tags       = ["${REGISTRY}/elitea-vector:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-vector:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-vector:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# elitea-doc-extractor (ADR-0030 I1): the Rust worker's document extraction
# sidecar (PDF, Office) on a Unix socket, in its own process and its own
# Cargo.lock because the worker's lock cannot hold the PDF backend. The same
# distroless base and cargo-auditable build as elitea-vector. Repo-root
# context: the Containerfile COPYs services/elitea-doc-extractor, libs/proto
# (its build script compiles elitea.extract.v1) and libs/rust (path
# dependencies).
#
# Not in `group "default"`, like the engines: it compiles from source.
target "elitea-doc-extractor" {
  context    = "."
  dockerfile = "services/elitea-doc-extractor/Containerfile"
  tags       = ["${REGISTRY}/elitea-doc-extractor:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-doc-extractor:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-doc-extractor:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# Standalone module that needs Go 1.26.4 or above (bifrost/core); its go.mod
# asks for the go1.26.9 security floor. The Containerfile pins
# golang:1.26 internally, so the correct toolchain is used regardless of the
# build runner. Context is the repository ROOT: the module replaces
# libs/go/egresslib, which lives outside the service directory. go.work is not
# copied into the image and the build sets GOWORK=off, so the module still
# builds off the workspace.
# The cloud executor's workspace sandbox image (ADR-0034 decision 9): one
# container per cloud turn runs `elitea-workspace-runner` over a disk-backed
# /workspace. Debian 13 (trixie-slim) with git and a build toolchain, because
# the agent's `run_command` builds the user's project: a Debian userland, so
# its scan is exempt (.github/image-scan-exempt.txt), not distroless.
# Repo-root context: the Containerfile COPYs libs/rust.
#
# Not in `group "default"`: it compiles from source.
target "elitea-workspace-runner" {
  context    = "."
  dockerfile = "libs/rust/workspace-runner/Containerfile"
  tags       = ["${REGISTRY}/elitea-workspace-runner:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-workspace-runner:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-workspace-runner:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}

target "elitea-llm-gateway" {
  context    = "."
  dockerfile = "services/elitea-llm-gateway/Containerfile"
  tags       = ["${REGISTRY}/elitea-llm-gateway:${TAG}"]
  cache-from = [
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-llm-gateway:release-linux-amd64",
    "type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-llm-gateway:linux-amd64",
  ]
  platforms  = ["linux/amd64", "linux/arm64"]
}
