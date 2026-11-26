-- 0284_admin_provisional_grants.sql — what an elevated person gives to
-- another person (ADR-0059 decision 5, "What an elevation cannot do"; stories
-- R2.12a, issue 2526, and R2.12b, issue 2606).
--
-- An administration role, or a project membership written through a central
-- permission, that an elevated person gives to SOMEONE ELSE is not written to
-- public.auth_core__user_role or public.auth_core__project_user_role. It is a
-- row here, bound to the elevation it was given under:
--
--   kind role        an administration role, by name (role_mode, role_name).
--                    The resolver unions the role's permissions into the
--                    grantee's administration set while the elevation is
--                    active (legacyrbac, the full resolution only: the
--                    standing-set call never reads this table).
--   kind membership  a project role (project_id, project_role_id). The
--                    resolver's project queries and the project access
--                    predicate (projectaccess.Membership) union it while the
--                    elevation is active.
--
-- "Active" is a PREDICATE over the elevation row, never a job: approved,
-- started, not ended and not past its expiry. When the elevation ends, every
-- grant of it stops at once.
--
-- The review of the elevation (R2.9a) can CONFIRM a grant when its outcome is
-- `justified`: the reviewer writes the standing assignment (a grant history
-- row with the source `review`) and confirmed_at/confirmed_by mark the row.
-- An `unauthorized` outcome confirms none, and neither does the terminal
-- state unreviewed_no_reviewer: an unconfirmed grant never becomes standing.
--
-- APPEND-ONLY, by trigger: no DELETE, no TRUNCATE, and the one UPDATE is the
-- confirmation, once. user ids carry no foreign key: the record outlives a
-- deleted account.
--
-- Every time column is timestamptz. Idempotent. No BEGIN/COMMIT: the ledgered
-- runner wraps the file.

CREATE TABLE IF NOT EXISTS elitea_auth.admin_provisional_grants (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    elevation_id       uuid NOT NULL REFERENCES elitea_auth.admin_elevations (id),
    kind               text NOT NULL,
    user_id            bigint NOT NULL,
    granted_by         bigint NOT NULL,
    role_mode          text,
    role_name          text,
    project_id         bigint,
    project_role_id    bigint,
    project_role_name  text,
    created_at         timestamptz NOT NULL,
    confirmed_at       timestamptz,
    confirmed_by       bigint,

    CONSTRAINT admin_provisional_grants_kind_known
        CHECK (kind IN ('role', 'membership')),
    CONSTRAINT admin_provisional_grants_role_shape
        CHECK (kind <> 'role'
               OR (role_mode = 'administration' AND role_name IS NOT NULL AND role_name <> ''
                   AND project_id IS NULL AND project_role_id IS NULL AND project_role_name IS NULL)),
    CONSTRAINT admin_provisional_grants_membership_shape
        CHECK (kind <> 'membership'
               OR (project_id IS NOT NULL AND project_role_id IS NOT NULL
                   AND role_mode IS NULL AND role_name IS NULL)),
    CONSTRAINT admin_provisional_grants_not_to_self
        CHECK (user_id <> granted_by),
    CONSTRAINT admin_provisional_grants_confirmed_together
        CHECK ((confirmed_at IS NULL) = (confirmed_by IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS admin_provisional_grants_one_role_idx
    ON elitea_auth.admin_provisional_grants (elevation_id, user_id, role_mode, role_name)
    WHERE kind = 'role';
CREATE UNIQUE INDEX IF NOT EXISTS admin_provisional_grants_one_membership_idx
    ON elitea_auth.admin_provisional_grants (elevation_id, user_id, project_id, project_role_id)
    WHERE kind = 'membership';
-- The resolver reads a grantee's rows on every administration resolution of
-- a browser session and on every project resolution.
CREATE INDEX IF NOT EXISTS admin_provisional_grants_user_idx
    ON elitea_auth.admin_provisional_grants (user_id, kind);
-- The review reads an elevation's rows.
CREATE INDEX IF NOT EXISTS admin_provisional_grants_elevation_idx
    ON elitea_auth.admin_provisional_grants (elevation_id, created_at);

CREATE OR REPLACE FUNCTION elitea_auth.admin_provisional_grants_append_only()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.confirmed_at IS NOT NULL THEN
            RAISE EXCEPTION 'elitea_auth.admin_provisional_grants: a grant cannot be inserted confirmed'
                USING ERRCODE = 'insufficient_privilege';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.confirmed_at IS NULL AND NEW.confirmed_at IS NOT NULL
       AND (to_jsonb(NEW) - 'confirmed_at' - 'confirmed_by') = (to_jsonb(OLD) - 'confirmed_at' - 'confirmed_by') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'elitea_auth.admin_provisional_grants is append-only: % refused', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

DROP TRIGGER IF EXISTS admin_provisional_grants_append_only ON elitea_auth.admin_provisional_grants;
CREATE TRIGGER admin_provisional_grants_append_only
    BEFORE INSERT OR UPDATE OR DELETE ON elitea_auth.admin_provisional_grants
    FOR EACH ROW EXECUTE FUNCTION elitea_auth.admin_provisional_grants_append_only();

DROP TRIGGER IF EXISTS admin_provisional_grants_no_truncate ON elitea_auth.admin_provisional_grants;
CREATE TRIGGER admin_provisional_grants_no_truncate
    BEFORE TRUNCATE ON elitea_auth.admin_provisional_grants
    FOR EACH STATEMENT EXECUTE FUNCTION elitea_auth.admin_provisional_grants_append_only();

COMMENT ON TABLE elitea_auth.admin_provisional_grants IS
    'ADR-0059 R2.12: administration roles and project memberships given under an elevation; effective while it is active, standing only once a justified review confirms them. Append-only but for the confirmation.';
