-- 0182_secret_write_ledger.sql — the staged-write reconciliation ledger of
-- the governed SecretStore seam (ADR-0039, ADR-0011 amendment, issue #1113).
--
-- A governed credential write spans two systems: this database and a secret
-- backend (the PostgreSQL vault, or Vault/OpenBao KV v2). There is no
-- transaction across them, so a write first records a `staged` row, then
-- writes the backend, then marks the row `committed`. A definitive backend
-- refusal (version conflict, denied, misconfigured) marks it `abandoned`. A
-- crash or an unknown outcome leaves it `staged`; the sweeper
-- (secretstore.Ledger.SweepStale) later marks it `abandoned` and retires the
-- backend version, unless a committed row already owns that version. It
-- retires only on a backend whose versions are each created by one
-- check-and-set write (Vault/OpenBao KV v2). A pgvault row is abandoned
-- without a retire: the vault removes by name, so the sweeper cannot prove
-- the value is the one this write created.
--
-- created_version is set once, on a staged row, when the backend accepted a
-- write but created a version other than intended_version. The sweeper then
-- retires created_version, the version that exists.
--
-- The row holds the locator (mount and path) and the version, never a value.
-- The StoreContext fields the sweeper needs to retire a version (tenant,
-- workload scope, policy revision) are on the row so it never guesses them.
--
-- With ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED off nothing writes
-- here (flag-off contract item 5). Additive only; no data.
-- Idempotent throughout. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.secret_write_ledger (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    credential_ref_id   text NOT NULL CHECK (length(credential_ref_id) BETWEEN 1 AND 128),
    tenant_project_id   bigint NOT NULL CHECK (tenant_project_id > 0),
    workload_scope      text NOT NULL CHECK (workload_scope ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
    policy_revision_id  text NOT NULL CHECK (length(policy_revision_id) BETWEEN 1 AND 128),
    backend             text NOT NULL CHECK (backend IN ('pgvault', 'vault')),
    locator_mount       text NOT NULL CHECK (length(locator_mount) <= 256),
    locator_path        text NOT NULL CHECK (length(locator_path) BETWEEN 1 AND 512),
    intended_version    bigint NOT NULL CHECK (intended_version > 0),
    created_version     bigint CHECK (created_version > 0),
    state               text NOT NULL DEFAULT 'staged'
                            CHECK (state IN ('staged', 'committed', 'abandoned')),
    created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    state_changed_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- A committed row owns intended_version; a row whose write created
    -- another version is never committed.
    CHECK (created_version IS NULL OR state <> 'committed')
);

-- One backend version is owned by at most one committed write.
CREATE UNIQUE INDEX IF NOT EXISTS secret_write_ledger_committed_version
    ON elitea_identity.secret_write_ledger (backend, locator_mount, locator_path, intended_version)
    WHERE state = 'committed';

-- The sweeper's scan: oldest staged rows first.
CREATE INDEX IF NOT EXISTS secret_write_ledger_staged
    ON elitea_identity.secret_write_ledger (created_at)
    WHERE state = 'staged';

-- A row leaves `staged` once and never returns to it, and its identity
-- columns never change. The one staged-to-staged update is recording
-- created_version, once, from NULL.
CREATE OR REPLACE FUNCTION elitea_identity.secret_write_ledger_transition()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state <> 'staged'
        OR (NEW.state = 'staged'
            AND NOT (OLD.created_version IS NULL AND NEW.created_version IS NOT NULL)) THEN
        RAISE EXCEPTION 'secret write ledger row % cannot move from % to %', OLD.id, OLD.state, NEW.state;
    END IF;
    IF OLD.created_version IS NOT NULL
        AND NEW.created_version IS DISTINCT FROM OLD.created_version THEN
        RAISE EXCEPTION 'secret write ledger row % created version is immutable', OLD.id;
    END IF;
    IF NEW.credential_ref_id IS DISTINCT FROM OLD.credential_ref_id
        OR NEW.tenant_project_id IS DISTINCT FROM OLD.tenant_project_id
        OR NEW.workload_scope IS DISTINCT FROM OLD.workload_scope
        OR NEW.policy_revision_id IS DISTINCT FROM OLD.policy_revision_id
        OR NEW.backend IS DISTINCT FROM OLD.backend
        OR NEW.locator_mount IS DISTINCT FROM OLD.locator_mount
        OR NEW.locator_path IS DISTINCT FROM OLD.locator_path
        OR NEW.intended_version IS DISTINCT FROM OLD.intended_version
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'secret write ledger row % identity is immutable', OLD.id;
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS secret_write_ledger_transition
    ON elitea_identity.secret_write_ledger;
CREATE TRIGGER secret_write_ledger_transition
    BEFORE UPDATE ON elitea_identity.secret_write_ledger
    FOR EACH ROW EXECUTE FUNCTION elitea_identity.secret_write_ledger_transition();
