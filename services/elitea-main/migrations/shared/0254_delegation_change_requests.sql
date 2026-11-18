-- 0254_delegation_change_requests.sql — the governed delegation grant is an
-- ADR-0040 change request of kind `delegation` (ADR-0044 amendment 1, A1.2
-- rule 2; delegation epic 2156 story S2b).
--
-- Under the governed profile a delegation grant for the edge Y -> X is
-- approved through a change request on the CALLER Y, with maker-checker at
-- every tier, low included: the approver differs from the proposer, holds the
-- approval permission of Y's tier, and the tier's approval count applies.
-- Shared migration 0204 and 0250 are released, so this one widens the CHECKs.
--
--   kind 'delegation'   the request carries `delegation_grant_id`, the grant of
--                       the tenant table delegation_grants (tenant 0252) it
--                       approves. The grant lives in the tenant schema and the
--                       request in the shared one, so there is no foreign key;
--                       the request and the grant are written in one
--                       transaction.
--   no release          an approved delegation records no release and
--                       certifies nothing, like a Marketplace hire.
--   one open request    0204 allows ONE open request per agent, which a pending
--                       release would block. A delegation request is one per
--                       EDGE (the grant's own unique active edge), so it is left
--                       out of that index and has its own: one open request per
--                       grant.
--
-- Additive and idempotent. No BEGIN/COMMIT: the ledgered runner wraps the file.
-- Guarded: some fixtures apply the shared corpus without the lifecycle schema.
DO $$
BEGIN
    IF to_regclass('elitea_lifecycle.change_requests') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE elitea_lifecycle.change_requests
        DROP CONSTRAINT IF EXISTS change_requests_kind_check;
    ALTER TABLE elitea_lifecycle.change_requests
        ADD CONSTRAINT change_requests_kind_check CHECK (kind IN
            ('release', 'recertification', 'legacy_unreviewed', 'hire', 'upgrade', 'upstream_merge', 'delegation'));

    ALTER TABLE elitea_lifecycle.change_requests
        ADD COLUMN IF NOT EXISTS delegation_grant_id bigint
        CHECK (delegation_grant_id IS NULL OR delegation_grant_id > 0);

    ALTER TABLE elitea_lifecycle.change_requests
        DROP CONSTRAINT IF EXISTS change_requests_published_has_release;
    ALTER TABLE elitea_lifecycle.change_requests
        ADD CONSTRAINT change_requests_published_has_release CHECK (
            state NOT IN ('published', 'approved') OR release_id IS NOT NULL
            OR kind IN ('legacy_unreviewed', 'hire', 'upgrade', 'upstream_merge', 'delegation')
        );

    ALTER TABLE elitea_lifecycle.change_requests
        DROP CONSTRAINT IF EXISTS change_requests_delegation_has_grant;
    ALTER TABLE elitea_lifecycle.change_requests
        ADD CONSTRAINT change_requests_delegation_has_grant CHECK (
            (kind = 'delegation') = (delegation_grant_id IS NOT NULL)
        );

    -- One open request per agent, except a delegation request (one per grant).
    DROP INDEX IF EXISTS elitea_lifecycle.change_requests_one_open;
    CREATE UNIQUE INDEX IF NOT EXISTS change_requests_one_open
        ON elitea_lifecycle.change_requests (tenant_project_id, application_id)
        WHERE state IN ('draft', 'submitted') AND kind <> 'delegation';
    CREATE UNIQUE INDEX IF NOT EXISTS change_requests_one_open_delegation
        ON elitea_lifecycle.change_requests (tenant_project_id, delegation_grant_id)
        WHERE state IN ('draft', 'submitted') AND kind = 'delegation';
END
$$;
