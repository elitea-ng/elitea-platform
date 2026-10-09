#!/usr/bin/env bash
# run-migration-tests.sh
# Validates the Go migration by running smoke tests, read-only API tests, and full CRUD tests.
#
# ── Why the pytest EXIT STATUS is read here (issue #429) ─────────────────────
#
# Each phase used to wrap pytest in `set +e` and pipe it into `tee`, then read
# the result out of the prose with `grep -Eo '[0-9]+ failed'`. Both the `set +e`
# and the pipe discard pytest's status, and the shapes that print NO "N failed"
# line are the dangerous ones:
#
#   exit 2  interrupted            exit 4  usage error
#   exit 3  internal error         exit 5  NO test was collected
#
# A collection error, an import error or a wrong test path therefore recorded
# `failed=0`, and the phase reported a pass while running nothing at all.
#
# The rule now: pytest's exit status decides. The parsed counts are for the
# report only. A non-zero status with no parsed failure still records a failure,
# and it names which of the five statuses it was.
#
# ── Skips ────────────────────────────────────────────────────────────────────
#
# A phase that could not run is counted and printed, and it makes this script
# exit non-zero. Pass --allow-skips for the older soft behaviour; the reason for
# each skip is printed either way.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
# Overridable so the runner can be pointed at a checkout somewhere else — and
# so this script's own guards can be exercised without moving the real ones.
API_TESTING_DIR="${API_TESTING_DIR:-/Users/Alexander_Kharkevich/projects/eliteaai/elitea-api-testing}"
E2E_TESTING_DIR="${E2E_TESTING_DIR:-/Users/Alexander_Kharkevich/projects/eliteaai/elitea-testing/automation}"

BASE_URL="http://localhost"
# The bearer token comes from the environment and is never committed. Mint a
# personal access token on the stack under test and export it, for example:
#   export ELITEA_MIGRATION_TEST_TOKEN=<token>
JWT_TOKEN="${ELITEA_MIGRATION_TEST_TOKEN:-}"

SMOKE_ENDPOINTS=(
  "/healthz"
  "/api/v2/elitea_core/applications/prompt_lib/1"
  "/api/v2/configurations/configurations/1"
  "/api/v2/artifacts/buckets/default/1"
  "/api/v2/elitea_core/conversations/prompt_lib/1"
  "/api/v2/elitea_core/tools/prompt_lib/1"
  "/api/v2/elitea_core/tags/prompt_lib/1"
  "/api/v2/elitea_core/author/prompt_lib/1"
)

# Env vars exported for pytest processes
export TEST_APP_HOST="http://localhost"
export TEST_PROJECT_ID="1"
export TEST_USER_NAME="system@centry.user"
export TEST_USER_ID="1"
export TEST_AUTH_TOKEN="${JWT_TOKEN}"
export TEST_SHARED_PROJECT_ID="4"
export TEST_PUBLIC_PROJECT_ID="1"
export TEST_CHAT_MODEL_NAME="gpt-4"
export TEST_SHARED_CHAT_MODEL_NAME="gpt-5.2"
export TEST_PGVECTOR_CONNECTION_STRING="postgresql://eliteausr:eliteausr@localhost:5432/eliteadmstage2"
export TEST_AZURE_OPENAI_API_BASE="https://test-azure-openai.openai.azure.com/"
export TEST_AZURE_OPENAI_API_KEY="test_azure_api_key_123456789"
export TEST_AWS_ACCESS_KEY_ID="AKIAIOSFODNN7EXAMPLE"
export TEST_AWS_SECRET_ACCESS_KEY="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
export TEST_AWS_REGION_NAME="us-east-1"
export TEST_EMBEDDING_MODEL_NAME="text-embedding-ada-002"
export TEST_GITHUB_REPOSITORY="octocat/Hello-World"
export TEST_GITHUB_ACCESS_TOKEN="ghp_placeholder_not_real"
export TEST_X_SECRET="staging-secret-for-local-dev"

export ELITEA_URL="http://localhost"
export ELITEA_API_BASE="http://localhost/api/v2"
export ELITEA_API_TOKEN="${JWT_TOKEN}"
export ELITEA_PROJECT_ID="1"
export HEADLESS="true"
export APP_PREFIX=""
export DEFAULT_MODEL_NAME="gpt-4"

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
PHASE="all"
VERBOSE=0
ALLOW_SKIPS=0

for arg in "$@"; do
  case "${arg}" in
    --phase=*)
      PHASE="${arg#--phase=}"
      ;;
    --verbose)
      VERBOSE=1
      ;;
    --allow-skips)
      ALLOW_SKIPS=1
      ;;
    *)
      echo "Unknown argument: ${arg}" >&2
      echo "Usage: $0 [--phase=1|2|3|all] [--verbose] [--allow-skips]" >&2
      exit 1
      ;;
  esac
done

if [[ -z "${JWT_TOKEN}" ]]; then
  echo "ELITEA_MIGRATION_TEST_TOKEN is not set; export a token for the stack under test." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Color helpers
# ---------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
RESET='\033[0m'

pass() { echo -e "${GREEN}[PASS]${RESET} $*"; }
fail() { echo -e "${RED}[FAIL]${RESET} $*"; }
info() { echo -e "${CYAN}[INFO]${RESET} $*"; }
warn() { echo -e "${YELLOW}[WARN]${RESET} $*"; }
header() { echo -e "\n${BOLD}${CYAN}=== $* ===${RESET}"; }

# ---------------------------------------------------------------------------
# Tracking
# ---------------------------------------------------------------------------
PHASE1_PASS=0; PHASE1_FAIL=0; PHASE1_TIME=0
PHASE2_PASS=0; PHASE2_FAIL=0; PHASE2_TIME=0
PHASE3_PASS=0; PHASE3_FAIL=0; PHASE3_TIME=0
OVERALL_EXIT=0
SKIPS=0
SKIP_REASONS=""

# skipped records a step that did NOT run. It is printed on every run and it is
# counted, so a run made entirely of skips can never read as a pass.
skipped() {
  SKIPS=$((SKIPS + 1))
  SKIP_REASONS="${SKIP_REASONS}
  - $1"
  warn "SKIPPED: $1"
}

record_result() {
  local phase="$1" passed="$2" failed="$3" elapsed="$4"
  case "${phase}" in
    phase1) PHASE1_PASS="${passed}"; PHASE1_FAIL="${failed}"; PHASE1_TIME="${elapsed}" ;;
    phase2) PHASE2_PASS="${passed}"; PHASE2_FAIL="${failed}"; PHASE2_TIME="${elapsed}" ;;
    phase3) PHASE3_PASS="${passed}"; PHASE3_FAIL="${failed}"; PHASE3_TIME="${elapsed}" ;;
  esac
  if [[ "${failed}" -gt 0 ]]; then
    OVERALL_EXIT=1
  fi
}

# ---------------------------------------------------------------------------
# pytest, run for its EXIT STATUS
# ---------------------------------------------------------------------------
# Sets PYTEST_RC, PYTEST_PASSED and PYTEST_FAILED. The counts are parsed for the
# report; the STATUS is what decides pass or fail. See the header for why.
PYTEST_RC=0
PYTEST_PASSED=0
PYTEST_FAILED=0
run_pytest() {
  local dir="$1"; shift
  local out
  out=$(mktemp)

  if [[ ! -d "${dir}" ]]; then
    fail "test directory not found: ${dir}"
    PYTEST_RC=1; PYTEST_PASSED=0; PYTEST_FAILED=1
    rm -f "${out}"
    return 0
  fi

  local rc=0
  set +e
  (cd "${dir}" && python3 -m pytest "$@" 2>&1) | tee "${out}"
  # Read IMMEDIATELY after the pipeline. Any command in between replaces
  # PIPESTATUS, and the whole point of this line is that `tee` is not the
  # process whose status matters.
  rc=${PIPESTATUS[0]}
  set -e

  local p f
  p=$(grep -Eo '[0-9]+ passed' "${out}" | grep -Eo '[0-9]+' | tail -1 || echo 0)
  f=$(grep -Eo '[0-9]+ failed' "${out}" | grep -Eo '[0-9]+' | tail -1 || echo 0)
  rm -f "${out}"

  PYTEST_RC="${rc}"
  PYTEST_PASSED="${p:-0}"
  PYTEST_FAILED="${f:-0}"

  # The status wins. These are the shapes that print no "N failed" line at all.
  if [[ "${PYTEST_RC}" -ne 0 && "${PYTEST_FAILED}" -eq 0 ]]; then
    case "${PYTEST_RC}" in
      5) fail "pytest collected NO test in ${dir} (exit 5). The path or the filter matches nothing, so this phase measured nothing." ;;
      2) fail "pytest was interrupted in ${dir} (exit 2)." ;;
      3) fail "pytest hit an internal error in ${dir} (exit 3) — usually a collection or import error." ;;
      4) fail "pytest usage error in ${dir} (exit 4) — check the arguments this script passes." ;;
      *) fail "pytest exited ${PYTEST_RC} in ${dir} and printed no failure count." ;;
    esac
    PYTEST_FAILED=1
  fi
}

# ---------------------------------------------------------------------------
# Prerequisites
# ---------------------------------------------------------------------------
check_prerequisites() {
  header "Checking prerequisites"
  local missing=0

  for cmd in python3 curl; do
    if command -v "${cmd}" &>/dev/null; then
      pass "${cmd} found at $(command -v "${cmd}")"
    else
      fail "${cmd} not found in PATH"
      missing=$((missing + 1))
    fi
  done

  # pytest can be installed as pytest or python3 -m pytest
  if command -v pytest &>/dev/null || python3 -m pytest --version &>/dev/null 2>&1; then
    pass "pytest available"
  else
    warn "pytest not found globally — will rely on per-venv install"
  fi

  if [[ "${missing}" -gt 0 ]]; then
    fail "Missing ${missing} prerequisite(s). Aborting."
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Dependency installation
# ---------------------------------------------------------------------------
install_deps() {
  local repo_dir="$1"
  local label="$2"

  if [[ ! -f "${repo_dir}/requirements.txt" ]]; then
    warn "No requirements.txt in ${repo_dir} — skipping install"
    return 0
  fi

  info "Installing dependencies for ${label}..."
  if [[ "${VERBOSE}" -eq 1 ]]; then
    python3 -m pip install -q -r "${repo_dir}/requirements.txt"
  else
    python3 -m pip install -q -r "${repo_dir}/requirements.txt" 2>&1 | tail -5
  fi
}

# ---------------------------------------------------------------------------
# Phase 1: Smoke tests
# ---------------------------------------------------------------------------
run_phase1() {
  header "Phase 1 — Smoke Tests"
  local start end elapsed passed=0 failed=0

  start=$(date +%s)

  # curl endpoint probes
  info "Probing ${#SMOKE_ENDPOINTS[@]} endpoints on ${BASE_URL}..."
  for ep in "${SMOKE_ENDPOINTS[@]}"; do
    local url="${BASE_URL}${ep}"
    local status
    status=$(curl -s -o /dev/null -w "%{http_code}" \
      -H "Authorization: Bearer ${JWT_TOKEN}" \
      --connect-timeout 5 --max-time 10 \
      "${url}" 2>/dev/null || echo "000")

    if [[ "${status}" =~ ^(200|201|204|400|401|403|404|405|422)$ ]]; then
      pass "HTTP ${status}  ${ep}"
      passed=$((passed + 1))
    else
      fail "HTTP ${status}  ${ep}  (expected 2xx/4xx, got ${status})"
      failed=$((failed + 1))
    fi
  done

  # elitea-testing health tests — skip if playwright not installed (requires browser)
  local health_test_dir="${E2E_TESTING_DIR}"
  if [[ ! -d "${health_test_dir}" ]]; then
    skipped "no elitea-testing checkout at ${health_test_dir} — the e2e smoke tests did not run"
  elif ! python3 -c "import playwright" &>/dev/null; then
    skipped "playwright is not installed — the e2e smoke tests did not run (the curl probes above are a weaker measure)"
  else
    info "Running elitea-testing health/smoke tests..."
    install_deps "${health_test_dir}" "elitea-testing"

    local pytest_args=("-x" "--tb=short" "-q")
    [[ "${VERBOSE}" -eq 1 ]] && pytest_args=("--tb=long" "-v")

    local test_target=""
    for candidate in "tests/test_health" "tests/health" "tests/smoke" "tests/"; do
      if [[ -e "${health_test_dir}/${candidate}" ]]; then
        test_target="${candidate}"
        break
      fi
    done

    if [[ -z "${test_target}" ]]; then
      skipped "no health test directory in ${health_test_dir} — the e2e smoke tests did not run"
    else
      run_pytest "${health_test_dir}" "${pytest_args[@]}" "${test_target}"
      passed=$((passed + PYTEST_PASSED))
      failed=$((failed + PYTEST_FAILED))
    fi
  fi

  end=$(date +%s)
  elapsed=$((end - start))

  record_result "phase1" "${passed}" "${failed}" "${elapsed}"
  echo -e "\nPhase 1 complete: ${GREEN}${passed} passed${RESET}  ${RED}${failed} failed${RESET}  (${elapsed}s)"
}

# ---------------------------------------------------------------------------
# Phase 2: Read-only API tests
# ---------------------------------------------------------------------------
run_phase2() {
  header "Phase 2 — Core API Tests (applications CRUD + validations)"
  local start end elapsed passed=0 failed=0

  start=$(date +%s)
  install_deps "${API_TESTING_DIR}" "elitea-api-testing"

  local pytest_args=("--tb=short" "-q")
  [[ "${VERBOSE}" -eq 1 ]] && pytest_args=("--tb=long" "-v")

  # Run the core application tests (CRUD, validations, list)
  # Skip publish/export/import/fork/categories which require unimplemented features
  run_pytest "${API_TESTING_DIR}" "${pytest_args[@]}" \
    applications/test_applications.py \
    -k "not toolkit"
  passed="${PYTEST_PASSED}"; failed="${PYTEST_FAILED}"

  end=$(date +%s)
  elapsed=$((end - start))

  record_result "phase2" "${passed}" "${failed}" "${elapsed}"
  echo -e "\nPhase 2 complete: ${GREEN}${passed} passed${RESET}  ${RED}${failed} failed${RESET}  (${elapsed}s)"
}

# ---------------------------------------------------------------------------
# Phase 3: Full CRUD suite
# ---------------------------------------------------------------------------
run_phase3() {
  header "Phase 3 — Full CRUD Suite"
  local start end elapsed passed=0 failed=0

  start=$(date +%s)
  install_deps "${API_TESTING_DIR}" "elitea-api-testing"

  local pytest_args=("--tb=short" "-q")
  [[ "${VERBOSE}" -eq 1 ]] && pytest_args=("--tb=long" "-v")

  run_pytest "${API_TESTING_DIR}" "${pytest_args[@]}"
  passed="${PYTEST_PASSED}"; failed="${PYTEST_FAILED}"

  end=$(date +%s)
  elapsed=$((end - start))

  record_result "phase3" "${passed}" "${failed}" "${elapsed}"
  echo -e "\nPhase 3 complete: ${GREEN}${passed} passed${RESET}  ${RED}${failed} failed${RESET}  (${elapsed}s)"
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
print_summary() {
  header "Test Summary"
  printf "%-12s %8s %8s %8s\n" "Phase" "Passed" "Failed" "Time(s)"
  printf "%-12s %8s %8s %8s\n" "------------" "--------" "--------" "--------"

  if [[ "${PHASE1_PASS}" -gt 0 || "${PHASE1_FAIL}" -gt 0 ]]; then
    local color="${GREEN}"; [[ "${PHASE1_FAIL}" -gt 0 ]] && color="${RED}"
    printf "${color}%-12s %8s %8s %8s${RESET}\n" "phase1" "${PHASE1_PASS}" "${PHASE1_FAIL}" "${PHASE1_TIME}"
  fi
  if [[ "${PHASE2_PASS}" -gt 0 || "${PHASE2_FAIL}" -gt 0 ]]; then
    local color="${GREEN}"; [[ "${PHASE2_FAIL}" -gt 0 ]] && color="${RED}"
    printf "${color}%-12s %8s %8s %8s${RESET}\n" "phase2" "${PHASE2_PASS}" "${PHASE2_FAIL}" "${PHASE2_TIME}"
  fi
  if [[ "${PHASE3_PASS}" -gt 0 || "${PHASE3_FAIL}" -gt 0 ]]; then
    local color="${GREEN}"; [[ "${PHASE3_FAIL}" -gt 0 ]] && color="${RED}"
    printf "${color}%-12s %8s %8s %8s${RESET}\n" "phase3" "${PHASE3_PASS}" "${PHASE3_FAIL}" "${PHASE3_TIME}"
  fi

  echo ""
  # The skip count is part of the result, not a footnote. A run with no failure
  # and three skips measured a third of what its name says it measures.
  if [[ "${SKIPS}" -gt 0 ]]; then
    echo -e "${YELLOW}${BOLD}${SKIPS} step(s) did not run:${RESET}${SKIP_REASONS}"
    if [[ "${ALLOW_SKIPS}" -eq 1 ]]; then
      echo -e "${YELLOW}--allow-skips was given, so these do not fail the run.${RESET}"
    else
      OVERALL_EXIT=1
    fi
  fi

  echo ""
  if [[ "${OVERALL_EXIT}" -eq 0 ]]; then
    echo -e "${GREEN}${BOLD}All phases passed, and every step ran.${RESET}"
  elif [[ "${SKIPS}" -gt 0 && "${ALLOW_SKIPS}" -eq 0 ]]; then
    echo -e "${RED}${BOLD}FAILED: a step that did not run is not a step that passed.${RESET}"
  else
    echo -e "${RED}${BOLD}One or more phases failed.${RESET}"
  fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  echo -e "${BOLD}Go Migration Test Runner${RESET}"
  echo "Phase: ${PHASE}  Verbose: ${VERBOSE}"
  echo "Base URL: ${BASE_URL}"
  date

  check_prerequisites

  case "${PHASE}" in
    1)       run_phase1 ;;
    2)       run_phase2 ;;
    3)       run_phase3 ;;
    all)
      run_phase1
      run_phase2
      run_phase3
      ;;
    *)
      echo "Invalid --phase value: ${PHASE}. Must be 1, 2, 3, or all." >&2
      exit 1
      ;;
  esac

  print_summary
  exit "${OVERALL_EXIT}"
}

main "$@"
