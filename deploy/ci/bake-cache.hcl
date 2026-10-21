// Per-target registry layer-cache refs for the compose-defined stack images,
// and the groups the CI jobs bake. Passed to `docker buildx bake` AFTER the
// compose file(s); a target here merges onto the compose service of the same
// name (context, dockerfile, target and tags come from compose, the cache
// directives from here).
//
// ONE REF PER IMAGE, `ghcr.io/elitea-ng/ci-cache-<image>:linux-amd64`. Three
// compose services share the elitea-main image and therefore its ref;
// BuildKit deduplicates their identical steps within one bake.
//
// WHY THE REGISTRY AND NOT `type=gha` (2026-10-20). The GHA layer cache lived
// in the repository's Actions cache, which is capped at 10 GB and evicted
// least-recently-used; it sat at the cap and churned, so a layer one run
// wrote was often gone by the next. A registry ref is outside that cap and
// holds a dependency layer of any size — which is what lets the Rust
// Containerfiles build their dependencies in a layer at all (see
// services/elitea-worker-rust/Containerfile).
//
// PUBLIC, so a fork pull request's in-job fallback build reads it with no
// token. GHCR creates a package with the org's default visibility on its
// first push and has no API to change it; the build-once job asserts after
// every write that an anonymous client can read each ref it wrote, and fails
// naming the package if not (build-stack-images, "publicly readable"). The
// fallback build also logs in with the job token, so a package that went
// private costs nothing worse than that red.
//
// The same packages hold the release's cache under a `release-<platform>`
// tag (publish.yml, also main-only), and docker-bake.hcl reads both.
//
// READ side only. The WRITE side (cache-to) is derived from these lines by
// .github/actions/build-stack-images at run time, because what a run may
// write depends on where it runs: a push to main exports every layer
// (mode=max), every other run (pull requests included) exports nothing. Keep
// cache-to OUT of this file: a cache-to here would write from every run, and
// scripts/ci/check-workflow-cache-writes.mjs fails it.
//
// The `ci-cache-` and `ci-` packages are CI-only. Nothing that ships (the
// chart, publish.yml, docker-bake.hcl) may name them:
// scripts/ci/check-ci-image-references.mjs holds that.
//
// The worker service's ref depends on WHICH worker the stack runs:
// bake-cache-rust.hcl overrides it for the native runtime.

target "elitea-main" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-main:linux-amd64"]
}
target "elitea-migrate" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-main:linux-amd64"]
}
target "elitea-agentstate-migrate" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-main:linux-amd64"]
}
target "elitea-scheduler" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-scheduler:linux-amd64"]
}
target "elitea-llm-gateway" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-llm-gateway:linux-amd64"]
}
// The DeepWiki provider service runs the Go sub-application host image.
target "elitea-deepwiki" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-subapp-host:linux-amd64"]
}
// Its engine sidecar and its migration both run the Rust-native engine image
// (elitea-deepwiki-engine-native, ADR-0026); one image, one scope.
target "elitea-deepwiki-engine" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-deepwiki-engine-native:linux-amd64"]
}
target "elitea-deepwiki-migrate" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-deepwiki-engine-native:linux-amd64"]
}
// The Inventory provider service runs the Go sub-application host image; its
// engine sidecar is the Rust-native elitea-inventory-engine image (ADR-0027).
target "elitea-inventory" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-subapp-host:linux-amd64"]
}
target "elitea-inventory-engine" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-inventory-engine:linux-amd64"]
}
// elitea-vector (ADR-0031) and its Qdrant, in the standalone stack.
target "elitea-vector" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-vector:linux-amd64"]
}
// elitea-doc-extractor (ADR-0030 I1): the Rust worker's PDF/Office sidecar
// in the rust agent overlay.
target "elitea-doc-extractor" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-doc-extractor:linux-amd64"]
}
target "elitea-web" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-web:linux-amd64"]
}
target "elitea-worker" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-worker-rust:linux-amd64"]
}
target "llm-mock" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-mock-llm:linux-amd64"]
}
target "mcp-mock" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-mock-mcp:linux-amd64"]
}
// `mcp-mock-trust` deliberately has NO target: it reuses
// mcp-mock's image (compose `image:` only, no `build:`), so the mock is built
// exactly once per run and every consumer carries the same TLS mint.
target "qtest-mock" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-mock-qtest:linux-amd64"]
}
target "ado-mock" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-mock-ado:linux-amd64"]
}

// deploy/docker-compose.e2e-standalone.yml
group "e2e" {
  targets = ["elitea-main", "elitea-deepwiki", "elitea-inventory", "elitea-web"]
}

// deploy/docker-compose.standalone-full.yml (+ the rust overlay)
group "standalone" {
  targets = [
    "elitea-migrate", "elitea-agentstate-migrate", "elitea-main", "elitea-scheduler",
    "elitea-llm-gateway", "llm-mock", "elitea-deepwiki", "elitea-deepwiki-engine",
    "elitea-deepwiki-migrate",
    "elitea-inventory", "elitea-inventory-engine",
    "elitea-vector",
    "elitea-web", "elitea-worker", "mcp-mock", "qtest-mock",
    "ado-mock",
  ]
}

// The standalone stack plus deploy/docker-compose.standalone-rust-agent.yml,
// whose elitea-doc-extractor sidecar only that overlay defines (a group that
// named it without the overlay would have no build context).
group "standalone-rust" {
  targets = ["standalone", "elitea-doc-extractor"]
}
