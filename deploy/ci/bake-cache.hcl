// Per-target GHA layer-cache scopes for the compose-defined stack images, and
// the groups the CI jobs bake. Passed to `docker buildx bake` AFTER the
// compose file(s); a target here merges onto the compose service of the same
// name (context, dockerfile, target and tags come from compose, the cache
// directives from here).
//
// ONE scope per IMAGE, shared with publish.yml's amd64 build
// (`<image>-<platform suffix>`) and ci-image-scan.yml, so a push to main warms
// the cache every later job hits. Three compose services share the elitea-main
// image and therefore its scope; BuildKit deduplicates their identical steps
// within one bake.
//
// READ side only. The WRITE side (cache-to) is derived from these lines by
// .github/actions/build-stack-images at run time, because what a run may
// write depends on where it runs: main exports every layer (mode=max), a pull
// request exports only the final image layers (mode=min). The repository's
// Actions cache is capped at 10 GB and a pull request's builder-stage layers
// are, for an unchanged service, byte-identical copies of main's.
//
// The worker service's scope depends on WHICH worker the stack runs:
// bake-cache-rust.hcl overrides it for the native runtime.

target "elitea-main" {
  cache-from = ["type=gha,scope=elitea-main-linux-amd64"]
}
target "elitea-migrate" {
  cache-from = ["type=gha,scope=elitea-main-linux-amd64"]
}
target "elitea-agentstate-migrate" {
  cache-from = ["type=gha,scope=elitea-main-linux-amd64"]
}
target "elitea-llm-gateway" {
  cache-from = ["type=gha,scope=elitea-llm-gateway-linux-amd64"]
}
// The DeepWiki provider service runs the Go sub-application host image.
target "elitea-deepwiki" {
  cache-from = ["type=gha,scope=elitea-subapp-host-linux-amd64"]
}
target "elitea-deepwiki-engine" {
  cache-from = ["type=gha,scope=elitea-deepwiki-linux-amd64"]
}
// The Inventory provider service runs the Go sub-application host image; its
// engine sidecar is the elitea-inventory image (a Python service, Debian base).
target "elitea-inventory" {
  cache-from = ["type=gha,scope=elitea-subapp-host-linux-amd64"]
}
target "elitea-inventory-engine" {
  cache-from = ["type=gha,scope=elitea-inventory-linux-amd64"]
}
target "elitea-web" {
  cache-from = ["type=gha,scope=elitea-web-linux-amd64"]
}
target "elitea-worker" {
  cache-from = ["type=gha,scope=elitea-worker-python-linux-amd64"]
}
target "llm-mock" {
  cache-from = ["type=gha,scope=elitea-mock-llm"]
}
target "mcp-mock" {
  cache-from = ["type=gha,scope=elitea-mock-mcp"]
}
target "mcp-mock-trust" {
  cache-from = ["type=gha,scope=elitea-mock-mcp"]
}

// deploy/docker-compose.e2e-standalone.yml
group "e2e" {
  targets = ["elitea-main", "elitea-deepwiki", "elitea-inventory", "elitea-web"]
}

// deploy/docker-compose.standalone-full.yml (+ the rust overlay)
group "standalone" {
  targets = [
    "elitea-migrate", "elitea-agentstate-migrate", "elitea-main",
    "elitea-llm-gateway", "llm-mock", "elitea-deepwiki", "elitea-deepwiki-engine",
    "elitea-inventory", "elitea-inventory-engine",
    "elitea-web", "elitea-worker", "mcp-mock", "mcp-mock-trust",
  ]
}
