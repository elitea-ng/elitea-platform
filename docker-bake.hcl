variable "TAG" {
  default = "dev"
}

variable "REGISTRY" {
  default = "ghcr.io/eliteaai"
}

group "default" {
  targets = ["elitea-main", "elitea-ui", "pylon-indexer", "elitea-scheduler", "elitea-llm-gateway"]
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
# worker: the Containerfile COPYs libs/proto from outside its own directory.
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

group "pylon" {
  targets = ["pylon-indexer"]
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

target "pylon-indexer" {
  context    = "./services/pylon-indexer"
  dockerfile = "Containerfile"
  tags       = ["${REGISTRY}/pylon-indexer:${TAG}"]
  args       = { PYLON_VERSION = "1.2.25" }
  cache-from = ["type=gha,scope=pylon-indexer"]
  cache-to   = ["type=gha,mode=max,scope=pylon-indexer"]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# The DeepWiki provider service (ADR-0022). Context is the repository root
# because the Containerfile COPYs services/elitea-deepwiki from there.
#
# EXTRAS is empty here, and that is the shipping default. The engine's
# dependency closure is torch-sized (~92 packages: torch, transformers,
# faiss-cpu, tree-sitter grammars), so the default image carries the engine
# SOURCE and refuses every tool — GET /health names the refusing runner, so it
# cannot look like it has an engine. Build the runnable image explicitly:
#
#   docker buildx bake elitea-deepwiki-engine
#
# Deliberately NOT in `group "default"`, for the same reason the two workers
# are not: a bare bake must not go from minutes to tens of minutes.
target "elitea-deepwiki" {
  context    = "."
  dockerfile = "services/elitea-deepwiki/Containerfile"
  args       = { EXTRAS = "[storage-postgres]" }
  tags       = ["${REGISTRY}/elitea-deepwiki:${TAG}"]
  cache-from = ["type=gha,scope=elitea-deepwiki"]
  cache-to   = ["type=gha,mode=max,scope=elitea-deepwiki"]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# The same image WITH the analysis engine's closure. It is a separate target
# rather than a build argument on the one above because the two produce
# different images with different sizes and different scan surfaces, and a
# release must be able to ship one without waiting for the other.
target "elitea-deepwiki-engine" {
  inherits   = ["elitea-deepwiki"]
  args       = { EXTRAS = "[engine,storage-postgres]" }
  tags       = ["${REGISTRY}/elitea-deepwiki:${TAG}-engine"]
  cache-from = ["type=gha,scope=elitea-deepwiki-engine"]
  cache-to   = ["type=gha,mode=max,scope=elitea-deepwiki-engine"]
}

group "deepwiki" {
  targets = ["elitea-deepwiki", "elitea-deepwiki-engine"]
}

# The Inventory provider engine (ADR-0023 H4c). Context is the repository root
# because the Containerfile COPYs services/elitea-inventory from there.
#
# EXTRAS is empty here, and that is the shipping default, for the same reason
# DeepWiki's is: the knowledge-graph engine's closure is 126 packages (down
# from 356 before stage I8, which removed torch, chromadb and every
# transformer) and still adds minutes to a release on two architectures. The
# default image carries the engine SOURCE and refuses every tool — the sidecar
# defaults to the refusing runner and GET /engine/health names the active one,
# so it cannot look like it has an engine.
#
# It DOES carry the fixture runner, which needs no closure: this is the image
# the compose stacks run, and the one that lets a browser journey reach an
# Inventory result with no engine.
#
# Deliberately NOT in `group "default"`, like DeepWiki's and the two workers.
target "elitea-inventory" {
  context    = "."
  dockerfile = "services/elitea-inventory/Containerfile"
  tags       = ["${REGISTRY}/elitea-inventory:${TAG}"]
  cache-from = ["type=gha,scope=elitea-inventory"]
  cache-to   = ["type=gha,mode=max,scope=elitea-inventory"]
  platforms  = ["linux/amd64", "linux/arm64"]
}

# The same image WITH the knowledge-graph engine's closure. A separate target
# rather than a build argument, for DeepWiki's reason: the two produce
# different images with different sizes and different scan surfaces, and a
# release must be able to ship one without waiting for the other.
target "elitea-inventory-engine" {
  inherits   = ["elitea-inventory"]
  args       = { EXTRAS = "[engine]" }
  tags       = ["${REGISTRY}/elitea-inventory:${TAG}-engine"]
  cache-from = ["type=gha,scope=elitea-inventory-engine"]
  cache-to   = ["type=gha,mode=max,scope=elitea-inventory-engine"]
}

group "inventory" {
  targets = ["elitea-inventory", "elitea-inventory-engine"]
}

# Standalone module pinned to Go 1.26.4 (bifrost/core). The Containerfile pins
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
