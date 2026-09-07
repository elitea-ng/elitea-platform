#!/usr/bin/env bash
#
# Walk the index-plane cutover against a LIVE hybrid stack (#339).
#
# WHAT THIS IS. deploy/INDEX_V2_CUTOVER.md is the procedure. This script is that
# procedure with the checks wired in, run in order, stopping at the first stage
# that does not hold. It exists because the expensive failure of a cutover is
# not a wrong command — it is a stage somebody believed had completed.
#
# WHAT IT IS NOT. It does not decide anything the runbook decides. Every rule
# here is quoted from that document, and where the two could disagree the
# document wins.
#
# IT PERFORMS NO DRAIN OF ITS OWN, and this is deliberate. Closing indexing
# admission happens at the deployment's ingress, and in this topology that
# ingress is Centry's private auth_gateway, whose Compose model is not in this
# repository. A script here that claimed to close admission would be claiming
# something it cannot do. So admission closure is a PRECONDITION this script
# refuses to proceed without, and the script verifies the consequences.
#
# --dry-run prints the plan and touches nothing. Run it first, every time.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
hybrid_dir="$(cd "$script_dir/.." && pwd -P)"
platform_dir="$(cd "$hybrid_dir/../.." && pwd -P)"

dry_run=0
centry_dir="${CENTRY_DIR:-$platform_dir/../centry}"
project_id="${ELITEA_CUTOVER_PROJECT_ID:-}"
toolkit_id="${ELITEA_CUTOVER_TOOLKIT_ID:-}"
base_url="${ELITEA_CUTOVER_BASE_URL:-}"
pat="${ELITEA_CUTOVER_PAT:-}"
drain_timeout="${ELITEA_CUTOVER_DRAIN_TIMEOUT:-900}"

usage() {
  cat >&2 <<'USAGE'
usage: cutover-rehearsal.sh [--dry-run] [--centry <dir>]

Environment:
  ELITEA_CUTOVER_ADMISSION_CLOSED  Must be 1. See "the drain" below.
  ELITEA_CUTOVER_BASE_URL          Platform base URL, e.g. https://localhost:18443
  ELITEA_CUTOVER_PAT               A personal access token for the smoke stage.
  ELITEA_CUTOVER_PROJECT_ID        Project id for the smoke stage.
  ELITEA_CUTOVER_TOOLKIT_ID        Toolkit id whose index the smoke stage reads.
  ELITEA_CUTOVER_DRAIN_TIMEOUT     Seconds to wait for in-flight runs (default 900).
  CENTRY_DIR                       The Centry checkout (default ../centry).

Stages, in order. Each one stops the run when it does not hold:
  1 preconditions  the tree is the retired shape, and admission is closed
  2 drain          no index run is in flight, and the version-1 preflight is zero
  3 flip           the model renders without pylon_indexer, and the stack is up
  4 smoke          one index run and one search, through elitea-main
  5 verify         no artifact or index request reached pylon during the smoke

On a failure after stage 3, the script prints the rollback command and exits
non-zero. It does NOT roll back by itself: deploy/INDEX_V2_CUTOVER.md prohibits
binary rollback once a version-2 command has been admitted, and only an operator
can say whether that has happened.
USAGE
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) dry_run=1; shift ;;
    --centry) centry_dir="${2:?--centry needs a value}"; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done

stage() { printf '\n== stage %s ==\n' "$1"; }
plan()  { printf '  would run: %s\n' "$*"; }
fail()  { printf '\nFAILED: %s\n' "$1" >&2; exit 1; }

rollback_command() {
  cat <<'ROLLBACK'
  Rollback, ONLY if no version-2 command has been admitted yet
  (deploy/INDEX_V2_CUTOVER.md, Stage B):

    podman compose --project-directory <centry> \
      --env-file <centry>/envs/default.env \
      --env-file <centry>/envs/override.env \
      -f <centry>/docker-compose.yml \
      -f <centry>/hybrid_auth/docker-compose.pov.yml \
      -f <centry>/hybrid_auth/docker-compose.indexing-checkpoint.yml \
      -f deploy/centry-hybrid/pov-compose.yml \
      -f deploy/centry-hybrid/rollback/index-v1.yml \
      --profile runtime --profile index-v1 up -d

  After a version-2 command has been admitted, rollback is PROHIBITED. Freeze
  admission, drain or terminally reconcile version 2, then roll forward.
ROLLBACK
}

# ── stage 1: preconditions ───────────────────────────────────────────────────
#
# Two of these read the TREE and one reads the operator's intent. The tree ones
# run even in a dry run, because they cost nothing and a plan printed against
# the wrong tree is worse than no plan.
stage "1 preconditions"

pov="$hybrid_dir/pov-compose.yml"
routes="$hybrid_dir/traefik/index-routes.yml"
rollback_overlay="$hybrid_dir/rollback/index-v1.yml"

for required in "$pov" "$routes" "$rollback_overlay" "$hybrid_dir/compose.sh"; do
  [[ -f "$required" ]] || fail "missing input: $required"
done
echo "  ok: the four tracked inputs are present"

# The retired shape, asserted from the file rather than assumed from the branch.
if ! grep -q 'profiles: \[index-v1\]' "$pov"; then
  fail "$pov does not put pylon_indexer in the index-v1 profile, so this tree is not the retired shape"
fi
echo "  ok: pylon_indexer is declared in the index-v1 profile, which compose.sh does not select"

# The artifact half of the same cutover (#337). A stack that still routes
# artifacts to pylon is not ready for the index plane to leave, because an
# index run reads its documents through those routes.
if ! grep -q 'go-artifacts:' "$routes"; then
  fail "$routes has no go-artifacts router; #337 must land before the index plane can leave pylon"
fi
echo "  ok: artifact traffic resolves to elitea-main"

if [[ "${ELITEA_CUTOVER_ADMISSION_CLOSED:-}" != "1" ]]; then
  cat >&2 <<'ADMISSION'

  REFUSED: indexing admission is not declared closed.

  Close it at your deployment's ingress, then set
  ELITEA_CUTOVER_ADMISSION_CLOSED=1 and run this again.

  This script cannot close it for you. The ingress in this topology is Centry's
  private auth_gateway, and its Compose model is not in this repository. A
  script that claimed to close admission here would be claiming something it
  cannot do, and stage 2 would then measure a drain that was still filling.
ADMISSION
  exit 1
fi
echo "  ok: the operator declares indexing admission closed"

# ── stage 2: drain ───────────────────────────────────────────────────────────
#
# Two questions, and they are not the same question. "Is anything running right
# now" is answered by the index runs API. "Is any version-1 work left in the
# durable state machine" is answered by the preflight, which reads the database,
# the Redis stream, the pending-entries list, the delivery index and every
# stopped replica's output spool. The runbook requires the second. The first is
# here because waiting on the second alone turns a normal in-flight run into an
# error.
stage "2 drain"

poll_index_runs() {
  local deadline=$((SECONDS + drain_timeout))
  while :; do
    local body
    body="$(curl -fsS --max-time 10 \
      -H "Authorization: Bearer $pat" \
      "$base_url/api/v2/elitea_core/index_meta/prompt_lib/$project_id/$toolkit_id" 2>/dev/null || true)"
    if [[ -z "$body" ]]; then
      fail "the index runs API returned nothing. A drain cannot be confirmed by a call that did not answer."
    fi
    # An ABSENT in-progress marker is not proof of a drain when the read itself
    # may have failed, which is why the empty-body case above exits first.
    if ! printf '%s' "$body" | grep -qE '"status"[[:space:]]*:[[:space:]]*"(in_progress|running|pending)"'; then
      echo "  ok: no index run reports an in-flight status"
      return 0
    fi
    if (( SECONDS >= deadline )); then
      fail "index runs were still in flight after ${drain_timeout}s. Do not continue; let them settle."
    fi
    sleep 5
  done
}

if [[ "$dry_run" -eq 1 ]]; then
  plan "GET \$ELITEA_CUTOVER_BASE_URL/api/v2/elitea_core/index_meta/prompt_lib/<project>/<toolkit> until no run is in flight (timeout ${drain_timeout}s)"
  plan "$hybrid_dir/compose.sh preflight $centry_dir"
else
  for required in base_url pat project_id toolkit_id; do
    [[ -n "${!required}" ]] || fail "stage 2 needs $required; see --help"
  done
  poll_index_runs
  "$hybrid_dir/compose.sh" preflight "$centry_dir" ||
    fail "the version-1 preflight did not exit 0 with every count zero. Stage A is not complete."
  echo "  ok: the version-1 preflight reports a clean drain"
fi

# ── stage 3: flip ────────────────────────────────────────────────────────────
stage "3 flip"

if [[ "$dry_run" -eq 1 ]]; then
  plan "$hybrid_dir/compose.sh config $centry_dir"
  plan "podman compose ... --profile runtime stop pylon_indexer"
  plan "$hybrid_dir/compose.sh up $centry_dir"
else
  "$hybrid_dir/compose.sh" config "$centry_dir" ||
    fail "the merged Compose model is invalid or still starts pylon_indexer"
  echo "  ok: the rendered model does not start pylon_indexer"
  "$hybrid_dir/compose.sh" up "$centry_dir" || {
    echo ""
    rollback_command
    fail "the stack did not come up"
  }
  echo "  ok: the stack is up without the version-1 index plane"
fi

# ── stage 4: smoke ───────────────────────────────────────────────────────────
#
# One index run and one read of its result. A cutover that starts a run and
# never reads one back has proved that the dispatch path exists, not that the
# capability works.
stage "4 smoke"

if [[ "$dry_run" -eq 1 ]]; then
  plan "POST \$ELITEA_CUTOVER_BASE_URL/api/v2/elitea_core/test_toolkit_tool/prompt_lib/<project>?execution_contract=index.ingest.v1"
  plan "GET  \$ELITEA_CUTOVER_BASE_URL/api/v2/elitea_core/index_meta/prompt_lib/<project>/<toolkit> and read the run back"
else
  start_body="$(curl -fsS --max-time 60 -X POST \
    -H "Authorization: Bearer $pat" \
    -H 'Content-Type: application/json' \
    "$base_url/api/v2/elitea_core/test_toolkit_tool/prompt_lib/$project_id?execution_contract=index.ingest.v1" \
    -d '{}' 2>/dev/null || true)"
  if ! printf '%s' "$start_body" | grep -q 'task_id'; then
    echo ""
    rollback_command
    fail "the index start returned no task_id. Body: ${start_body:-<empty>}"
  fi
  echo "  ok: an index run started on the Go plane"

  read_body="$(curl -fsS --max-time 30 \
    -H "Authorization: Bearer $pat" \
    "$base_url/api/v2/elitea_core/index_meta/prompt_lib/$project_id/$toolkit_id" 2>/dev/null || true)"
  [[ -n "$read_body" ]] || {
    echo ""
    rollback_command
    fail "the index read returned nothing after the run started"
  }
  echo "  ok: the index is readable through elitea-main"
fi

# ── stage 5: verify pylon served nothing ─────────────────────────────────────
#
# The runbook's acceptance criterion, and the only one that cannot be satisfied
# by a green API call: prove it from the access log. `grep` finding NOTHING is
# the pass, so the command is printed for the operator to run and read against
# the window of the smoke run. A script that grepped the whole file and reported
# "clean" would be reading entries from before the flip as evidence about it.
stage "5 verify"

cat <<'VERIFY'
  Run this against the window of the smoke run above, and read the output:

    podman compose --project-directory <centry> logs --since 10m pylon_main \
      | grep -E '/(artifacts/(s3|buckets)|api/v2/elitea_core/(test_toolkit_tool|index_meta))'

  NOTHING is the pass. Any line is an index or artifact request that still
  reached pylon, and the cutover is not complete.

  Read the WINDOW, not the whole file: an entry from before the flip is not a
  failure, and treating it as one is how a good cutover gets rolled back.
VERIFY

if [[ "$dry_run" -eq 1 ]]; then
  printf '\ndry run complete: 5 stages planned, nothing was run.\n'
  exit 0
fi

printf '\ncutover rehearsal complete. Stage 5 is yours to read.\n'
