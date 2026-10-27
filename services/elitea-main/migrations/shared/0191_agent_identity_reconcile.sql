-- 0191_agent_identity_reconcile.sql — agent identity reconciliation and drift
-- suspension (ADR-0039 decision 11, issue #1154).
--
--   agent_identity_provider_health  one row per identity provider the
--                                   reconcile loop runs for: the pass lease
--                                   (one replica per provider per interval)
--                                   and the outage state. admission_denied is
--                                   what every replica's agent session open
--                                   reads: true from the policy's threshold of
--                                   consecutive failed passes until the next
--                                   successful one (fail closed).
--   external_identity_drift_findings
--                                   the findings a pass recorded: an
--                                   out-of-band provider change to a record
--                                   (disabled, missing, replaced, changed) or
--                                   an Elitea-tagged provider object with no
--                                   record (orphan). Open while resolved_at is
--                                   NULL; a later pass resolves a finding it no
--                                   longer sees.
--
-- References only: no column holds a token, an assertion, a secret, a key or
-- anything a provider said about an object beyond its immutable id. Every
-- drift transition and every new orphan finding appends a security audit
-- event (0183 outbox) in the same transaction.
--
-- Nothing writes or reads these tables with credential governance off.
-- Additive only. Idempotent throughout. No BEGIN/COMMIT: the ledgered runner
-- wraps the file. No data, no permission.

CREATE SCHEMA IF NOT EXISTS elitea_identity;

CREATE TABLE IF NOT EXISTS elitea_identity.agent_identity_provider_health (
    provider              text PRIMARY KEY CHECK (provider ~ '^[a-z][a-z0-9_-]{0,31}$'),
    consecutive_failures  integer NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    admission_denied      boolean NOT NULL DEFAULT false,
    -- The pass lease: the replica holding it runs the provider's next pass.
    lease_holder          text NOT NULL DEFAULT '' CHECK (length(lease_holder) <= 128),
    lease_until           timestamptz NOT NULL DEFAULT '-infinity',
    last_pass_at          timestamptz,
    last_success_at       timestamptz,
    updated_at            timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS elitea_identity.external_identity_drift_findings (
    id                          uuid PRIMARY KEY,
    kind                        text NOT NULL
                                CHECK (kind IN ('disabled', 'missing', 'replaced', 'changed', 'orphan')),
    provider                    text NOT NULL CHECK (provider ~ '^[a-z][a-z0-9_-]{0,31}$'),
    provider_tenant             text NOT NULL CHECK (provider_tenant ~ '^[A-Za-z0-9._:/-]{1,128}$'),
    external_id                 text NOT NULL CHECK (length(external_id) BETWEEN 1 AND 256),
    -- NULL, and the scope 0, for an orphan: no record names the object.
    external_agent_identity_id  uuid REFERENCES elitea_identity.external_agent_identities (id),
    tenant_project_id           bigint NOT NULL DEFAULT 0 CHECK (tenant_project_id >= 0),
    application_id              bigint NOT NULL DEFAULT 0 CHECK (application_id >= 0),
    -- The record's epoch the finding's transition wrote; 0 for an orphan.
    revocation_epoch            bigint NOT NULL DEFAULT 0 CHECK (revocation_epoch >= 0),
    detected_at                 timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_seen_at                timestamptz NOT NULL DEFAULT clock_timestamp(),
    resolved_at                 timestamptz,
    CONSTRAINT external_identity_drift_findings_orphan_has_no_record
        CHECK ((kind = 'orphan') = (external_agent_identity_id IS NULL))
);

-- One open finding per provider object and kind.
CREATE UNIQUE INDEX IF NOT EXISTS external_identity_drift_findings_one_open
    ON elitea_identity.external_identity_drift_findings (provider, provider_tenant, external_id, kind)
    WHERE resolved_at IS NULL;

CREATE INDEX IF NOT EXISTS external_identity_drift_findings_open_detected
    ON elitea_identity.external_identity_drift_findings (detected_at DESC)
    WHERE resolved_at IS NULL;
