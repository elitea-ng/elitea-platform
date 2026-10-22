#!/usr/bin/env bash
# setup-and-test.sh
# Applies the UUID fix SQL and then runs Go migration tests (phases 1 and 2).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SQL_FILE="${SCRIPT_DIR}/fix-uuid-defaults.sql"
TEST_RUNNER="${SCRIPT_DIR}/run-migration-tests.sh"

# ---------------------------------------------------------------------------
# Color helpers
# ---------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
BOLD='\033[1m'
RESET='\033[0m'

info()   { echo -e "${CYAN}[INFO]${RESET} $*"; }
pass()   { echo -e "${GREEN}[PASS]${RESET} $*"; }
fail()   { echo -e "${RED}[FAIL]${RESET} $*"; }
header() { echo -e "\n${BOLD}${CYAN}=== $* ===${RESET}"; }

# ---------------------------------------------------------------------------
# Step 1: Apply UUID fix via podman exec
# ---------------------------------------------------------------------------
apply_uuid_fix() {
  header "Step 1 — Applying UUID fix SQL"

  if [[ ! -f "${SQL_FILE}" ]]; then
    fail "SQL file not found: ${SQL_FILE}"
    exit 1
  fi

  # Detect container runtime (prefer podman, fall back to docker)
  local runtime
  if command -v podman &>/dev/null; then
    runtime="podman"
  elif command -v docker &>/dev/null; then
    runtime="docker"
  else
    fail "Neither podman nor docker found in PATH"
    exit 1
  fi

  info "Using container runtime: ${runtime}"

  # Find the running postgres container
  local pg_container
  pg_container=$(${runtime} ps --format '{{.Names}}' 2>/dev/null \
    | grep -iE 'postgres|db' | head -1 || true)

  if [[ -z "${pg_container}" ]]; then
    fail "No running postgres container found."
    fail "Containers currently running:"
    ${runtime} ps --format '{{.Names}}' >&2 || true
    exit 1
  fi

  info "Found database container: ${pg_container}"
  info "Executing fix-uuid-defaults.sql ..."

  local db_user="${POSTGRES_USER:-eliteausr}"
  local db_name="${POSTGRES_DB:-eliteadmstage2}"

  ${runtime} exec -i "${pg_container}" \
    psql -U "${db_user}" -d "${db_name}" < "${SQL_FILE}"

  pass "UUID fix SQL applied successfully."
}

# ---------------------------------------------------------------------------
# Step 2: Run migration tests (phases 1 and 2)
# ---------------------------------------------------------------------------
run_tests() {
  header "Step 2 — Running Migration Tests (phases 1 and 2)"

  if [[ ! -x "${TEST_RUNNER}" ]]; then
    fail "Test runner not found or not executable: ${TEST_RUNNER}"
    exit 1
  fi

  local extra_args=()
  # Forward --verbose if passed to this script
  for arg in "$@"; do
    [[ "${arg}" == "--verbose" ]] && extra_args+=("--verbose")
  done

  "${TEST_RUNNER}" --phase=1 "${extra_args[@]}"
  "${TEST_RUNNER}" --phase=2 "${extra_args[@]}"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  echo -e "${BOLD}Setup and Test — Go Migration${RESET}"
  date

  apply_uuid_fix
  run_tests "$@"

  echo ""
  echo -e "${GREEN}${BOLD}setup-and-test.sh completed successfully.${RESET}"
}

main "$@"
