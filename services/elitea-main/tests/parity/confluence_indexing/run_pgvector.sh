#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MAIN_DIR="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

if [[ -z "${ELITEA_CURRENT_SDK_ROOT:-}" ]]; then
  echo "ELITEA_CURRENT_SDK_ROOT must point to the exact SDK revision in current_pylon_sdk_baseline.json" >&2
  exit 2
fi

# macOS strips DYLD_* while launching /usr/bin/env bash. Restore the standard
# Homebrew cairo location inside this process so the pinned SDK image loader
# imports the same way as the workspace's documented test command.
if [[ "$(uname -s)" == "Darwin" && -d /opt/homebrew/opt/cairo/lib ]]; then
  export DYLD_LIBRARY_PATH="/opt/homebrew/opt/cairo/lib:${DYLD_LIBRARY_PATH:-}"
fi

container=""
cleanup() {
  if [[ -n "${container}" ]]; then
    docker rm -f "${container}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if [[ -z "${ELITEA_CONFLUENCE_PARITY_PGVECTOR_URL:-}" ]]; then
  container="elitea-confluence-parity-${$}"
  docker run --rm -d \
    --name "${container}" \
    -e POSTGRES_USER=parity \
    -e POSTGRES_PASSWORD=parity \
    -e POSTGRES_DB=parity \
    -p 127.0.0.1::5432 \
    pgvector/pgvector:0.8.1-pg18-trixie >/dev/null
  port="$(docker port "${container}" 5432/tcp | sed -E 's/.*:([0-9]+)$/\1/')"
  for _ in $(seq 1 30); do
    if docker exec "${container}" pg_isready -U parity -d parity >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  docker exec "${container}" psql -U parity -d parity \
    -c 'CREATE EXTENSION IF NOT EXISTS vector' >/dev/null
  export ELITEA_CONFLUENCE_PARITY_PGVECTOR_URL="postgresql+psycopg://parity:parity@127.0.0.1:${port}/parity"
fi

cd "${MAIN_DIR}"
python -m pytest tests/parity/confluence_indexing -m pgvector -vv
