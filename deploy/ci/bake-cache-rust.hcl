// Passed after bake-cache.hcl when the stack runs the native Rust runtime
// (deploy/docker-compose.standalone-rust-agent.yml swaps `elitea-worker` to
// services/elitea-worker-rust/Containerfile), so the worker's layers land in
// the Rust image's registry ref.
target "elitea-worker" {
  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-worker-rust:linux-amd64"]
}
