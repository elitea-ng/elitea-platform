#!/usr/bin/env bash
#
# The test for cutover-rehearsal.sh (#339).
#
# WHY IT EXISTS. The rehearsal script's whole value is that it REFUSES: it stops
# at the first stage that does not hold. A script like that fails silently in
# one direction only — by refusing nothing — and every run then reads as a
# successful rehearsal. `bash -n` cannot see that, and neither can a reviewer
# who reads the happy path.
#
# So this suite drives the two behaviours that matter and cannot be observed
# against a live stack cheaply: the plan the dry run prints, and each refusal.
# Every refusal case runs against a COPY of the tracked hybrid directory, so a
# case that must fail is produced by editing the copy rather than by trusting a
# flag.
#
# It needs no stack, no network and no Centry checkout. About a second.
set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
hybrid_dir="$(cd "$script_dir/.." && pwd -P)"
rehearsal="$script_dir/cutover-rehearsal.sh"

passed=0
failed=0

ok()   { printf '  ok: %s\n' "$1"; passed=$((passed + 1)); }
bad()  { printf '  FAIL: %s\n' "$1" >&2; failed=$((failed + 1)); }

# run <expected-exit> <description> -- <command...>
run_case() {
  local expect="$1" what="$2"; shift 3
  local out status
  out="$("$@" 2>&1)"
  status=$?
  if [ "$status" -ne "$expect" ]; then
    bad "$what: exit $status, expected $expect"
    printf '%s\n' "$out" | sed 's/^/      /' >&2
    LAST_OUTPUT=""
    return 1
  fi
  LAST_OUTPUT="$out"
  return 0
}

contains() {
  case "$LAST_OUTPUT" in
    *"$1"*) ok "$2" ;;
    *) bad "$2 (the output did not contain: $1)" ;;
  esac
}

echo "== the dry run prints the whole plan and runs nothing =="
if run_case 0 "dry run exits 0" -- env ELITEA_CUTOVER_ADMISSION_CLOSED=1 bash "$rehearsal" --dry-run; then
  # Every stage. A dry run that printed three of five would still exit 0, and
  # the plan is the only thing an operator reads before committing to the real
  # run.
  contains "stage 1 preconditions" "stage 1 is planned"
  contains "stage 2 drain"         "stage 2 is planned"
  contains "stage 3 flip"          "stage 3 is planned"
  contains "stage 4 smoke"         "stage 4 is planned"
  contains "stage 5 verify"        "stage 5 is planned"
  # The commands, not only the headings. A stage that prints its title and no
  # command is a stage that would do nothing in the real run.
  contains "compose.sh preflight"    "the drain names the version-1 preflight"
  contains "compose.sh config"       "the flip names the model validation"
  contains "compose.sh up"           "the flip names the bring-up"
  contains "index.ingest.v1"         "the smoke names the index start contract"
  contains "index_meta/prompt_lib"   "the smoke names the read-back"
  contains "dry run complete"        "the dry run says it ran nothing"
  # The refusal to grep the whole log. This is the acceptance criterion of #339
  # that a green API call cannot satisfy.
  contains "Read the WINDOW, not the whole file" "stage 5 states the log-reading rule"
fi

echo "== it refuses when admission is not declared closed =="
if run_case 1 "an unset ELITEA_CUTOVER_ADMISSION_CLOSED refuses" -- \
    env -u ELITEA_CUTOVER_ADMISSION_CLOSED bash "$rehearsal" --dry-run; then
  contains "indexing admission is not declared closed" "the refusal names the cause"
  contains "cannot close it for you" "the refusal says why the script does not do it itself"
fi

if run_case 1 "a value other than 1 refuses" -- \
    env ELITEA_CUTOVER_ADMISSION_CLOSED=yes bash "$rehearsal" --dry-run; then
  contains "indexing admission is not declared closed" "a truthy-looking value is still refused"
fi

echo "== it refuses a tree that is not the retired shape =="
#
# Both cases edit a COPY. The point of each is that the script reads the FILE
# and does not assume the branch it is on is correct.
copy_root="$(mktemp -d)"
trap 'rm -rf "$copy_root"' EXIT
cp -R "$hybrid_dir" "$copy_root/centry-hybrid"
copy_rehearsal="$copy_root/centry-hybrid/scripts/cutover-rehearsal.sh"

# Sanity first: the untouched copy still passes. Without this, both cases below
# could be failing for a reason the copy introduced, and the suite would report
# two refusals it did not actually test.
if run_case 0 "the untouched copy still plans a full run" -- \
    env ELITEA_CUTOVER_ADMISSION_CLOSED=1 bash "$copy_rehearsal" --dry-run; then
  contains "stage 5 verify" "the copy behaves like the tracked tree"
fi

# 1. pylon_indexer is no longer confined to the index-v1 profile.
sed -i.bak 's/profiles: \[index-v1\]/profiles: [runtime]/' "$copy_root/centry-hybrid/pov-compose.yml"
if run_case 1 "a pov-compose.yml that would START pylon_indexer refuses" -- \
    env ELITEA_CUTOVER_ADMISSION_CLOSED=1 bash "$copy_rehearsal" --dry-run; then
  contains "not the retired shape" "the refusal names the unretired service"
fi
mv "$copy_root/centry-hybrid/pov-compose.yml.bak" "$copy_root/centry-hybrid/pov-compose.yml"

# 2. artifacts still route to pylon (#337 not landed).
sed -i.bak 's/go-artifacts:/runtime-worker-current-artifacts:/' \
  "$copy_root/centry-hybrid/traefik/index-routes.yml"
if run_case 1 "an edge that still routes artifacts to pylon refuses" -- \
    env ELITEA_CUTOVER_ADMISSION_CLOSED=1 bash "$copy_rehearsal" --dry-run; then
  contains "#337 must land before" "the refusal names the artifact dependency"
fi
mv "$copy_root/centry-hybrid/traefik/index-routes.yml.bak" \
   "$copy_root/centry-hybrid/traefik/index-routes.yml"

# 3. a missing tracked input.
rm -f "$copy_root/centry-hybrid/rollback/index-v1.yml"
if run_case 1 "a missing rollback overlay refuses" -- \
    env ELITEA_CUTOVER_ADMISSION_CLOSED=1 bash "$copy_rehearsal" --dry-run; then
  contains "missing input" "the refusal names the missing file"
fi

printf '\n%d passed, %d failed.\n' "$passed" "$failed"
# A floor on the SUITE, not only on each case. A suite whose cases stopped
# running would otherwise report "0 failed" and read as a pass.
if [ "$passed" -lt 15 ]; then
  printf 'only %d assertion(s) reported a result; the suite measured less than it claims.\n' "$passed" >&2
  exit 1
fi
[ "$failed" -eq 0 ] || exit 1
