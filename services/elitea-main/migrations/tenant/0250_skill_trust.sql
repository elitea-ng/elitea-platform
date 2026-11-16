-- 0250_skill_trust.sql — a project's trust in the text of a skill written in
-- another project (ADR-0051 amendment 1, A1.6; skills epic story S1).
--
-- A skill forked from another project is third-party text. On the chat surfaces
-- (a message's picks, a conversation's pins, Try) the worker shows it as quoted
-- untrusted data (ADR-0043 decision 4) until a person in THIS project has read
-- that exact text and trusted it. One row = one skill and one REVISION, the
-- hex SHA-256 of the instructions, which is the value the worker freezes. Any
-- change to the text is a new revision, with no row, so it is untrusted again.
-- Trust is per project (the table lives in the tenant schema) and per skill
-- version, not per publisher: a publisher-level grant would let a later
-- upstream edit take effect with no reading.
--
-- A trust row gives a skill no tool and no permission. It only decides how the
-- text is framed.
--
-- `revoked_at` keeps the history: a revoke does not delete the row, so the
-- audit trail of who trusted what and when survives. At most ONE active
-- (unrevoked) row exists per skill and revision. `source` = 'migration' marks
-- the rows this migration writes so that forks that existed before this table
-- keep behaving as they did.
--
-- Guarded like 0136: some integration fixtures build a tenant schema with no
-- `skills` table, and ALTER/CREATE against a missing relation would fail the
-- whole tenant chain. Unqualified names: the runner pins search_path to the
-- tenant schema. No BEGIN/COMMIT. Idempotent.
DO $$
BEGIN
    IF to_regclass('skills') IS NULL OR to_regclass('skill_versions') IS NULL THEN
        RETURN;
    END IF;

    CREATE TABLE IF NOT EXISTS skill_trust (
        id serial PRIMARY KEY,
        skill_id integer NOT NULL REFERENCES skills (id) ON DELETE CASCADE,
        revision text NOT NULL,
        publisher_project_id integer,
        trusted_by integer,
        trusted_at timestamptz NOT NULL DEFAULT now(),
        revoked_by integer,
        revoked_at timestamptz,
        source text NOT NULL DEFAULT 'user'
            CONSTRAINT skill_trust_source_check CHECK (source IN ('user', 'migration')),
        CONSTRAINT skill_trust_revision_check CHECK (revision ~ '^[0-9a-f]{64}$')
    );

    CREATE UNIQUE INDEX IF NOT EXISTS skill_trust_active_key
        ON skill_trust (skill_id, revision) WHERE revoked_at IS NULL;
    CREATE INDEX IF NOT EXISTS skill_trust_skill_idx ON skill_trust (skill_id);

    -- Forks that exist today keep behaving as before: trust their current
    -- text once. A fork whose base version records a parent project other
    -- than this schema's project is foreign; the project id is the schema's
    -- own `p_<id>` suffix. A schema not named that way is left alone (its
    -- forks start untrusted) rather than guessed at.
    INSERT INTO skill_trust (skill_id, revision, publisher_project_id, source)
    SELECT sv.skill_id,
           encode(sha256(convert_to(COALESCE(sv.instructions, ''), 'UTF8')), 'hex'),
           CASE WHEN sv.meta ->> 'parent_project_id' ~ '^[0-9]+$'
                THEN (sv.meta ->> 'parent_project_id')::integer END,
           'migration'
    FROM skill_versions sv
    WHERE sv.name = 'base'
      AND current_schema() ~ '^p_[0-9]+$'
      AND COALESCE(sv.meta ->> 'parent_project_id', '') <> ''
      AND sv.meta ->> 'parent_project_id' IS DISTINCT FROM
          substring(current_schema() FROM '^p_([0-9]+)$')
    ON CONFLICT DO NOTHING;
END
$$;
