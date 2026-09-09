-- 0122_webhooks_and_deliveries.sql — the outbound webhook registry's actual
-- storage, and its delivery log (issue #876's second half).
--
-- WHY THIS FILE EXISTS AT ALL. #876 shipped internal/api/webhook's five CRUD
-- routes, internal/infra/db/repos/webhooks.go's SQL, and the Settings →
-- Webhooks page — every one of them written against a `webhooks` table this
-- corpus never created. router.go's own comment said so in as many words:
-- "no migration in this repository creates a `webhooks` table". On a database
-- built from this corpus every one of the five routes therefore answered
-- 42P01 (relation "webhooks" does not exist), not the 403 the #496 gate
-- correctly applies first on a caller with no permission — the two defects
-- stack, and only the first one is visible to a caller who never gets past
-- it. This file is the fix for the first one; the second (the dispatcher
-- itself having no producer) is #876's other half, fixed in
-- internal/api/webhook/dispatcher.go and internal/events/publisher.go in the
-- same change as this migration.
--
-- WHY `project_id text`, NOT `integer`. Every other project-scoped SHARED
-- table in this corpus (0064, 0071) uses `integer`, matching the Go side's
-- `int64`/`int32` project ids. This table does not, because
-- internal/infra/db/repos/webhooks.go's five queries all take `projectID
-- string` straight from `chi.URLParam(r, "projectID")` with NO int64 parse in
-- between — unlike, say, internal/api/v2/artifacts, which parses to int64
-- before ever reaching SQL. Declaring the column `integer` here would bind a
-- Go string against an inferred int4 parameter on every one of those five
-- queries, which pgx v5's extended protocol does not coerce, and every route
-- would 500 on its first real call instead of the 42P01 it answers today.
-- Matching the column to the value the ALREADY-SHIPPED, ALREADY-REVIEWED
-- repository code actually sends is the smaller, safer correction; rewriting
-- five queries and their tests to parse and format an int64 is a second
-- change this file does not also make.
--
-- WHY NO FOREIGN KEY TO A projects TABLE. Same reason 0071's project_id and
-- 0064's project_id carry none: project membership lives in pylon-owned
-- `centry.project`/`auth_core` tables this corpus does not always have (a
-- pylon-less Go-only database), and the #496 permission gate — not a foreign
-- key — is what stops a caller naming a project it cannot see.
--
-- WHAT webhook_deliveries IS. One row per COMPLETED attempt sequence — not
-- one row per HTTP attempt. internal/api/webhook/dispatcher.go retries a
-- failed delivery up to three times with a short backoff entirely inside one
-- goroutine and logs ONE row for the whole sequence, with `attempts` counting
-- how many tries it took. `payload` is the exact signed body that was sent,
-- kept so Redeliver resends byte-identical content instead of re-deriving a
-- payload that may have drifted (a project renamed between the original
-- event and a redelivery days later, say). `redelivery_of` self-references
-- the delivery a manual Redeliver click resent — NULL for an original,
-- automatic delivery — so the "Recent deliveries" panel and a future audit
-- both have "was this a click or a real event" without a second table.
--
-- WHY status IS A CHECK CONSTRAINT, NOT AN ENUM. webhook.DeliveryStatus in
-- Go is a closed three-value type (pending/success/failed) and this
-- constraint is its database-side twin, the same discipline
-- pipeline_schedules.last_result already uses for the same reason: an
-- unrecognised value in this column would render as nothing in the panel,
-- which is the empty state the feature exists to remove.
--
-- Table names are QUALIFIED (public.), following 0064's and 0071's
-- convention for a SHARED-schema table with project_id as an ordinary
-- column, unlike a TENANT migration's schema-per-project unqualified names.

CREATE TABLE IF NOT EXISTS public.webhooks (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id text NOT NULL,
    url        text NOT NULL,
    events     text[] NOT NULL DEFAULT '{}',
    secret     text NOT NULL DEFAULT '',
    active     boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Every one of the five CRUD routes filters on project_id first; the
-- dispatcher's ListByEvent additionally filters on active and matches events
-- with `$2 = ANY(events)`, which cannot use a plain btree index on events —
-- a GIN index would, but no deployment has enough webhooks per project yet
-- to make that anything but premature.
CREATE INDEX IF NOT EXISTS webhooks_project_id_idx ON public.webhooks (project_id);

CREATE TABLE IF NOT EXISTS public.webhook_deliveries (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    webhook_id     uuid NOT NULL REFERENCES public.webhooks (id) ON DELETE CASCADE,
    -- Denormalised from webhooks.project_id so the deliveries listing route
    -- (which is already scoped `.../{projectID}/{webhookID}/deliveries`) can
    -- filter without a join, and so a delivery row outlives nothing it
    -- shouldn't if a future change ever needs to query deliveries across
    -- webhooks within one project.
    project_id     text NOT NULL,
    event          text NOT NULL,
    status         text NOT NULL,
    attempts       integer NOT NULL DEFAULT 0,
    response_code  integer,
    last_error     text,
    payload        jsonb NOT NULL,
    -- Self-reference, not a foreign key to webhooks: a redelivery is always
    -- of a row in THIS table. ON DELETE SET NULL rather than CASCADE — if the
    -- original delivery is ever pruned, the redelivery that followed it is
    -- still real evidence of an attempt and must not vanish with it.
    redelivery_of  uuid REFERENCES public.webhook_deliveries (id) ON DELETE SET NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT webhook_deliveries_status_check
        CHECK (status IN ('pending', 'success', 'failed')),
    -- attempts is never 0 in practice (Dispatcher only calls Create after at
    -- least one HTTP attempt has run), but a stray future writer sending 0
    -- would otherwise silently pass as "logged, tried nothing", which reads
    -- to an operator as a real, un-retried success.
    CONSTRAINT webhook_deliveries_attempts_check CHECK (attempts >= 1)
);

-- The "Recent deliveries" panel's one query: this webhook's rows, newest
-- first.
CREATE INDEX IF NOT EXISTS webhook_deliveries_webhook_created_idx
    ON public.webhook_deliveries (webhook_id, created_at DESC);
