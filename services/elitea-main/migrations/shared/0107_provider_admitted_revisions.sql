-- 0107_provider_admitted_revisions.sql — the provider ADMISSION plane
-- (ADR-0012 phase P3, and the storage the three service-descriptor routes have
-- been refusing for want of).
--
-- WHAT THIS REPLACES. internal/api/v2/eliteacore/service_descriptors.go answers
-- 501 on all three routes, and its header sets out at length why: there is no
-- table to read, and two of the four columns the page shows never came from
-- storage at all. `healthy` was which of two in-process Python dicts an entry
-- landed in when a legacy runtime worker last started, and the row set was the
-- union of those dicts rather than a query.
--
-- This corpus does not reproduce that shape. ADR-0012 replaces the mutable
-- descriptor blob with an admitted, immutable revision reached through review,
-- and replaces the free-form service_location_url with a normalised reviewed
-- origin. Four tables, and the split between them is the design:
--
--   provider_origin_registration — WHO may be reached. One normalised origin
--     per (project, provider), reviewed once. Mutable: an operator may correct
--     an origin without republishing a manifest.
--   provider_published_manifest  — WHAT was published. Content-addressed and
--     INSERT-ONLY: the digest IS the key, so the same bytes published twice
--     are one row and a manifest can never be edited under an admission that
--     already cites it.
--   provider_admitted_revision   — WHAT THIS DEPLOYMENT ADMITS. The join of an
--     origin and a manifest, with a lifecycle.
--   provider_health_projection   — WHETHER IT ANSWERED, LAST TIME ANYONE
--     ASKED. A separate table because health is a PROJECTION, not a property.
--
-- THE HEALTH SPLIT IS THE POINT, and it is the whole lesson of the file this
-- migration unblocks. Storing `healthy` on the revision row would reproduce
-- the legacy runtime's category error in durable form: a boolean that looks
-- like a fact about the provider when it is a fact about the last probe. A
-- projection has an observed_at, so a reader can tell "unhealthy" from "nobody
-- has asked lately" — and the API answers `healthy: null` for the second, which
-- is the one answer the legacy runtime page could never give.
--
-- TWO CONSTRAINTS CARRY THE ARCHITECTURE, both at the bottom of this file:
--
--   UNIQUE (project_id, provider_id) WHERE status = 'active'
--   CHECK  (status <> 'active' OR overlay_revision IS NOT NULL)
--
-- The second is ADR-0012's "a missing overlay policy fails admission" written
-- as a constraint instead of a TODO. This deployment can RECORD and SHOW a
-- descriptor and physically CANNOT activate one, because it has no way to
-- issue an overlay yet. That is deliberate: a registration surface for a
-- runtime that cannot invoke what it registers is worse than a refusal, and
-- the constraint is what stops the next change from quietly crossing that line.
--
-- NOTHING HERE IS OWNED BY THE LEGACY RUNTIME. These are new tables in a new
-- schema; no legacy table is claimed, adopted or renamed. The corpus has been
-- broken three times by a migration claiming a legacy-owned table and colliding
-- with a seeded fixture at 42P07, and this file cannot do that.

CREATE SCHEMA IF NOT EXISTS provider_hub;

-- ── who may be reached ──────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS provider_hub.provider_origin_registration (
    project_id          BIGINT      NOT NULL,
    provider_id         TEXT        NOT NULL,
    -- Normalised at the application boundary: scheme, host, optional port, no
    -- path, no credentials. A free-form URL is what ADR-0012 replaces.
    origin              TEXT        NOT NULL,
    registered_at       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    registered_by       TEXT        NOT NULL,
    PRIMARY KEY (project_id, provider_id),
    CONSTRAINT provider_origin_registration_origin_absolute
        CHECK (origin ~ '^https?://[^/[:space:]]+$'),
    CONSTRAINT provider_origin_registration_provider_id_bounded
        CHECK (provider_id <> '' AND length(provider_id) <= 128),
    CONSTRAINT provider_origin_registration_actor_present
        CHECK (registered_by <> '')
);

-- ── what was published ──────────────────────────────────────────────────────
--
-- Content-addressed and insert-only. The digest is the primary key, so
-- publishing the same bytes twice is one row, and an admission that cites a
-- digest cites bytes that cannot subsequently change.
CREATE TABLE IF NOT EXISTS provider_hub.provider_published_manifest (
    digest              TEXT        PRIMARY KEY,
    -- The descriptor exactly as it was published. Stored as bytes rather than
    -- jsonb: the digest is over these bytes, and jsonb normalises key order
    -- and whitespace, so a round trip through it would not re-digest to the
    -- same value.
    manifest_bytes      BYTEA       NOT NULL,
    published_at        TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT provider_published_manifest_digest_is_sha256
        CHECK (digest ~ '^[0-9a-f]{64}$'),
    CONSTRAINT provider_published_manifest_not_empty
        CHECK (octet_length(manifest_bytes) > 0)
);

-- ── what this deployment admits ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS provider_hub.provider_admitted_revision (
    revision_id         TEXT        PRIMARY KEY,
    project_id          BIGINT      NOT NULL,
    provider_id         TEXT        NOT NULL,
    manifest_digest     TEXT        NOT NULL
        REFERENCES provider_hub.provider_published_manifest(digest),
    -- The policy overlay this revision was admitted under. NULL until this
    -- deployment can issue one, which is why nothing can be active yet.
    overlay_revision    TEXT,
    status              TEXT        NOT NULL DEFAULT 'inactive',
    -- Why it is in that status, in the operator's terms. Never NULL: a
    -- revocation with no reason is a row nobody can act on.
    reason              TEXT        NOT NULL,
    admitted_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    admitted_by         TEXT        NOT NULL,
    revoked_at          TIMESTAMPTZ,
    revoked_by          TEXT,
    FOREIGN KEY (project_id, provider_id)
        REFERENCES provider_hub.provider_origin_registration(project_id, provider_id)
        ON DELETE RESTRICT,
    CONSTRAINT provider_admitted_revision_status
        CHECK (status IN ('inactive', 'active', 'revoked')),
    CONSTRAINT provider_admitted_revision_reason_present
        CHECK (reason <> ''),
    -- ADR-0012: a missing overlay policy FAILS admission. Written as a
    -- constraint rather than left as a TODO, so this deployment cannot
    -- activate a provider it has no way to police.
    CONSTRAINT provider_admitted_revision_active_needs_overlay
        CHECK (status <> 'active' OR overlay_revision IS NOT NULL),
    -- A revocation records who and when. Both or neither.
    CONSTRAINT provider_admitted_revision_revocation_complete
        CHECK ((status = 'revoked') = (revoked_at IS NOT NULL AND revoked_by IS NOT NULL))
);

-- At most ONE active revision per provider per project. Two would make
-- "which manifest is this deployment running" unanswerable.
CREATE UNIQUE INDEX IF NOT EXISTS provider_admitted_revision_one_active_idx
    ON provider_hub.provider_admitted_revision (project_id, provider_id)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS provider_admitted_revision_project_idx
    ON provider_hub.provider_admitted_revision (project_id, provider_id, admitted_at DESC);

-- ── whether it answered, last time anyone asked ─────────────────────────────
--
-- A PROJECTION. One row per (project, provider), overwritten by whatever
-- probed it last. observed_at is what lets a reader tell "unhealthy" from
-- "nobody has asked lately" — the distinction the legacy runtime's two
-- in-process dicts could not express, and the reason the API answers `healthy:
-- null` rather than defaulting to true or false.
CREATE TABLE IF NOT EXISTS provider_hub.provider_health_projection (
    project_id          BIGINT      NOT NULL,
    provider_id         TEXT        NOT NULL,
    healthy             BOOLEAN     NOT NULL,
    observed_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- What the probe saw. Present on both outcomes: a healthy probe that took
    -- nine seconds is worth reading too.
    detail              TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, provider_id),
    FOREIGN KEY (project_id, provider_id)
        REFERENCES provider_hub.provider_origin_registration(project_id, provider_id)
        ON DELETE CASCADE
);
