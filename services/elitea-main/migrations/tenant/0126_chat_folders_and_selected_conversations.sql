-- Give the ledgered tenant corpus the three chat objects that only
-- 001_initial.sql has ever created: `chat_conversation_folders`,
-- `chat_selected_conversations`, and `chat_conversations.folder_id` /
-- `.attachment_participant_id`.
--
-- MEASURED, by building one database twice on the same PostgreSQL server and
-- reading pg_catalog after each build:
--
--     internal/infra/db/migrations/001_initial.sql + full corpus
--         p_1.chat_conversation_folders        present
--         p_1.chat_selected_conversations      present
--         p_1.chat_conversations.folder_id     present
--
--     the ledgered corpus ALONE (which is what the repos test template is)
--         p_1.chat_conversation_folders        ABSENT
--         p_1.chat_selected_conversations      ABSENT
--         p_1.chat_conversations.folder_id     ABSENT
--
-- Production is not broken by this today: cmd/elitea-migrate/main.go:48 calls
-- migrate.Bootstrap(ctx, pool, bootstrapschema.Initial) before the corpus, so a
-- real deployment gets the wider shape from 001_initial.sql:462-473, :486-487
-- and :544-549. What is broken is OWNERSHIP. The corpus is what becomes the
-- definition as the legacy runtime is dropped, and it declares a
-- `chat_conversations` (tenant/0123:53-66) that is a strict SUBSET of the one
-- every deployment actually runs. `CREATE TABLE IF NOT EXISTS` hides the
-- disagreement on a bootstrapped database — the statement is a no-op there — so
-- the narrower declaration has never once been the table anyone queried.
--
-- 0123's own header says where its column list came from: it mirrors
-- internal/db/schema/agent_chat_baseline.sql, the sqlc COMPILER projection,
-- which carries neither `folder_id` nor `attachment_participant_id` because no
-- generated query names them. A projection of the queries is not a projection
-- of the schema. This is the same class of defect as 0124 (`elitea_tools`) and
-- 0125 (`entity_tool_mapping.entity_id`), and the same fix: reconcile the
-- corpus against the deployed legacy runtime shape in a NEW file, because
-- migrations are checksum-immutable and 0123 is already applied and ledgered.
--
-- WHAT ACTUALLY FAILS WHERE THE CORPUS IS THE ONLY SOURCE
--
-- Every folder and selected-conversation route is correct against production
-- and raises 42P01/42703 against a corpus-only database:
--
--   * internal/infra/db/repos/folders.go:33,66,90,121,136 — the whole folders
--     CRUD, 42P01 on `chat_conversation_folders`. The list read swallows the
--     error, so the sidebar renders an empty folder list rather than an error.
--   * internal/infra/db/repos/conversations.go:147,256,276 — `Get` and `Update`
--     select and write `c.folder_id`, 42703, which is a 500 on
--     GET/PUT /conversation/prompt_lib/{projectID}/{conversationID}.
--   * internal/infra/db/repos/conversations.go:526,530,539 — select/deselect,
--     42P01 on `chat_selected_conversations`.
--   * internal/api/v2/folders/handler.go:176,478 — the folder sidebar's
--     conversation join and its `?date_group=` variant; the latter is swallowed
--     and answers `selected_conversation_id: null`.
--
-- WHY NO TEST SAW IT
--
-- repos/configuration_validation_postgres_integration_test.go:748 builds the
-- shared template from `dbtest.Spec{Files: platformmigrations.Files, ...}` —
-- the ledgered corpus and a hand-written legacy seed, never 001_initial.sql. So
-- in the `repos` package these columns do not exist and no folder code path can
-- be executed at all. Everything that nominally covers folders avoids SQL:
-- internal/api/v2/folders/handler_test.go uses a mock repo with no pool,
-- production_router_test.go and tests/deployedge assert route surface with a
-- zero-value `&pgxpool.Pool{}`, and router_elitea_core_project_scope_test.go
-- checks permission gates with an empty Repository. One fixture even asserts
-- the opposite of the truth in a comment —
-- repos/index_activity_postgres_integration_test.go:513-521 hand-creates
-- `folder_id` for p_2 and calls it a deployed-legacy extra "which no query in
-- this repository reads", while four call sites read it. Landing this migration
-- is what makes the template carry the columns, and therefore what makes the
-- first real integration test for folders possible.
--
-- SHAPE, AND WHY EACH CHOICE
--
-- The authority is the legacy runtime, cross-checked against 001_initial.sql so
-- this file cannot disagree with the table every existing deployment already
-- carries:
--
--   * `chat_conversation_folders` — legacy/plugins/elitea_core/models/folder.py:12-33
--     against 001_initial.sql:462-473. Identical: `uuid` is UNIQUE and
--     NULLABLE (the SQLAlchemy `default=uuid.uuid4` is a client-side default,
--     which is why the bootstrap writes a server DEFAULT and no NOT NULL), and
--     `position` is NOT NULL DEFAULT 0 because 0 means "unpositioned" in the
--     ordering model rather than "missing".
--
--   * `chat_conversations.folder_id` — conversation.py:32-35, an FK with NO
--     `ON DELETE` clause. That absence is deliberate and load-bearing: the
--     legacy runtime's api/v2/folder.py:846-865 NULLs every member
--     conversation's `folder_id` in application code before deleting the
--     folder. Writing `ON DELETE CASCADE` here would silently destroy
--     conversations when a folder is removed, and `ON DELETE SET NULL` would
--     look harmless while quietly diverging from the constraint every deployed
--     database holds. Neither was taken.
--
--   * `chat_conversations.attachment_participant_id` — conversation.py:29-31 and
--     001_initial.sql:486, nullable, FK to `chat_participants(id)`.
--
--   * `chat_selected_conversations` — models/all.py:210-224 and
--     001_initial.sql:544-549. There is deliberately NO unique constraint on
--     `user_id`: that is exactly why repos/conversations.go's SelectConversation
--     is a DELETE-then-INSERT rather than an upsert, and adding the unique that
--     "obviously belongs there" would turn that pair into a constraint
--     violation the moment two rows raced. `conversation_id` is written NOT NULL
--     to match the legacy runtime's `nullable=False`; 001_initial.sql leaves it
--     nullable, and the two cannot conflict because on any database that has
--     the table this statement is a no-op.
--
-- WHY EVERY STATEMENT IS GUARDED
--
-- The outer `to_regclass` guard is the same one 0124 and 0125 carry: several
-- integration fixtures apply the tenant chain to a schema of their own making
-- that has no chat tables at all, and DDL against a missing relation raises
-- 42P01 and fails the WHOLE chain, not just this file. Where the chat graph is
-- absent this migration must record itself and move on.
--
-- The FK additions need a second guard of their own. `ALTER TABLE ... ADD
-- CONSTRAINT` has no IF NOT EXISTS in PostgreSQL, so on a legacy-provisioned or
-- bootstrapped database — where the column and its constraint already exist and
-- `ADD COLUMN IF NOT EXISTS` therefore did nothing — an unconditional ADD
-- CONSTRAINT would raise 42710 and fail the chain on precisely the databases
-- this migration is supposed to leave untouched. The existence probe is written
-- against pg_constraint by (conrelid, contype, referencing column) rather than
-- by constraint NAME, because the bootstrap's inline REFERENCES and the legacy
-- runtime's ALTER produce different auto-generated names for the same
-- constraint, and a name match would miss one of them and then collide.
-- pg_catalog is searched implicitly, so the probe works under the tenant-only
-- search_path; information_schema would NOT, which is why it is not used here.
--
-- All table names are UNQUALIFIED. Tenant migrations run with a
-- transaction-local search_path pinned to the tenant schema
-- (internal/infra/db/migrate/runner.go:83-96), which is also verified to be the
-- effective schema before any file runs, so `p_1` or `format(%I)` would be both
-- wrong and unnecessary.
DO $$
BEGIN
    -- One guard for the whole file. `chat_conversations` is the right sentinel
    -- rather than the folders table: it is what 0123 creates, so its presence is
    -- what says "this schema carries the chat graph", and the two tables below
    -- are meaningless without it.
    IF to_regclass('chat_conversations') IS NULL THEN
        RETURN;
    END IF;

    CREATE TABLE IF NOT EXISTS chat_conversation_folders (
        id serial PRIMARY KEY,
        uuid uuid UNIQUE DEFAULT gen_random_uuid(),
        name varchar NOT NULL,
        owner_id integer NOT NULL,
        position integer NOT NULL DEFAULT 0,
        meta jsonb NOT NULL DEFAULT '{}'::jsonb,
        created_at timestamp NOT NULL DEFAULT now(),
        updated_at timestamp
    );

    ALTER TABLE chat_conversations ADD COLUMN IF NOT EXISTS folder_id integer;

    -- No ON DELETE: the legacy runtime nulls folder_id in application code
    -- before dropping a folder (api/v2/folder.py:846-865). See the header.
    IF NOT EXISTS (
        SELECT 1
          FROM pg_constraint AS con
          JOIN pg_attribute AS att
            ON att.attrelid = con.conrelid
           AND att.attnum = ANY (con.conkey)
         WHERE con.conrelid = to_regclass('chat_conversations')
           AND con.contype = 'f'
           AND att.attname = 'folder_id'
    ) THEN
        ALTER TABLE chat_conversations
            ADD CONSTRAINT chat_conversations_folder_id_fkey
            FOREIGN KEY (folder_id) REFERENCES chat_conversation_folders(id);
    END IF;

    ALTER TABLE chat_conversations ADD COLUMN IF NOT EXISTS attachment_participant_id integer;

    -- `chat_participants` is created by 0123 alongside `chat_conversations`, so
    -- in practice it is always here once the sentinel above passed. The probe
    -- costs nothing and keeps a hand-built fixture schema that carries only
    -- `chat_conversations` from failing the chain on a missing FK target.
    IF to_regclass('chat_participants') IS NOT NULL AND NOT EXISTS (
        SELECT 1
          FROM pg_constraint AS con
          JOIN pg_attribute AS att
            ON att.attrelid = con.conrelid
           AND att.attnum = ANY (con.conkey)
         WHERE con.conrelid = to_regclass('chat_conversations')
           AND con.contype = 'f'
           AND att.attname = 'attachment_participant_id'
    ) THEN
        ALTER TABLE chat_conversations
            ADD CONSTRAINT chat_conversations_attachment_participant_id_fkey
            FOREIGN KEY (attachment_participant_id) REFERENCES chat_participants(id);
    END IF;

    -- No unique on user_id, on purpose. See the header.
    CREATE TABLE IF NOT EXISTS chat_selected_conversations (
        id serial PRIMARY KEY,
        user_id integer NOT NULL,
        conversation_id integer NOT NULL REFERENCES chat_conversations(id) ON DELETE CASCADE
    );
END
$$;
