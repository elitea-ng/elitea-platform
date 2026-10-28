#!/usr/bin/env bash
# Governed-profile conformance (ADR-0039 P6b, issue #1124).
#
# Brings the standalone stack up with deploy/docker-compose.governed.yml,
# walks it through the governance phases, runs conformance/governed against
# each, then scans everything the run left behind for the secret canaries.
#
#   deploy/scripts/governed-conformance.sh run [--keep]   the whole lifecycle
#   deploy/scripts/governed-conformance.sh up             stack + seed, flag off
#   deploy/scripts/governed-conformance.sh test           phases + scan, stack already up
#   deploy/scripts/governed-conformance.sh scan           evidence + canary scan only
#   deploy/scripts/governed-conformance.sh down           tear the stack down (-v)
#   deploy/scripts/governed-conformance.sh compose ARGS   any compose verb on this stack
#
# THE PHASES. One stack, five states, each reached with `up -d` of the
# services whose environment changed (the overlay reads two variables):
#
#   off        flag off, as installed       TestGovernedFlagOff     (GOV-001)
#   on         flag on, governed worker     TestGovernedProfile     (GOV-002..010)
#   oldworker  flag on, worker WITHOUT the  TestGovernedOldWorker   (GOV-007)
#              governed context
#   restored   flag on, governed worker     TestGovernedRestored    (GOV-007, rollback prep)
#   rollback   flag off again               TestGovernedRollback    (rollback)
#
# Between off and on the policy revision is stored with the operator CLI
# (`elitea-main credential-policy put`): Main refuses to boot with the flag on
# and no revision, which is the documented operator order.
#
# THE SEED. `seed-governed` adds what the E2E seed does not have, in rows an
# operator would create through the admin console: project 90700
# (e2e-governed) with admin/editor/member roles, a second global admin (the
# approver: every two-person step is proposed by one admin and approved by
# the other) and a member persona without models.credentials.governed.use.
# It writes NO governance row: bindings, references, grants and mandates are
# all created by the suite through the public API and the operator CLI.
#
# THE SCAN. After the phases: the NATS tap (governed-nats-tap: every payload
# published on the bus), pg_dump of every database (input bundles, security
# contexts, agent checkpoints, the audit outbox and events), every container's
# log (the OTLP collector's debug exporter writes the OTLP export there), the
# audit outbox as JSON lines and the mocks' journal. canary-scan runs its
# negative self-test first, then fails on any canary hit.
#
# Environment: STANDALONE_PROJECT (elitea-governed-conformance),
# STANDALONE_PORT (8094), STANDALONE_SKIP_BUILD=1 (images built by the caller;
# governed-mocks is still built here), ELITEA_GOVERNED_EVIDENCE_DIR
# (<repo>/.governed-evidence), GOVERNED_TEST_TIMEOUT (60m).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SELF="${REPO_ROOT}/deploy/scripts/governed-conformance.sh"
export STANDALONE_PROJECT="${STANDALONE_PROJECT:-elitea-governed-conformance}"
export STANDALONE_PORT="${STANDALONE_PORT:-8094}"
export STANDALONE_GATEWAY_PORT="${STANDALONE_GATEWAY_PORT:-8095}"
export E2E_OIDC_PORT="${E2E_OIDC_PORT:-9400}"
export STANDALONE_OVERLAY="${REPO_ROOT}/deploy/docker-compose.governed.yml${STANDALONE_OVERLAY:+ ${STANDALONE_OVERLAY}}"
GOVERNED_PROJECT_ID="${GOVERNED_PROJECT_ID:-90700}"
POLICY_REVISION="${GOVERNED_POLICY_REVISION:-gov-conformance-r1}"
EVIDENCE="${ELITEA_GOVERNED_EVIDENCE_DIR:-${REPO_ROOT}/.governed-evidence}"
SUITE="${REPO_ROOT}/conformance/governed"
SUITE_PACKAGE="github.com/elitea-ng/elitea-platform/conformance/governed"

stack() { "${REPO_ROOT}/deploy/scripts/standalone-stack.sh" "$@"; }

# The two variables the overlay reads. Every compose call runs with both set
# for the current phase, so no `up` can recreate a service in another state.
set_phase() {
  case "$1" in
    off)       export ELITEA_GOVERNED_FLAG=false ELITEA_GOVERNED_WORKER=false ;;
    on)        export ELITEA_GOVERNED_FLAG=true  ELITEA_GOVERNED_WORKER=true ;;
    oldworker) export ELITEA_GOVERNED_FLAG=true  ELITEA_GOVERNED_WORKER=false ;;
    restored)  export ELITEA_GOVERNED_FLAG=true  ELITEA_GOVERNED_WORKER=true ;;
    rollback)  export ELITEA_GOVERNED_FLAG=false ELITEA_GOVERNED_WORKER=true ;;
    *) echo "ERROR: unknown phase $1" >&2; exit 2 ;;
  esac
  PHASE="$1"
}

wait_healthy() {
  local service="$1" deadline=$(( $(date +%s) + ${2:-240} ))
  until [ "$(stack compose ps --format '{{.Health}}' "$service" 2>/dev/null | head -n1)" = "healthy" ]; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: ${service} is not healthy (phase ${PHASE})." >&2
      stack compose ps --all >&2 || true
      stack logs "$service" >&2 || true
      exit 1
    fi
    sleep 3
  done
}

# recreate SERVICES...: re-create the services whose environment the phase
# changed, without touching their dependencies.
recreate() {
  echo "→ Phase ${PHASE}: recreating $* (flag=${ELITEA_GOVERNED_FLAG}, worker governed=${ELITEA_GOVERNED_WORKER})…"
  stack compose up -d --no-deps --force-recreate "$@"
  for service in "$@"; do
    if [ "$service" = "elitea-main" ]; then
      wait_healthy elitea-main
    fi
  done
  # The worker has no healthcheck; give it time to bind its consumers.
  sleep 15
}

seed_governed() {
  echo "→ Seeding the governed fixture project ${GOVERNED_PROJECT_ID} and its personas…"
  stack compose exec -T postgres \
    psql -v ON_ERROR_STOP=1 -v project="$GOVERNED_PROJECT_ID" -U elitea -d elitea <<'SQL'
SELECT set_config('gov.project', :'project', false);

-- Two personas the E2E seed does not have. The approver is a second global
-- admin (it gets every global role the E2E admin holds); the member is a
-- plain account.
INSERT INTO auth_core__user (email, name) VALUES
    ('e2e-gov-approver@autotest.local', 'E2E Governed Approver'),
    ('e2e-gov-member@autotest.local', 'E2E Governed Member')
ON CONFLICT (email) DO NOTHING;
INSERT INTO auth_core__user_role (user_id, role_id)
SELECT approver.id, ur.role_id
FROM auth_core__user approver
JOIN auth_core__user admin ON admin.email = 'e2e-admin@autotest.local'
JOIN auth_core__user_role ur ON ur.user_id = admin.id
WHERE approver.email = 'e2e-gov-approver@autotest.local'
ON CONFLICT (user_id, role_id) DO NOTHING;
INSERT INTO centry.social_users (user_id, title)
SELECT u.id, u.name FROM auth_core__user u
WHERE u.email IN ('e2e-gov-approver@autotest.local', 'e2e-gov-member@autotest.local')
ON CONFLICT (user_id) DO NOTHING;

-- The member's personal project: a model call resolves the CALLER's personal
-- project for its provider credential, so a member turn without one would
-- die before the gate under test is reached. Same shape as the chat
-- driver's (90100 + id, its admin permission list).
INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success, suspended)
SELECT 90100 + u.id, 'project_user_' || u.id, u.id, '{}', true, false
FROM auth_core__user u WHERE u.email = 'e2e-gov-member@autotest.local'
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, owner_id = EXCLUDED.owner_id, suspended = false;
INSERT INTO auth_core__project_role (project_id, name)
SELECT 90100 + u.id, 'admin' FROM auth_core__user u WHERE u.email = 'e2e-gov-member@autotest.local'
ON CONFLICT (project_id, name) DO NOTHING;
INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
SELECT r.project_id, u.id, r.id FROM auth_core__user u
JOIN auth_core__project_role r ON r.project_id = 90100 + u.id AND r.name = 'admin'
WHERE u.email = 'e2e-gov-member@autotest.local'
ON CONFLICT (project_id, user_id, role_id) DO NOTHING;
INSERT INTO auth_core__project_role_permission (project_id, role_id, permission)
SELECT target.project_id, target.id, source.permission
FROM auth_core__user chat
JOIN auth_core__project_role origin ON origin.project_id = 90100 + chat.id AND origin.name = 'admin'
JOIN auth_core__project_role_permission source ON source.role_id = origin.id AND source.project_id = origin.project_id
JOIN auth_core__user member ON member.email = 'e2e-gov-member@autotest.local'
JOIN auth_core__project_role target ON target.project_id = 90100 + member.id AND target.name = 'admin'
WHERE chat.email = 'e2e-chat@autotest.local'
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

-- The governed fixture project.
INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success, suspended)
SELECT current_setting('gov.project')::int, 'e2e-governed', u.id, '{}', true, false
FROM auth_core__user u WHERE u.email = 'e2e-admin@autotest.local'
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, suspended = false;
INSERT INTO auth_core__project_role (project_id, name)
SELECT current_setting('gov.project')::int, role_name FROM (VALUES ('admin'), ('editor'), ('member')) AS v(role_name)
ON CONFLICT (project_id, name) DO NOTHING;

-- Every role gets project 1's admin or editor list (copied, never restated,
-- as the E2E seed does) plus the chat driver's chat list.
INSERT INTO auth_core__project_role_permission (project_id, role_id, permission)
SELECT target.project_id, target.id, source.permission
FROM auth_core__project_role target
JOIN auth_core__project_role origin
  ON origin.project_id = 1 AND origin.name = CASE target.name WHEN 'admin' THEN 'admin' ELSE 'editor' END
JOIN auth_core__project_role_permission source ON source.role_id = origin.id AND source.project_id = 1
WHERE target.project_id = current_setting('gov.project')::int
ON CONFLICT (project_id, role_id, permission) DO NOTHING;
INSERT INTO auth_core__project_role_permission (project_id, role_id, permission)
SELECT target.project_id, target.id, source.permission
FROM auth_core__user chat
JOIN auth_core__project_role origin ON origin.project_id = 90100 + chat.id AND origin.name = 'admin'
JOIN auth_core__project_role_permission source ON source.role_id = origin.id AND source.project_id = origin.project_id
JOIN auth_core__project_role target ON target.project_id = current_setting('gov.project')::int
WHERE chat.email = 'e2e-chat@autotest.local'
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

-- The governed permissions. No role holds them by default. The member role
-- deliberately holds none: GOV-005's RBAC case is that difference.
INSERT INTO auth_core__project_role_permission (project_id, role_id, permission)
SELECT r.project_id, r.id, p.permission
FROM auth_core__project_role r
JOIN (VALUES
    ('admin', 'models.credentials.governed.use'),
    ('admin', 'models.credentials.governed.manage'),
    ('admin', 'models.credentials.governed.approve'),
    ('admin', 'models.credentials.governed.probe'),
    ('editor', 'models.credentials.governed.use')
) AS p(role_name, permission) ON p.role_name = r.name
WHERE r.project_id = current_setting('gov.project')::int
ON CONFLICT (project_id, role_id, permission) DO NOTHING;
DELETE FROM auth_core__project_role_permission prp
USING auth_core__project_role r
WHERE prp.role_id = r.id AND r.project_id = current_setting('gov.project')::int
  AND r.name = 'member' AND prp.permission LIKE 'models.credentials.governed.%';

INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
SELECT r.project_id, u.id, r.id
FROM auth_core__user u
JOIN (VALUES
    ('e2e-admin@autotest.local', 'admin'),
    ('e2e-gov-approver@autotest.local', 'admin'),
    ('e2e-chat@autotest.local', 'editor'),
    ('e2e-gov-member@autotest.local', 'member')
) AS m(email, role_name) ON m.email = u.email
JOIN auth_core__project_role r ON r.project_id = current_setting('gov.project')::int AND r.name = m.role_name
ON CONFLICT (project_id, user_id, role_id) DO NOTHING;

-- The empty vault pair every seeded project copies from project 1.
INSERT INTO centry.secrets_key (id, data)
SELECT 'project-' || current_setting('gov.project'), data FROM centry.secrets_key WHERE id = 'project-1'
ON CONFLICT (id) DO NOTHING;
INSERT INTO centry.secrets_data (id, data)
SELECT 'project-' || current_setting('gov.project'), data FROM centry.secrets_data WHERE id = 'project-1'
ON CONFLICT (id) DO NOTHING;
INSERT INTO centry.secrets_key (id, data)
SELECT 'project-' || (90100 + u.id), k.data
FROM auth_core__user u JOIN centry.secrets_key k ON k.id = 'project-1'
WHERE u.email = 'e2e-gov-member@autotest.local'
ON CONFLICT (id) DO NOTHING;
INSERT INTO centry.secrets_data (id, data)
SELECT 'project-' || (90100 + u.id), d.data
FROM auth_core__user u JOIN centry.secrets_data d ON d.id = 'project-1'
WHERE u.email = 'e2e-gov-member@autotest.local'
ON CONFLICT (id) DO NOTHING;
SQL
  echo "→ Applying tenant migrations for the governed projects…"
  stack compose run --rm --no-deps elitea-migrate -all-tenants
  # PATs for the new personas (the same idempotent block `seed-runtime` runs).
  stack seed-runtime
  # X-SECRET backfill for the new projects runs at elitea-main start.
  stack compose restart elitea-main
  wait_healthy elitea-main
}

store_policy() {
  if stack compose exec -T elitea-main /elitea-main credential-policy show --id "$POLICY_REVISION" >/dev/null 2>&1; then
    echo "→ Policy revision ${POLICY_REVISION} is already stored."
    return
  fi
  echo "→ Storing policy revision ${POLICY_REVISION} (operator step before the flag goes on)…"
  stack compose exec -T elitea-main /elitea-main credential-policy put --id "$POLICY_REVISION" \
    --file /etc/elitea/governed/credential-policy.json --operator governed-conformance
}

# run_phase PHASE TEST: run one phase's test and hold its skips to the ledger.
run_phase() {
  local phase="$1" test="$2" log="${EVIDENCE}/go-test-$1.log" status=0
  echo "→ Phase ${phase}: ${test}"
  (
    cd "$SUITE"
    ELITEA_GOVERNED_ORIGIN="http://localhost:${STANDALONE_PORT}" \
    ELITEA_GOVERNED_PHASE="$phase" \
    ELITEA_GOVERNED_STACK="$SELF" \
    ELITEA_GOVERNED_MOCKS_ADMIN="http://127.0.0.1:${GOVERNED_MOCKS_ADMIN_PORT:-18480}" \
    ELITEA_GOVERNED_PAT_SIGNING_KEY="${REPO_ROOT}/deploy/certs/runtime/auth-pat-signing-key" \
    ELITEA_GOVERNED_PROJECT_ID="$GOVERNED_PROJECT_ID" \
    ELITEA_GOVERNED_EVIDENCE_DIR="$EVIDENCE" \
      go test -tags conformance -count=1 -v -timeout "${GOVERNED_TEST_TIMEOUT:-60m}" -run "^${test}\$" .
  ) 2>&1 | tee "$log" || status=$?
  (cd "$SUITE" && go run ./cmd/skip-check -declared "${REPO_ROOT}/scripts/go/declared-skips.txt" \
      -package "$SUITE_PACKAGE" "$log") || status=1
  return "$status"
}

collect_evidence() {
  echo "→ Collecting evidence into ${EVIDENCE}…"
  mkdir -p "$EVIDENCE"
  stack compose cp governed-nats-tap:/evidence/nats-tap.bin "${EVIDENCE}/nats-tap.bin"
  local databases
  databases="$(stack compose exec -T postgres psql -U elitea -d elitea -tAc \
    "SELECT datname FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres'" | tr -d '\r')"
  for db in $databases; do
    stack compose exec -T postgres pg_dump -U elitea -d "$db" > "${EVIDENCE}/pg-${db}.sql"
  done
  stack compose exec -T postgres psql -U elitea -d elitea -tAc \
    "SELECT event FROM elitea_runtime.security_audit_outbox ORDER BY seq" > "${EVIDENCE}/audit-outbox.jsonl"
  stack compose exec -T postgres psql -U elitea -d elitea -tAc \
    "SELECT event FROM elitea_identity.security_audit_events ORDER BY seq" > "${EVIDENCE}/audit-events.jsonl"
  stack compose logs --no-color --timestamps > "${EVIDENCE}/compose-logs.txt"
  curl -fsS "http://127.0.0.1:${GOVERNED_MOCKS_ADMIN_PORT:-18480}/__journal" > "${EVIDENCE}/mocks-journal.json"
}

scan() {
  collect_evidence
  echo "→ Canary scan: negative self-test first…"
  (cd "$SUITE" && go run ./cmd/canary-scan -selftest)
  if [ ! -s "${EVIDENCE}/canaries.json" ]; then
    echo "ERROR: no canaries were seeded (${EVIDENCE}/canaries.json); the scan would prove nothing." >&2
    exit 1
  fi
  (cd "$SUITE" && go run ./cmd/canary-scan -canaries "${EVIDENCE}/canaries.json" -dir "$EVIDENCE" \
      -require nats-tap.bin,pg-elitea.sql,compose-logs.txt,audit-outbox.jsonl)
}

up() {
  set_phase off
  stack certs
  if [ "${STANDALONE_SKIP_BUILD:-0}" = "1" ]; then
    echo "→ STANDALONE_SKIP_BUILD=1: platform images come from the caller; building governed-mocks only"
    stack build governed-mocks
  else
    stack build
  fi
  stack up
  stack seed
  seed_governed
  SEED_EXTRA_PROJECTS="${GOVERNED_PROJECT_ID}" stack seed-llm
}

phases() {
  mkdir -p "$EVIDENCE"
  local status=0
  set_phase off
  run_phase off TestGovernedFlagOff || status=1

  store_policy
  set_phase on
  recreate elitea-main elitea-worker
  run_phase on TestGovernedProfile || status=1

  set_phase oldworker
  recreate elitea-worker
  run_phase oldworker TestGovernedOldWorker || status=1

  set_phase restored
  recreate elitea-worker
  run_phase restored TestGovernedRestored || status=1

  set_phase rollback
  recreate elitea-main elitea-worker
  run_phase rollback TestGovernedRollback || status=1
  return "$status"
}

case "${1:-}" in
  up)
    up
    ;;

  test)
    status=0
    phases || status=1
    scan || status=1
    exit "$status"
    ;;

  scan)
    scan
    ;;

  run)
    KEEP=0
    [ "${2:-}" = "--keep" ] && KEEP=1
    # shellcheck disable=SC2329 # invoked by the EXIT trap
    cleanup() {
      local code=$?
      if [ "$code" -ne 0 ]; then
        echo "→ Stack logs (${STANDALONE_PROJECT}) — the run exited ${code}:"
        STANDALONE_LOG_TAIL="${STANDALONE_LOG_TAIL:-200}" stack logs elitea-main elitea-worker governed-mocks 2>&1 || true
      fi
      if [ "$KEEP" -eq 0 ]; then
        echo "→ Tearing down ${STANDALONE_PROJECT}…"
        stack down -v >/dev/null 2>&1 || true
      fi
    }
    trap cleanup EXIT
    rm -rf "$EVIDENCE"
    mkdir -p "$EVIDENCE"
    up
    status=0
    phases || status=1
    scan || status=1
    exit "$status"
    ;;

  down)
    stack down -v
    ;;

  compose)
    shift
    # The suite's way into the stack (psql, the operator CLI, OpenBao).
    stack compose "$@"
    ;;

  *)
    sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
