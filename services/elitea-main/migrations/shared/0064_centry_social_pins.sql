-- 0064_centry_social_pins.sql — the table every configuration read joins.
--
-- `queries/configurations.sql:57` reads `centry.social_pins` to project each
-- row's `is_pinned`, so EVERY call to
-- `GET /api/v2/configurations/configurations/{projectID}` failed with
--
--   ERROR: relation "centry.social_pins" does not exist (SQLSTATE 42P01)
--
-- on any deployment where nothing had created it (#293). The chat model picker
-- reads that route, so a user could not choose a model, so an agent turn was
-- rejected for not naming one — three layers from the actual cause.
--
-- Why nothing created it. `internal/db/schema/centry_projects_baseline.sql`
-- DECLARES the table, which is what let the query compile — but that file is a
-- sqlc compiler input and says so in its own header: "This file is NOT a
-- runtime migration." In a legacy runtime deployment the Projects plugin owns
-- the table. `001_initial.sql:585` does create a `social_pins`, but inside each
-- TENANT schema (`%I.social_pins`), which is a different table from the
-- `centry` one this query names. So the schema the generator type-checks
-- against and the schema the server runs against disagreed, and nothing in the
-- build could see it: sqlc was satisfied, and the failure only appears at
-- runtime, on a route no test had ever reached.
--
-- IF NOT EXISTS is load-bearing rather than defensive: on a legacy-backed
-- database the table already exists and is owned there, and this migration must
-- leave it exactly as it is. Column order, types and the uniqueness constraint
-- are transcribed from the baseline projection so the two cannot drift.
CREATE SCHEMA IF NOT EXISTS centry;

CREATE TABLE IF NOT EXISTS centry.social_pins (
    id serial PRIMARY KEY,
    entity varchar NOT NULL,
    user_id integer NOT NULL,
    project_id integer,
    entity_id integer NOT NULL,
    created_at timestamp NOT NULL DEFAULT now(),
    updated_at timestamp NOT NULL DEFAULT now(),
    UNIQUE (entity, project_id, entity_id)
);
