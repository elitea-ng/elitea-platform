#!/usr/bin/env bash
# E2E stack wrapper (issue #60, unit V1).
# Usage:
#   ./scripts/e2e-stack.sh up       — bring up the full E2E stack (waits for healthy)
#   ./scripts/e2e-stack.sh seed     — provision oidc-mock users + DB rows
#   ./scripts/e2e-stack.sh down     — tear down the stack
#
# Podman/docker detection:
#   CI (GitHub Actions ubuntu-latest) has `docker compose` (v2 plugin).
#   Local dev on this machine uses podman compose (podman is installed, docker is not).
#   Override with: COMPOSE_BIN="docker compose" ./scripts/e2e-stack.sh up
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"

# resolve_container_name() — see scripts/lib/container-lookup.sh for why the
# `cmd | grep -m1 A || cmd | grep -m1 B` idiom this replaces returned TWO
# names under `set -o pipefail` (#228).
# shellcheck source=lib/container-lookup.sh
. "$(dirname "$0")/lib/container-lookup.sh"
# shellcheck source=lib/compose-detect.sh
. "$(dirname "$0")/lib/compose-detect.sh"
# Standalone file: self-contained, no port conflicts with the centry dev stack.
# In CI set E2E_PORT=8080 (free on ubuntu-latest); locally defaults to 8082.
# -p elitea-e2e: explicit project name avoids clashing with the deploy- project.
#
# E2E_PROJECT overrides it so a second stack can run beside the first — set it
# together with E2E_PORT, E2E_PG_PORT, E2E_REDIS_PORT and E2E_OIDC_PORT, all of
# which must differ. It is a variable rather than a constant because the
# container lookups below GREP for it: with a hardcoded name and a differently
# named stack running, those greps fell through to their "any container called
# postgres" fallback and the seed would have been applied to the other stack's
# database.
E2E_PROJECT="${E2E_PROJECT:-elitea-e2e}"
COMPOSE_F="-p ${E2E_PROJECT} -f ${REPO_ROOT}/deploy/docker-compose.e2e-standalone.yml"

# The IANA zone this seed computes its calendar-day boundaries in (issue #214).
#
# It must be the SAME zone playwright.config.ts pins the browser to, because a
# fixture anchored on the database's day and read through a filter computed from
# the browser's day only agree while the two zones do. `E2E_TIMEZONE` in
# playwright.config.ts reads this same variable and carries the full argument;
# the audit trail fixture below is what consumes it here.
#
# UTC by default on both sides, so CI and every existing local invocation keep
# the boundary they already had — but now by declaration, not by inheriting
# whatever the runner and the postgres session happened to be set to.
E2E_TZ="${E2E_TZ:-UTC}"

# ── compose binary detection ─────────────────────────────────────────────────
detect_compose_bin

CMD="${1:-}"

case "$CMD" in
  up)
    # The facade → provider hop is mTLS with no plaintext fallback on either
    # side; the script is idempotent and keeps fresh material.
    echo "→ Generating the provider mTLS material…"
    DEEPWIKI_CERT_SANS="DNS:elitea-deepwiki,DNS:localhost,IP:127.0.0.1" \
      bash "${REPO_ROOT}/deploy/scripts/gen-deepwiki-certs.sh"
    # The second provider's server cert, off the same CA and the same client
    # certificate. Issued after DeepWiki's, so a CA this run created is already
    # in place and neither hop is signed by a root the other does not trust.
    INVENTORY_CERT_SANS="DNS:elitea-inventory,DNS:localhost,IP:127.0.0.1" \
      bash "${REPO_ROOT}/deploy/scripts/gen-inventory-certs.sh"
    echo "→ Bringing up E2E stack (${COMPOSE_BIN})…"
    # --wait: wait for every healthcheck to report healthy before returning.
    # elitea-main runs migrations on startup; postgres is its dependency.
    $COMPOSE_BIN $COMPOSE_F up -d --wait
    echo "→ Stack ready."
    ;;

  down)
    echo "→ Tearing down E2E stack…"
    $COMPOSE_BIN $COMPOSE_F down --remove-orphans -v
    ;;

  seed)
    echo "→ Seeding E2E users…"

    # Drop last run's audit fixture anchor BEFORE anything can fail (#214).
    # Journey 29 freezes its clock to the day named in this file. A seed that
    # dies halfway would otherwise leave yesterday's file in place, and the
    # journey would run against a day this stack no longer holds rows for — a
    # green-looking seed followed by an unreadable failure. Absent, the journey
    # says which command to run.
    rm -f "$(cd "$(dirname "$0")/.." && pwd)/.playwright-state/audit-fixture.json"

    # ── 0. Bootstrap DB schema if running on a fresh postgres ────────────────
    # elitea-main uses SKIP_MIGRATIONS=1 in dev (migrations assume a legacy pylon
    # DB is already present). For the standalone E2E stack we first apply
    # 001_initial.sql (creates centry + p_1 tenant schema from scratch), then run
    # elitea-migrate to bring the shared history tables up to date.
    INIT_SQL="${REPO_ROOT}/services/elitea-main/internal/infra/db/migrations/001_initial.sql"
    if [ -f "$INIT_SQL" ]; then
      EXEC_BIN_EARLY="${COMPOSE_BIN%% *}"
      # Detect the postgres container early (needed here before the full lookup below).
      PG_EARLY=$(resolve_container_name "$E2E_PROJECT" 'postgres' \
        "$($EXEC_BIN_EARLY ps --format '{{.Names}}' 2>/dev/null || true)")
      if [ -n "$PG_EARLY" ]; then
        echo "  → Applying 001_initial.sql (centry schema bootstrap)…"
        $EXEC_BIN_EARLY exec -i "$PG_EARLY" psql -U elitea -d elitea < "$INIT_SQL" >/dev/null 2>&1 || true
      fi
    fi
    # ── elitea-migrate, in TWO passes ────────────────────────────────────────
    #
    # This one applies the SHARED history only. The TENANT history (the
    # migrations/tenant/ chain, 0120-…) needs `-all-tenants`, and that flag
    # cannot be passed HERE — it runs after the seed SQL, below. Why:
    #
    #   `-all-tenants` PREFLIGHTS. migrate.TenantProjects enumerates every
    #   `create_success` project and validateTenantProjects REFUSES THE WHOLE
    #   RUN if any of them lacks a `p_<id>` schema. The admin fixtures
    #   90001-90003 are INSERTED by the seed SQL below, and their schemas are
    #   created by its `DO $schemas$` loop at the end. At this point in the
    #   seed those projects therefore either do not exist yet (first run) or
    #   exist without schemas (they were inserted by a previous seed and this
    #   one has not reached the loop) — and in the second case the flag refuses
    #   and no tenant migration runs for ANY project, project 1 included.
    #
    # So: shared here, tenant after the loop that provisions the schemas the
    # preflight demands. This ordering is the same one deploy/scripts/
    # standalone-stack.sh uses — it runs `elitea-migrate -all-tenants` AFTER
    # calling this very script's `seed`, and for the same stated reason.
    #
    # This pass is still load-bearing where it stands, and is not merely the
    # first half of the second one: migrate.Bootstrap applies 001_initial.sql
    # to an empty database, and the psql apply above swallows every error with
    # `|| true`, so this is what guarantees the seed SQL has a schema to write
    # into at all. It also has to stay a SEPARATE invocation because the seed
    # SQL between the two passes writes rows the tenant pass then depends on.
    MAIN_CONTAINER=$(resolve_container_name "$E2E_PROJECT" "${E2E_PROJECT}.*elitea-main" \
      "$("${COMPOSE_BIN%% *}" ps --format '{{.Names}}' 2>/dev/null || true)")
    # Absence used to be silent (`if [ -n "$MAIN_CONTAINER" ]`), which is the
    # same failure the postgres lookup below refuses to accept: a seed that
    # cannot find the container cannot migrate, and every consequence of that
    # surfaces hours later as a 500 from a route naming a missing relation.
    if [ -z "$MAIN_CONTAINER" ]; then
      echo "ERROR: could not locate the elitea-main container. Is the stack up?" >&2
      echo "  Without it neither migration pass can run, and the tenant schemas" >&2
      echo "  keep the shape 001_initial.sql left them in." >&2
      exit 1
    fi

    # Both passes go through this, and a failure is PRINTED and FATAL.
    #
    # The shared pass used to end in `>/dev/null 2>&1 || true`. That is how the
    # tenant history came to be missing from this stack with nothing reporting
    # it, and it is what would hide a refusing `-all-tenants` preflight — the
    # one failure mode the ordering above exists to avoid, made invisible by
    # the way the call was written. A migration that does not run must say so.
    run_elitea_migrate() {
      local label="$1"
      shift
      local out
      local status=0
      out=$("${COMPOSE_BIN%% *}" exec "$MAIN_CONTAINER" /elitea-migrate "$@" 2>&1) || status=$?
      if [ "$status" -ne 0 ]; then
        echo "ERROR: elitea-migrate ${label} failed (exit ${status})." >&2
        echo "  command: /elitea-migrate $*" >&2
        printf '%s\n' "$out" >&2
        exit 1
      fi
    }

    echo "  → Running elitea-migrate (shared history)…"
    run_elitea_migrate "shared history"

    # Resolve postgres container name.
    # Project name is `${E2E_PROJECT}` so the container is <project>-postgres-1.
    # Fallback: probe by name pattern in case the compose tool normalises differently.
    POSTGRES_CONTAINER=$(resolve_container_name "$E2E_PROJECT" 'postgres' \
      "$(${COMPOSE_BIN%% *} ps --format '{{.Names}}' 2>/dev/null || true)")
    if [ -z "$POSTGRES_CONTAINER" ]; then
      echo "ERROR: could not locate the postgres container. Is the stack up?" >&2
      exit 1
    fi

    # ── 1. Provision oidc-mock users via REST API ─────────────────────────
    # Must match the compose file's ${E2E_OIDC_PORT:-9400} — the seed talks to
    # the mock over the PUBLISHED port, so a stack brought up on a different one
    # would be seeded through a hole in the wall that is not there.
    OIDC_PORT="${E2E_OIDC_PORT:-9400}"
    OIDC_BASE="http://localhost:${OIDC_PORT}"

    # Wait for the mock to be reachable (up already handles healthcheck,
    # but the seed command may be called independently).
    for i in $(seq 1 20); do
      if curl -sf "${OIDC_BASE}/.well-known/openid-configuration" >/dev/null 2>&1; then
        break
      fi
      sleep 1
    done

    # persona: member (normal project member)
    curl -sf -X PUT "${OIDC_BASE}/users/e2e-member@autotest.local" \
      -H "Content-Type: application/json" \
      -d '{"email":"e2e-member@autotest.local","name":"E2E Member"}' >/dev/null
    echo "  ✓ oidc-mock user: e2e-member@autotest.local"

    # persona: admin (needs admin role — journey 27/28)
    curl -sf -X PUT "${OIDC_BASE}/users/e2e-admin@autotest.local" \
      -H "Content-Type: application/json" \
      -d '{"email":"e2e-admin@autotest.local","name":"E2E Admin"}' >/dev/null
    echo "  ✓ oidc-mock user: e2e-admin@autotest.local"

    # ── 2. Insert matching DB rows ─────────────────────────────────────────
    # Adapted from deploy/scripts/seed-staging-oidc-user.sql.
    # DB: elitea (local compose default), user: elitea, host: postgres.
    # Write SQL to a temp file — avoids bash 3.x heredoc-in-subshell limits.
    # The X placeholders must be the LAST characters of the template: BSD mktemp
    # (macOS) rejects anything after them, so `…-XXXXXX.sql` created a file
    # LITERALLY named `e2e-seed-XXXXXX.sql` on the first run and then failed
    # `mkstemp failed: File exists` on every run after it — with `set -e`, the
    # seed aborted before a single row was written while still printing its
    # oidc-mock successes. GNU mktemp accepts the suffix, so CI never saw it and
    # only a second local seed did.
    SEED_TMP="$(mktemp "${TMPDIR:-/tmp}/e2e-seed-XXXXXX")"
    trap 'rm -f "$SEED_TMP"' EXIT
    # Quoted delimiter: the SQL below contains backticks inside its comments
    # (`configuration.artifacts.artifacts.*` and friends). With an UNQUOTED
    # heredoc the shell ran those as command substitutions — every seed printed
    # `configuration.artifacts.artifacts.*: command not found` three times and
    # silently rewrote the comments in the SQL it then executed. Nothing in this
    # body is meant to expand: there is not a single `$` in it.
    cat > "$SEED_TMP" <<'ENDSQL'
-- E2E seed (issue #60): member + admin personas.
-- Idempotent via ON CONFLICT DO NOTHING.

-- FIRST statement, deliberately: it is the only one that reads `:'e2e_tz'`
-- besides the audit trail fixture ~900 lines below, and an unknown zone name
-- must stop the seed here rather than after everything else has landed.
-- Postgres answers `invalid value for parameter "TimeZone"` and ON_ERROR_STOP=1
-- halts. It also prints the wall clock the fixtures will be anchored against,
-- which is the one number a "why is journey 29 empty" investigation wants.
\echo '  → audit fixture day boundary:'
SELECT :'e2e_tz' AS e2e_tz, now() AT TIME ZONE :'e2e_tz' AS seed_local_now;

-- Ensure suspended column exists (001_initial.sql predates this column).
ALTER TABLE auth_core__user ADD COLUMN IF NOT EXISTS suspended BOOLEAN NOT NULL DEFAULT false;

-- member persona
INSERT INTO auth_core__user (email, name)
VALUES ('e2e-member@autotest.local', 'E2E Member')
ON CONFLICT (email) DO NOTHING;

INSERT INTO auth_core__user_role (user_id, role_id)
SELECT u.id, r.id
FROM auth_core__user u
JOIN auth_core__role r ON r.name = 'admin'
WHERE u.email = 'e2e-member@autotest.local'
  AND r.mode = 'default'
ON CONFLICT (user_id, role_id) DO NOTHING;

-- admin persona
INSERT INTO auth_core__user (email, name)
VALUES ('e2e-admin@autotest.local', 'E2E Admin')
ON CONFLICT (email) DO NOTHING;

INSERT INTO auth_core__user_role (user_id, role_id)
SELECT u.id, r.id
FROM auth_core__user u
JOIN auth_core__role r ON r.name = 'admin'
WHERE u.email = 'e2e-admin@autotest.local'
  AND r.mode = 'default'
ON CONFLICT (user_id, role_id) DO NOTHING;

-- ── suspension fixtures (issue #519) ─────────────────────────────────────
-- Journey 28 suspends a user, reloads, and unsuspends it. It used to do that
-- to the MEMBER PERSONA, which is the identity ~110 of the 125 journeys
-- authenticate as. A suspended principal is refused by
-- `authsvc.PrincipalValidator` (`GetActiveUserPrincipalByID` filters on
-- `suspended = false`), so for the whole window every concurrent request of
-- every other journey answered 401 "authenticated principal is inactive".
-- `fullyParallel` is on, so that window overlapped a different set of tests
-- on every run — which is exactly the "same code, different failures" report
-- of issue #519. Measured in this repository: journey 33, in the same file,
-- reads the user listing as the member and asserts 403; it received 401.
--
-- These rows exist so journey 28 can suspend a user that NOTHING signs in as.
-- Every assertion of that journey is unchanged; only its subject moves.
--
-- ONE ROW PER BROWSER PROJECT, for the reason `admin.features.spec.ts`
-- documents for its Help Center card: chromium and webkit run the same file
-- against the same rows when the suite is run locally with both projects, and
-- one shared row would have each engine observing the other's window.
--
-- They hold no role and belong to no project: journey 28 needs a row in
-- `auth_core__user` that the admin listing shows, and nothing else. A plain
-- `@autotest.local` address is a PLATFORM user for `systemUserPredicate`
-- (services/elitea-main/internal/api/v2/admin/users.go), so both appear on the
-- tab the journey opens.
INSERT INTO auth_core__user (email, name)
VALUES
    ('e2e-suspend-chromium@autotest.local', 'E2E Suspend Fixture chromium'),
    ('e2e-suspend-webkit@autotest.local', 'E2E Suspend Fixture webkit')
ON CONFLICT (email) DO NOTHING;

-- Re-run safety: a run that was killed between the suspend and the unsuspend
-- leaves the fixture suspended, and journey 28 asserts it STARTS from Active.
-- The seed is the only place that can put it back without weakening that
-- precondition into "suspend it if it is not suspended already".
UPDATE auth_core__user
SET suspended = false
WHERE email IN (
    'e2e-suspend-chromium@autotest.local',
    'e2e-suspend-webkit@autotest.local'
);

-- ── administration-mode RBAC (unit A14) ──────────────────────────────────
-- The ROLES are not created here any more. When A14 wrote this block,
-- 001_initial.sql seeded `default`-mode roles only, so no persona held a single
-- administration-mode permission and every admin-panel WRITE answered 403 for
-- everyone — while the user listing, then ungated, made the page LOOK fine.
-- Creating the roles here fixed this stack and left every other deployment
-- broken, which is why A14 could not gate the READS.
--
-- That is fixed at the source now: 001_initial.sql seeds the four
-- administration roles and their pylon-parity grants on a fresh database, and
-- migrations/shared/0060_admin_central_rbac.sql back-fills databases that
-- predate it. Both run before this seed (see `up`), so the roles already exist
-- by the time this file runs, and the assertions at the end of `seed` check
-- what those migrations produced.
--
-- The grants below stay, and are deliberately a SUPERSET of what the migrations
-- seed. The migrations seed the pylon-parity baseline for the routes the server
-- gates; this list is what the JOURNEYS need the admin persona to hold, which
-- includes permissions no migration grants to `admin` (the audit, secrets and
-- project-member ones below) and one it grants only to `super_admin`. Every
-- statement is ON CONFLICT DO NOTHING, so the overlap is inert.
--
-- `admin.auth.users.super_admin` is the deliberate divergence: pylon and the
-- migrations both make it super_admin-only, and it is granted to `admin` here
-- so the journey can exercise the role-assignment path. The escalation guard it
-- gates is covered by the Go integration tests, which can revoke it.
--
-- Note what none of this changes: `window.admin_ui_config.permissions` is
-- hardcoded by adminui/handler.go and was always present. The rows below are
-- what the SERVER resolves per request, and they are the only thing that
-- authorises anything.
INSERT INTO auth_core__role_permission (role_id, permission)
SELECT r.id, p.permission
FROM auth_core__role r
CROSS JOIN (VALUES
    ('admin.auth.users'),
    ('admin.auth.users.super_admin'),
    -- Unit A14, Audit Trail: all four `/elitea_core/audit*` READS are gated on
    -- this, matching the pylon originals. Unlike the user LISTING, the audit
    -- listing is gated rather than open — an audit row names the user, the
    -- project and the action taken, so the listing itself is the sensitive
    -- part. Without this row the page renders with four 403s.
    ('models.admin.audit_trail.view'),
    -- Unit A14, Roles: the permission matrix. The READ is gated too — it is the
    -- deployment's authorisation model, and which role holds which privilege is
    -- itself sensitive — so without these two rows the page renders 403 and the
    -- journey cannot tell that apart from an unwired route.
    ('configuration.roles.permissions.view'),
    ('configuration.roles.permissions.edit'),
    -- Unit A14, Projects. The listing is gated for the same reason the audit
    -- listing is: a project row names the project, its owner and its admins
    -- across every tenant. `.edit` gates the suspend write.
    ('projects.projects.projects.view'),
    ('projects.projects.projects.edit'),
    -- The admin Projects page's member dialog posts to
    -- `/admin/users/administration/{projectID}`, whose administration-mode
    -- routes resolve these two CENTRALLY — a global administrator is not a
    -- member of the project they are acting on, so the default-mode grant
    -- above cannot authorise them.
    ('configuration.users.users.create'),
    ('configuration.users.users.edit'),
    -- Unit A14, Secrets: the GLOBAL vault. `.view` gates BOTH reads (the
    -- listing and the single-value reveal), matching pylon's AdminAPI, and it
    -- is a different grant from `.list` — the admin SPA's sidebar has always
    -- gated the page on `.list` while the server checks `.view`, and on the
    -- reference deployment the administration-mode `editor` role holds the
    -- first and not the second. Both are granted here so journey 32 exercises
    -- the working path; the refusal path is covered by the Go integration
    -- tests, which can withhold either.
    ('configuration.secrets.secret.view'),
    ('configuration.secrets.secret.list'),
    ('configuration.secrets.secret.create'),
    ('configuration.secrets.secret.edit'),
    ('configuration.secrets.secret.delete'),
    -- Unit A14, Schedules & Tasks. The READ is gated because the listing names
    -- every internal RPC the platform invokes on a timer, which is
    -- reconnaissance on its own; `.edit` gates the switch that enables and
    -- disables those platform jobs.
    ('configuration.scheduling.schedules.view'),
    ('configuration.scheduling.schedules.edit'),
    -- Unit A14, App Requests. The queue lists every tenant's access requests,
    -- each naming a user, a project and what they asked for, so the listing
    -- itself is the sensitive part; `.edit` gates the approve/reject decision,
    -- which notifies the requester. Before this unit the queue route was
    -- mounted UNGATED on a stub returning a fixed empty page, and the decision
    -- had no route at all.
    ('admin.moderation'),
    ('admin.moderation.edit'),
    -- Unit A14, Configuration. `runtime.plugins` is the SINGLE permission every
    -- pylon handler in that set declares — schemas, values, suggestions,
    -- restart, maintenance and all four runtime_* endpoints — and pylon
    -- registers its recommended roles as super_admin only. Without this row the
    -- page renders a 403 for the section list, which a journey must be able to
    -- tell apart from an unwired route.
    ('runtime.plugins'),
    -- Unit A14, Service Descriptors. Both routes answer 501 — this platform has
    -- no provider hub — and the gate runs BEFORE the refusal, so without these
    -- rows the page would render a 403 and the journey could not tell "the
    -- deployment declines to serve this, and says why" from "the admin persona
    -- lost a permission". `runtime.airun.serviceproviders` gates the listing
    -- (elitea_core/api/v2/admin.py) and `provider_hub.descriptor.register` the
    -- two registration verbs (elitea_core/api/v2/register_descriptor.py).
    ('runtime.airun.serviceproviders'),
    ('provider_hub.descriptor.register'),
    -- ADR-0023 S3, migration 0109. Activation is gated SEPARATELY from
    -- registration — every facade registrar files a registration at boot, so
    -- `.register` is handed out freely, while `.activate` is the switch that
    -- lets agents call the provider. The migration grants it to this same
    -- administration `admin` role; the row is repeated here for the reason the
    -- whole block exists, which the header above states: this seed supplies
    -- permissions directly and SUPPRESSES the central default-mode fallback, so
    -- a string missing from here is a 403 on a stack the migration would have
    -- covered. The Activate control is on the Service Descriptors page and
    -- DWIKI-013c drives it; without this row the journey would fail on a
    -- permission rather than on the behaviour it measures.
    ('provider_hub.descriptor.activate')
) AS p(permission)
WHERE r.name = 'admin' AND r.mode = 'administration'
ON CONFLICT (role_id, permission) DO NOTHING;

-- Unit A14, Schedules & Tasks: a row of the platform cron table for the journey
-- to read and toggle.
--
-- `centry.schedule` is the table `services/elitea-scheduler` polls every minute,
-- so this row is deliberately INACTIVE and points at an `e2e.` function name
-- that no RPC handler answers: a stack that happens to run a scheduler must not
-- start dispatching anything because the E2E seed ran. The journey enables it,
-- re-reads, and disables it again so the run is repeatable.
-- ONE ROW PER BROWSER PROJECT. `fullyParallel` is on and chromium and webkit
-- run the same spec at the same time; `describe.configure({ mode: 'serial' })`
-- orders the tests WITHIN a project but not ACROSS them, so a single shared row
-- would have chromium's "enable, reload, assert enabled" racing webkit's
-- "assert disabled". There is no per-worker fixture that can partition a
-- platform-wide table, so the partition is seeded here instead.
-- \`centry.schedule\` has no unique constraint on \`name\` (it is pylon's model,
-- column for column), so \`ON CONFLICT DO NOTHING\` has no conflict to detect and
-- a second seed simply INSERTED the probe rows again. Two identically named rows
-- then made the listing's name button ambiguous and every schedules journey
-- failed Playwright strict mode. CI seeds once and never saw it. Delete first.
DELETE FROM centry.schedule WHERE name LIKE 'e2e_schedule_probe_%';
INSERT INTO centry.schedule (name, project_id, cron, active, rpc_func, rpc_kwargs, last_run)
VALUES
    ('e2e_schedule_probe_chromium', NULL, '0 4 * * *', false, 'e2e_schedule_probe_noop', '{}'::jsonb, NULL),
    ('e2e_schedule_probe_webkit',   NULL, '0 4 * * *', false, 'e2e_schedule_probe_noop', '{}'::jsonb, NULL);

-- Idempotent re-seed: each journey leaves its row disabled with the original
-- cron, but a run interrupted mid-way would not, and the first assertion is
-- that it starts disabled.
UPDATE centry.schedule
SET active = false, cron = '0 4 * * *'
WHERE name LIKE 'e2e_schedule_probe_%';

-- Unit A14, App Requests: one PENDING access request per browser project, for
-- the journey to read and decide.
--
-- ONE ROW PER BROWSER PROJECT, for the same reason the schedule probe above is
-- partitioned: `fullyParallel` is on and chromium and webkit run the same spec
-- concurrently, while `describe.configure({ mode: 'serial' })` orders tests
-- only WITHIN a project. A single shared row would have one engine's approval
-- landing between the other's "assert pending" and its own decision.
--
-- Authored by the MEMBER persona, not the admin one: an operator answering
-- their own request would not exercise the join that resolves the requester's
-- address, and that column is the whole point of the queue.
-- Remove what earlier runs left behind (issue #544).
--
-- Journey 34 files a run-unique row per decision it proves
-- (`e2e_app_request_probe_<engine>_<action>_<run>`), and NOTHING can remove
-- one: `moderation_status` has a POST, a GET and a decision PUT, and no DELETE
-- at any mode. So the table grows by three rows per browser project per run on
-- a stack that is not re-created. Measured: 20 runs left 111 rows, and the
-- UPDATE below then returned every one of them to `pending`, which fills the
-- operator's Pending tab with a hundred dead probes.
--
-- The two SEEDED rows are named exactly and kept: they are the read-only rows
-- journeys 34 and 34c assert against.
DELETE FROM centry.moderation_state
WHERE entity_id LIKE 'e2e_app_request_probe%'
  AND entity_id NOT IN ('e2e_app_request_probe_chromium', 'e2e_app_request_probe_webkit');

INSERT INTO centry.moderation_state
    (user_id, project_id, issue_type, entity_id, description, status)
SELECT u.id, 1, p.label, p.entity, 'E2E probe: please enable this catalogue entry.', 'pending'
FROM auth_core__user u
CROSS JOIN (VALUES
    ('E2E Probe chromium', 'e2e_app_request_probe_chromium'),
    ('E2E Probe webkit',   'e2e_app_request_probe_webkit')
) AS p(label, entity)
WHERE u.email = 'e2e-member@autotest.local'
  AND NOT EXISTS (
      SELECT 1 FROM centry.moderation_state existing WHERE existing.entity_id = p.entity
  );

-- Idempotent re-seed: each journey approves or rejects its row, so a second run
-- would start from a decided one and the first assertion is that it is pending.
UPDATE centry.moderation_state
SET status = 'pending', rejection_comment = NULL
WHERE entity_id LIKE 'e2e_app_request_probe_%';
-- Unit A14, Configuration: the resources section starts from its SCHEMA
-- DEFAULTS, so the journeys assert against a known baseline and can prove the
-- value they write was not already there.
--
-- ONE CARD PER BROWSER PROJECT rather than one row: `fullyParallel` is on and
-- chromium and webkit run the same spec concurrently against a single,
-- platform-wide configuration table. `describe.configure({ mode: 'serial' })`
-- orders tests within a project and does nothing across them, so both engines
-- editing `resources_documentation_*` would race. chromium owns the
-- Documentation card and webkit owns Tutorials (see admin.features.spec.ts,
-- journey 36g — the section moved to Admin > Features with unit A14).
DELETE FROM centry.platform_config
WHERE section = 'resources'
  AND (key LIKE 'resources_documentation_%' OR key LIKE 'resources_tutorials_%');

-- The Features page's three LIVE flag sections start unconfigured, so the
-- schema defaults are the baseline the journey asserts against.
--
-- This matters more than clearing a text field: `mcp_enabled` left `false` by an
-- interrupted run would 403 every MCP route for the whole stack, and
-- `is_publish_blocked` left `true` would refuse every publish, and
-- `vite_voice_features_enabled` left `false` would hide the chat voice button
-- for every user -- all three silently, as platform-wide behaviour changes that
-- look like a different bug in whichever journey trips over them next. Journey 36 restores its
-- own rows in a `finally`; this is the second line of defence for the run that
-- was killed before the `finally` ran.
DELETE FROM centry.platform_config
WHERE section IN ('mcp_configuration', 'agent_publishing', 'voice_features');

-- A permission that exists ONLY in the database, granted to the administration
-- `viewer` role. The Roles matrix derives its rows from the recorded grants
-- rather than from any compiled-in list, so this string appearing on the page
-- is proof the matrix is the deployment's own — it is in no bundle, and the
-- journey toggles it on `editor` to exercise the write end to end.
INSERT INTO auth_core__role_permission (role_id, permission)
SELECT r.id, 'e2e.roles.probe'
FROM auth_core__role r
WHERE r.name = 'viewer' AND r.mode = 'administration'
ON CONFLICT (role_id, permission) DO NOTHING;

-- …and it must NOT already be on `editor`: the journey asserts it is absent,
-- grants it, re-reads, then revokes it again so the run is repeatable.
DELETE FROM auth_core__role_permission grant_row
USING auth_core__role r
WHERE r.id = grant_row.role_id
  AND r.name = 'editor' AND r.mode = 'administration'
  AND grant_row.permission = 'e2e.roles.probe';

-- Only the ADMIN persona gets it. The member persona deliberately does not, so
-- the difference between the two is a real server-side authorisation
-- difference and not a UI-visibility one — and since A14 that difference is
-- visible on the LISTING too, not just on the writes: `GET
-- /admin/auth_users/administration` is gated on `admin.auth.users`, so the
-- member persona is refused the global user list outright.
INSERT INTO auth_core__user_role (user_id, role_id)
SELECT u.id, r.id
FROM auth_core__user u
JOIN auth_core__role r ON r.name = 'admin' AND r.mode = 'administration'
WHERE u.email = 'e2e-admin@autotest.local'
ON CONFLICT (user_id, role_id) DO NOTHING;

-- Project-scoped roles for project 1 (Default Project).
-- ListCurrentUserProjects JOINs on auth_core__project_user_role so users
-- see "No projects" until they have at least one row here.
INSERT INTO auth_core__project_role (project_id, name)
VALUES (1, 'admin'), (1, 'editor'), (1, 'viewer')
ON CONFLICT (project_id, name) DO NOTHING;

-- Grant a comprehensive permission set to all three project roles.
-- The RBAC resolver checks auth_core__project_role_permission first;
-- without these rows the permission list comes back empty.
-- Permissions needed by E2E tests:
--   projects.*                     → project list endpoint auth + project switcher
--   models.applications.*          → agents/pipelines/skills create+list nav
--   models.applications.tools.*    → toolkits/mcps/applications create+list nav
--   models.chat.*                  → chat create + conversation list
--   configuration.*                → credentials/secrets/users/settings nav
--   configurations.configuration.* → model-configuration page
INSERT INTO auth_core__project_role_permission (project_id, role_id, permission)
SELECT 1, r.id, p.permission
FROM auth_core__project_role r
CROSS JOIN (VALUES
    ('projects.projects.project.view'),
    ('projects.projects.project.edit'),
    ('projects.projects.project.create'),
    ('projects.projects.project.delete'),
    ('models.applications.public_applications.list'),
    ('models.applications.applications.create'),
    ('models.applications.application.update'),
    ('models.applications.application.delete'),
    ('models.applications.publish.post'),
    ('models.applications.export_import.export'),
    ('models.applications.fork.post'),
    ('models.applications.tools.list'),
    ('models.applications.tools.create'),
    ('models.applications.tool.update'),
    ('models.applications.tool.delete'),
    ('models.applications.tool.details'),
    ('models.applications.tool.patch'),
    ('models.applications.tools.export'),
    -- `.details` is the LIST permission for the indexes rail
    -- (`internal/api/v2/indexing/index_meta.go:18`) and a DIFFERENT string
    -- from `.edit`. Project 1 carries per-project rows, so the central
    -- default-mode fallback that shared/0066 grants is suppressed here and the
    -- string has to be listed explicitly; without it the rail 403s while the
    -- Indexes tab still renders, which reads as a hung fetch — measured.
    ('models.applications.index_meta.details'),
    ('models.applications.index_meta.edit'),
    ('models.chat.conversations.list'),
    ('models.chat.conversations.create'),
    ('models.chat.folders.get'),
    ('models.chat.folders.create'),
    ('models.chat.folders.update'),
    ('models.chat.folders.delete'),
    -- Starting an agent turn. `agentexecution/route.go:32` requires
    -- `models.chat.messages.create` and the regenerate route its sibling;
    -- without them a seeded stack answers 403 to the first message ever sent,
    -- which is a seed gap that reads exactly like a broken chat backend.
    ('models.chat.messages.create'),
    ('models.chat.conversations.regenerate'),
    ('models.project_context.view'),
    ('models.project_context.edit'),
    ('configuration.users.users.view'),
    ('configuration.users.users.edit'),
    ('configuration.users.users.create'),
    ('configuration.users.users.delete'),
    ('configuration.secrets.secret.view'),
    ('configuration.secrets.secret.list'),
    ('configuration.secrets.secret.edit'),
    ('configuration.secrets.secret.create'),
    ('configuration.secrets.secret.delete'),
    -- The project secrets routes are gated in-package on the permission each
    -- pylon `ProjectAPI` method declares (internal/api/v2/secrets/handler.go).
    -- `.unsecret` gates the two routes that return a secret VALUE in plaintext
    -- — the mode-ful GET and the mode-less one elitea-sdk's `unsecret()` calls
    -- — and `.hide` gates POST /secrets/hide/…, which the Secrets page's row
    -- menu invokes. Neither was in this list, because until the gate landed
    -- neither route checked anything; absent, they 403.
    ('configuration.secrets.secret.unsecret'),
    ('configuration.secrets.secret.hide'),
    -- mountArtifactRoutes (services/elitea-main/internal/api/router.go:255-262)
    -- gates EVERY artifact route — buckets included — on the four
    -- `configuration.artifacts.artifacts.*` strings. `edit` (PATCH: retention,
    -- pin) and `delete` (DELETE, :batchDelete) were missing here, so every
    -- destructive artifact route 403'd and the specs could not clean up after
    -- themselves. The `buckets.*` pair mirrors the legacy permission catalogue;
    -- the Go router does not read it.
    ('configuration.artifacts.artifacts.create'),
    ('configuration.artifacts.artifacts.view'),
    ('configuration.artifacts.artifacts.edit'),
    ('configuration.artifacts.artifacts.delete'),
    ('configuration.artifacts.buckets.create'),
    ('configuration.artifacts.buckets.view'),
    ('configuration.artifacts.buckets.edit'),
    ('configuration.artifacts.buckets.delete'),
    -- The per-bucket ACCESS LIST routes (`/api/v2/artifacts/bucket_permissions/
    -- {projectID}`) are gated on the two strings the artifacts plugin declares
    -- for them, which are DISTINCT from the four above: pylon keeps the access
    -- map inside an `s3_api_credentials` row, so it gates it on that resource.
    -- Shared migration 0118 grants both centrally; project 1 carries
    -- per-project rows, which SUPPRESS that central fallback, so they have to
    -- be listed here as well or the "Manage access" dialog 403s.
    ('configuration.artifacts.s3_credentials.view'),
    ('configuration.artifacts.s3_credentials.edit'),
    -- #496 gated the whole /api/v2/configurations mount, which until then
    -- applied no permission of any kind. Project 1 carries per-project rows,
    -- so the central default-mode fallback shared/0072 seeds is SUPPRESSED
    -- here and every string has to be listed explicitly.
    --
    -- Only `update` and `delete` were listed, and both were inert: the routes
    -- they name checked nothing. The three below are what the browser actually
    -- calls, and each one 403s without its row:
    --
    --   `configurations.configurationS.list` — the plural is the route's, not
    --   a typo. It gates the credential LIST the AI-configuration page reads,
    --   the model catalogue `GET /configurations/models/{id}` the chat picker
    --   and the tokens page read, `GET /configurations/types/{id}`, and
    --   `GET /configurations/tts_voices/{id}`.
    --   `configurations.configuration.details` — the singular is a DIFFERENT
    --   permission (one credential's row). useFormSeeding reads it to fill the
    --   edit dialog.
    --   `configurations.configuration.create` — the credential save, and the
    --   pre-save "Test connection" probe beside it.
    ('configurations.configurations.list'),
    ('configurations.configuration.details'),
    ('configurations.configuration.create'),
    ('configurations.configuration.update'),
    ('configurations.configuration.delete'),
    -- `GET /api/v2/elitea_core/chat_config/prompt_lib/{projectID}` resolves
    -- this exact string in promptcontextreads' RequireResolvedPermissionsForProject
    -- (CurrentChatConfigPermission). It is the endpoint
    -- features/artifacts' chatConfigApi calls on every artifacts page load to
    -- learn the project's upload limits (#194).
    ('models.chat.conversation.details'),
    -- The notification SSE stream
    -- (GET /api/v2/notifications/events/prompt_lib/{projectID}) resolves this
    -- exact string in `currentNotificationEventsHandler.authorize`. Absent, the
    -- stream answers 403 — which is what it did the moment the route was first
    -- mounted (#152), and which is indistinguishable in the browser from the
    -- 404 it used to answer, since useNotificationsSSE treats every failed
    -- stream the same way.
    ('models.notifications.notifications.list'),
    -- Unit A14, App Requests: the PRODUCT side of the same table the admin
    -- queue reads. The application catalogue's "Request Access" button resolves
    -- `admin.moderation.create` against the caller's membership of the project,
    -- and reading back one's own requests resolves `admin.moderation.view`.
    -- Both were unreachable before A14 — the routes existed but answered from a
    -- constant — so neither string had ever needed to be granted anywhere.
    ('admin.moderation.view'),
    ('admin.moderation.create'),
    -- #302/#313: the /elitea_core group no longer enforces membership alone —
    -- every route now resolves the permission its pylon module declares
    -- (services/elitea-main/internal/api/router.go). This project carries
    -- per-project rows, which SUPPRESSES the central default-mode fallback
    -- that shared/0068 seeds, so each of those strings has to be listed here
    -- explicitly or the route 403s for every persona. Same reasoning, and the
    -- same measured symptom, as the index_meta pair above.
    ('configuration.roles.roles.view'),
    ('models.applications.application.details'),
    ('models.applications.application_relation.patch'),
    ('models.applications.applications.details'),
    ('models.applications.applications.list'),
    ('models.applications.export_import.import'),
    ('models.applications.export_toolkit.export'),
    ('models.applications.index_meta.delete'),
    ('models.applications.index_types.details'),
    ('models.applications.skills.create'),
    ('models.applications.skills.delete'),
    ('models.applications.skills.details'),
    ('models.applications.skills.export'),
    ('models.applications.skills.list'),
    ('models.applications.skills.publish'),
    ('models.applications.skills.update'),
    ('models.applications.task.delete'),
    ('models.applications.toolkit_validator.check'),
    ('models.applications.toolkits.details'),
    ('models.applications.trending_authors.list'),
    ('models.applications.unpublish.post'),
    ('models.applications.upload_icon.delete'),
    ('models.applications.upload_icon.get'),
    ('models.applications.upload_icon.post'),
    ('models.applications.upload_icon.update'),
    ('models.applications.version.delete'),
    ('models.applications.version.details'),
    ('models.applications.version.update'),
    ('models.applications.version_validator.check'),
    ('models.applications.versions.create'),
    ('models.applications.versions.get'),
    ('models.chat.attachments.create'),
    ('models.chat.attachments.delete'),
    ('models.chat.canvas.create'),
    ('models.chat.canvas.details'),
    ('models.chat.canvas.update'),
    ('models.chat.conversation.edit'),
    ('models.chat.conversation.update'),
    ('models.chat.conversations.delete'),
    ('models.chat.entity_settings.update'),
    ('models.chat.messages.delete'),
    ('models.chat.messages.details'),
    ('models.chat.messages.list'),
    ('models.chat.participant.delete'),
    ('models.chat.participants.create'),
    ('models.monitoring.tracing.view'),
    ('models.promptlib_shared.search'),
    ('models.promptlib_shared.tags.list')
) AS p(permission)
WHERE r.project_id = 1
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

-- Assign e2e-admin as project admin, e2e-member as project editor.
INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
SELECT 1, u.id, r.id
FROM auth_core__user u
JOIN auth_core__project_role r ON r.project_id = 1 AND r.name = 'admin'
WHERE u.email = 'e2e-admin@autotest.local'
ON CONFLICT (project_id, user_id, role_id) DO NOTHING;

INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
SELECT 1, u.id, r.id
FROM auth_core__user u
JOIN auth_core__project_role r ON r.project_id = 1 AND r.name = 'editor'
WHERE u.email = 'e2e-member@autotest.local'
ON CONFLICT (project_id, user_id, role_id) DO NOTHING;

-- A dedicated chat-driver persona and its personal project (#290).
--
-- Every server-side model call is authenticated as the ACTOR — the worker
-- carries the user's PAT — and `middleware.Project` resolves that user's
-- PERSONAL project to find the provider credential
-- (`db/queries/auth_projects.sql:5-37`). Without one the LLM hop answers
-- `project_not_resolved` and an agent turn streams `agent_llm_start` straight
-- to `pipeline_finish` with no token ever produced: admitted, streamed, empty.
--
-- A SEPARATE persona rather than driving chat as `e2e-member`/`e2e-admin`.
--
-- THE PERSONAS ALL HAVE A PERSONAL PROJECT NOW, AND THIS SEED NO LONGER
-- DECIDES THAT. Sign-in provisions one for every account that reaches it
-- (`ensurePersonalProject`, services/elitea-main/internal/api/v2/auth/
-- signin.go), which is the product behaviour: a new user lands in Private.
-- So the property this comment used to rely on — that only the chat driver
-- owns a `project_user_%` row — is gone, and anything written on top of it is
-- gone with it.
--
-- What is still true is why the chat driver is a persona of its own: the
-- permissions below are granted INSIDE its personal project, and the `/llm`
-- hop resolves the caller's personal project for the provider credential
-- (#290). Granting the same list inside `e2e-member`'s personal project would
-- widen a persona that ~230 journeys share, for one journey's benefit.
--
-- WHY THE OTHER PERSONAS STILL WORK IN PROJECT 1. Every journey and every
-- visual baseline is written against the seeded project 1, and the app selects
-- the caller's personal project when nothing is selected. `auth.setup.ts`
-- therefore PINS project 1 for those two personas — through the switcher, the
-- product's own path — and records that in the storage state, instead of
-- deriving it from whether a personal project happens to exist yet. It used to
-- derive it, and the answer changed per continuous-integration job (the
-- provisioning wait is bounded at three seconds), which moved fifty journeys
-- and forty-three screenshots off project 1 in three jobs out of four.
--
-- The resolver needs BOTH halves — a `project_user_<uid>` project AND a
-- project-role assignment on it — so the pair is created together; the project
-- alone resolves to nothing. The id is derived (90100 + user id) because this
-- persona is inserted with a serial id, and stays inside the 9xxxx range the
-- fixtures below reserve.
INSERT INTO auth_core__user (email, name)
VALUES ('e2e-chat@autotest.local', 'E2E Chat Driver')
ON CONFLICT (email) DO NOTHING;

INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success, suspended)
SELECT 90100 + u.id, 'project_user_' || u.id, u.id, '{}', true, false
FROM auth_core__user u
WHERE u.email = 'e2e-chat@autotest.local'
ON CONFLICT (id) DO UPDATE
    SET name = EXCLUDED.name, owner_id = EXCLUDED.owner_id, suspended = EXCLUDED.suspended;

INSERT INTO auth_core__project_role (project_id, name)
SELECT 90100 + u.id, 'admin'
FROM auth_core__user u
WHERE u.email = 'e2e-chat@autotest.local'
ON CONFLICT (project_id, name) DO NOTHING;

INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
SELECT r.project_id, u.id, r.id
FROM auth_core__user u
JOIN auth_core__project_role r ON r.project_id = 90100 + u.id AND r.name = 'admin'
WHERE u.email = 'e2e-chat@autotest.local'
ON CONFLICT (project_id, user_id, role_id) DO NOTHING;

-- The chat driver acts INSIDE its personal project, so the permissions the chat
-- routes check have to exist there too — the project-1 grants above do not
-- reach it.
--
-- The INDEX permissions below (#93 Surface A) are on this SAME persona rather
-- than on a fourth one, and that is the smaller change of the two available:
--   * The index-start route resolves `models.applications.tool.patch` against
--     the project in its URL, while the embedding hop underneath it resolves
--     the CALLER's PERSONAL project for the provider credential — so the one
--     caller who can drive an index run end to end is a caller who has both.
--     Only this persona has a personal project the SEED can name (sign-in
--     provisions one for the others, with an id nothing here can predict), and
--     only project 1 grants `tool.patch` — the two never met, which is why an
--     index run used to die on `project_not_resolved` before the permission
--     was even consulted.
--   * Granting the same list inside `e2e-member`/`e2e-admin`'s personal
--     project is the option the comment above rules out: it widens a persona
--     ~230 journeys share, and the seed cannot address those projects anyway.
--   * A fourth persona would need its own OIDC login, storageState, Playwright
--     project, `social_users` row, vault blob and tenant schema — and would
--     still end up with exactly this permission list. Widening one autotest
--     persona's rights INSIDE ITS OWN personal project changes nothing any
--     other persona can see or assert: no journey reads this project, and the
--     project-1 grant list is untouched.
INSERT INTO auth_core__project_role_permission (project_id, role_id, permission)
SELECT r.project_id, r.id, p.permission
FROM auth_core__user u
JOIN auth_core__project_role r ON r.project_id = 90100 + u.id AND r.name = 'admin'
CROSS JOIN (VALUES
    ('projects.projects.project.view'),
    ('models.chat.conversations.list'),
    ('models.chat.conversations.create'),
    ('models.chat.conversations.regenerate'),
    ('models.chat.conversation.details'),
    ('models.chat.messages.create'),
    ('models.chat.folders.get'),
    -- The notification bell mounts on EVERY page, including chat, and its poll
    -- and its SSE stream both resolve this string against the project in the
    -- URL — which, for this persona, is its PERSONAL project, not project 1.
    --
    -- Hand-listed for the same reason `index_meta.details` below is, but the
    -- mechanism is worth stating because it is counter-intuitive: project 1
    -- already carries this pair as an override (see the grant near the top of
    -- this file), and shared/0090's backfill deliberately SKIPS any
    -- (role, permission) pair that some project already overrides — it cannot
    -- tell "never delivered" from "deliberately revoked here". So granting it on
    -- project 1 is precisely what stops it reaching this project, and adding it
    -- to the central corpus would not help either.
    --
    -- Measured symptom when absent: the bell's poll is refused on every page,
    -- and because `http.ts` escalated a 401 into re-auth, the whole app became
    -- an unbreakable login redirect loop. The client no longer escalates a
    -- background poll's 401, but the permission is still what the bell needs to
    -- work rather than merely fail quietly.
    ('models.notifications.notifications.list'),
    -- #93 Surface A. `tool.patch` is the index-start route's own permission
    -- (`internal/api/v2/indexing/route.go:15`); the rest are what the browser
    -- needs to REACH the run control — list the toolkits, open one, read and
    -- write its index_meta rows.
    ('models.applications.tools.list'),
    ('models.applications.tools.create'),
    ('models.applications.tool.details'),
    ('models.applications.tool.update'),
    ('models.applications.tool.patch'),
    -- `.details` is the LIST permission for `GET .../index_meta/...`
    -- (`internal/api/v2/indexing/index_meta.go:18`), and it is a different
    -- string from `.edit`. Without it the indexes rail 403s and renders
    -- loading skeletons forever, which looks like a hung fetch rather than a
    -- refusal — measured.
    ('models.applications.index_meta.details'),
    ('models.applications.index_meta.edit'),
    ('models.applications.index_meta.delete'),
    -- The `artifact` toolkit indexes an artifact bucket, so the driver has to
    -- be able to create one and put a document in it.
    ('configuration.artifacts.artifacts.view'),
    ('configuration.artifacts.artifacts.create'),
    ('configuration.artifacts.artifacts.edit'),
    ('configuration.artifacts.artifacts.delete'),
    -- `configurations.configurationS.list` — the plural is the route's, not a
    -- typo: handler.go:102 requires it for the model catalogue the picker
    -- reads. The singular form below is a DIFFERENT permission (one config's
    -- details), and granting only that leaves the picker empty behind a 403.
    ('configurations.configurations.list'),
    ('configurations.configuration.details'),
    -- The three configuration WRITES, for the same reason as the two reads
    -- above and for one more that is specific to this project (#496).
    --
    -- deploy/scripts/standalone-stack.sh reuses this seeder verbatim, and its
    -- own `#457` check writes a credential and two model rows through the
    -- product route — POST /api/v2/configurations/configurations/{projectID} —
    -- as THIS persona, in THIS project, then deletes them again through the
    -- product's delete route. That mount applied no permission at all until
    -- #496, so the write needed no grant. It does now, and without these three
    -- rows the check reports "the credential write failed: insufficient
    -- permissions" — measured.
    ('configurations.configuration.create'),
    ('configurations.configuration.update'),
    ('configurations.configuration.delete'),
    -- #302/#313: the /elitea_core group no longer enforces membership alone —
    -- every route now resolves the permission its pylon module declares
    -- (services/elitea-main/internal/api/router.go). This project carries
    -- per-project rows, which SUPPRESSES the central default-mode fallback
    -- that shared/0068 seeds, so each of those strings has to be listed here
    -- explicitly or the route 403s for every persona. Same reasoning, and the
    -- same measured symptom, as the index_meta pair above.
    ('configuration.roles.roles.view'),
    ('configuration.users.users.view'),
    ('models.applications.application.delete'),
    ('models.applications.application.details'),
    ('models.applications.application.update'),
    ('models.applications.application_relation.patch'),
    ('models.applications.applications.create'),
    ('models.applications.applications.details'),
    ('models.applications.applications.list'),
    ('models.applications.export_import.export'),
    ('models.applications.export_import.import'),
    ('models.applications.export_toolkit.export'),
    ('models.applications.fork.post'),
    ('models.applications.index_types.details'),
    ('models.applications.publish.post'),
    ('models.applications.skills.create'),
    ('models.applications.skills.delete'),
    ('models.applications.skills.details'),
    ('models.applications.skills.export'),
    ('models.applications.skills.list'),
    ('models.applications.skills.publish'),
    ('models.applications.skills.update'),
    ('models.applications.task.delete'),
    ('models.applications.tool.delete'),
    ('models.applications.toolkit_validator.check'),
    ('models.applications.toolkits.details'),
    ('models.applications.trending_authors.list'),
    ('models.applications.unpublish.post'),
    ('models.applications.upload_icon.delete'),
    ('models.applications.upload_icon.get'),
    ('models.applications.upload_icon.post'),
    ('models.applications.upload_icon.update'),
    ('models.applications.version.delete'),
    ('models.applications.version.details'),
    ('models.applications.version.update'),
    ('models.applications.version_validator.check'),
    ('models.applications.versions.create'),
    ('models.applications.versions.get'),
    ('models.chat.attachments.create'),
    ('models.chat.attachments.delete'),
    ('models.chat.canvas.create'),
    ('models.chat.canvas.details'),
    ('models.chat.canvas.update'),
    ('models.chat.conversation.edit'),
    ('models.chat.conversation.update'),
    ('models.chat.conversations.delete'),
    ('models.chat.entity_settings.update'),
    ('models.chat.folders.create'),
    ('models.chat.folders.delete'),
    ('models.chat.folders.update'),
    ('models.chat.messages.delete'),
    ('models.chat.messages.details'),
    ('models.chat.messages.list'),
    ('models.chat.participant.delete'),
    ('models.chat.participants.create'),
    -- STOP. `models.chat.task.delete` gates DELETE
    -- /elitea_core/task/prompt_lib/{projectID}/{responseMessageID}
    -- (`internal/api/v2/agentexecution/cancel.go:17`), which is the route the
    -- composer's Stop button calls.
    --
    -- Hand-listed for the third time in this file and for the same mechanism as
    -- `index_meta.details` and the notification string above: shared/0072
    -- grants it CENTRALLY to default-mode admin/editor/viewer, this project
    -- carries per-project rows, and per-project rows suppress the central
    -- fallback wholesale. shared/0090's backfill does not reach it either --
    -- 0090 runs at stack-up and this project's override rows are written by
    -- THIS seeder afterwards, so there is nothing for the backfill to
    -- reconcile when it runs.
    --
    -- Measured symptom when absent, and why nothing caught it: the cancel is
    -- fired as `void stopChatTask(...).catch(() => undefined)`
    -- (`features/chat-messages/model/useChatStreamTransport.ts`), so the 403 is
    -- swallowed. The browser detaches its own stream and the spinner clears, so
    -- Stop LOOKS like it worked while the execution keeps running server-side.
    -- `e2e/streaming/chat.stop.spec.ts` requires the 204 for exactly that
    -- reason.
    ('models.chat.task.delete'),
    ('models.monitoring.tracing.view'),
    ('models.project_context.edit'),
    ('models.project_context.view'),
    ('models.promptlib_shared.search'),
    ('models.promptlib_shared.tags.list')
) AS p(permission)
WHERE u.email = 'e2e-chat@autotest.local'
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

-- social_users rows (needed for personal_project_id resolution).
--
-- The chat driver is in this list and must stay in it: `/social/author` reads
-- these rows to answer `personal_project_id`, and WITHOUT one it falls back to
-- project 1. The app then opens the chat in a project the driver holds no chat
-- permission in and the turn 403s — measured, and indistinguishable from a
-- broken start route until you look at which project the request names.
INSERT INTO centry.social_users (user_id, title)
SELECT u.id, u.name
FROM auth_core__user u
WHERE u.email IN ('e2e-admin@autotest.local', 'e2e-member@autotest.local', 'e2e-chat@autotest.local')
ON CONFLICT (user_id) DO NOTHING;

-- p_1.configuration: one mock model config so the personal-token create button is
-- enabled (tokens.tsx: isAddButtonDisabled = configurations.length === 0). This
-- row is the CREDENTIAL, section='models', which is what
-- /configurations/configurations/{id}?section=models lists.
INSERT INTO p_1.configuration
    (project_id, elitea_title, type, section, data, meta, shared, status_ok, source, created_at, updated_at)
VALUES
    (1, 'e2e-mock-model', 'azure_open_ai', 'models',
     '{"api_key":"e2e-mock-key","api_base":"http://localhost/mock","api_version":"2024-02-01","model":"gpt-4o"}',
     '{}', false, true, 'user', NOW(), NOW())
ON CONFLICT (elitea_title) DO UPDATE SET section = EXCLUDED.section, updated_at = NOW();

-- The row the MODEL PICKER reads, and a DIFFERENT section from the credential
-- above. The picker was moved off the credentials list onto the model
-- CATALOGUE (`/configurations/models/{projectId}`), and that route selects
-- `section = 'llm'` (CurrentModelSectionLLM, application/configurations/models.go)
-- — so the section='models' credential row alone leaves the picker with nothing
-- to show and it renders "NONE".
--
-- Caught by the @visual chat snapshots, which are the only place the picker's
-- resolved label is asserted: no unit test covers it, because the seed and the
-- route live on opposite sides of the wire. The three sections are genuinely
-- distinct and easy to conflate — `ai_credentials` (what the gateway resolves),
-- `models` (the credentials list), and `llm`/`llm_model` (the catalogue the
-- picker and GET /llm/v1/models read). deploy/scripts/standalone-stack.sh seeds
-- all three for the same reason.
INSERT INTO p_1.configuration
    (project_id, elitea_title, label, type, section, data, meta, shared, status_ok, source, created_at, updated_at)
VALUES
    (1, 'e2e-mock-model-llm', 'E2E-MOCK-MODEL', 'llm_model', 'llm',
     '{"name":"E2E-MOCK-MODEL"}',
     '{}', false, true, 'user', NOW(), NOW())
ON CONFLICT (elitea_title) DO UPDATE
    SET data = EXCLUDED.data, section = EXCLUDED.section, type = EXCLUDED.type,
        label = EXCLUDED.label, status_ok = true, updated_at = NOW();

-- centry secret vaults for the admin scope and project 1.
--
-- `GET /elitea_core/chat_config/prompt_lib/{projectID}` reads the project's
-- upload limits out of the Fernet-encrypted centry vault
-- (promptcontextreads' CurrentChatConfigVaultReader loads BOTH the admin and
-- the project snapshot, and errors if either row is absent). Nothing in this
-- stack creates those rows — centry's `create_project_space` does it in the
-- legacy world, and the Go project-create path has no equivalent — so without
-- this the route answers 500 and the artifacts page silently falls back to the
-- client's own 150 MB default (#194).
--
-- The blobs are deliberate, deterministic, NON-SECRET test material: a
-- fixed 32-byte Fernet key stored in centry's own 44-byte URL-safe base64
-- form, over a vault whose entire contents are five upload-limit integers in a
-- throwaway E2E database. They are written as literals because Fernet
-- (AES-128-CBC + HMAC-SHA256) cannot be produced from psql, and they are
-- stable because Fernet carries no TTL that this reader enforces.
--
-- The five values live in the ADMIN vault, not project 1's, and project 1's is
-- seeded EMPTY. Two reasons, both measured:
--   * `lookupCurrentChatInteger` resolves project-regular → project-hidden →
--     admin-regular, so the admin vault is the documented shared fallback and
--     putting them there exercises that precedence path rather than the
--     trivial one.
--   * these are ordinary project secrets. Seeding them into project 1's vault
--     made all five appear as rows on Settings > Secrets — verified against the
--     running stack, GET /secrets/secrets/default/1 returned them — which
--     changed the `settings-secrets` visual baseline and gave J21 a non-empty
--     list to start from. The admin vault is not listed by that page.
--
-- The five values are all DIFFERENT from
-- promptcontextreads' built-in defaults (10/150/150/10/3), which is what makes
-- J20f discriminating: a client that never receives this config renders the
-- default limit instead, and the assertion fails.
--   chat_max_upload_count         4   (default 10)
--   chat_max_upload_size_mb       5   (default 150)
--   chat_max_file_upload_size_mb  1   (default 150)
--   chat_max_image_upload_count   2   (default 10)
--   chat_max_image_upload_size_mb 6   (default 3)
--
-- Stored as JSON STRINGS, not numbers, on purpose: the secrets API handler
-- (internal/api/v2/secrets/handler.go) round-trips a vault blob through
-- `map[string]string`, and a JSON number would fail that unmarshal.
-- centrysecrets' Python-int contract accepts either.
--
-- That format now matters MORE than when this was written. Unit A14 gave the
-- `admin` row an HTTP surface of its own — `internal/api/v2/secrets/admin.go`,
-- the global vault behind Admin > Secrets — so these five entries are rows on
-- that page and journey 32 asserts they are. They are also why that handler
-- refuses to write a vault it could not read rather than replacing it with an
-- empty one: on THIS row, "replace with empty" would silently delete the
-- deployment's shared secrets and, here, break J20f.
--
-- J21 (Settings > Secrets) is still unaffected: that page reads
-- `project-1`, which stays empty.
INSERT INTO centry.secrets_key (id, data) VALUES
    ('admin', '\x6f4b47696f36536c7071656f71617172724b32757237437873724f3074626133754c6d36753779397672383d'::bytea),
    ('project-1', '\x45424553457851564668635947526f62484230654879416849694d6b4a53596e4b436b714b7977744c69383d'::bytea)
ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data;

INSERT INTO centry.secrets_data (id, data) VALUES
    ('admin', '\x674141414141426f6d544b4141414543417751464267634943516f4c4441304f44795765516b54395f515053716d754d506d585f5855576e6d566257494b58314e4d596146712d62386d6f67337145556e76336642595252484135475a666278576974436943364e764b37616c4d4f365346505558364277714c4672714546357146314b424a384d35723667426843363361625648764c32344a6d75705a434e49546c4c53674635725357726e4f333169386b4d506f4e686a4839704444333146784374726c645f4779635f6132713356735446756562786a614b313831664152715065535f5a553034776578617a74426b7a4458427977456f306e4b367a7449625f527851654c655353327a4971594c6e435a6a494b794743786e645267694968436c776874493132424f73487059774942653755527444515a772d307671617a4538706e4c4e7a45464f4c37384d527459717454392d352d37596b6a6b4864673d3d'::bytea),
    ('project-1', '\x674141414141426f6d544b4141414543417751464267634943516f4c4441304f447738384e6179597230503157334c364279534e5257346647764e5f6778596831726f472d386d646b77547a5155666a42735a785366694a62304f35726a4b2d455a707971362d5436704c7252674a4c6851395935376753595546446955383237486a36634473756a4b2d78'::bytea)
ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data;

-- The same EMPTY vault for every personal project this seed creates.
--
-- `CurrentModelCatalogReader` loads the project's model defaults out of the
-- centry vault and fails the whole read when the rows are ABSENT — not when
-- they are empty. So a project without them answers 500 to
-- `GET /configurations/models/{project}`, which is what the chat page asks for
-- its model catalogue: the picker then presents nothing and a turn cannot name
-- a model (#293).
--
-- The pair is COPIED from project-1 rather than generated: the blobs are Fernet
-- (AES-128-CBC + HMAC-SHA256), which psql cannot produce, and project-1's vault
-- is deliberately empty — so copying key AND data together yields a valid,
-- consistent, empty vault under a key that decrypts it. The five real upload
-- limits live in the ADMIN vault, which stays the shared fallback for every
-- project exactly as before.
INSERT INTO centry.secrets_key (id, data)
SELECT 'project-' || p.id::text, source.data
FROM centry.project p
CROSS JOIN (SELECT data FROM centry.secrets_key WHERE id = 'project-1') AS source
WHERE p.name LIKE 'project\_user\_%'
ON CONFLICT (id) DO NOTHING;

INSERT INTO centry.secrets_data (id, data)
SELECT 'project-' || p.id::text, source.data
FROM centry.project p
CROSS JOIN (SELECT data FROM centry.secrets_data WHERE id = 'project-1') AS source
WHERE p.name LIKE 'project\_user\_%'
ON CONFLICT (id) DO NOTHING;

-- ── admin projects fixture (unit A14) ────────────────────────────────────
-- 001_initial.sql seeds ONE project ("Default Project"), which is not enough
-- to tell a working listing from a broken one: with a single row, the tab
-- counts, the personal/team split, the search filter and the sort are all
-- indistinguishable from constants. These rows give journey 31 a set where
-- each of those has a wrong answer that differs from the right one.
--
--   e2e-team-suspended   suspended, so the status chip has two values to
--                        distinguish and the unsuspend path has a target
--   e2e-team-active      two project admins besides the owner, so a listing
--                        that JOINs rather than aggregates emits it twice
--   project_user_90001   PERSONAL by pylon's `project_user_%` rule, so the
--                        two tabs return different sets — a client-side tab
--                        that filtered nothing would show it on both
--
-- Ids are in the 9xxxx range, above anything the stack creates, so the seed is
-- re-runnable and never collides with a real row.
INSERT INTO auth_core__user (id, email, name, suspended) VALUES
    (90001, 'e2e-project-owner@autotest.local', 'E2E Project Owner', false),
    (90002, 'e2e-project-admin@autotest.local', 'E2E Project Admin', false)
ON CONFLICT (id) DO NOTHING;

INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success, suspended) VALUES
    (90001, 'e2e-team-active',    90001, '{}', true, false),
    (90002, 'e2e-team-suspended', 90001, '{}', true, true),
    (90003, 'project_user_90001', 90001, '{}', true, false)
ON CONFLICT (id) DO UPDATE
    SET name = EXCLUDED.name, owner_id = EXCLUDED.owner_id, suspended = EXCLUDED.suspended;

INSERT INTO auth_core__project_role (project_id, name) VALUES
    (90001, 'admin'), (90001, 'editor'), (90001, 'viewer')
ON CONFLICT (project_id, name) DO NOTHING;

-- Two admins on e2e-team-active. Both must appear in ONE row's Admins cell; a
-- join-based lookup would instead emit the project twice.
INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
SELECT 90001, u.id, r.id
FROM auth_core__user u
JOIN auth_core__project_role r ON r.project_id = 90001 AND r.name = 'admin'
WHERE u.email IN ('e2e-project-admin@autotest.local', 'e2e-admin@autotest.local')
ON CONFLICT (project_id, user_id, role_id) DO NOTHING;

-- ITS OWN PROJECT, and that is not tidiness (issue #519's lesson, and #290's).
--
-- The first version put this toolkit in project 1, the shared one every
-- persona lands in. J17.1 ("an empty toolkit list redirects to the create
-- page") polls until project 1's toolkit list reports `total: 0`, because
-- sibling journeys create and delete toolkits there. A PERMANENT toolkit makes
-- that condition unreachable, so J17.1 could only ever time out — a journey
-- broken by a fixture it never mentions.
--
-- The chat work hit the same wall and answered it the same way: a dedicated
-- persona and project (#290). Here only the project is dedicated; the member
-- persona is granted editor on it and the journey selects it.
INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success, suspended)
VALUES (90200, 'e2e-deepwiki', 1, '{}', true, false)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, suspended = EXCLUDED.suspended;

INSERT INTO auth_core__project_role (project_id, name) VALUES
    (90200, 'admin'), (90200, 'editor'), (90200, 'viewer')
ON CONFLICT (project_id, name) DO NOTHING;

-- The permissions this project's journey needs, copied from what project 1's
-- editor role holds. Copied rather than restated: a hand-written subset drifts
-- from the real role the moment a permission is added, and the journey would
-- fail on a permission nobody thought about.
INSERT INTO auth_core__project_role_permission (project_id, role_id, permission)
SELECT 90200, target.id, source.permission
FROM auth_core__project_role_permission source
JOIN auth_core__project_role origin
  ON origin.id = source.role_id AND origin.project_id = 1
JOIN auth_core__project_role target
  ON target.project_id = 90200 AND target.name = origin.name
WHERE source.project_id = 1
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
SELECT 90200, u.id, r.id
FROM auth_core__user u
JOIN auth_core__project_role r ON r.project_id = 90200 AND r.name = 'editor'
WHERE u.email = 'e2e-member@autotest.local'
ON CONFLICT (project_id, user_id, role_id) DO NOTHING;

-- ── the Inventory journeys' own project (INV-001..010) ───────────────────────
--
-- A SECOND dedicated project, and NOT 90200. The two applications are
-- independent — an Inventory toolkit is `type = 'inventory'` and a wiki one is
-- `type = 'wikis'` — but `/inventory` RESOLVES ITS OWN TOOLKIT and renders the
-- chooser only when the project holds more than one. Sharing 90200 would make
-- the count of Inventory toolkits a function of what the DeepWiki fixtures
-- happen to seed, so INV-001's chooser assertion would break for a change
-- nobody made to Inventory.
--
-- Its rows are HERE and not beside the toolkit block below for the reason the
-- comment under the schema loop gives: `centry.project` must exist before the
-- loop runs, or `p_90300` is never created and the tenant migration pass that
-- follows refuses the WHOLE stack on its preflight.
INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success, suspended)
VALUES (90300, 'e2e-inventory', 1, '{}', true, false)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, suspended = EXCLUDED.suspended;

INSERT INTO auth_core__project_role (project_id, name) VALUES
    (90300, 'admin'), (90300, 'editor'), (90300, 'viewer')
ON CONFLICT (project_id, name) DO NOTHING;

-- Copied from project 1's overrides, not restated — 90200's block above says
-- why: a hand-written subset drifts the moment a permission is added.
INSERT INTO auth_core__project_role_permission (project_id, role_id, permission)
SELECT 90300, target.id, source.permission
FROM auth_core__project_role_permission source
JOIN auth_core__project_role origin
  ON origin.id = source.role_id AND origin.project_id = 1
JOIN auth_core__project_role target
  ON target.project_id = 90300 AND target.name = origin.name
WHERE source.project_id = 1
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
SELECT 90300, u.id, r.id
FROM auth_core__user u
JOIN auth_core__project_role r ON r.project_id = 90300 AND r.name = 'editor'
WHERE u.email = 'e2e-member@autotest.local'
ON CONFLICT (project_id, user_id, role_id) DO NOTHING;

-- Tenant schemas for every project this seed created.
--
-- POSITION IS LOAD-BEARING: this must run after EVERY `centry.project` insert,
-- including the admin fixtures below-that-were-above. Placed earlier it
-- provisions only the rows that happen to exist yet, and the next migrate call
-- fails its preflight on the rest — a FIRST seed then breaks while a second
-- one "fixes" it, which is exactly how this was found.
--
-- In a real deployment pylon provisions `p_<id>` when it creates the project;
-- this seeder is that provisioner's stand-in, so it owes the same schema.
-- elitea-migrate VALIDATES the schema and never creates one
-- (`migrate/runner.go:50`), and its `-all-tenants` preflight ERRORS on any
-- create_success project that lacks one — so a project row seeded without a
-- schema does not merely miss migrations, it makes the next `up` fail for the
-- whole stack. The admin fixtures (90001-90003) have always been in that state;
-- they only escaped notice because migrate runs before the seeder on a first
-- `up` and nobody re-ran it afterwards.
DO $schemas$
DECLARE
    target RECORD;
BEGIN
    FOR target IN
        -- Every create_success project, not only those missing a schema:
        -- `create_tenant_schema` is idempotent — every CREATE TABLE in it is
        -- an IF NOT EXISTS and so is every CREATE INDEX — so running it
        -- unconditionally also repairs a schema that exists but is empty, the
        -- state a bare CREATE SCHEMA would have left behind.
        --
        -- The table COUNT that used to be in that sentence ("all 47 tables")
        -- is gone on purpose. It was already stale — 001_initial.sql holds 51
        -- CREATE TABLE statements now — and a number narrated in a comment
        -- cannot notice when it stops being true. The property that actually
        -- has to hold is asserted instead, after the tenant migration pass
        -- below, against the migration ledger.
        SELECT p.id
        FROM centry.project p
        WHERE p.create_success = TRUE
        ORDER BY p.id
    LOOP
        -- `create_tenant_schema` is the stack's own tenant DDL, defined by
        -- 001_initial.sql and used there to build p_1. A bare CREATE SCHEMA is
        -- NOT enough: the tenant migration history expands tables the baseline
        -- is expected to have already created, so an empty schema fails on
        -- `relation "configuration" does not exist`.
        PERFORM create_tenant_schema('p_' || target.id::text);
    END LOOP;
END
$schemas$;

-- ── audit trail fixture (unit A14) ───────────────────────────────────────
-- `centry.audit_events` is written by the legacy tracing plugin, which the Go
-- E2E stack does not run — so without these rows the Audit Trail page is
-- correctly empty and journey 29 could only ever assert an empty state, which
-- the pre-A14 STUB (an unconditional `{"items":[],"total":0}`) would also have
-- passed. These rows are what make "the page reads the database" a claim with
-- a failure mode.
--
-- Timestamps are relative to now() so they land inside the page's default
-- "Today" window — a window the BROWSER computes, from `DEFAULT_PRESET` in
-- src/pages/admin/auditFormat.ts, as local midnight to local end of day.
--
-- `now() - interval '20 minutes'` alone does NOT land inside it in the twenty
-- minutes after midnight: a row stamped 23:5x belongs to yesterday, and
-- journey 29 fails with "element not found" on a row that is plainly in the
-- table. Seen for real at 00:15 local. So the four rows anchor on a BASE that
-- is never earlier than the start of the current day, and run forward from it
-- (+0/+1/+2/+10 min) to preserve the ordering the span tree needs. Away from
-- midnight the base is `now() - 20 minutes` and the timestamps are unchanged.
--
-- WHICH day, though, is the whole of issue #214. `date_trunc('day', now())`
-- takes the day boundary from the postgres session's zone; the browser takes
-- it from the runner's. Clamping to a day the browser does not agree is today
-- only moves the failure: at UTC+10 and 00:10 local, a UTC clamp yields a base
-- that is still yesterday for that browser. So both sides now name ONE zone —
-- `E2E_TZ`, which playwright.config.ts pins the browser to as `E2E_TIMEZONE`
-- and psql receives below as `:'e2e_tz'`. `now() AT TIME ZONE :'e2e_tz'` is
-- the wall clock the browser reads, `date_trunc('day', …)` is that browser's
-- midnight, and the second `AT TIME ZONE :'e2e_tz'` returns it as the instant
-- the page will send as `date_from`. The two boundaries are then the same
-- instant by construction, at every time of day, in every zone.
--
-- One trace of three spans, and one single-span trace. The counts are chosen
-- so the two views CANNOT agree by accident: 4 spans, 2 traces.
DELETE FROM centry.audit_events WHERE trace_id IN ('e2e-trace-alpha', 'e2e-trace-beta');
INSERT INTO centry.audit_events
    (timestamp, user_id, user_email, project_id, event_type, action, http_method,
     status_code, duration_ms, is_error, tool_name, trace_id, span_id, parent_span_id)
SELECT base.ts + seeded.offset_minutes, actor.user_id, actor.user_email, proj.project_id, seeded.event_type,
       seeded.action, seeded.http_method, seeded.status_code, seeded.duration_ms,
       seeded.is_error, seeded.tool_name, seeded.trace_id, seeded.span_id, seeded.parent_span_id
FROM (VALUES
    (interval '0 minutes',  'api',  'POST /chat/e2e',   'POST', 200::smallint, 25.0,    false, NULL,           'e2e-trace-alpha', 'e2ealpharoot', NULL),
    (interval '1 minutes',  'llm',  'completion/e2e',   NULL,   200::smallint, 2400.0,  false, NULL,           'e2e-trace-alpha', 'e2ealphac1',   'e2ealpharoot'),
    (interval '2 minutes',  'tool', 'search/e2e',       NULL,   500::smallint, 640.0,   true,  'e2e_toolkit',  'e2e-trace-alpha', 'e2ealphac2',   'e2ealpharoot'),
    (interval '10 minutes', 'api',  'GET /agents/e2e',  'GET',  200::smallint, 15.0,    false, NULL,           'e2e-trace-beta',  'e2ebetaroot',  NULL)
) AS seeded(offset_minutes, event_type, action, http_method, status_code, duration_ms, is_error, tool_name, trace_id, span_id, parent_span_id)
CROSS JOIN LATERAL (
    SELECT greatest(
        now() - interval '20 minutes',
        date_trunc('day', now() AT TIME ZONE :'e2e_tz') AT TIME ZONE :'e2e_tz'
    ) AS ts
) AS base
CROSS JOIN LATERAL (
    SELECT id AS user_id, email AS user_email FROM auth_core__user WHERE email = 'e2e-admin@autotest.local'
) AS actor
CROSS JOIN LATERAL (SELECT 1 AS project_id) AS proj;
ENDSQL

    # Use the correct binary for exec: podman exec or docker exec.
    EXEC_BIN="${COMPOSE_BIN%% *}"  # first word of COMPOSE_BIN (podman or docker)
    # ON_ERROR_STOP=1: without it psql reports a failed statement on stderr and
    # then carries on to the next one, exiting 0 — so the script printed
    #   ERROR:  duplicate key value violates unique constraint "auth_core__user_pkey"
    #   ✓ DB rows seeded.
    # back to back, and the only thing that noticed was the postcondition below,
    # several statements later and with none of the context. A statement that
    # fails here must stop the seed at the statement that failed.
    # `-v e2e_tz`: the heredoc above is quoted (`<<'ENDSQL'`), so the shell does
    # not expand anything inside it and the zone must arrive as a psql variable.
    # The audit trail fixture reads it as `:'e2e_tz'`, which psql expands to a
    # quoted SQL literal. An unknown zone name raises `invalid value for
    # parameter "TimeZone"` and ON_ERROR_STOP=1 halts the seed there, which is
    # the correct outcome: a seed that fell back to the session's zone would
    # reintroduce exactly the disagreement #214 is about, silently.
    $EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -v ON_ERROR_STOP=1 -v e2e_tz="$E2E_TZ" \
      -U elitea -d elitea < "$SEED_TMP"
    rm -f "$SEED_TMP"
    echo "  ✓ DB rows seeded (calendar-day fixtures anchored in ${E2E_TZ})."

    # ── tenant migrations, the SECOND migrate pass ───────────────────────────
    #
    # HERE and not at the shared pass, because only now does every
    # `create_success` project have the `p_<id>` schema `-all-tenants` demands:
    # the `DO $schemas$` loop at the end of the seed SQL above has just
    # provisioned them, the admin fixtures 90001-90003 included. Run before it,
    # the flag's preflight refuses for the whole deployment and NOTHING is
    # migrated — not even project 1, whose schema has been there since
    # 001_initial.sql.
    #
    # Without this pass the tenant chain never ran on this stack at all. The
    # shared pass applies migrations/shared/; the tenant history is only
    # reached under `-tenant-project` or `-all-tenants`
    # (cmd/elitea-migrate/main.go), the compose file has no migrate service,
    # and elitea-main runs SKIP_MIGRATIONS=1 — so the tenant schemas kept
    # exactly the shape 001_initial.sql gave them. MEASURED consequences on
    # this stack before this call existed: `configuration_revisions` and
    # `configuration_validation_projection` absent (tenant/0120),
    # `elitea_tools.shared_id` / `.shared_owner_id` / `.updated_at` absent
    # (0124), `entity_tool_mapping.entity_id` absent (0125). Any Go read path
    # naming one of them answers 42P01/42703, which reaches the browser as a
    # 500 — the same shape the transcript route had before the chat tables
    # landed in the baseline.
    #
    # `-all-tenants` matches what the two other deployments of this binary
    # pass: deploy/scripts/standalone-stack.sh and the Helm pre-install hook in
    # deploy/helm/elitea/values.yaml. Both flags are idempotent — an applied
    # migration is skipped by version+checksum — so re-seeding re-runs nothing.
    echo "  → Running elitea-migrate -all-tenants (tenant history)…"
    run_elitea_migrate "tenant history (-all-tenants)" -all-tenants

    # ── postcondition: the tenant history reached EVERY tenant schema ────────
    #
    # Asserted against the migration ledger rather than narrated, and asserted
    # per project rather than in aggregate: `-all-tenants` applies the chain in
    # a loop and exits on the first failure, so a run that stopped halfway
    # leaves the projects it reached complete and the rest untouched.
    #
    # The expected number is DERIVED from the shipped chain, not restated. A
    # literal here would be the "all 47 tables" comment again — correct when
    # written, silently wrong at the next migration, and with no way to notice.
    # The image under test is built from this same tree, so the files on disk
    # are the files inside the container.
    TENANT_MIGRATIONS=$(find "${REPO_ROOT}/services/elitea-main/migrations/tenant" \
      -name '*.sql' -type f 2>/dev/null | wc -l | tr -d ' ')
    # A path that stopped resolving must not read as "nothing to check". This
    # gate has been defeated that way before (check-playwright-image-tag, #157):
    # the directory moved, the lookup found nothing, and the "nothing found →
    # OK" branch passed everything from then on.
    if [ "${TENANT_MIGRATIONS:-0}" -lt 1 ]; then
      echo "ERROR: found no tenant migrations under" >&2
      echo "  ${REPO_ROOT}/services/elitea-main/migrations/tenant" >&2
      echo "  The chain cannot be empty; the path has moved and this assertion" >&2
      echo "  would otherwise pass on every stack, migrated or not." >&2
      exit 1
    fi
    TENANT_LEDGER_GAPS=$($EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -tAc "
      SELECT COALESCE(string_agg(id::text || ':' || applied::text, ', ' ORDER BY id), '')
      FROM (
        SELECT p.id AS id,
               (SELECT COUNT(*)
                  FROM elitea_runtime.schema_migrations m
                 WHERE m.target_kind = 'tenant'
                   AND m.target_id = p.id::text) AS applied
        FROM centry.project p
        WHERE p.create_success = TRUE
      ) AS ledger
      WHERE applied <> ${TENANT_MIGRATIONS};")
    if [ -n "$TENANT_LEDGER_GAPS" ]; then
      echo "ERROR: the tenant migration history did not reach every tenant schema." >&2
      echo "  expected ${TENANT_MIGRATIONS} tenant migration(s) per project" >&2
      echo "  projects short of that (id:applied): ${TENANT_LEDGER_GAPS}" >&2
      echo "  A p_<id> schema left at the 001_initial.sql baseline is missing" >&2
      echo "  configuration_revisions, configuration_validation_projection and" >&2
      echo "  the 0124/0125 columns, and every route that names one answers 500." >&2
      echo "  If every project is short by the SAME small number, suspect a stale" >&2
      echo "  image instead: the expected count is read from this tree, the applied" >&2
      echo "  ones come from the chain embedded in the running elitea-main image." >&2
      echo "  Rebuild it (compose build / standalone-stack.sh build) and re-seed." >&2
      exit 1
    fi
    TENANT_SCHEMAS=$($EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -tAc "
      SELECT COUNT(*) FROM centry.project WHERE create_success = TRUE;")
    echo "  ✓ tenant history verified: ${TENANT_MIGRATIONS} migration(s) applied to all ${TENANT_SCHEMAS} tenant schema(s)."

    # ── postcondition: the personas must actually resolve permissions ────────
    #
    # Every statement above is `ON CONFLICT DO NOTHING` over a `SELECT`, so a
    # row whose join finds nothing inserts nothing AND reports success. That is
    # exactly what happened on a fresh volume: `auth_core__project_user_role`
    # came out empty, `/auth/permissions/prompt_lib/1` returned `[]`, and the
    # entire UI came up with every create affordance disabled — with `seed`
    # having printed "Seed complete."
    #
    # A second `seed` run fixed it, and this comment used to read that as "the
    # signature of an ordering dependency". It was not. The cause was
    # 001_initial.sql seeding `auth_core__user` at an explicit id 1 without
    # advancing `auth_core__user_id_seq`: the first persona INSERT drew nextval
    # = 1, collided on the primary key, and was swallowed by psql; the second
    # drew 2 and landed. Re-running dragged the sequence past the collision,
    # which is why the bug could only ever be seen on a database created from
    # scratch — i.e. on CI, the first time the job actually ran (issue #154).
    # Fixed at the source in 001_initial.sql; this assertion is what caught it.
    #
    # Assert the end state rather than trusting the exit codes. A stack that
    # cannot authorise anything must fail here, loudly, instead of handing every
    # downstream journey a mystery.
    GRANTS=$($EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -tAc "
      SELECT COUNT(*)
      FROM auth_core__project_user_role pur
      JOIN auth_core__user u ON u.id = pur.user_id
      WHERE pur.project_id = 1
        AND u.email IN ('e2e-admin@autotest.local','e2e-member@autotest.local');")
    PERMS=$($EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -tAc "
      SELECT COUNT(*) FROM auth_core__project_role_permission WHERE project_id = 1;")

    if [ "${GRANTS:-0}" -lt 2 ] || [ "${PERMS:-0}" -lt 1 ]; then
      echo "ERROR: seed did not grant both personas a project role." >&2
      echo "  project_user_role rows for the two personas: ${GRANTS:-0} (want 2)" >&2
      echo "  project_role_permission rows for project 1:  ${PERMS:-0} (want >0)" >&2
      echo "  Without these, /auth/permissions/prompt_lib/1 returns [] and every" >&2
      echo "  create button in the app is permanently disabled." >&2
      exit 1
    fi
    echo "  ✓ RBAC verified: ${GRANTS} persona grant(s), ${PERMS} project permission(s)."

    # Same posture for the administration mode, and now for BOTH sides of it.
    #
    # The grants no longer come from this script — 001_initial.sql seeds them on
    # a fresh database and migrations/shared/0060_admin_central_rbac.sql
    # back-fills an existing one. So this assertion has a second job: if either
    # migration failed to run or failed to seed, `admin.auth.users` resolves
    # nowhere, and since the listing GET is gated the admin Users page is 403
    # for everyone. Asserted as the RESOLVED permission — the exact join
    # legacyrbac.PostgresResolver performs — not as "the rows were inserted".
    resolves_admin_users() {
      $EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -tAc "
        SELECT COUNT(DISTINCT rp.permission)
        FROM auth_core__user u
        JOIN auth_core__user_role ur ON ur.user_id = u.id
        JOIN auth_core__role r ON r.id = ur.role_id AND r.mode = 'administration'
        JOIN auth_core__role_permission rp ON rp.role_id = r.id
        WHERE u.email = '$1'
          AND rp.permission = 'admin.auth.users';"
    }
    ADMIN_PERMS=$(resolves_admin_users 'e2e-admin@autotest.local')
    MEMBER_PERMS=$(resolves_admin_users 'e2e-member@autotest.local')

    if [ "${ADMIN_PERMS:-0}" -lt 1 ]; then
      echo "ERROR: the admin persona does not resolve 'admin.auth.users' in administration mode." >&2
      echo "  Every /admin/auth_users and /admin/user_suspend request — READ and write —" >&2
      echo "  answers 403, so the admin Users page is empty for everyone. Check that" >&2
      echo "  001_initial.sql and shared/0060_admin_central_rbac.sql both applied." >&2
      exit 1
    fi
    # The negative half is what makes the difference a real authorisation
    # boundary. A migration that promoted "everyone who looks like an admin"
    # would hand it to the member persona and quietly delete the distinction the
    # admin journeys rest on — and now that the user LISTING is gated too, J33
    # asserts exactly this refusal over HTTP.
    if [ "${MEMBER_PERMS:-0}" -ne 0 ]; then
      echo "ERROR: the member persona resolves 'admin.auth.users' in administration mode." >&2
      echo "  The two personas must differ in SERVER-SIDE authorisation, not just in" >&2
      echo "  what the UI renders. Something granted the member an administration role." >&2
      exit 1
    fi

    # Same again for the Roles matrix (unit A14). Its READ is gated, so without
    # this grant the page is a 403 and the journey would be asserting against an
    # authorisation failure rather than against the matrix.
    ROLES_GRANT=$($EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -tAc "
      SELECT COUNT(DISTINCT rp.permission)
      FROM auth_core__user u
      JOIN auth_core__user_role ur ON ur.user_id = u.id
      JOIN auth_core__role r ON r.id = ur.role_id AND r.mode = 'administration'
      JOIN auth_core__role_permission rp ON rp.role_id = r.id
      WHERE u.email = 'e2e-admin@autotest.local'
        AND rp.permission IN ('configuration.roles.permissions.view',
                              'configuration.roles.permissions.edit');")
    if [ "${ROLES_GRANT:-0}" -lt 2 ]; then
      echo "ERROR: seed did not grant the admin persona the roles-matrix permissions." >&2
      echo "  resolved: ${ROLES_GRANT:-0} of 2" >&2
      exit 1
    fi

    echo "  ✓ administration RBAC verified: admin persona resolves admin.auth.users"
    echo "    (member does not) and the roles-matrix view/edit pair."

    # The admin PROJECTS surface (unit A14). Its listing is gated, as the user
    # listing now is too, so a missing grant here is a 403 on the page itself
    # rather than a write that quietly fails — but the failure mode is the same
    # class, so it is asserted the same way: as the resolved permission, not as
    # an inserted row.
    PROJECT_PERMS=$($EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -tAc "
      SELECT COUNT(DISTINCT rp.permission)
      FROM auth_core__user u
      JOIN auth_core__user_role ur ON ur.user_id = u.id
      JOIN auth_core__role r ON r.id = ur.role_id AND r.mode = 'administration'
      JOIN auth_core__role_permission rp ON rp.role_id = r.id
      WHERE u.email = 'e2e-admin@autotest.local'
        AND rp.permission IN (
          'projects.projects.projects.view',
          'projects.projects.projects.edit',
          'configuration.users.users.create',
          'configuration.users.users.edit'
        );")
    if [ "${PROJECT_PERMS:-0}" -lt 4 ]; then
      echo "ERROR: seed did not grant the admin persona the four administration-mode" >&2
      echo "  project permissions (got ${PROJECT_PERMS:-0} of 4)." >&2
      echo "  Without them /admin/projects/administration answers 403 and the admin" >&2
      echo "  Projects page renders its load error instead of the table." >&2
      exit 1
    fi

    SEEDED_PROJECTS=$($EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -tAc "
      SELECT COUNT(*) FROM centry.project WHERE id IN (90001, 90002, 90003);")
    if [ "${SEEDED_PROJECTS:-0}" -lt 3 ]; then
      echo "ERROR: seed did not create the three admin-projects fixture rows (got ${SEEDED_PROJECTS:-0})." >&2
      echo "  Journey 31 asserts the team/personal split and the suspended status against them;" >&2
      echo "  with only 'Default Project' present it could assert neither." >&2
      exit 1
    fi
    echo "  ✓ admin projects fixture verified: ${SEEDED_PROJECTS} project(s), ${PROJECT_PERMS}/4 permission(s)."

    # The suspension fixtures (issue #519). Asserted here rather than trusted,
    # for the reason every other block on this page is asserted: journey 28
    # would otherwise report "the row is not on the page", which reads as a
    # broken listing and not as a seed that wrote nothing. Both rows must
    # exist AND both must start Active, because that journey asserts the
    # Active precondition before it suspends.
    SUSPEND_FIXTURES=$($EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -tAc "
      SELECT COUNT(*) FROM auth_core__user
      WHERE email IN (
        'e2e-suspend-chromium@autotest.local',
        'e2e-suspend-webkit@autotest.local'
      ) AND suspended = false;")
    if [ "${SUSPEND_FIXTURES:-0}" -lt 2 ]; then
      echo "ERROR: seed did not leave the two suspension fixture users Active (got ${SUSPEND_FIXTURES:-0} of 2)." >&2
      echo "  Journey 28 suspends one of them. It must not suspend the member persona:" >&2
      echo "  a suspended principal answers 401 for every concurrent journey (issue #519)." >&2
      exit 1
    fi
    echo "  ✓ suspension fixtures verified: ${SUSPEND_FIXTURES}/2 active."

    # ── postcondition: publish the audit fixture's DAY to the test run (#214) ──
    #
    # One zone (above) stops the browser and the database disagreeing about when
    # a day STARTS. It does not stop them disagreeing about WHICH day it is: the
    # seed runs once, at the front of the run, and journey 29 asserts minutes or
    # hours later. A run that seeds at 23:59 and reaches that journey at 00:01
    # has four rows on yesterday and a page filtering on today, which is the
    # failure #214 reports and the one no clamp can reach — the two events are
    # simply on different days.
    #
    # So the seed states which day it wrote, and journey 29 pins its browser
    # clock to it (`page.clock.setFixedTime`). Neither side then reads the wall
    # clock at assertion time, and the window contains the rows by construction.
    #
    # The values are READ BACK from the rows, not recomputed here. A restatement
    # of the same expression would agree with a broken INSERT; this cannot.
    # `.playwright-state/` is where the run already keeps per-run provisioning
    # output (the persona storageState files), it is gitignored, and in CI it is
    # inside the directory the Playwright container mounts at /work. The path is
    # `AUDIT_FIXTURE_ANCHOR` in playwright.config.ts; a shell script cannot
    # import it, so the two must be changed together.
    APP_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
    AUDIT_FIXTURE_FILE="${APP_ROOT}/.playwright-state/audit-fixture.json"
    # Over stdin, not `-c`: psql does NOT interpolate `-v` variables into a `-c`
    # command string. Measured — `-c "… :'e2e_tz' …"` answers
    #   ERROR:  syntax error at or near ":"
    # so the zone would have had to be pasted in by the shell instead.
    AUDIT_FIXTURE=$($EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -tAq \
      -v ON_ERROR_STOP=1 -v e2e_tz="$E2E_TZ" <<'ENDFIXTURE'
SELECT json_build_object(
  'timeZone', :'e2e_tz',
  'rows', COUNT(*),
  'firstRow', MIN(timestamp),
  'lastRow', MAX(timestamp),
  'localDay', to_char(MIN(timestamp) AT TIME ZONE :'e2e_tz', 'YYYY-MM-DD')
)::text
FROM centry.audit_events
WHERE trace_id IN ('e2e-trace-alpha', 'e2e-trace-beta');
ENDFIXTURE
    )
    # `"rows":4` is the whole postcondition. An empty table answers
    # `{"rows":0,"firstRow":null,…}` — valid JSON, and a file journey 29 would
    # read and then fail against for a reason it could not name.
    case "$AUDIT_FIXTURE" in
      *'"rows" : 4'*|*'"rows":4'*) ;;
      *)
        echo "ERROR: the audit trail fixture did not write its four rows." >&2
        echo "  psql answered: ${AUDIT_FIXTURE:-<nothing>}" >&2
        echo "  Journey 29 asserts 2 traces over 4 spans against them; without the rows" >&2
        echo "  the page is correctly empty and the journey cannot tell that from a stub." >&2
        exit 1
        ;;
    esac
    mkdir -p "${APP_ROOT}/.playwright-state"
    printf '%s\n' "$AUDIT_FIXTURE" > "$AUDIT_FIXTURE_FILE"
    echo "  ✓ audit trail fixture verified and published: ${AUDIT_FIXTURE}"

    # ── DeepWiki: one wiki toolkit and one generated wiki (DWIKI-001..003) ───
    #
    # WHY THIS IS SEEDED AND NOT PRODUCED. Nothing in this service creates a
    # wiki toolkit row: `toolkitTypeSchemas` (internal/api/v2/toolkits) is a
    # hand-written map plus the pinned SDK snapshot, and neither carries a
    # provider-supplied type — that is the provider hub, ADR-0012 phase P3. And
    # nothing here can GENERATE a wiki either: this stack runs no DeepWiki
    # provider service, so every invocation would fail at the facade's mTLS hop.
    #
    # So the journey covers what this stack can actually serve — listing and
    # reading a wiki that already exists — and the wiki is put there the way
    # the provider would: objects in the artifact store, under the key layout
    # the provider writes and the browser reads.
    echo "  → Seeding a DeepWiki toolkit and one wiki…"

    # The toolkit. `type = 'wikis'` is the provider's own toolkit name
    # lowercased — the descriptor names it `Wikis` and the SPI addresses it as
    # `wikis` in /tools/{toolkit_name}/{tool_name}/invoke. entities/wiki's
    # WIKI_TOOLKIT_TYPE is the reader that has to agree with this line.
    #
    # `code_toolkit` is deliberately ABSENT. Following that reference needs a
    # github toolkit with a resolvable credential, and this stack has none; the
    # repository is set directly instead, which is the other branch of the same
    # resolver and the one a project without a code toolkit uses.
    $EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -v ON_ERROR_STOP=1 >/dev/null <<'DEEPWIKI_SQL'
-- `owner_id` AND `author_id`, both NOT NULL. Measured against a running stack:
-- omitting owner_id fails with a null-constraint violation naming a column the
-- INSERT never mentioned, because the positional row it prints is the table's
-- order and not the statement's.
--
-- `max_tokens` is NOT seeded. The reason recorded here before is no longer
-- true and must not be copied: it said the read endpoint STRIPS the key,
-- because `isSensitiveSettingKey` matched the substring "token". Issue #705
-- replaced that substring rule with a whole-word rule
-- (services/elitea-main/internal/api/v2/toolkits/secret_settings_key.go), so
-- `max_tokens` and `toolkit_configuration_max_tokens` now survive every read
-- and only a credential is removed.
--
-- The key stays out of this fixture for a different reason:
-- src/widgets/deepwiki/api/wikiChatApi.ts reads it and puts it in
-- `llm_settings.max_tokens`, so seeding it changes the request every DeepWiki
-- journey sends. Seed it in a test that asserts that request, not here.
INSERT INTO p_90200.elitea_tools (id, name, type, description, owner_id, author_id, settings)
VALUES (
    9001,
    'E2E Wiki',
    'wikis',
    'Seeded by scripts/e2e-stack.sh for the DeepWiki journeys',
    90200,
    1,
    '{"repository": "acme/e2e-service", "branch": "main",
      "llm_model": "gpt-4o-mini", "code_toolkit": 9010}'::jsonb
)
ON CONFLICT (id) DO UPDATE
    SET type = EXCLUDED.type, name = EXCLUDED.name, settings = EXCLUDED.settings;

-- A SECOND wiki toolkit, for the journeys that MUTATE: generate, settings,
-- delete. Its repository is different, so the wiki the fixture runner
-- generates for it (acme--e2e-generated--main) never touches the seeded
-- read-only wiki above that DWIKI-001–003 and the quick-fix/edit journeys
-- read — spec files run in any order and in parallel.
INSERT INTO p_90200.elitea_tools (id, name, type, description, owner_id, author_id, settings)
VALUES (
    9002,
    'E2E Generated Wiki',
    'wikis',
    'Seeded by scripts/e2e-stack.sh for the DeepWiki generation journeys',
    90200,
    1,
    '{"repository": "acme/e2e-generated", "branch": "main",
      "llm_model": "gpt-4o-mini", "code_toolkit": 9010}'::jsonb
)
ON CONFLICT (id) DO UPDATE
    SET type = EXCLUDED.type, name = EXCLUDED.name, settings = EXCLUDED.settings;

-- The DeepWiki permissions, on THIS project's roles. Migration 0106 grants
-- them centrally and to every project that had role overrides at migration
-- time; 90200 is created afterwards and copies project 1's overrides, which
-- predate 0106 too — so without these rows every facade call answers 403
-- (the standalone run of DWIKI-005 saw exactly that, on a member who is an
-- editor here). Project 1 gets them for the same reason.
INSERT INTO auth_core__project_role_permission (project_id, role_id, permission)
SELECT r.project_id, r.id, grant_row.permission
FROM auth_core__project_role r
JOIN (VALUES
    ('admin',  'models.applications.deepwiki.read'),
    ('editor', 'models.applications.deepwiki.read'),
    ('viewer', 'models.applications.deepwiki.read'),
    ('admin',  'models.applications.deepwiki.generate'),
    ('editor', 'models.applications.deepwiki.generate')
) AS grant_row(role_name, permission) ON grant_row.role_name = r.name
WHERE r.project_id IN (1, 90200)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

-- The repository toolkit `code_toolkit` names. THE FACADE REQUIRES IT: every
-- generate/ask resolves `configuration.parameters.code_toolkit` — an integer
-- naming a p_<project>.configuration row of a repository type — through the
-- project's vault before the request reaches the provider (credentials.go,
-- ADR-0022 decision 6). A toolkit without one cannot generate at all.
--
-- The token is a literal, not a {{secret.NAME}} reference: the unsecreter
-- leaves plain values as they are, and the fixture runner never clones, so
-- nothing ever presents it to GitHub.
INSERT INTO p_90200.configuration
    (id, project_id, elitea_title, type, section, data, meta, shared, status_ok, source, created_at, updated_at)
VALUES
    (9010, 90200, 'e2e-github', 'github', 'toolkits',
     '{"base_url":"https://api.github.com","access_token":"e2e-not-a-real-token"}',
     '{}', false, true, 'user', NOW(), NOW())
ON CONFLICT (id) DO UPDATE
    SET type = EXCLUDED.type, section = EXCLUDED.section, data = EXCLUDED.data, updated_at = NOW();

-- The project vault the resolver opens to read that row. COPIED from
-- project-1 for the reason the personal-project clone above gives: the
-- blobs are Fernet, psql cannot mint them, and project-1's pair is a valid,
-- consistent, empty vault. A project with NO vault rows fails the whole
-- resolve ("vault that will not open" → ErrCredentialsUnavailable → 503),
-- which on screen is a Generate button that always fails.
INSERT INTO centry.secrets_key (id, data)
SELECT 'project-90200', data FROM centry.secrets_key WHERE id = 'project-1'
ON CONFLICT (id) DO NOTHING;
INSERT INTO centry.secrets_data (id, data)
SELECT 'project-90200', data FROM centry.secrets_data WHERE id = 'project-1'
ON CONFLICT (id) DO NOTHING;

-- The bucket the wiki objects live in. The artifacts surface refuses every
-- object route for a bucket with no row (requireBucket), so the objects below
-- would be invisible without it — a listing that answers 404 and reads on
-- screen as "you have no wikis".
INSERT INTO elitea_storage.buckets (project_id, name, display_name, bucket_type)
VALUES (90200, 'wiki-artifacts', 'Wiki artifacts', 'local')
ON CONFLICT (project_id, name) WHERE deleted_at IS NULL DO NOTHING;
DEEPWIKI_SQL

    # The wiki objects, written straight into the object store.
    #
    # The physical layout is elitea-main's, not an invention:
    # `[prefix/]p/{project}/b/{bucket}/o/{key}` (internal/infra/storage/ref.go),
    # and this stack sets no STORAGE_KEY_PREFIX. The LISTING reads the store
    # directly (`h.store.List`) rather than elitea_storage.objects, so objects
    # placed here are listed; the bucket row above is what admits the request.
    WIKI_ID="acme--e2e-service--main"
    WIKI_TMP="$(mktemp -d)"
    mkdir -p "${WIKI_TMP}/${WIKI_ID}/wiki_pages/architecture"
    cat > "${WIKI_TMP}/${WIKI_ID}/wiki_manifest_1.json" <<WIKI_MANIFEST
{
  "schema_version": 1,
  "wiki_id": "${WIKI_ID}",
  "wiki_title": "E2E Service Wiki",
  "description": "A wiki seeded for the DeepWiki journeys.",
  "wiki_version_id": "1",
  "created_at": "2026-01-01T00:00:00Z",
  "canonical_repo_identifier": "acme/e2e-service",
  "repository": "acme/e2e-service",
  "branch": "main",
  "provider_type": "github",
  "pages": ["wiki_pages/architecture/router.md", "wiki_pages/architecture/request-flow.md"]
}
WIKI_MANIFEST
    # A page whose diagram does not parse: what the mermaid quick-fix journey
    # (DWIKI-007/008) repairs, saves over, and undoes — against a wiki that
    # exists before any generation has run on this stack.
    cat > "${WIKI_TMP}/${WIKI_ID}/wiki_pages/architecture/request-flow.md" <<'WIKI_PAGE'
# Request flow

The diagram below is deliberately broken. It exists so the quick fix has
something to repair.

```mermaid
graph TD
  A[Client] -->
```

After the diagram.
WIKI_PAGE
    cat > "${WIKI_TMP}/${WIKI_ID}/wiki_pages/architecture/router.md" <<'WIKI_PAGE'
# Router

The HTTP router is assembled in `internal/api/router.go`.

## Notes

This page is a seeded fixture. It exists so the DeepWiki journey can read a
wiki page that a provider would have written, on a stack that runs no provider.
WIKI_PAGE

    # `mc` is the tool the compose file already uses to create the S3 bucket
    # (rustfs-bucket-init), so this adds no new dependency to the stack.
    $EXEC_BIN run --rm --network "${E2E_PROJECT}_default" \
      -v "${WIKI_TMP}:/wiki:ro" \
      --entrypoint sh docker.io/minio/mc:latest -c "
        mc alias set rustfs http://rustfs:9000 elitea elitea-dev-secret >/dev/null &&
        mc cp --recursive /wiki/${WIKI_ID} rustfs/elitea-artifacts/p/90200/b/wiki-artifacts/o/ >/dev/null
      " || {
        echo "ERROR: could not write the seeded wiki objects into rustfs." >&2
        echo "  The DeepWiki journey would then find an empty bucket, which is" >&2
        echo "  indistinguishable on screen from a project with no wikis." >&2
        rm -rf "$WIKI_TMP"
        exit 1
      }
    rm -rf "$WIKI_TMP"

    # ── postcondition: the objects are READABLE through the API's own layout ──
    #
    # Not "mc reported success": a copy into the wrong prefix succeeds and
    # leaves the browser empty. This lists the exact prefix elitea-main derives
    # for (project 1, bucket wiki-artifacts) and requires the manifest in it.
    WIKI_OBJECTS=$($EXEC_BIN run --rm --network "${E2E_PROJECT}_default" \
      --entrypoint sh docker.io/minio/mc:latest -c "
        mc alias set rustfs http://rustfs:9000 elitea elitea-dev-secret >/dev/null &&
        mc ls --recursive rustfs/elitea-artifacts/p/90200/b/wiki-artifacts/o/ 2>/dev/null
      " || true)
    case "$WIKI_OBJECTS" in
      *wiki_manifest_1.json*) ;;
      *)
        echo "ERROR: the seeded wiki manifest is not under the prefix elitea-main reads." >&2
        echo "  Expected p/90200/b/wiki-artifacts/o/${WIKI_ID}/wiki_manifest_1.json" >&2
        echo "  mc listed: ${WIKI_OBJECTS:-<nothing>}" >&2
        exit 1
        ;;
    esac
    echo "  ✓ DeepWiki project 90200, toolkit 9001 and wiki ${WIKI_ID} seeded"

    # ── Inventory: two toolkits and one source (INV-001..010) ────────────────
    #
    # NOTHING IS PRE-SEEDED INTO THE BUCKET, and that is the difference from
    # the DeepWiki block above. A wiki is READ AS OBJECTS — the browser fetches
    # `wiki_manifest_1.json` through the artifacts API — so a stack with no
    # provider still needs the objects put there by hand. Every Inventory read
    # is an INVOCATION instead (`get_stats`, `search_graph`, `get_entity`, …),
    # and this stack DOES run a provider: `elitea-inventory` with
    # `ELITEA_INVENTORY_RUNNER=fixture`, whose canned graph
    # (services/elitea-subapp-host/internal/apps/inventory/run/fixture.go) is
    # what answers. So the browsing journeys need no graph in the bucket: the
    # six entities and five relations they assert on come from the runner.
    #
    # The bucket rows below are still required — `run_ingestion` UPLOADS
    # graph.json, sources_status.json and the checkpoint through the artifacts
    # surface, and `requireBucket` 404s every object route for a bucket with no
    # row (the same trap the DeepWiki block records).
    echo "  → Seeding two Inventory toolkits and their source…"

    $EXEC_BIN exec -i "$POSTGRES_CONTAINER" psql -U elitea -d elitea -v ON_ERROR_STOP=1 >/dev/null <<'INVENTORY_SQL'
-- `type = 'inventory'` is the provider's own toolkit name lowercased, the way
-- the SPI addresses it in /tools/{toolkit_name}/{tool_name}/invoke. It is also
-- what `entities/inventory`'s INVENTORY_TOOLKIT_TYPE filters the project's
-- toolkit listing by, so a different spelling here empties `/inventory`.
--
-- `inventory_search` is deliberately NOT seeded as a second toolkit row. That
-- family is served by the SAME toolkit — the ask drawer sends `investigate` to
-- family `inventory_search` for toolkit 9101 — and a row of that type would
-- only add an entry to the chooser that owns no graph.
--
-- `owner_id` AND `author_id`, both NOT NULL: see the DeepWiki block's note.
INSERT INTO p_90300.elitea_tools (id, name, type, description, owner_id, author_id, settings)
VALUES (
    9101,
    'E2E Inventory',
    'inventory',
    'Seeded by scripts/e2e-stack.sh for the Inventory browsing journeys',
    90300,
    1,
    '{"bucket": "inventory-graph", "llm_model": "gpt-4o-mini",
      "sources": [9110], "source_configs": {"9110": {"branch": "main"}}}'::jsonb
)
ON CONFLICT (id) DO UPDATE
    SET type = EXCLUDED.type, name = EXCLUDED.name, settings = EXCLUDED.settings;

-- The MUTABLE one, for INV-009. Its bucket differs from 9101's so the objects
-- an ingestion lands can never overwrite anything a browsing journey reads —
-- spec files run in any order and in parallel, the same rule 9002 follows for
-- DeepWiki.
INSERT INTO p_90300.elitea_tools (id, name, type, description, owner_id, author_id, settings)
VALUES (
    9102,
    'E2E Inventory Mutable',
    'inventory',
    'Seeded by scripts/e2e-stack.sh for the Inventory ingestion journey',
    90300,
    1,
    '{"bucket": "inventory-graph-mutable", "llm_model": "gpt-4o-mini",
      "sources": [9110], "source_configs": {"9110": {"branch": "main"}}}'::jsonb
)
ON CONFLICT (id) DO UPDATE
    SET type = EXCLUDED.type, name = EXCLUDED.name, settings = EXCLUDED.settings;

-- The Inventory permissions, on THIS project's roles. Migration 0108 grants
-- them centrally and to every project that had role overrides at migration
-- time; 90300 is created afterwards and copies project 1's overrides, which
-- predate 0108 too — so without these rows every facade call answers 403.
-- Project 1 gets them for the same reason. The read/invoke split is 0108's:
-- read for admin+editor+viewer, invoke for admin+editor.
INSERT INTO auth_core__project_role_permission (project_id, role_id, permission)
SELECT r.project_id, r.id, grant_row.permission
FROM auth_core__project_role r
JOIN (VALUES
    ('admin',  'models.applications.inventory.read'),
    ('editor', 'models.applications.inventory.read'),
    ('viewer', 'models.applications.inventory.read'),
    ('admin',  'models.applications.inventory.invoke'),
    ('editor', 'models.applications.inventory.invoke')
) AS grant_row(role_name, permission) ON grant_row.role_name = r.name
WHERE r.project_id IN (1, 90300)
ON CONFLICT (project_id, role_id, permission) DO NOTHING;

-- The source toolkit both Inventory toolkits name in `sources`. THE FACADE
-- REQUIRES IT: `run_ingestion` is one of the three ExpandingTools, and the
-- expander resolves `toolkit_id: 9110` against a p_90300.configuration row of
-- an admitted type through the project's vault before the request reaches the
-- provider (internal/api/v2/inventory/sources.go). An id naming no row is
-- refused with 403 "not one of this Inventory toolkit's configured sources".
--
-- `github`, because SourceKinds wires exactly two types and its DefaultHost
-- (https://api.github.com) is what the stack's ELITEA_INVENTORY_GIT_ALLOWLIST
-- (`github.com,*.github.com`) admits. A type outside that pair, or a base_url
-- outside that allowlist, turns INV-009 into an egress refusal.
--
-- The token is a literal rather than a {{secret.NAME}} reference, for the
-- reason the DeepWiki row gives: the unsecreter leaves plain values alone, and
-- the fixture runner never clones, so nothing presents it to GitHub.
INSERT INTO p_90300.configuration
    (id, project_id, elitea_title, type, section, data, meta, shared, status_ok, source, created_at, updated_at)
VALUES
    (9110, 90300, 'e2e-inventory-source', 'github', 'toolkits',
     '{"base_url":"https://api.github.com","access_token":"e2e-not-a-real-token"}',
     '{}', false, true, 'user', NOW(), NOW())
ON CONFLICT (id) DO UPDATE
    SET type = EXCLUDED.type, section = EXCLUDED.section, data = EXCLUDED.data, updated_at = NOW();

-- The project vault the resolver opens to read that row. Copied from
-- project-1: the blobs are Fernet, psql cannot mint them, and project-1's pair
-- is a valid, consistent, empty vault. A project with NO vault rows fails the
-- whole resolve (ErrCredentialsUnavailable → 503), which on screen is a "Run
-- ingestion" button that always fails.
INSERT INTO centry.secrets_key (id, data)
SELECT 'project-90300', data FROM centry.secrets_key WHERE id = 'project-1'
ON CONFLICT (id) DO NOTHING;
INSERT INTO centry.secrets_data (id, data)
SELECT 'project-90300', data FROM centry.secrets_data WHERE id = 'project-1'
ON CONFLICT (id) DO NOTHING;

-- One bucket per toolkit. `requireBucket` refuses every object route for a
-- bucket with no row, so an ingestion would run, compose its three artifacts
-- and fail on the upload — a run that streams six progress lines and then
-- reports a transport error.
INSERT INTO elitea_storage.buckets (project_id, name, display_name, bucket_type)
VALUES
    (90300, 'inventory-graph', 'Inventory graph', 'local'),
    (90300, 'inventory-graph-mutable', 'Inventory graph (mutable)', 'local')
ON CONFLICT (project_id, name) WHERE deleted_at IS NULL DO NOTHING;

-- ── postcondition, in SQL, because every one of these is silent on screen ──
--
-- A missing permission is a 403 the workspace renders as "this toolkit could
-- not be loaded"; a missing bucket is a 404 the ingestion reports as a
-- transport error; a missing source row is a 403 on the run. None of them can
-- be told apart from the other by a journey, so they are asserted here where
-- the failure names itself.
DO $inventory$
DECLARE
    toolkits INTEGER;
    grants INTEGER;
    buckets INTEGER;
    sources INTEGER;
    vaults INTEGER;
BEGIN
    SELECT count(*) INTO toolkits
      FROM p_90300.elitea_tools WHERE id IN (9101, 9102) AND type = 'inventory';
    SELECT count(*) INTO grants
      FROM auth_core__project_role_permission
     WHERE project_id = 90300 AND permission LIKE 'models.applications.inventory.%';
    SELECT count(*) INTO buckets
      FROM elitea_storage.buckets
     WHERE project_id = 90300 AND name IN ('inventory-graph', 'inventory-graph-mutable');
    SELECT count(*) INTO sources
      FROM p_90300.configuration WHERE id = 9110 AND type = 'github';
    SELECT count(*) INTO vaults
      FROM centry.secrets_key WHERE id = 'project-90300';

    IF toolkits <> 2 OR grants < 5 OR buckets <> 2 OR sources <> 1 OR vaults <> 1 THEN
        RAISE EXCEPTION
            'Inventory seed incomplete: % toolkits (want 2), % grants (want >=5), % buckets (want 2), % sources (want 1), % vaults (want 1)',
            toolkits, grants, buckets, sources, vaults;
    END IF;
END
$inventory$;
INVENTORY_SQL

    echo "  ✓ Inventory project 90300, toolkits 9101/9102 and source 9110 seeded"
    echo "    (no graph objects: the fixture runner answers every read from its canned graph)"

    echo "→ Seed complete."
    ;;

  logs)
    # Every service's log, for a failure that only the SERVER can explain.
    #
    # WHY THIS EXISTS. A journey failed with "Failed to load users." on the
    # screen after a write, and the job captured a screenshot, a video and an
    # accessibility tree — none of which can say whether the listing answered
    # 500, timed out, or was refused. The stack was torn down in the next step
    # and the answer went with it. A failure nobody can diagnose gets retried
    # until it passes, which is how a real defect becomes "flaky".
    #
    # Written to a FILE rather than the job log: `docker compose logs` for a
    # nine-service stack is tens of thousands of lines, and burying the
    # Playwright output under it costs more than it gives. The file is uploaded
    # beside the report.
    #
    # NEVER FATAL. This runs on the failure path, and a diagnostic that can fail
    # the job it is diagnosing would replace one unexplained failure with
    # another. Every branch ends in `|| true`.
    # APP_ROOT is set inside the `seed` branch, so it is derived here rather
    # than borrowed: a default that silently resolved to "." would write the
    # file next to wherever the caller happened to stand.
    LOGS_APP_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
    OUT="${2:-${LOGS_APP_ROOT}/playwright-results/stack-logs.txt}"
    mkdir -p "$(dirname "$OUT")" 2>/dev/null || true
    echo "→ Collecting stack logs into ${OUT}…"
    {
      echo "=== compose ps ==="
      $COMPOSE_BIN $COMPOSE_F ps 2>&1 || true
      echo
      echo "=== compose logs (--no-color, timestamps) ==="
      $COMPOSE_BIN $COMPOSE_F logs --no-color --timestamps 2>&1 || true
    } > "$OUT" 2>&1 || true
    # The count is printed so a caller can tell "collected nothing" from
    # "collected and there was nothing wrong" — an empty file that nobody
    # noticed is the same dead end this command exists to remove.
    echo "  ✓ $(wc -l < "$OUT" 2>/dev/null || echo 0) line(s) collected"
    ;;

  *)
    echo "Usage: $0 {up|down|seed|logs [outfile]}" >&2
    exit 1
    ;;
esac
