-- 0252_delegation_grants.sql — composition is a grant (ADR-0044 amendment 1,
-- A1.2 rule 2; delegation epic 2156 story S2a).
--
-- Attaching a sub-agent, an agent tool or a pipeline child X to a caller Y
-- creates one delegation grant for the EDGE Y -> X. The grant lists what X
-- held at attach time (its toolkits, the tools selected on each, its
-- credential references, declared data classes and configured egress hosts)
-- in the canonical sorted form the worker's DelegationEvidence digests, and
-- pins the SHA-256 of that set. The worker consumes the approved set as
-- G(Y -> X) in E(X) = own(X) ∩ (E(Y) ∪ G(Y -> X)) ∩ P (story S2c).
--
--   requested_set / requested_digest  what X held when the grant was
--                       proposed. Drift is X's CURRENT digest differing from
--                       this one (state `stale`).
--   grant_set / digest  what the approver approved: the requested set, less
--                       any item the approver chose to leave out or does not
--                       hold access to (a partial approval). The worker
--                       reads this one, by set containment, not version
--                       equality.
--   omitted             the items left out of a partial approval, for the
--                       review dialog.
--   governed            the maker-checker flag: set when the governed profile
--                       was on at proposal. The CHECK below then refuses an
--                       approver who is the proposer, as shared migration 0184
--                       does for credential use grants.
--
-- States: pending -> approved -> stale -> superseded (re-approval replaces
-- the row and keeps the old one); pending or approved or stale -> revoked
-- (detach, reject, withdraw). At most ONE active grant (pending, approved or
-- stale) per (caller, callee, edge kind); superseded and revoked rows are
-- history. `legacy` marks the pending grants the S6 migration writes for
-- compositions that exist today. Nothing here approves anything.
--
-- No foreign keys to applications or application_versions: those tables are
-- owned by the legacy schema and some fixtures apply the tenant chain to a
-- schema without them; a delete flow removes the edge's grants itself.
-- Users are central identities, never tenant keys, so proposed_by and
-- approved_by are plain bigint, as pinned_by is in 0251.
--
-- Guarded (idempotent): the runner pins search_path to the tenant schema.
-- No BEGIN/COMMIT.
CREATE TABLE IF NOT EXISTS delegation_grants (
    id bigserial PRIMARY KEY,
    project_id bigint NOT NULL CONSTRAINT delegation_grants_project_check CHECK (project_id > 0),
    caller_entity_id integer NOT NULL,
    callee_entity_id integer NOT NULL,
    edge_kind text NOT NULL
        CONSTRAINT delegation_grants_edge_kind_check
        CHECK (edge_kind IN ('sub_agent', 'agent_tool', 'pipeline_child', 'pipeline_node')),
    callee_version_id integer NOT NULL,
    requested_set jsonb NOT NULL,
    requested_digest text NOT NULL
        CONSTRAINT delegation_grants_requested_digest_check CHECK (requested_digest ~ '^[0-9a-f]{64}$'),
    grant_set jsonb NOT NULL,
    digest text NOT NULL
        CONSTRAINT delegation_grants_digest_check CHECK (digest ~ '^[0-9a-f]{64}$'),
    omitted jsonb NOT NULL DEFAULT '[]'::jsonb,
    state text NOT NULL DEFAULT 'pending'
        CONSTRAINT delegation_grants_state_check
        CHECK (state IN ('pending', 'approved', 'stale', 'superseded', 'revoked')),
    legacy boolean NOT NULL DEFAULT false,
    governed boolean NOT NULL DEFAULT false,
    origin text NOT NULL DEFAULT 'attach'
        CONSTRAINT delegation_grants_origin_check
        CHECK (origin IN ('attach', 'marketplace_hire', 'marketplace_upgrade', 'migration')),
    proposed_by bigint,
    approved_by bigint,
    approved_at timestamptz,
    revoked_by bigint,
    revoked_at timestamptz,
    superseded_by bigint REFERENCES delegation_grants (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT delegation_grants_distinct_entities CHECK (caller_entity_id <> callee_entity_id),
    -- An approved or stale grant (or a superseded one that was approved) has
    -- an approver and a time; a pending one has neither.
    CONSTRAINT delegation_grants_approval_recorded CHECK (
        (state = 'pending' AND approved_by IS NULL AND approved_at IS NULL)
        OR state IN ('approved', 'stale', 'superseded', 'revoked')
    ),
    CONSTRAINT delegation_grants_active_is_approved CHECK (
        state NOT IN ('approved', 'stale') OR (approved_by IS NOT NULL AND approved_at IS NOT NULL)
    ),
    -- Maker-checker (ADR-0040 decision 3): under the governed profile the
    -- proposer is known and the approver is somebody else.
    CONSTRAINT delegation_grants_approver_not_proposer CHECK (
        NOT governed OR (proposed_by IS NOT NULL AND (approved_by IS NULL OR approved_by <> proposed_by))
    ),
    CONSTRAINT delegation_grants_superseded_named CHECK (
        (state = 'superseded') = (superseded_by IS NOT NULL)
    ),
    CONSTRAINT delegation_grants_revoked_recorded CHECK (
        state <> 'revoked' OR revoked_at IS NOT NULL
    )
);

-- One active grant per edge.
CREATE UNIQUE INDEX IF NOT EXISTS delegation_grants_active_edge_key
    ON delegation_grants (caller_entity_id, callee_entity_id, edge_kind)
    WHERE state IN ('pending', 'approved', 'stale');

CREATE INDEX IF NOT EXISTS delegation_grants_caller_idx ON delegation_grants (caller_entity_id);
CREATE INDEX IF NOT EXISTS delegation_grants_callee_idx ON delegation_grants (callee_entity_id);
