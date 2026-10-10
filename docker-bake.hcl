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
  cache-from = ["type=gha,scope=elitea-main"]
  cache-to   = ["type=gha,mode=max,scope=elitea-main"]
  platforms  = ["linux/amd64", "linux/arm64"]
}

target "elitea-ui" {
  context    = "./apps/elitea-ui"
  dockerfile = "../deploy/docker/Containerfile.elitea-ui"
  tags       = ["${REGISTRY}/elitea-ui:${TAG}"]
  cache-from = ["type=gha,scope=elitea-ui"]
  cache-to   = ["type=gha,mode=max,scope=elitea-ui"]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# New UI (spec §7.3): context is the repo root and the Containerfile lives with
# the app, so bake, publish.yml and Taskfile build with identical contexts —
# fixing defect D1 by construction. Built in parallel with elitea-ui for the
# whole cutover; rollback is a traefik weight change, never a rebuild.
target "elitea-web" {
  context    = "."
  dockerfile = "apps/elitea-web/Containerfile"
  tags       = ["${REGISTRY}/elitea-web:${TAG}"]
  cache-from = ["type=gha,scope=elitea-web"]
  cache-to   = ["type=gha,mode=max,scope=elitea-web"]
  platforms  = ["linux/amd64", "linux/arm64"]
}


# The agent worker. Repo-root context, because the Containerfile COPYs
# libs/proto and testdata/proto/runtime/v1 from outside its own directory.
#
# Deliberately NOT in `group "default"`: this is the only target here that
# compiles a Python dependency closure from source, so a bare `docker buildx
# bake` would go from minutes to tens of minutes. Name it to build it.
target "elitea-worker-python" {
  context    = "."
  dockerfile = "services/elitea-worker-python/Containerfile"
  tags       = ["${REGISTRY}/elitea-worker-python:${TAG}"]
  cache-from = ["type=gha,scope=elitea-worker-python"]
  cache-to   = ["type=gha,mode=max,scope=elitea-worker-python"]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# The native Rust agent worker, and the chart's default `worker.runtime` since
# it started shipping. Repo-root context for the same reason as the Python
# worker: the Containerfile COPYs libs/proto and libs/rust from outside its
# own directory.
#
# Out of `group "default"` on the same grounds as elitea-worker-python, and
# more so: this compiles 418 crates from source. Name it to build it.
target "elitea-worker-rust" {
  context    = "."
  dockerfile = "services/elitea-worker-rust/Containerfile"
  tags       = ["${REGISTRY}/elitea-worker-rust:${TAG}"]
  cache-from = ["type=gha,scope=elitea-worker-rust"]
  cache-to   = ["type=gha,mode=max,scope=elitea-worker-rust"]
  platforms  = ["linux/amd64", "linux/arm64"]
}

target "elitea-scheduler" {
  # Repo root, not ./services/elitea-scheduler: the Containerfile COPYs
  # libs/go/observability (issue #250's local replace target) from there.
  context    = "."
  dockerfile = "services/elitea-scheduler/Containerfile"
  tags       = ["${REGISTRY}/elitea-scheduler:${TAG}"]
  cache-from = ["type=gha,scope=elitea-scheduler"]
  cache-to   = ["type=gha,mode=max,scope=elitea-scheduler"]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# The sub-application host (ADR-0023 H1): the provider SPI once, in Go, for
# DeepWiki and every sub-application after it. Repo-root context like the
# other Go services; the module is standard-library only.
target "elitea-subapp-host" {
  context    = "."
  dockerfile = "services/elitea-subapp-host/Containerfile"
  tags       = ["${REGISTRY}/elitea-subapp-host:${TAG}"]
  cache-from = ["type=gha,scope=elitea-subapp-host"]
  cache-to   = ["type=gha,mode=max,scope=elitea-subapp-host"]
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
  cache-from = ["type=gha,scope=elitea-deepwiki-engine-native"]
  cache-to   = ["type=gha,mode=max,scope=elitea-deepwiki-engine-native"]
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
  cache-from = ["type=gha,scope=elitea-inventory-engine"]
  cache-to   = ["type=gha,mode=max,scope=elitea-inventory-engine"]
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
  cache-from = ["type=gha,scope=elitea-vector"]
  cache-to   = ["type=gha,mode=max,scope=elitea-vector"]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# Standalone module that needs Go 1.26.4 or above (bifrost/core); its go.mod
# asks for the go1.26.9 security floor. The Containerfile pins
# golang:1.26 internally, so the correct toolchain is used regardless of the
# build runner. Context is the repository ROOT: the module replaces
# libs/go/egresslib, which lives outside the service directory. go.work is not
# copied into the image and the build sets GOWORK=off, so the module still
# builds off the workspace.
target "elitea-llm-gateway" {
  context    = "."
  dockerfile = "services/elitea-llm-gateway/Containerfile"
  tags       = ["${REGISTRY}/elitea-llm-gateway:${TAG}"]
  cache-from = ["type=gha,scope=elitea-llm-gateway"]
  cache-to   = ["type=gha,mode=max,scope=elitea-llm-gateway"]
  platforms  = ["linux/amd64", "linux/arm64"]
}
