-- 0212_run_caps.sql — per-run caps by risk tier and the cap override
-- (ADR-0041 decision 3, issue #1498).
--
--   elitea_runtime.execution_jobs.run_caps
--       The effective caps of an agent run, written when Main prepares its
--       worker command: the root agent's tier caps (Configuration › Run
--       caps) lowered by its author's per-agent caps, and raised by an
--       override grant the run consumed. A JSON object:
--       {"max_tool_calls": n, "max_model_turns": n, "max_records_read": n,
--        "max_spend": usd, "override": "<cap>"|null}; 0 is no cap. It is
--       the evidence of what bounded the run, and the spend routine reads
--       max_spend from it. A redispatch (a new generation of the same
--       execution) reuses the first snapshot, so a grant is consumed once.
--       NULL for a run admitted before 0212 and for a run kind with no caps.
--
--   elitea_runtime.execution_jobs.cancel_cap
--       The cap of a run Main cancelled with cancel_reason cap_reached:
--       max_wall_seconds (the lease renewal check) or max_spend (the spend
--       routine). NULL on a cap_reached row written before 0212, which is
--       always the wall cap.
--
--   elitea_runtime.run_cap_overrides
--       One row per override: a holder of safety.override_cap lets the
--       conversation of a response that ended with CAP_REACHED continue
--       past that cap once. The NEXT agent run of the conversation consumes
--       the grant (consumed_execution_id) and runs with that cap doubled; a
--       grant not consumed within an hour expires. At most one grant per
--       stopped response (the unique index) and one open grant per
--       conversation. Rows are never deleted with the run: they are the
--       evidence of who let a run pass a cap. Each grant is also a security
--       audit event (ai.elitea.security.v1.agent.cap.override).
--
-- Permission: safety.override_cap goes to the platform administrators
-- (administration mode: super_admin, admin) and to the project admin
-- (default mode: admin). The route is project-scoped. 0060 returns early on
-- an existing database, so internal/infra/db/migrations/001_initial.sql
-- seeds the same grants on a fresh one.
--
-- Additive only. Idempotent. Every statement tolerates a database without
-- the auth_core tables. No BEGIN/COMMIT: the ledgered runner wraps the file.

ALTER TABLE elitea_runtime.execution_jobs
    ADD COLUMN IF NOT EXISTS run_caps jsonb,
    ADD COLUMN IF NOT EXISTS cancel_cap text;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'execution_jobs_run_caps_object'
          AND conrelid = 'elitea_runtime.execution_jobs'::regclass
    ) THEN
        ALTER TABLE elitea_runtime.execution_jobs
            ADD CONSTRAINT execution_jobs_run_caps_object
            CHECK (run_caps IS NULL OR jsonb_typeof(run_caps) = 'object');
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'execution_jobs_cancel_cap_known'
          AND conrelid = 'elitea_runtime.execution_jobs'::regclass
    ) THEN
        ALTER TABLE elitea_runtime.execution_jobs
            ADD CONSTRAINT execution_jobs_cancel_cap_known
            CHECK (cancel_cap IS NULL OR cancel_cap IN ('max_wall_seconds', 'max_spend'));
    END IF;
END
$$;

-- The spend routine reads the running agent runs that have a spend cap.
CREATE INDEX IF NOT EXISTS execution_jobs_running_spend_cap
    ON elitea_runtime.execution_jobs (resource_project_id)
    WHERE desired_state = 'RUNNING'
      AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')
      AND run_caps IS NOT NULL;

CREATE TABLE IF NOT EXISTS elitea_runtime.run_cap_overrides (
    id                    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id            integer NOT NULL CHECK (project_id > 0),
    conversation_uuid     text NOT NULL CHECK (length(conversation_uuid) BETWEEN 1 AND 64),
    source_message_id     text NOT NULL CHECK (length(source_message_id) BETWEEN 1 AND 64),
    source_execution_id   text CHECK (source_execution_id IS NULL
                                      OR length(source_execution_id) BETWEEN 1 AND 128),
    cap                   text NOT NULL CHECK (cap IN (
                              'max_wall_seconds', 'max_tool_calls', 'max_model_turns',
                              'max_spend', 'max_records_read')),
    granted_by            bigint NOT NULL CHECK (granted_by > 0),
    granted_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at            timestamptz NOT NULL,
    consumed_execution_id text CHECK (consumed_execution_id IS NULL
                                      OR length(consumed_execution_id) BETWEEN 1 AND 128),
    consumed_at           timestamptz,
    CONSTRAINT run_cap_overrides_consumed
        CHECK ((consumed_execution_id IS NULL) = (consumed_at IS NULL)),
    CONSTRAINT run_cap_overrides_expiry CHECK (expires_at > granted_at)
);

-- Once per stopped response.
CREATE UNIQUE INDEX IF NOT EXISTS run_cap_overrides_source
    ON elitea_runtime.run_cap_overrides (project_id, source_message_id);

-- One open grant per conversation: the next run takes it.
CREATE UNIQUE INDEX IF NOT EXISTS run_cap_overrides_open
    ON elitea_runtime.run_cap_overrides (project_id, conversation_uuid)
    WHERE consumed_execution_id IS NULL;

-- The run that consumed a grant.
CREATE INDEX IF NOT EXISTS run_cap_overrides_consumed_execution
    ON elitea_runtime.run_cap_overrides (consumed_execution_id)
    WHERE consumed_execution_id IS NOT NULL;

DO $$
BEGIN
    IF to_regclass('public.auth_core__role') IS NULL
       OR to_regclass('public.auth_core__role_permission') IS NULL THEN
        RAISE NOTICE '0212: auth_core tables absent, nothing to grant';
        RETURN;
    END IF;

    -- The mode spelled as a literal, one per INSERT: the router's grant
    -- gate (router_permission_grant_gate_test.go) reads the grants of this
    -- directory per `mode = '...'` block.
    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'safety.override_cap'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'administration' AND role.name IN ('super_admin', 'admin')
    ON CONFLICT (role_id, permission) DO NOTHING;

    INSERT INTO public.auth_core__role_permission (role_id, permission)
    SELECT role.id, 'safety.override_cap'
    FROM public.auth_core__role AS role
    WHERE role.mode = 'default' AND role.name = 'admin'
    ON CONFLICT (role_id, permission) DO NOTHING;

    -- legacyrbac reads the central grants only for a caller with no
    -- per-project rows (shared/0090). A project whose matrix was saved gets
    -- the string on its `admin` role here, as 0194 does for safety.suspend.
    -- The string never existed, so no saved matrix withheld it on purpose.
    IF to_regclass('public.auth_core__project_role') IS NULL
       OR to_regclass('public.auth_core__project_role_permission') IS NULL THEN
        RETURN;
    END IF;

    INSERT INTO public.auth_core__project_role_permission (project_id, role_id, permission)
    SELECT DISTINCT saved.project_id, saved.role_id, 'safety.override_cap'
    FROM (
        SELECT DISTINCT project_id, role_id
        FROM public.auth_core__project_role_permission
        WHERE role_id IS NOT NULL
    ) AS saved
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = saved.role_id
     AND project_role.project_id = saved.project_id
    WHERE project_role.name = 'admin'
    ON CONFLICT (project_id, role_id, permission) DO NOTHING;
END
$$;
