#!/usr/bin/env bash
# Static API Contract Validator
#
# Reads the Go handler source and asserts, for each listed handler, that the
# response shape it writes is the shape the SPA reads. It runs no server.
#
# Usage: ./scripts/validate_contract_static.sh
#
# ── Why a skip is a failure here (issue #428) ────────────────────────────────
#
# Every negative outcome used to raise a WARN counter, and only a FAIL exited
# non-zero. So a handler file that was renamed, a package that moved or a
# method that was renamed turned its assertion into a WARN, and the script
# printed "0 failed" and exited 0. Measured: renaming eliteacore/handler.go
# turned 11 of the 17 assertions into warnings, and the script still exited 0.
#
# The rule now: an assertion that cannot be made is a FAILURE. "The file is
# gone", "the function is gone" and "the shape is unreadable" are all reports
# that this validator did not measure the thing it names, and a validator that
# measured nothing must not report success.
#
# The last line reports how many assertions RAN against how many were listed.
# Read that line, not the exit code alone. The expected count is the length of
# the CHECKS table below, so it can never drift from the table.
set -euo pipefail

HANDLER_DIR="$(cd "$(dirname "$0")/../internal/api/v2" && pwd)"
PASS=0
FAIL=0
RAN=0

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

# Counters are raised with an arithmetic ASSIGNMENT, never with `((X++))`.
# `((X++))` returns the value before the increment, so the first increment of a
# counter that holds 0 exits 1 — and under `set -e` that stops the script on
# its first result. That is the second half of #428, in the sibling script.
pass_result() {
  RAN=$((RAN + 1))
  PASS=$((PASS + 1))
  printf "${GREEN}PASS${NC} %-50s (%s)\n" "$1" "$2"
}
fail_result() {
  RAN=$((RAN + 1))
  FAIL=$((FAIL + 1))
  printf "${RED}FAIL${NC} %-50s (%s)\n" "$1" "$2" >&2
}

check_handler_returns_key() {
  local file="$1"
  local func_name="$2"
  local expected_key="$3"  # "rows" | "items" | "applications" | "categories" | "ARRAY" | "OBJECT"
  local label="$4"

  if [ ! -f "$file" ]; then
    fail_result "$label" "file not found: ${file} — the handler moved or was renamed, so this contract is unmeasured"
    return
  fi

  # Extract function body (from func declaration to closing brace at column 0)
  local func_body
  func_body=$(awk "BEGIN{p=0} /^func.*$func_name\(/{p=1} p{print} p && /^\}/{p=0}" "$file" 2>/dev/null | head -80)

  if [ -z "$func_body" ]; then
    fail_result "$label" "func ${func_name} not found in ${file##*/} — it was renamed or removed, so this contract is unmeasured"
    return
  fi

  # Detect the last writeJSON call pattern in the function
  local has_envelope has_plain_array
  has_envelope=$(echo "$func_body" | grep -c 'writeJSON.*map\[string\]any{' || true)
  # Plain array: literal []any{}, []map[...]{}, or a bare variable (items, icons, roles)
  has_plain_array=$(echo "$func_body" | grep -cE 'writeJSON\(w, [^,]+, (\[\](any|map)[^)]*|[a-z][a-zA-Z]*)\)' || true)

  case "$expected_key" in
    ARRAY)
      if [ "$has_plain_array" -gt 0 ] && [ "$has_envelope" -eq 0 ]; then
        pass_result "$label" "returns plain array"
      elif [ "$has_envelope" -gt 0 ] && [ "$has_plain_array" -eq 0 ]; then
        fail_result "$label" "returns envelope, expected plain array"
      elif [ "$has_plain_array" -gt 0 ] && [ "$has_envelope" -gt 0 ]; then
        # Mixed — both empty-case and data-case (common pattern: empty returns [], data returns items)
        pass_result "$label" "returns plain array with empty fallback"
      else
        fail_result "$label" "no writeJSON call this validator can read — the shape is unmeasured, not correct"
      fi
      ;;
    *)
      if echo "$func_body" | grep -q "\"$expected_key\":"; then
        pass_result "$label" "has key \"${expected_key}\""
      elif [ "$has_plain_array" -gt 0 ]; then
        fail_result "$label" "returns plain array, expected key \"${expected_key}\""
      else
        fail_result "$label" "key \"${expected_key}\" not found in any writeJSON call — the shape is unmeasured, not correct"
      fi
      ;;
  esac
}

# ── The contracts, as one table ──────────────────────────────────────────────
#
# A table rather than 17 separate calls, so the expected assertion count is the
# table's own length. A hardcoded count drifts the first time somebody adds a
# row and forgets the number.
#
# Fields: file|func|expected key (or ARRAY)|label
CHECKS=(
  "eliteacore/handler.go|TrendingAuthors|ARRAY|eliteacore.TrendingAuthors → []"
  "eliteacore/handler.go|Recommendations|applications|eliteacore.Recommendations → {applications}"
  "eliteacore/handler.go|AgentCategories|categories|eliteacore.AgentCategories → {categories}"
  "eliteacore/handler.go|ListUploadedIcons|rows|eliteacore.ListUploadedIcons → {rows}"
  "eliteacore/skill_icon.go|ListSkillIcons|rows|eliteacore.ListSkillIcons → {rows}"
  "eliteacore/handler.go|DefaultIcons|ARRAY|eliteacore.DefaultIcons → []"
  "eliteacore/public_applications.go|PublicApplications|rows|eliteacore.PublicApplications → {rows}"
  "eliteacore/handler.go|Notifications|rows|eliteacore.Notifications → {rows}"
  "eliteacore/handler.go|Users|rows|eliteacore.Users → {rows}"
  "eliteacore/handler.go|Roles|ARRAY|eliteacore.Roles → []"
  "eliteacore/handler.go|Permissions|ARRAY|eliteacore.Permissions → []"
  "eliteacore/handler.go|ListCollections|rows|eliteacore.ListCollections → {rows}"
  "social/handler.go|ListAuthors|ARRAY|social.ListAuthors → []"
  "social/handler.go|TrendingAuthors|ARRAY|social.TrendingAuthors → []"
  "toolkits/handler.go|ListTypes|rows|toolkits.ListTypes → {rows}"
  "toolkits/handler.go|IndexMeta|ARRAY|toolkits.IndexMeta → []"
  "toolkits/handler.go|AvailableTools|tools|toolkits.AvailableTools → {tools}"
  "tags/handler.go|List|rows|tags.List → {rows}"
)
EXPECTED_ASSERTIONS=${#CHECKS[@]}

echo "============================================"
echo "Static API Contract Validation"
echo "Handler dir: $HANDLER_DIR"
echo "============================================"
echo ""

for entry in "${CHECKS[@]}"; do
  IFS='|' read -r c_file c_func c_key c_label <<<"$entry"
  check_handler_returns_key "${HANDLER_DIR}/${c_file}" "$c_func" "$c_key" "$c_label"
done

echo ""
echo "============================================"
printf "Results: ${GREEN}%d passed${NC}, ${RED}%d failed${NC} — %d of %d listed assertions ran\n" \
  "$PASS" "$FAIL" "$RAN" "$EXPECTED_ASSERTIONS"
echo "============================================"

# The count guard. Every branch above raises RAN, so a mismatch means the loop
# itself stopped early. It is cheap and it makes the invariant explicit.
if [ "$RAN" -ne "$EXPECTED_ASSERTIONS" ]; then
  echo "FAILED: only ${RAN} of ${EXPECTED_ASSERTIONS} assertions ran. A skipped assertion is not a pass." >&2
  exit 1
fi

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
