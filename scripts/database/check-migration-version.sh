#!/usr/bin/env bash
# check-migration-version.sh — issue #520.
#
# A migration states its version number when an author writes it. The number is
# only true when the pull request MERGES. Two authors can each read the branch
# head, each find the number free, and each be correct — and still collide.
# It happened twice in one day: #369 and #388 both claimed 0072, #495 and #516
# both claimed 0084. A repeated version makes LoadManifest fail, and a
# deployment then stops.
#
# So this check does not read the pull request's own history. It reads the BASE
# BRANCH as origin holds it AT THE TIME THE CHECK RUNS, takes the highest
# version there, and fails when the pull request adds a migration that is not
# above it.
#
#   scripts/database/check-migration-version.sh <base-ref>
#   MIGRATION_BASE_REF=<base-ref> scripts/database/check-migration-version.sh
#
# <base-ref> is a BRANCH NAME, for example `main` or the release staging
# branch. In a workflow it is ${{ github.base_ref }}.
#
# ── What this does NOT do ────────────────────────────────────────────────────
#
# It does not renumber. A rename at merge time stays necessary, and it belongs
# to the merge queue, because only the queue knows the order in which pull
# requests land. This check finds the collision early and names the free
# number; a person or the queue does the rename.
set -euo pipefail

MIGRATION_ROOT="services/elitea-main/migrations"
SCOPES="shared tenant"
NAME_PATTERN='^[0-9]{4}_[a-z][a-z0-9_]*\.sql$'

BASE_REF="${1:-${MIGRATION_BASE_REF:-}}"

fatal() {
  printf '::error::%s\n' "$1" >&2
  exit 1
}

if [ -z "${BASE_REF}" ]; then
  fatal "no base branch given. Pass it as the first argument or in MIGRATION_BASE_REF. This check cannot fall back to the branch under test: a check that reads the pull request's own tree cannot see the collision."
fi

REPOSITORY_ROOT="$(git rev-parse --show-toplevel)"
cd "${REPOSITORY_ROOT}"

# Prefer the remote-tracking ref. "The base branch at the time the check runs"
# means what origin holds now, not a local copy that somebody fetched last week.
resolve_base() {
  local ref="$1"
  local candidate
  for candidate in "refs/remotes/origin/${ref}" "${ref}"; do
    if git rev-parse --verify --quiet "${candidate}^{commit}" >/dev/null 2>&1; then
      printf '%s' "${candidate}"
      return 0
    fi
  done
  git fetch --no-tags --quiet --depth=1 origin \
    "+refs/heads/${ref}:refs/remotes/origin/${ref}" >/dev/null 2>&1 || true
  if git rev-parse --verify --quiet "refs/remotes/origin/${ref}^{commit}" >/dev/null 2>&1; then
    printf '%s' "refs/remotes/origin/${ref}"
    return 0
  fi
  return 1
}

BASE_RESOLVED="$(resolve_base "${BASE_REF}")" || BASE_RESOLVED=""
if [ -z "${BASE_RESOLVED}" ]; then
  fatal "cannot resolve base branch '${BASE_REF}'. A base this check cannot read is a base it cannot compare against, so it stops here rather than report a pass."
fi
BASE_SHA="$(git rev-parse "${BASE_RESOLVED}")"
printf 'base branch: %s -> %s (%s)\n' "${BASE_REF}" "${BASE_RESOLVED}" "${BASE_SHA}"

failures=0
new_total=0
checked_total=0

for scope in ${SCOPES}; do
  directory="${MIGRATION_ROOT}/${scope}"

  base_paths="$(git ls-tree -r --name-only "${BASE_RESOLVED}" -- "${directory}/" || true)"
  if [ -z "${base_paths}" ]; then
    fatal "the base branch holds no file under ${directory}/. The history moved, so this check measured nothing."
  fi

  base_versions=""
  base_max=0
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    file="${path##*/}"
    printf '%s' "${file}" | grep -Eq "${NAME_PATTERN}" || continue
    version="${file%%_*}"
    base_versions="${base_versions} ${version}"
    if [ "$((10#${version}))" -gt "${base_max}" ]; then
      base_max="$((10#${version}))"
    fi
  done <<EOF
${base_paths}
EOF

  if [ "${base_max}" -eq 0 ]; then
    fatal "read no migration version out of ${directory}/ on the base branch. This check measured nothing."
  fi

  head_paths="$(find "${directory}" -maxdepth 1 -type f -name '*.sql' | sort)"
  if [ -z "${head_paths}" ]; then
    fatal "this branch holds no file under ${directory}/. The history moved, so this check measured nothing."
  fi

  # Pass one: split the branch's files into "already on the base" and "new".
  new_paths=""
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    checked_total=$((checked_total + 1))
    file="${path##*/}"
    if ! printf '%s' "${file}" | grep -Eq "${NAME_PATTERN}"; then
      printf '::error file=%s::migration filename %s does not match NNNN_lower_snake_case.sql, so LoadManifest refuses the whole history.\n' \
        "${path}" "${file}" >&2
      failures=$((failures + 1))
      continue
    fi
    # A here-string, not `printf | grep -q`: grep -q exits at its first
    # match, printf then dies of SIGPIPE on a list larger than the pipe
    # buffer, and under pipefail that failure read as "not on the base". A
    # file already on the base was then reported as a NEW migration that
    # collides with itself (PR #1294: 0094_mcp_prebuilt_catalogue.sql).
    if grep -Fxq -- "${path}" <<<"${base_paths}"; then
      continue
    fi
    # A RENAME keeps its version: the base file at this number is gone from
    # the branch, so nothing else claims it. migrate/released.go is what lets
    # a deployed ledger accept the new name (PR #1405 renamed 0091 and 0124).
    version="${file%%_*}"
    holder="$(grep -E "/${version}_[^/]*\.sql$" <<<"${base_paths}" | head -1 || true)"
    if [ -n "${holder}" ] && [ ! -e "${holder}" ]; then
      printf '  %s: %s renames %s and keeps version %s\n' \
        "${scope}" "${file}" "${holder##*/}" "${version}"
      continue
    fi
    new_paths="${new_paths}${path}
"
  done <<EOF
${head_paths}
EOF

  if [ -z "${new_paths}" ]; then
    printf '  %s: highest version on the base is %04d; this branch adds no migration\n' \
      "${scope}" "${base_max}"
    continue
  fi

  # Pass two: the versions that are already spoken for. The base's own versions,
  # plus the versions of the new files that clear the head — those are free, and
  # a suggested rename must not land on one of them.
  used=" ${base_versions} "
  colliding=""
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    file="${path##*/}"
    version="${file%%_*}"
    new_total=$((new_total + 1))
    if [ "$((10#${version}))" -gt "${base_max}" ]; then
      printf '  %s: %s claims %s, above the base head %04d — free\n' \
        "${scope}" "${file}" "${version}" "${base_max}"
      used="${used}${version} "
    else
      colliding="${colliding}${path}
"
    fi
  done <<EOF
${new_paths}
EOF

  [ -n "${colliding}" ] || continue

  next=$((base_max + 1))
  while IFS= read -r path; do
    [ -n "${path}" ] || continue
    file="${path##*/}"
    version="${file%%_*}"
    while :; do
      candidate="$(printf '%04d' "${next}")"
      case "${used}" in
        *" ${candidate} "*) next=$((next + 1)) ;;
        *) break ;;
      esac
    done
    suggested="$(printf '%04d' "${next}")"
    used="${used}${suggested} "
    next=$((next + 1))

    holder="$(printf '%s\n' "${base_paths}" | grep -E "/${version}_" | head -1)"
    printf '::error file=%s::version %s is not free. The base branch %s holds %s at that number, and its highest version is %04d. Rename this file to %s/%s_%s and update every reference to it.\n' \
      "${path}" "${version}" "${BASE_REF}" "${holder:-a file at that number}" "${base_max}" \
      "${directory}" "${suggested}" "${file#*_}" >&2
    failures=$((failures + 1))
  done <<EOF
${colliding}
EOF
done

printf 'migration version check: %d file(s) read, %d new, %d collision(s)\n' \
  "${checked_total}" "${new_total}" "${failures}"

if [ "${failures}" -gt 0 ]; then
  printf '::error::%d migration version collision(s) against %s. This check finds the collision; it does not renumber. The rename at merge time stays with the merge queue, because only the queue knows the order in which pull requests land.\n' \
    "${failures}" "${BASE_REF}" >&2
  exit 1
fi

if [ "${checked_total}" -eq 0 ]; then
  fatal "read no migration file at all. This check measured nothing."
fi
