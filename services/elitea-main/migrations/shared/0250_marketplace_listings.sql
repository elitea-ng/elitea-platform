-- 0250_marketplace_listings.sql — the Marketplace's listings, listing versions
-- and hires (ADR-0051 decisions 2 and 3, issue 1629, phase M3).
--
-- A listing is what the Marketplace shows: one per published agent or skill,
-- keyed by where it came from (the source project and entity), so a republish
-- after an unpublish finds the same row. Each publish adds a listing version:
-- an immutable snapshot of the published definition, its requirements (types
-- and names, never credentials), its release notes and its digest. An install
-- is built from the snapshot, never from the public-project twin, because the
-- twin carries no attachments.
--
-- listing_versions.version_id is the catalogue version id, the id of the
-- published version in the public project's schema. It is a plain number, not
-- a foreign key: unpublish deletes the twin, and the snapshot has to outlive it
-- while a project has it installed or as a fork origin. That retention is the
-- restricting foreign key from installs to listing_versions.
--
-- The state column holds `available`, `deprecated` or `yanked`. `current` is
-- the newest version that is neither yanked nor withdrawn and is derived when
-- the API answers. withdrawn_at is the publisher's unpublish of one version.
--
-- Rating columns are filled by the feedback phase (M5); until then they stay
-- null and zero.
--
-- The change request kinds hire, upgrade and upstream_merge are added to
-- elitea_lifecycle.change_requests (shared 0204 is released and immutable, so
-- the check constraints are replaced here). A request of these kinds carries the
-- listing, the version and the digest in place of candidate_version_id, and an
-- approved one needs no release.
--
-- No backfill: the listings of versions published before this file are built
-- lazily by the service on first read (internal/marketplace/backfill.go), from
-- the public project's published versions, whose id the database does not know.
-- No permission. No BEGIN/COMMIT: the ledgered runner wraps the file.

CREATE SCHEMA IF NOT EXISTS elitea_marketplace;

CREATE TABLE IF NOT EXISTS elitea_marketplace.listings (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind               text NOT NULL CHECK (kind IN ('agent', 'skill')),
    source_project_id  bigint NOT NULL CHECK (source_project_id > 0),
    source_entity_id   bigint NOT NULL CHECK (source_entity_id > 0),
    -- The public catalogue agent or skill: what Try in chat, likes and the
    -- public read use. Null when the twin was deleted by an unpublish.
    entity_id          bigint CHECK (entity_id IS NULL OR entity_id > 0),
    name               text NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
    description        text NOT NULL DEFAULT '',
    icon               text,
    category           text NOT NULL DEFAULT '' CHECK (length(category) <= 64),
    tags               text[] NOT NULL DEFAULT '{}' CHECK (cardinality(tags) <= 64),
    required_types     text[] NOT NULL DEFAULT '{}' CHECK (cardinality(required_types) <= 64),
    has_write_tools    boolean NOT NULL DEFAULT false,
    -- Lower-cased name, description and soul summary: what full-text search
    -- matches.
    search_text        text NOT NULL DEFAULT '',
    publisher_id       bigint CHECK (publisher_id IS NULL OR publisher_id > 0),
    risk_tier          text NOT NULL DEFAULT 'low' CHECK (risk_tier IN ('low', 'medium', 'high', 'critical')),
    -- The editorial Featured flag, set by a platform administrator.
    featured           boolean NOT NULL DEFAULT false,
    featured_at        timestamptz,
    featured_by        bigint CHECK (featured_by IS NULL OR featured_by > 0),
    state              text NOT NULL DEFAULT 'listed' CHECK (state IN ('listed', 'delisted')),
    delisted_at        timestamptz,
    delisted_by        bigint CHECK (delisted_by IS NULL OR delisted_by > 0),
    delist_reason      text NOT NULL DEFAULT '' CHECK (length(delist_reason) <= 500),
    rating_avg         numeric(3, 2) CHECK (rating_avg IS NULL OR rating_avg BETWEEN 0 AND 5),
    rating_count       integer NOT NULL DEFAULT 0 CHECK (rating_count >= 0),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT listings_origin_key UNIQUE (kind, source_project_id, source_entity_id)
);

CREATE INDEX IF NOT EXISTS listings_browse
    ON elitea_marketplace.listings (state, kind, created_at DESC);
CREATE INDEX IF NOT EXISTS listings_entity
    ON elitea_marketplace.listings (kind, entity_id) WHERE entity_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS listings_featured
    ON elitea_marketplace.listings (featured_at DESC) WHERE featured;
CREATE INDEX IF NOT EXISTS listings_tags
    ON elitea_marketplace.listings USING gin (tags);
CREATE INDEX IF NOT EXISTS listings_required_types
    ON elitea_marketplace.listings USING gin (required_types);

CREATE TABLE IF NOT EXISTS elitea_marketplace.listing_versions (
    listing_id         bigint NOT NULL REFERENCES elitea_marketplace.listings (id) ON DELETE CASCADE,
    version_id         bigint NOT NULL CHECK (version_id > 0),
    name               text NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    digest             bytea NOT NULL CHECK (length(digest) = 32),
    definition         jsonb NOT NULL,
    requirements       jsonb NOT NULL DEFAULT '{}'::jsonb,
    release_notes      text NOT NULL DEFAULT '' CHECK (length(release_notes) <= 4000),
    state              text NOT NULL DEFAULT 'available' CHECK (state IN ('available', 'deprecated', 'yanked')),
    state_reason       text NOT NULL DEFAULT '' CHECK (length(state_reason) <= 500),
    state_changed_at   timestamptz,
    state_changed_by   bigint CHECK (state_changed_by IS NULL OR state_changed_by > 0),
    published_by       bigint CHECK (published_by IS NULL OR published_by > 0),
    published_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- The publisher's unpublish of this version: not hireable and not
    -- listed, but the snapshot stays while a project holds it.
    withdrawn_at       timestamptz,
    PRIMARY KEY (listing_id, version_id)
);

CREATE INDEX IF NOT EXISTS listing_versions_newest
    ON elitea_marketplace.listing_versions (listing_id, published_at DESC, version_id DESC);

-- One row per project agent or skill hired from a listing. It is what the
-- hires count, the hired-here facet and the retention of a snapshot read.
CREATE TABLE IF NOT EXISTS elitea_marketplace.installs (
    project_id      bigint NOT NULL CHECK (project_id > 0),
    entity_kind     text NOT NULL CHECK (entity_kind IN ('agent', 'skill')),
    entity_id       bigint NOT NULL CHECK (entity_id > 0),
    listing_id      bigint NOT NULL,
    version_id      bigint NOT NULL,
    digest          bytea NOT NULL CHECK (length(digest) = 32),
    mode            text NOT NULL CHECK (mode IN ('install', 'fork')),
    -- The bindings the person chose, kept while the tools wait for a hire
    -- change request to be approved.
    bindings        jsonb NOT NULL DEFAULT '[]'::jsonb,
    tools_enabled   boolean NOT NULL DEFAULT true,
    installed_by    bigint NOT NULL CHECK (installed_by > 0),
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (project_id, entity_kind, entity_id),
    -- A snapshot is not deleted while a project has it installed or as a
    -- fork origin.
    CONSTRAINT installs_version_fk FOREIGN KEY (listing_id, version_id)
        REFERENCES elitea_marketplace.listing_versions (listing_id, version_id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS installs_listing
    ON elitea_marketplace.installs (listing_id, project_id);

-- Names the one-time backfills of this schema, as elitea_lifecycle.lifecycle_backfills
-- does for the lifecycle.
CREATE TABLE IF NOT EXISTS elitea_marketplace.backfills (
    name     text PRIMARY KEY,
    done_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The change request kinds of the Marketplace (ADR-0051 decisions 3 and 4).
ALTER TABLE elitea_lifecycle.change_requests
    DROP CONSTRAINT IF EXISTS change_requests_kind_check;
ALTER TABLE elitea_lifecycle.change_requests
    ADD CONSTRAINT change_requests_kind_check CHECK (kind IN
        ('release', 'recertification', 'legacy_unreviewed', 'hire', 'upgrade', 'upstream_merge'));

ALTER TABLE elitea_lifecycle.change_requests
    ADD COLUMN IF NOT EXISTS marketplace_listing_id bigint CHECK (marketplace_listing_id IS NULL OR marketplace_listing_id > 0);
ALTER TABLE elitea_lifecycle.change_requests
    ADD COLUMN IF NOT EXISTS marketplace_version_id bigint CHECK (marketplace_version_id IS NULL OR marketplace_version_id > 0);
ALTER TABLE elitea_lifecycle.change_requests
    ADD COLUMN IF NOT EXISTS marketplace_digest bytea CHECK (marketplace_digest IS NULL OR length(marketplace_digest) = 32);

-- An approved hire, upgrade or upstream merge changes the agent in place and
-- records no release.
ALTER TABLE elitea_lifecycle.change_requests
    DROP CONSTRAINT IF EXISTS change_requests_published_has_release;
ALTER TABLE elitea_lifecycle.change_requests
    ADD CONSTRAINT change_requests_published_has_release CHECK (
        state NOT IN ('published', 'approved') OR release_id IS NOT NULL
        OR kind IN ('legacy_unreviewed', 'hire', 'upgrade', 'upstream_merge')
    );

ALTER TABLE elitea_lifecycle.change_requests
    DROP CONSTRAINT IF EXISTS change_requests_marketplace_has_version;
ALTER TABLE elitea_lifecycle.change_requests
    ADD CONSTRAINT change_requests_marketplace_has_version CHECK (
        kind NOT IN ('hire', 'upgrade', 'upstream_merge')
        OR (marketplace_listing_id IS NOT NULL AND marketplace_version_id IS NOT NULL AND marketplace_digest IS NOT NULL)
    );
