-- Give the chat CANVAS tables an owner in this repository.
--
-- internal/infra/db/repos/conversations.go references two tables that nothing
-- in this repository creates: CreateCanvas INSERTs into `chat_messages_canvas`
-- and `chat_canvas_versions`, and GetMessageByUUID LEFT JOINs the first. They
-- existed only in testdata/postgres/legacy-centry-catalog.json — the pg_catalog
-- dump of the live legacy database — which is to say they existed only where
-- the legacy runtime had made them. This is the same gap 0123 closed for the
-- transcript tables and 0127 closed for attachments, one branch of the graph
-- further out.
--
-- WHICH CALL SITE IS ACTUALLY LIVE, because the two are not equal and the
-- distinction is the whole reason this file exists rather than a deletion:
--
-- * POST /api/v2/elitea_core/canvases/prompt_lib/{projectID} IS a registered
--   production route. router.go binds it behind `models.chat.canvas.create`,
--   which shared/0068_elitea_core_route_permissions.sql seeds and the legacy
--   RBAC catalogue also carries; production_router_test.go pins the path and
--   router_elitea_core_project_scope_test.go exercises its project scoping. On
--   every deployment the legacy runtime never touched, that route answered
--   `relation "p_1.chat_messages_canvas" does not exist` (42P01) as soon as a
--   caller cleared the permission gate. GET and PUT
--   /elitea_core/canvas/prompt_lib/{projectID}/{canvasID} are registered
--   alongside it and read chat_conversations, so they were unaffected.
--
-- * GetMessageByUUID's LEFT JOIN is LATENT. Its only caller,
--   `(*conversations.Handler).GetMessage`, is not registered by any router —
--   router.go binds 23 conversation handlers and that is not one of them, and
--   `Handler.Routes()` neither lists it nor is ever mounted. So it 500s nowhere
--   today and would 500 for EVERY message, canvas or not, the day the missing
--   route line is added, because the join is unconditional. Owning the table
--   now is what stops that line from being a regression.
--
-- The web client is a third state again, and it is why deleting this surface
-- would have been the wrong call: apps/elitea-web has the canvas feature PORTED
-- but not composed — entities/canvas (REST + socket hooks, normalisers,
-- selectors), features/chat-messages/ui/canvas/{Canvas,CanvasEditor}.tsx, and
-- the three canvas endpoints in shared/api/endpoints.manifest.json — with no
-- composition point yet (processes/chat/ui/ChatWithEditors.hooks.ts records
-- that in its own header, and useEditCanvas.ts still has its two mutation calls
-- commented out). A missing table is not the thing standing in that unit's way,
-- but it would have been the next thing.
--
-- THE SHAPE IS MEASURED, NOT TRANSCRIBED, from legacy-centry-catalog.json,
-- schema p_1 — the same authority 0127 used and for the same reason: a
-- SQLAlchemy model can drift from what was actually applied, and it is the
-- applied shape a mixed deployment has to keep matching.
--
--     chat_messages_canvas
--       name        text                NOT NULL
--       canvas_type character varying    NOT NULL   -- no length, as deployed
--       id          integer              NOT NULL
--       PRIMARY KEY (id)
--       FOREIGN KEY (id) REFERENCES chat_message_items(id) ON DELETE CASCADE
--       no index beyond the primary key
--
--     chat_canvas_versions
--       id             integer                     NOT NULL  (nextval)
--       code_language  character varying(32)        NULL
--       canvas_content text                         NOT NULL
--       canvas_item_id integer                      NOT NULL
--       created_at     timestamp without time zone  NOT NULL DEFAULT now()
--       PRIMARY KEY (id)
--       FOREIGN KEY (canvas_item_id) REFERENCES chat_messages_canvas(id) ON DELETE CASCADE
--       INDEX ix_tenant_chat_canvas_versions_created_at (created_at)
--
--     chat_canvas_version_authors
--       id                integer  NOT NULL  (nextval)
--       participant_id    integer  NOT NULL
--       canvas_version_id integer  NOT NULL
--       PRIMARY KEY (id)
--       FOREIGN KEY (participant_id)    REFERENCES chat_participants(id)      ON DELETE CASCADE
--       FOREIGN KEY (canvas_version_id) REFERENCES chat_canvas_versions(id)   ON DELETE CASCADE
--       UNIQUE (participant_id, canvas_version_id) AS _participant_id_canvas_version_id_uc
--
-- `id` IS LISTED LAST ON chat_messages_canvas, which reads as an oversight and
-- is not one: it is the deployed column order, and 0123 copied the identical
-- ordering for chat_messages_context rather than tidying it. Sharing
-- chat_message_items' primary key instead of carrying its own is what makes the
-- item's `item_type` discriminator (`canvas_message`) and its payload
-- impossible to disagree — the same 1:1 shape as chat_messages_text.
--
-- THE THIRD TABLE IS HERE EVEN THOUGH NO GO QUERY TOUCHES IT YET. 0123's own
-- header records why: an earlier revision of it created only the leaf tables a
-- 42P01 had named and took their parents as pre-existing, and that failed twice
-- over — the chain could not apply to a clean database, and a deployment that
-- got past it met the same error one table further in. chat_canvas_version_authors
-- is the FK child of chat_canvas_versions and the source of the `editors` list
-- CreateCanvas already returns (hardcoded empty). Owning the branch whole costs
-- three columns and removes the next instance of this same defect.
--
-- `canvas_type` HAS NO LENGTH LIMIT, matching the deployed `character varying`.
-- The legacy runtime's model may declare one; the applied column does not, and
-- a shorter column here would reject a value the legacy runtime accepts in a
-- database they share.
--
-- Every statement is IF NOT EXISTS, which is the bargain 0123, 0124, 0125, 0126
-- and 0127 each struck: wherever the legacy runtime already created these
-- tables and owns their shape this migration records itself and changes
-- nothing, and it becomes the definition only where nothing else provides one.
-- Neither ordering can produce 42P07. The index keeps its deployed name,
-- `ix_tenant_...`, for the same reason — a name of our own would be a second
-- index on the same column in every mixed deployment.
--
-- Guarded on the PARENTS with to_regclass, exactly as 0125, 0126 and 0127 are:
-- several integration fixtures apply the tenant chain to a schema of their own
-- making that has no chat tables at all, and a REFERENCES clause pointing at a
-- missing relation raises 42P01 and fails the whole chain. Both parents are
-- checked, not just the first: chat_message_items carries the payload row and
-- chat_participants carries the version authors, and a schema holding one
-- without the other would still fail halfway through.
--
-- These shapes are ALSO declared by `create_tenant_schema` in
-- internal/infra/db/migrations/001_initial.sql, because the journeys E2E stack
-- applies that file with psql and then runs `/elitea-migrate` with no flags —
-- Bootstrap plus ApplyShared, never the tenant history — so 001_initial is the
-- only definition that deployment ever sees. A Postgres integration test
-- compares the two column for column rather than trusting they agree
-- (repos/conversation_canvas_fresh_install_postgres_integration_test.go).
DO $$
BEGIN
    IF to_regclass('chat_message_items') IS NULL
       OR to_regclass('chat_participants') IS NULL THEN
        RETURN;
    END IF;

    CREATE TABLE IF NOT EXISTS chat_messages_canvas (
        name text NOT NULL,
        canvas_type varchar NOT NULL,
        id integer PRIMARY KEY REFERENCES chat_message_items(id) ON DELETE CASCADE
    );

    CREATE TABLE IF NOT EXISTS chat_canvas_versions (
        id serial PRIMARY KEY,
        code_language varchar(32),
        canvas_content text NOT NULL,
        canvas_item_id integer NOT NULL REFERENCES chat_messages_canvas(id) ON DELETE CASCADE,
        created_at timestamp NOT NULL DEFAULT now()
    );

    CREATE INDEX IF NOT EXISTS ix_tenant_chat_canvas_versions_created_at
        ON chat_canvas_versions (created_at);

    CREATE TABLE IF NOT EXISTS chat_canvas_version_authors (
        id serial PRIMARY KEY,
        participant_id integer NOT NULL REFERENCES chat_participants(id) ON DELETE CASCADE,
        canvas_version_id integer NOT NULL REFERENCES chat_canvas_versions(id) ON DELETE CASCADE,
        CONSTRAINT _participant_id_canvas_version_id_uc UNIQUE (participant_id, canvas_version_id)
    );
END
$$;
