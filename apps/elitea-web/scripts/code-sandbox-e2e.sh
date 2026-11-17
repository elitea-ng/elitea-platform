#!/usr/bin/env bash
# The agent `run_code` journeys on the REAL Docker code sandbox (#2206, #2208).
#
#   apps/elitea-web/scripts/code-sandbox-e2e.sh            # up + seed + run
#   apps/elitea-web/scripts/code-sandbox-e2e.sh --keep     # leave the stack up
#
# Same stack, seeds, assertions and teardown as chat-stream-e2e.sh (it is the
# script this one calls), plus the Docker code sandbox on top:
#
#   docker-compose.sandbox.yml               the supervisor, on the Docker socket
#   docker-compose.sandbox-postgres-tls.yml  TLS on the stack's PostgreSQL (the
#                                            supervisor's ledger is VerifyFull)
#   deploy/scripts/sandbox-compose-material.sh
#                                            supervisor profiles, certificates,
#                                            the worker's `sandbox_runtimes`
#
# The three images are the caller's to build and name:
#
#   ELITEA_SANDBOX_SUPERVISOR_IMAGE   services/elitea-worker-rust, target supervisor
#   ELITEA_SANDBOX_DENO_IMAGE         services/elitea-code-runner, target default
#   ELITEA_SANDBOX_RUST_IMAGE         services/elitea-code-runner, target rust-runtime
#
# They must already be in the Docker daemon the supervisor reaches through
# ELITEA_SANDBOX_DOCKER_SOCKET (default /var/run/docker.sock): the supervisor
# validates cached images and never pulls a runner image per job.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
: "${ELITEA_SANDBOX_SUPERVISOR_IMAGE:?Set the built supervisor image}"
: "${ELITEA_SANDBOX_DENO_IMAGE:?Set the built Python/JavaScript/TypeScript runner image}"
: "${ELITEA_SANDBOX_RUST_IMAGE:?Set the built Rust runner image}"

export PLAYWRIGHT_PROJECT=code-sandbox-stack
export CHAT_STREAM_PROJECT="${CODE_SANDBOX_PROJECT:-elitea-code-sandbox}"
export CHAT_STREAM_PORT="${CODE_SANDBOX_PORT:-8091}"
# The supervisor's own log is the first thing a failed run needs.
export CHAT_STREAM_EXTRA_LOG_SERVICES="elitea-sandbox postgres"

# The runtime CA exists after `certs`, which is idempotent: chat-stream-e2e.sh
# runs it again.
STANDALONE_PROJECT="$CHAT_STREAM_PROJECT" "${REPO_ROOT}/deploy/scripts/standalone-stack.sh" certs

material="${CODE_SANDBOX_MATERIAL_DIR:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/elitea-code-sandbox-material}"
CONTAINER_BIN="${CONTAINER_BIN:-docker}" \
  "${REPO_ROOT}/deploy/scripts/sandbox-compose-material.sh" "$material" > "${material}.env"
# shellcheck disable=SC1090
. "${material}.env"

export STANDALONE_OVERLAY="${REPO_ROOT}/deploy/docker-compose.sandbox.yml ${REPO_ROOT}/deploy/docker-compose.sandbox-postgres-tls.yml"
exec "${REPO_ROOT}/apps/elitea-web/scripts/chat-stream-e2e.sh" "$@"
