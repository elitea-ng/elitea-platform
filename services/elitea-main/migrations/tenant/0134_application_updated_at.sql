-- Give `applications` the `updated_at` column the API has always claimed to
-- serve.
--
-- MEASURED. `GET /api/v2/elitea_core/applications/prompt_lib/{project}`
-- answers `updated_at` on every row, and the value is the Go zero time:
--
--     rows[0].updated_at = "0001-01-01T00:00:00Z"
--
-- The field is declared on the domain struct
-- (internal/domain/applications/types.go) with `json:"updated_at,omitempty"`,
-- and `omitempty` does nothing for a struct type — encoding/json never omits
-- a time.Time — so the key is always on the wire. Nothing ever scanned it,
-- because the table has no such column: `applications` in
-- internal/infra/db/migrations/001_initial.sql carries `created_at` alone,
-- and so does the pylon model this service inherited the shape from.
--
-- So every client that sorts or displays "last modified" for an agent has
-- been reading a constant. The spec said so out loud
-- (api/openapi/v2.yaml, Application.updated_at: "the wire carries the zero
-- sentinel on every row"), which made the lie documented rather than fixed.
--
-- WHY A COLUMN AND NOT A DERIVED VALUE
--
-- There is nothing to derive it from. `application_versions` has no
-- `updated_at` either, and `created_at` on either table answers a different
-- question. The writers must therefore stamp it, and they do — one statement
-- per write, folded into the write itself so a save cannot land with the
-- timestamp missing (internal/infra/db/repos/applications.go: Update,
-- insertVersion, UpdateVersion, DeleteVersion, SetDefaultVersion).
--
-- WHY `NOT NULL` WITH A BACKFILL
--
-- A nullable column would push the "no value yet" case onto every reader, and
-- the honest value for a row that has never been edited is the moment it was
-- created. The backfill says exactly that. It runs before the NOT NULL so the
-- statement after it is safe rather than merely lucky.
--
-- It reads `created_at` only where that column EXISTS. Several integration
-- fixtures build a minimal `applications` of their own — id, name, owner_id,
-- meta and nothing else — and a backfill written against the real table fails
-- the whole tenant chain there with 42703. Where there is no creation date to
-- inherit, `now()` is the only answer available, and those tables hold rows a
-- fixture wrote a moment ago in any case.
--
-- Guarded on the TABLE with to_regclass for the reason 0124 and 0125 are:
-- several integration fixtures apply the tenant chain to a schema of their own
-- making that has no `applications` at all, and ALTER TABLE on a missing
-- relation raises 42P01 and fails the whole chain. Where the table is absent
-- this migration records itself and moves on; where 001_initial's
-- `create_tenant_schema` has already declared the column, ADD COLUMN IF NOT
-- EXISTS and SET NOT NULL are both no-ops, so a fresh install and a
-- pylon-inherited schema converge on the same shape.
DO $$
BEGIN
    IF to_regclass('applications') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE applications ADD COLUMN IF NOT EXISTS updated_at TIMESTAMP;

    -- `to_regclass` resolves through the search_path the runner sets for the
    -- tenant, so this asks about THIS schema's table and no other.
    IF EXISTS (
        SELECT 1 FROM pg_attribute
        WHERE attrelid = to_regclass('applications')
          AND attname = 'created_at'
          AND NOT attisdropped
    ) THEN
        EXECUTE 'UPDATE applications SET updated_at = created_at WHERE updated_at IS NULL';
    END IF;
    UPDATE applications SET updated_at = now() WHERE updated_at IS NULL;

    ALTER TABLE applications ALTER COLUMN updated_at SET DEFAULT now();
    ALTER TABLE applications ALTER COLUMN updated_at SET NOT NULL;
END
$$;
