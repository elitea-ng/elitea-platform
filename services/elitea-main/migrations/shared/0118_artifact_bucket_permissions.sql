-- 0118_artifact_bucket_permissions.sql — the per-bucket access list the
-- artifacts plugin calls `bucket_permissions`, and the two default-mode grants
-- its routes gate on.
--
-- THE LEGACY CONTRACT THIS STORES.
--
-- legacy/plugins/artifacts has NO bucket table. A bucket is a MinIO bucket plus
-- three S3 tags. The access list lives on a CREDENTIAL row instead: a
-- `configurations` row of type `s3_api_credentials` whose JSON `data` carries
--
--     {"user_id": 5, "bucket_permissions": {"reports": ["read"]}}
--
-- (models/pd/s3_credentials.py:50-98, rpc/s3_credentials.py:70-87). The map is
-- read by utils/utils.py:99-132, whose docstring states the semantics verbatim:
--
--     Empty dict {}                = unrestricted access to all buckets
--     Bucket not in dict           = no restriction for this bucket (allowed)
--     Bucket in dict with []       = explicitly blocked (no access)
--     Bucket in dict with ['read'] = read-only
--     Bucket in dict with ['read','write'] = full access
--
-- So the model is EXCEPTIONS, not an allow-list. The reference UI says the same
-- sentence on screen: "All users have read/write permissions by default"
-- (frontends/EliteaUI [fsd]/features/artifacts/ui/bucket-access/
-- DefaultPermissionsBanner.jsx:20), and its table is headed "Exceptions".
--
-- WHY A TABLE AND NOT A JSON MAP ON A CREDENTIAL ROW.
--
-- This platform has no `s3_api_credentials` configuration rows, and it is not
-- going to grow them: the SDK reaches artifact storage through the platform's
-- own authenticated routes, not through a per-user S3 access key
-- (internal/api/router.go, the /artifacts/s3 block). The credential was only
-- ever the CARRIER of the map. What the product means is one row per
-- (project, user, bucket), which is what this table stores, and the wire shape
-- the routes serve still assembles the legacy `bucket_permissions` map per
-- user so the contract the reference UI reads is unchanged.
--
-- The primary key is the (project, user, bucket) triple, so the enforcement
-- read on every object verb is one index lookup and NO row means "no
-- exception" — the legacy default, which is ALLOW.
--
-- `bucket` is the NAME, not a foreign key to elitea_storage.buckets(id). That
-- is deliberate and it is what legacy does: an exception may name a bucket that
-- does not exist yet, and it must survive a delete-and-recreate of the bucket,
-- because the operator authored a statement about a NAME.

CREATE TABLE elitea_storage.bucket_permissions (
    project_id  BIGINT      NOT NULL,
    user_id     BIGINT      NOT NULL,
    bucket      TEXT        NOT NULL,
    permissions TEXT[]      NOT NULL DEFAULT '{}'::text[],
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_id, bucket),
    -- The value vocabulary is exactly legacy's: read, write, or nothing.
    -- api/v2/bucket_permissions.py:96-99 rejects anything else with a 400, and
    -- the table refuses to hold what the route refuses to accept.
    CONSTRAINT bucket_permissions_values
        CHECK (permissions <@ ARRAY['read', 'write']::text[]),
    CONSTRAINT bucket_permissions_bucket_valid
        CHECK (bucket ~ '^[a-z][a-z0-9-]{1,62}$')
);

-- The listing route reads every exception in one project.
CREATE INDEX bucket_permissions_project
    ON elitea_storage.bucket_permissions (project_id);

-- The DEFAULT-mode grants for the two permissions the ACL routes gate on.
--
-- legacy/plugins/artifacts/api/v2/bucket_permissions.py declares
-- `configuration.artifacts.s3_credentials.view` on its GET (:36) and
-- `configuration.artifacts.s3_credentials.edit` on its PUT (:69) and DELETE
-- (:123). Those are DISTINCT from the four `configuration.artifacts.artifacts.*`
-- strings shared/0074 grants, so 0074 does not cover them and every ACL route
-- would answer 403 on a clean database — the class
-- internal/api/router_permission_grant_gate_test.go exists to catch.
--
-- THE SPLIT COMES FROM THE MATRIX, NOT FROM THE ROUTE DECORATOR.
-- testdata/postgres/legacy-rbac-matrix.json holds the grants a real pylon
-- deployment carries. In `default` mode it gives `s3_credentials.view` to
-- admin, editor and viewer, and `s3_credentials.edit` to admin and editor.
-- That is the same split 0074 copies for the artifact strings, and this file
-- copies it too. The route decorator's own `recommended_roles` block says
-- viewer False on the GET; the matrix is what deployments actually hold, and a
-- viewer who can read a listing they cannot change is the weaker grant of the
-- two. It makes no new policy either way.
--
-- The matrix also gives both to `system` and to `super_admin`. This file omits
-- both, as 0074 and every sibling file do: Go seeds neither role in the default
-- mode.
--
-- 0060's VIRGIN-mode guard is NOT reproduced, for 0074's reason: these
-- permissions never existed on a Go deployment, so no operator can have revoked
-- them, and a guard that skips configured deployments leaves exactly those
-- unable to author an exception.
DO $$
BEGIN

IF to_regclass('public.auth_core__role') IS NULL
   OR to_regclass('public.auth_core__role_permission') IS NULL THEN
    RAISE NOTICE '0118: auth_core tables absent, nothing to grant';
    RETURN;
END IF;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configuration.artifacts.s3_credentials.view')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor', 'viewer')
ON CONFLICT (role_id, permission) DO NOTHING;

INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, grant_row.permission
FROM public.auth_core__role AS role
CROSS JOIN (VALUES
    ('configuration.artifacts.s3_credentials.edit')
) AS grant_row(permission)
WHERE role.mode = 'default' AND role.name IN ('admin', 'editor')
ON CONFLICT (role_id, permission) DO NOTHING;

END
$$;
