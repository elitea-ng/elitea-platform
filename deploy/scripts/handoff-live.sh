#!/usr/bin/env bash
# The ADR-0034 cloud handoff journey (issue #1187, batch 4): the desktop
# host's `handoff::tests::live` tests (apps/elitea-desktop/src-tauri/src/
# handoff_live_tests.rs) against a REAL elitea-main on the full standalone
# stack: send a workspace thread to the cloud, a cloud turn that changes
# files, the web's reads of it (state, Files changed, one diff, the
# conversation), take back, and the folder and history match.
#
#   deploy/scripts/handoff-live.sh run           # whole lifecycle, then down -v
#   deploy/scripts/handoff-live.sh run --keep    # leave the stack up afterwards
#   deploy/scripts/handoff-live.sh test          # the tests only, against a stack
#                                                #   this script already brought up
#
# WHO PLAYS THE CLOUD EXECUTOR. The sandbox executor is not in this stack (it
# needs gVisor or Kata nodes), so the cloud turn is played by the simulator in
# services/elitea-main/cmd/handoff-live-cloudturn: it drives Main's own
# application services (one question, one answer from the cloud executor, the
# published refs/elitea/cloud and the recorded changes) exactly as the worker's
# runtime-plane calls do, with the deterministic "model" being the test's own
# commit. The desktop half and every web-facing read are the real code against
# the real Main, Postgres and RustFS.
#
# Variables (all optional):
#   STANDALONE_PROJECT   compose project   (elitea-handoff-live)
#   STANDALONE_PORT      elitea-main port  (8089)
#   HANDOFF_RUSTFS_PORT  RustFS on the host (19000)
#   STANDALONE_PG_PORT   PostgreSQL on the host (15433)
#   STANDALONE_SKIP_BUILD 1 = the caller built and asserted the images (CI)
#   HANDOFF_LIVE_TARGET_DIR  cargo target dir for the test build
#   HANDOFF_LIVE_PERSONA the chat persona (e2e-chat@autotest.local)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
export STANDALONE_PROJECT="${STANDALONE_PROJECT:-elitea-handoff-live}"
export STANDALONE_PORT="${STANDALONE_PORT:-8089}"
export HANDOFF_RUSTFS_PORT="${HANDOFF_RUSTFS_PORT:-19000}"
export STANDALONE_PG_PORT="${STANDALONE_PG_PORT:-15433}"
export STANDALONE_OVERLAY="${REPO_ROOT}/deploy/docker-compose.handoff-live.yml${STANDALONE_OVERLAY:+ ${STANDALONE_OVERLAY}}"
PERSONA="${HANDOFF_LIVE_PERSONA:-e2e-chat@autotest.local}"
COMPOSE_BIN="${COMPOSE_BIN:-docker compose}"
COMPOSE_F="-p ${STANDALONE_PROJECT} -f ${REPO_ROOT}/deploy/docker-compose.standalone-full.yml"
for overlay in ${STANDALONE_OVERLAY}; do COMPOSE_F="${COMPOSE_F} -f ${overlay}"; done
# Declared before the first compose call, as standalone-stack.sh does.
export COMPOSE_BIN

stack() { "${REPO_ROOT}/deploy/scripts/standalone-stack.sh" "$@"; }
psql_main() { $COMPOSE_BIN $COMPOSE_F exec -T postgres psql -v ON_ERROR_STOP=1 -At -U elitea -d elitea "$@"; }

# Main answers HTTP again (any status: a connection failure is code 000).
wait_main() {
  local deadline=$(( $(date +%s) + 180 )) code
  while :; do
    code="$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:${STANDALONE_PORT}/api/v2/" || true)"
    [ "$code" != "000" ] && [ -n "$code" ] && return 0
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "ERROR: elitea-main did not answer after the restart." >&2
      exit 1
    fi
    sleep 2
  done
}

# The default-encryption rule, then a restart: Main checks the bucket once, at
# start (ADR-0034 decision 4), and the stack's first start saw no rule.
encrypt_bucket() {
  echo "→ Adding the bucket's default encryption rule…"
  AWS_ACCESS_KEY_ID=elitea AWS_SECRET_ACCESS_KEY=elitea-dev-secret AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "http://127.0.0.1:${HANDOFF_RUSTFS_PORT}" s3api put-bucket-encryption \
      --bucket elitea-artifacts \
      --server-side-encryption-configuration \
      '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
  AWS_ACCESS_KEY_ID=elitea AWS_SECRET_ACCESS_KEY=elitea-dev-secret AWS_DEFAULT_REGION=us-east-1 \
    aws --endpoint-url "http://127.0.0.1:${HANDOFF_RUSTFS_PORT}" s3api get-bucket-encryption --bucket elitea-artifacts
}

# What a deployment's admin and a signed-in desktop leave behind, written the
# way the product stores it: the platform sections, the project's setting and
# a native device session. Prints "<project id> <access token>".
seed_handoff() {
  echo "→ Seeding the handoff policy and a native device session for ${PERSONA}…" >&2
  local token hash uid project
  token="elnat_$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n')"
  hash="$(printf '%s' "$token" | openssl dgst -sha256 -hex | sed 's/^.* //')"
  uid="$(psql_main -v persona="$PERSONA" <<'SQL'
SELECT u.id FROM public.auth_core__user u
JOIN centry.project p ON p.id = 90100 + u.id
WHERE u.email = :'persona';
SQL
)"
  if ! [[ "$uid" =~ ^[0-9]+$ ]]; then
    echo "ERROR: ${PERSONA} has no personal project 90100+id; the seed did not run" >&2
    exit 1
  fi
  project=$((90100 + uid))
  # uid and project are integers and hash is hex, checked above and by openssl.
  psql_main >/dev/null <<SQL
-- The platform ceiling (admin Configuration, "Cloud executor") and the native
-- client policy's device switches.
INSERT INTO centry.platform_config (section, key, value, updated_at) VALUES
  ('cloud_executor', 'enabled', 'true', now() AT TIME ZONE 'UTC'),
  ('cloud_executor', 'env_harvest', 'true', now() AT TIME ZONE 'UTC'),
  ('cloud_executor', 'allow_user_include', 'true', now() AT TIME ZONE 'UTC'),
  ('native_client_policy', 'local_work_allowed', 'true', now() AT TIME ZONE 'UTC'),
  ('native_client_policy', 'local_work_cloud_handoff', 'true', now() AT TIME ZONE 'UTC')
ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at;
-- The project's setting (Project settings, "Cloud executor").
DELETE FROM p_${project}.configuration WHERE type = 'cloud_executor';
INSERT INTO p_${project}.configuration
    (id, project_id, elitea_title, type, section, data, meta, shared, status_ok, source, created_at, updated_at)
VALUES ((SELECT COALESCE(max(id), 0) + 1 FROM p_${project}.configuration), ${project}, 'cloud_executor_handoff_live',
        'cloud_executor', 'project_settings',
        '{"enabled": true, "env_harvest": true, "exclusions_add": ["secrets/**"]}', '{}', false, true, 'user', now(), now());
-- A native device session, as the code exchange creates it (nativeauth
-- createFamily): the anchor token, the session and the access token's hash.
WITH anchor AS (
  INSERT INTO public.auth_core__token (uuid, expires, user_id, name)
  VALUES (NULL, NULL, ${uid}, 'native:handoff-live') RETURNING id
), session AS (
  INSERT INTO elitea_auth.native_sessions
      (user_id, token_id, client_id, device_name, platform, client_version, idle_timeout_seconds)
  SELECT ${uid}, anchor.id, 'handoff-live', 'handoff live run', 'linux', '0.1.0', 86400 FROM anchor
  RETURNING id
)
INSERT INTO elitea_auth.native_access_tokens (token_hash, session_id, expires_at)
SELECT '${hash}', session.id, now() + interval '6 hours' FROM session;
SQL
  printf '%s %s\n' "$project" "$token"
}

build_sim() {
  echo "→ Building the cloud-turn simulator…" >&2
  ( cd "${REPO_ROOT}/services/elitea-main" \
      && go build -tags handoff_live -o "${SIM_PATH}" ./cmd/handoff-live-cloudturn )
}

run_tests() {
  local project token
  read -r project token < <(seed_handoff)
  export ELITEA_HANDOFF_LIVE_URL="http://localhost:${STANDALONE_PORT}"
  export ELITEA_HANDOFF_LIVE_TOKEN="${token}"
  export ELITEA_HANDOFF_LIVE_PROJECT="${project}"
  export ELITEA_HANDOFF_LIVE_SIM="${SIM_PATH}"
  # The simulator is Main's own composition: Main's database and object store.
  export DATABASE_URL="postgres://elitea:elitea@127.0.0.1:${STANDALONE_PG_PORT}/elitea?sslmode=disable"
  export STORAGE_BACKEND=s3 STORAGE_CONTAINER=elitea-artifacts
  export S3_ENDPOINT_URL="http://127.0.0.1:${HANDOFF_RUSTFS_PORT}"
  export S3_ACCESS_KEY=elitea S3_SECRET_KEY=elitea-dev-secret S3_REGION=us-east-1 S3_FORCE_PATH_STYLE=true
  export AWS_REQUEST_CHECKSUM_CALCULATION=when_required
  echo "→ Running the handoff live tests (project ${project})…"
  (
    cd "${REPO_ROOT}/apps/elitea-desktop/src-tauri"
    cargo test --locked --lib handoff::tests::live -- --ignored --test-threads=1 --nocapture
  )
}

SIM_PATH="${HANDOFF_LIVE_SIM_PATH:-${RUNNER_TEMP:-/tmp}/handoff-cloudturn-sim}"

case "${1:-}" in
  test)
    build_sim
    run_tests
    ;;

  run)
    KEEP=0
    [ "${2:-}" = "--keep" ] && KEEP=1
    cleanup() {
      status=$?
      if [ "$status" -ne 0 ]; then
        echo "→ Stack logs (${STANDALONE_PROJECT}) — the run exited ${status}:"
        STANDALONE_LOG_TAIL="${STANDALONE_LOG_TAIL:-300}" stack logs elitea-main elitea-worker rustfs 2>&1 || true
      fi
      if [ "$KEEP" -eq 0 ]; then
        echo "→ Tearing down ${STANDALONE_PROJECT}…"
        stack down -v >/dev/null 2>&1 || true
      else
        echo "→ Leaving ${STANDALONE_PROJECT} up (--keep). Re-run the tests with:"
        echo "   STANDALONE_PROJECT=${STANDALONE_PROJECT} STANDALONE_PORT=${STANDALONE_PORT} $0 test"
      fi
    }
    trap cleanup EXIT
    stack certs
    if [ "${STANDALONE_SKIP_BUILD:-0}" = "1" ]; then
      echo "→ STANDALONE_SKIP_BUILD=1: images were built and asserted by the caller; not rebuilding"
    else
      stack build
    fi
    stack up
    stack seed
    encrypt_bucket
    $COMPOSE_BIN $COMPOSE_F restart elitea-main
    wait_main
    build_sim
    run_tests
    ;;

  *)
    sed -n '2,/^set -euo pipefail/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac
