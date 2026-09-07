-- Inbound pipeline triggers (issue 192) and scheduled pipeline runs (issue 193).
--
-- WHAT THESE TWO TABLES ARE.
--
-- A pipeline is an `application_versions` row with `agent_type = 'pipeline'`.
-- Until now the only way to run one was a person typing into a chat. These
-- tables add the two unattended entry points legacy had and this stack did not:
--
--   * `pipeline_triggers` — ONE externally held credential per pipeline
--     version. An external system calls `POST /api/v2/pipeline_trigger/<token
--     id>` with the secret and the pipeline runs.
--   * `pipeline_schedules` — ONE cron expression per pipeline version. The
--     platform fires it unattended.
--
-- Both rows point at a VERSION and not at an application. A pipeline's graph
-- lives on the version, so "run this pipeline" without a version names nothing
-- executable. That is also what makes revocation meaningful: a trigger issued
-- against version 7 cannot run version 8.
--
-- WHY TENANT AND NOT SHARED.
--
-- Both are a project's own authored configuration, addressed through
-- `{projectID}` on every route, exactly like the agent version they hang off.
-- Neither is cross-project. No new RBAC permission is introduced either: the
-- read is `models.applications.version.details` and every write is
-- `models.applications.version.update`, both already in the shared corpus, so
-- this file has no shared sibling.
--
-- WHERE THE SECRET IS, AND WHERE IT IS NOT.
--
-- `token_hash` is a SHA-256 of the trigger secret. It is what the inbound
-- endpoint compares against, in constant time. The secret ITSELF is not in this
-- table at all: it lives in the global vault's HIDDEN bucket under
-- `AdminHiddenSecretName("pipeline_trigger", "<project>:<version>:<token id>",
-- "secret")` (internal/api/v2/secrets/admin_hidden.go), which is the same
-- mechanism the pre-built MCP catalogue uses and is excluded from the
-- `{{secret.<name>}}` merge every project workload resolves against.
--
-- The split is deliberate in both directions:
--
--   * The HASH is here because the inbound path must be able to refuse a wrong
--     token without opening a Fernet vault. A vault that will not open is a
--     recurring 500 class in this service, and an outage of it must not become
--     an outage of every customer's webhook.
--   * The SECRET is in the vault because the settings tab has to show the URL
--     again after the dialog is closed. A hash cannot do that, and storing the
--     secret in this table would put a live credential in plaintext next to the
--     rows any project-scoped read touches.
--
-- `token_id` is the PUBLIC half and the only part in the URL path. It is
-- unguessable on its own account (128 bits) so an inbound request cannot
-- enumerate pipelines, and it is what makes the vault name derivable without
-- storing it.
--
-- WHY `revoked_at` AND NOT A DELETE.
--
-- A revoked trigger is evidence. An operator asking "was this webhook still
-- live last Tuesday?" gets an answer from a row that is present and revoked and
-- no answer at all from a row that is gone. The inbound path refuses any row
-- with `revoked_at IS NOT NULL`, so a revoked row is inert.
--
-- ROTATION reuses the row: a new `token_id`, a new hash, a new vault entry,
-- `rotated_at` stamped, and the previous vault entry deleted. The old secret
-- stops working the moment the row is updated, which is what "rotate" has to
-- mean for a credential somebody else holds.
--
-- WHAT `last_result` IS FOR.
--
-- The third question issue 193 says is hard: "how a tenant sees failures of
-- something nobody was watching". A scheduled run writes an audit event AND
-- stamps the outcome here, because an audit trail answers "what happened on the
-- platform" while this column answers "is my schedule working", and the
-- settings tab can only ask the second one.
--
-- `last_run` follows elitea-scheduler's rule for `centry.schedule.last_run`
-- exactly (services/elitea-scheduler/internal/scheduler/maintenance.go): it is
-- stamped ONLY when a run was actually dispatched. A maintenance window, or a
-- tick that skipped the row because the previous run is still active, leaves it
-- alone — so the schedule is due exactly once when the reason clears, and never
-- once per missed minute.
--
-- Table names are UNQUALIFIED: tenant migrations run with a transaction-local
-- search_path pinned to the tenant schema (internal/infra/db/migrate/
-- runner.go:83-96).
DO $$
BEGIN
    CREATE TABLE IF NOT EXISTS pipeline_triggers (
        id serial PRIMARY KEY,
        application_id integer NOT NULL,
        version_id integer NOT NULL,
        token_id varchar(64) NOT NULL,
        token_hash bytea NOT NULL,
        secret_name varchar(255) NOT NULL,
        created_by integer NOT NULL,
        created_at timestamptz NOT NULL DEFAULT now(),
        rotated_at timestamptz,
        revoked_at timestamptz,
        revoked_by integer,
        last_used_at timestamptz,

        CONSTRAINT pipeline_triggers_version_key UNIQUE (version_id),
        CONSTRAINT pipeline_triggers_token_id_key UNIQUE (token_id),
        -- 32 bytes, no more and no less. A shorter digest here would mean the
        -- writer used something other than SHA-256, and the constant-time
        -- compare would then be comparing two things of different lengths —
        -- which `subtle.ConstantTimeCompare` answers 0 for regardless of
        -- content, turning a hashing bug into "every token is wrong".
        CONSTRAINT pipeline_triggers_token_hash_len_check
            CHECK (octet_length(token_hash) = 32)
    );

    CREATE TABLE IF NOT EXISTS pipeline_schedules (
        id serial PRIMARY KEY,
        application_id integer NOT NULL,
        version_id integer NOT NULL,
        cron varchar(255) NOT NULL,
        active boolean NOT NULL DEFAULT false,
        user_input text NOT NULL DEFAULT '',
        author_id integer NOT NULL,
        created_at timestamptz NOT NULL DEFAULT now(),
        updated_at timestamptz NOT NULL DEFAULT now(),
        last_run timestamptz,
        last_result varchar(64),
        last_result_detail text,
        last_result_at timestamptz,
        last_execution_id varchar(64),

        CONSTRAINT pipeline_schedules_version_key UNIQUE (version_id),
        -- The outcome vocabulary the settings tab renders. It is CLOSED
        -- because the tab has one sentence per value; an unknown value would
        -- render as nothing at all, which is the empty state this feature
        -- exists to remove.
        CONSTRAINT pipeline_schedules_last_result_check
            CHECK (last_result IS NULL OR last_result IN (
                'dispatched',
                'skipped_overlap',
                'skipped_unauthorized',
                'skipped_missing_version',
                'failed'
            ))
    );

    -- The scheduler's own scan: "every ACTIVE schedule in this project". It is
    -- the only query that reads the table without a version id.
    CREATE INDEX IF NOT EXISTS pipeline_schedules_active_idx
        ON pipeline_schedules (active) WHERE active;

    -- The FKs are guarded exactly as 0130 guards its own: ALTER TABLE ... ADD
    -- CONSTRAINT has no IF NOT EXISTS, so an unguarded ADD raises 42710 on a
    -- re-run and 42P01 in the fixture schemas that build a partial tenant
    -- chain, and a raise there fails the WHOLE chain rather than this file.
    IF to_regclass('application_versions') IS NOT NULL AND NOT EXISTS (
        SELECT 1
          FROM pg_constraint AS con
         WHERE con.conrelid = to_regclass('pipeline_triggers')
           AND con.contype = 'f'
    ) THEN
        ALTER TABLE pipeline_triggers
            ADD CONSTRAINT pipeline_triggers_version_id_fkey
            FOREIGN KEY (version_id) REFERENCES application_versions(id)
            ON DELETE CASCADE;
    END IF;

    IF to_regclass('application_versions') IS NOT NULL AND NOT EXISTS (
        SELECT 1
          FROM pg_constraint AS con
         WHERE con.conrelid = to_regclass('pipeline_schedules')
           AND con.contype = 'f'
    ) THEN
        ALTER TABLE pipeline_schedules
            ADD CONSTRAINT pipeline_schedules_version_id_fkey
            FOREIGN KEY (version_id) REFERENCES application_versions(id)
            ON DELETE CASCADE;
    END IF;
END
$$;

COMMENT ON TABLE pipeline_triggers IS
    'One externally held credential per pipeline VERSION (issue 192). The secret is in the global vault hidden bucket; only its SHA-256 is here.';
COMMENT ON COLUMN pipeline_triggers.token_id IS
    'The public half, and the only part in the inbound URL. Unguessable on its own so a caller cannot enumerate pipelines.';
COMMENT ON COLUMN pipeline_triggers.token_hash IS
    'SHA-256 of the secret. Compared in constant time so the inbound path never opens the vault.';
COMMENT ON COLUMN pipeline_triggers.secret_name IS
    'The hidden-bucket vault name holding the plaintext, so the settings tab can show the URL again.';
COMMENT ON COLUMN pipeline_triggers.revoked_at IS
    'Set instead of deleting the row: a revoked trigger is evidence, and the inbound path refuses any row where this is not NULL.';
COMMENT ON TABLE pipeline_schedules IS
    'One cron expression per pipeline VERSION (issue 193). The run executes as author_id, whose permission is re-validated at fire time.';
-- pipeline_schedules.author_id is written in the form 0128 records owner and
-- author meanings (a VALUES row of table, column, meaning), because
-- migrations/owner_column_meanings_test.go reads that form and nothing else:
-- one place holds the table, and a free-text comment does not count.
DO $$
DECLARE
    entry record;
BEGIN
    FOR entry IN
        SELECT *
          FROM (VALUES
            ('pipeline_schedules', 'author_id',
             'USER id, not project id. Who a scheduled run executes as (issue 193). Re-validated at every fire: an author who loses the project permission stops firing, with last_result = skipped_unauthorized.')
          ) AS t(table_name, column_name, meaning)
    LOOP
        EXECUTE format('COMMENT ON COLUMN %I.%I IS %L',
                       entry.table_name, entry.column_name, entry.meaning);
    END LOOP;
END $$;
COMMENT ON COLUMN pipeline_schedules.last_run IS
    'Stamped ONLY on a real dispatch. A maintenance window or a skipped overlap leaves it alone, so the row is due once when the reason clears rather than once per missed minute.';
COMMENT ON COLUMN pipeline_schedules.last_result IS
    'dispatched | skipped_overlap | skipped_unauthorized | skipped_missing_version | failed. The tenant-visible answer to "is my schedule working".';
