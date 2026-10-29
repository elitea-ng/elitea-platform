-- 0203_release_gates.sql — the evaluation release gate (ADR-0040
-- decision 4, issue #1497) and its scheduled re-evaluation (decision 5).
--
--   elitea_lifecycle.release_gate_suites
--       One row per gate suite: an evaluation dataset of an agent marked
--       `release_gate`. `criteria` is the pass criterion per dimension, an
--       array of {dimension_id, kind, min_score | max_drop}: kind
--       `threshold` needs the dimension's mean normalised score (0-100) to
--       be at least min_score; kind `no_worse_than` needs it to be no more
--       than max_drop points below the current release's. One suite per
--       agent may be the regression suite: it also runs against the
--       current release, and a case that passed there and fails on the
--       candidate fails the gate. `reevaluate_every_days`, when set, makes
--       the scheduled re-evaluation run the agent's gate suites against
--       its current release. No row = not a gate suite.
--
--   elitea_lifecycle.release_gates
--       One row per gate: kind `change` (a candidate version against the
--       current release) or `scheduled` (the current release against
--       itself, the drift signal). `suites` freezes the suite rows the gate
--       started with. The row is written before any run starts;
--       `runs_started_at` is NULL until every run of the gate is started
--       and recorded, and a gate is not evaluated before then (a start
--       that failed settles it failed, gate_start_failed). The gate is
--       `pending` until every run is terminal,
--       then `passed` or `failed` with `outcome` (per suite and dimension)
--       and `failures` (typed codes). An override records who accepted a
--       failed gate and why; it does not change the state. A trigger
--       refuses any change to a settled gate's outcome and any second
--       override.
--
--   elitea_lifecycle.release_gate_runs
--       The evaluation runs of a gate: one per suite and role (`candidate`
--       or `baseline`). eval_run_id names a row of the tenant's eval_runs.
--       `reused` marks a baseline run that an earlier gate started (a
--       scheduled gate compares with the reference run of its release).
--
-- Application, version, dataset and run ids are per tenant schema, so
-- every key carries tenant_project_id. The rows stay when an application
-- or a dataset is deleted: they are the evidence of what was tested. No
-- new permission: the routes reuse the evaluation and publish permissions.
-- Additive only. Idempotent. No BEGIN/COMMIT: the ledgered runner wraps the
-- file.

CREATE SCHEMA IF NOT EXISTS elitea_lifecycle;

CREATE TABLE IF NOT EXISTS elitea_lifecycle.release_gate_suites (
    tenant_project_id      bigint NOT NULL CHECK (tenant_project_id > 0),
    application_id         bigint NOT NULL CHECK (application_id > 0),
    dataset_id             bigint NOT NULL CHECK (dataset_id > 0),
    regression             boolean NOT NULL DEFAULT false,
    criteria               jsonb NOT NULL CHECK (
                               jsonb_typeof(criteria) = 'array'
                               AND jsonb_array_length(criteria) BETWEEN 1 AND 5
                           ),
    reevaluate_every_days  integer CHECK (
                               reevaluate_every_days IS NULL
                               OR reevaluate_every_days BETWEEN 1 AND 365
                           ),
    updated_by             bigint CHECK (updated_by IS NULL OR updated_by > 0),
    updated_at             timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_project_id, application_id, dataset_id)
);

-- One regression suite per agent.
CREATE UNIQUE INDEX IF NOT EXISTS release_gate_suites_one_regression
    ON elitea_lifecycle.release_gate_suites (tenant_project_id, application_id)
    WHERE regression;

CREATE TABLE IF NOT EXISTS elitea_lifecycle.release_gates (
    id                    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_project_id     bigint NOT NULL CHECK (tenant_project_id > 0),
    application_id        bigint NOT NULL CHECK (application_id > 0),
    kind                  text NOT NULL CHECK (kind IN ('change', 'scheduled')),
    candidate_version_id  bigint NOT NULL CHECK (candidate_version_id > 0),
    current_release_id    bigint,
    baseline_version_id   bigint CHECK (baseline_version_id IS NULL OR baseline_version_id > 0),
    state                 text NOT NULL DEFAULT 'pending'
                          CHECK (state IN ('pending', 'passed', 'failed')),
    suites                jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(suites) = 'array'),
    outcome               jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(outcome) = 'array'),
    failures              jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(failures) = 'array'),
    created_by            bigint CHECK (created_by IS NULL OR created_by > 0),
    created_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    runs_started_at       timestamptz,
    evaluated_at          timestamptz,
    overridden_by         bigint CHECK (overridden_by IS NULL OR overridden_by > 0),
    overridden_at         timestamptz,
    override_reason       text CHECK (override_reason IS NULL OR length(override_reason) <= 200),
    CONSTRAINT release_gates_current_release_same_application
        FOREIGN KEY (current_release_id, tenant_project_id, application_id)
        REFERENCES elitea_lifecycle.application_releases (id, tenant_project_id, application_id),
    CONSTRAINT release_gates_scheduled_has_release
        CHECK (kind <> 'scheduled' OR current_release_id IS NOT NULL),
    CONSTRAINT release_gates_settled_stamp
        CHECK ((state = 'pending') = (evaluated_at IS NULL)),
    CONSTRAINT release_gates_settled_started
        CHECK (state = 'pending' OR runs_started_at IS NOT NULL),
    CONSTRAINT release_gates_failed_has_failures
        CHECK (state <> 'failed' OR jsonb_array_length(failures) > 0),
    CONSTRAINT release_gates_override_complete CHECK (
        (overridden_by IS NULL AND overridden_at IS NULL AND override_reason IS NULL)
        OR (overridden_by IS NOT NULL AND overridden_at IS NOT NULL
            AND state = 'failed' AND length(btrim(override_reason)) > 0)
    )
);

CREATE INDEX IF NOT EXISTS release_gates_application
    ON elitea_lifecycle.release_gates (tenant_project_id, application_id, id DESC);

CREATE INDEX IF NOT EXISTS release_gates_pending
    ON elitea_lifecycle.release_gates (id)
    WHERE state = 'pending';

CREATE INDEX IF NOT EXISTS release_gates_scheduled
    ON elitea_lifecycle.release_gates (tenant_project_id, application_id, current_release_id, created_at DESC)
    WHERE kind = 'scheduled';

-- A gate's identity never changes; its outcome is written once (pending →
-- settled) and its override once.
CREATE OR REPLACE FUNCTION elitea_lifecycle.release_gates_guard()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.tenant_project_id IS DISTINCT FROM OLD.tenant_project_id
       OR NEW.application_id IS DISTINCT FROM OLD.application_id
       OR NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.candidate_version_id IS DISTINCT FROM OLD.candidate_version_id
       OR NEW.current_release_id IS DISTINCT FROM OLD.current_release_id
       OR NEW.baseline_version_id IS DISTINCT FROM OLD.baseline_version_id
       OR NEW.suites IS DISTINCT FROM OLD.suites
       OR NEW.created_by IS DISTINCT FROM OLD.created_by
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'a release gate''s identity is immutable'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state <> 'pending'
       AND (NEW.state IS DISTINCT FROM OLD.state
            OR NEW.outcome IS DISTINCT FROM OLD.outcome
            OR NEW.failures IS DISTINCT FROM OLD.failures
            OR NEW.evaluated_at IS DISTINCT FROM OLD.evaluated_at) THEN
        RAISE EXCEPTION 'a settled release gate''s outcome is immutable'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.overridden_by IS NOT NULL
       AND (NEW.overridden_by IS DISTINCT FROM OLD.overridden_by
            OR NEW.overridden_at IS DISTINCT FROM OLD.overridden_at
            OR NEW.override_reason IS DISTINCT FROM OLD.override_reason) THEN
        RAISE EXCEPTION 'a release gate override is recorded once'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS release_gates_guard ON elitea_lifecycle.release_gates;
CREATE TRIGGER release_gates_guard
    BEFORE UPDATE ON elitea_lifecycle.release_gates
    FOR EACH ROW EXECUTE FUNCTION elitea_lifecycle.release_gates_guard();

CREATE TABLE IF NOT EXISTS elitea_lifecycle.release_gate_runs (
    gate_id                 bigint NOT NULL
                            REFERENCES elitea_lifecycle.release_gates (id) ON DELETE CASCADE,
    dataset_id              bigint NOT NULL CHECK (dataset_id > 0),
    role                    text NOT NULL CHECK (role IN ('candidate', 'baseline')),
    application_version_id  bigint CHECK (application_version_id IS NULL OR application_version_id > 0),
    eval_run_id             bigint NOT NULL CHECK (eval_run_id > 0),
    reused                  boolean NOT NULL DEFAULT false,
    PRIMARY KEY (gate_id, dataset_id, role)
);
