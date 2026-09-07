-- Agent Evaluation slice 2: the DATASET, the RUN and the RESULT.
--
-- WHAT THIS ADDS TO 0130, AND WHAT IT STILL DOES NOT.
--
-- 0130 created `eval_dimensions` alone — the library — because a dimension is
-- authored, stored and read back with nothing behind it. This file creates the
-- four tables the smallest END-TO-END slice needs: a dataset of cases, a run
-- over that dataset, and one result row per (case, dimension). That is the
-- issue's own minimum, quoted from #617: "one dataset, one AI dimension, one
-- run, one score".
--
-- STILL ABSENT, and absent rather than stubbed: `eval_suites`,
-- `eval_bindings`, `eval_human_scores` and the platform-dimension registry.
-- A suite is a NAMED, REUSABLE set of bindings; this slice's run carries the
-- same information as a per-run SNAPSHOT instead (see `snapshot` below), which
-- is what the reference UI reads to render a scorecard anyway. Human scores are
-- an append-only second scoring channel with a server-side re-aggregation, and
-- the scorecard this slice serves is READ-ONLY, so nothing would write them.
-- Both arrive with the code that reads them.
--
-- WHY THE COLUMN NAMES ARE THE REFERENCE UI's AND NOT A LEGACY PLUGIN's.
--
-- There is NO evaluation plugin in the pylon corpus this repository carries.
-- The whole of `legacy/plugins/*` was searched for `eval_dataset`, `eval_run`,
-- `eval_result`, `eval_suite` and `eval_dimension`: the only traces are three
-- vestiges of a feature that was planned and never built — a placeholder
-- `"eval_task_node"` string in elitea_core's bootstrap topology, a
-- `generate_eval_dimensions` prompt key in `configurations`, and a comment
-- naming a function `build_eval_dimensions_system_prompt` that does not exist
-- in the file it points at. So there is no schema to align with. The contract
-- is the reference UI (`frontends/EliteaUI`, `src/[fsd]/widgets/evaluation/`),
-- whose components name every field they read, and those names are what the
-- columns below carry: `case_count`, `source_type`, `headline_score`,
-- `native_score`, `normalized_score`, `verdict`, `evidence`, `snapshot`,
-- `trigger_type`, and the run status vocabulary
-- `created | running | finished | errored | cancelled`.
--
-- ONE DELIBERATE DEVIATION. The dataset's agent column is `application_id`,
-- not the reference's `agent_id`. `agent_id` is what the reference sends as a
-- QUERY PARAMETER, and 0130 already stores the same relationship as
-- `application_id` on `eval_dimensions` (matching every other table in this
-- schema, where an agent is an `applications` row). Two names for one column
-- across two tables of one feature is how a join gets written against the
-- wrong one. The query parameter stays `agent_id`, exactly as the dimension
-- list route already accepts it.
--
-- WHY THE RUN CARRIES A SNAPSHOT.
--
-- `eval_runs.snapshot` is not a cache. A dimension is editable: its rubric,
-- its scale and its polarity can all change after a run finished, and a
-- scorecard that re-read the live rows would silently re-scale months-old
-- scores — the normalisation divides by (scale_max - scale_min) and flips on
-- polarity, so an edit to either rewrites history. The snapshot freezes what
-- the run was scored AGAINST, and the scorecard reads it rather than the
-- library. This is also what makes bindings unnecessary in this slice: the
-- snapshot's `bindings` array is the binding table's content for one run.
--
-- ITS SHAPE, which the handler builds and the client reads:
--   { "cases":      [ { "id": <int>, "order_index": <int> } ],
--     "dimensions": { "<id>": { "name", "scale_type", "scale_min",
--                               "scale_max", "polarity" } },
--     "bindings":   [ { "dimension_id", "engine", "weight",
--                       "target", "target_operator", "order_index" } ] }
--
-- WHY THE PROGRESS AND HEARTBEAT COLUMNS EXIST.
--
-- The run executes inside elitea-main as a bounded background goroutine, NOT
-- on the runtime plane and NOT on the scheduler (recorded in the package doc of
-- internal/api/v2/evaluation). A goroutine dies with its process, so a run that
-- was `running` when the pod was replaced would stay `running` for ever and its
-- dataset would never be scored. `heartbeat_at` is stamped by the orchestrator
-- while it works, and a `running` row whose heartbeat is older than the TTL is
-- re-queued to `created` at startup. Without the column the ONLY way to detect
-- the orphan is `started_at`, which cannot tell a dead run from a slow one.
--
-- `progress_done` / `progress_total` are stored rather than counted from
-- `eval_results` for the same reason the reference polls them: a case that
-- produced NO result row (skipped, or the run was cancelled before it) is
-- still progress, so COUNT(results) understates it and the progress bar sticks.
--
-- WHY `execution_mode` IS A COLUMN AND NOT A CONSTANT.
--
-- This slice runs the agent as ONE BLOCKING LLM TURN (the version's
-- instructions plus the case input, through the same PredictCompleter
-- /predict_llm and the AI-draft routes use). That is NOT the agent: it has no
-- tools, no toolkits and no conversation memory. Recording the mode on the row
-- is what stops a score from claiming more than it measured — the read routes
-- return it and the UI shows it. When the runtime plane executes a run, the
-- column distinguishes the two kinds of historical run instead of silently
-- re-labelling the old ones.
--
-- WHY THE CHECKS ARE IN THE DATABASE.
--
-- 0130's header gives the argument in full and it is unchanged here: the
-- orchestrator is not the only writer these tables will ever have, and a
-- scorer reading a stored row cannot re-run the handler's validation. A result
-- with `status = 'ok'` and a NULL `native_score` is a score that is not a
-- score; a run with `progress_done > progress_total` renders a bar past 100%;
-- a case belonging to no dataset is unreachable. Each is a CHECK below.
--
-- Table names are UNQUALIFIED: tenant migrations run with a transaction-local
-- search_path pinned to the tenant schema (internal/infra/db/migrate/
-- runner.go:83-96).
DO $$
BEGIN

    CREATE TABLE IF NOT EXISTS eval_datasets (
        id serial PRIMARY KEY,
        uuid uuid UNIQUE DEFAULT gen_random_uuid(),
        name varchar(128) NOT NULL,
        description text,
        -- The agent a dataset was authored for. NULL is a project-wide
        -- dataset, which is what the reference's dataset list shows when no
        -- `agent_id` filter is sent.
        application_id integer,
        is_shared boolean NOT NULL DEFAULT false,
        created_at timestamptz NOT NULL DEFAULT now(),
        updated_at timestamptz NOT NULL DEFAULT now(),

        CONSTRAINT eval_datasets_name_nonempty_check
            CHECK (btrim(name) <> '')
    );

    CREATE TABLE IF NOT EXISTS eval_dataset_cases (
        id serial PRIMARY KEY,
        -- ON DELETE CASCADE, unlike 0130's FK-less `application_id`. A case
        -- has no meaning apart from its dataset — it is not authored content
        -- that outlives the thing it belongs to — so orphaning it would leave
        -- rows no route can ever reach or remove.
        dataset_id integer NOT NULL REFERENCES eval_datasets(id) ON DELETE CASCADE,
        input text NOT NULL,
        -- The template variables the case supplies. `{}` and not NULL: the
        -- reference spreads this object, and a NULL there is a crash rather
        -- than an empty map.
        variables jsonb NOT NULL DEFAULT '{}'::jsonb,
        -- NULL means "this case has no expected answer", which is different
        -- from an expected answer of "". A judge told to compare against ""
        -- would mark every non-empty answer wrong.
        expected_output text,
        source_type varchar(32) NOT NULL DEFAULT 'manual',
        order_index integer NOT NULL DEFAULT 0,
        created_at timestamptz NOT NULL DEFAULT now(),
        updated_at timestamptz NOT NULL DEFAULT now(),

        CONSTRAINT eval_dataset_cases_source_type_check
            CHECK (source_type IN ('manual', 'import', 'conversation')),
        CONSTRAINT eval_dataset_cases_input_nonempty_check
            CHECK (btrim(input) <> ''),
        CONSTRAINT eval_dataset_cases_variables_object_check
            CHECK (jsonb_typeof(variables) = 'object')
    );

    CREATE INDEX IF NOT EXISTS eval_dataset_cases_dataset_idx
        ON eval_dataset_cases (dataset_id, order_index, id);

    CREATE TABLE IF NOT EXISTS eval_runs (
        id serial PRIMARY KEY,
        uuid uuid UNIQUE DEFAULT gen_random_uuid(),
        -- ON DELETE CASCADE, and the protection lives in the ROUTE instead.
        -- The chain has to reach all the way down: `applications` cascades to
        -- `eval_datasets`, so a NO ACTION key here would make the cascade fail
        -- at 23503 and an agent with an evaluated dataset undeletable — which
        -- is the exact pair of defects tenant/0131 had to repair on
        -- eval_dimensions ("an agent with a dimension cannot be deleted at
        -- all, and a project delete stops on the same row"). Losing a run's
        -- evidence to a casual dataset delete is a real cost, so the DELETE
        -- route refuses while a run references the dataset and names the
        -- count; the cascade is what happens when the AGENT itself goes.
        dataset_id integer NOT NULL REFERENCES eval_datasets(id) ON DELETE CASCADE,
        application_id integer,
        application_version_id integer,
        trigger_type varchar(32) NOT NULL DEFAULT 'on_demand',
        -- The person who started the run. NO foreign key, for tenant/0131's
        -- reason: a USER-kind column referencing public.auth_core__user would
        -- make the admin panel's user delete fail on a run row, and a run's
        -- provenance must survive the account that started it. It is carried
        -- because the run's model calls are signed with it — the gateway bills
        -- and authorizes the PERSON, and a background job with no actor bills
        -- the project to nobody.
        created_by integer,
        status varchar(32) NOT NULL DEFAULT 'created',
        execution_mode varchar(32) NOT NULL DEFAULT 'predict_blocking',
        snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
        progress_done integer NOT NULL DEFAULT 0,
        progress_total integer NOT NULL DEFAULT 0,
        -- NULL is "this run has no headline score", not zero. A run that
        -- errored before scoring anything must not read as a perfect failure.
        headline_score double precision,
        error text,
        created_at timestamptz NOT NULL DEFAULT now(),
        started_at timestamptz,
        finished_at timestamptz,
        heartbeat_at timestamptz,

        CONSTRAINT eval_runs_status_check
            CHECK (status IN ('created', 'running', 'finished', 'errored', 'cancelled')),
        CONSTRAINT eval_runs_trigger_type_check
            CHECK (trigger_type IN ('on_demand', 'offline_batch')),
        CONSTRAINT eval_runs_execution_mode_check
            CHECK (execution_mode IN ('predict_blocking', 'runtime_plane')),
        CONSTRAINT eval_runs_progress_check
            CHECK (progress_done >= 0
                   AND progress_total >= 0
                   AND progress_done <= progress_total),
        CONSTRAINT eval_runs_snapshot_object_check
            CHECK (jsonb_typeof(snapshot) = 'object'),
        -- The normalisation the scorecard averages is a 0..100 number. A
        -- headline outside that range is not a score the UI can render.
        CONSTRAINT eval_runs_headline_score_range_check
            CHECK (headline_score IS NULL
                   OR (headline_score >= 0 AND headline_score <= 100))
    );

    -- The run list is "this project's runs, newest first", optionally narrowed
    -- to one agent or one dataset. The re-queue sweep is
    -- `status = 'running' AND heartbeat_at < ...`, so status leads.
    CREATE INDEX IF NOT EXISTS eval_runs_status_heartbeat_idx
        ON eval_runs (status, heartbeat_at);
    CREATE INDEX IF NOT EXISTS eval_runs_listing_idx
        ON eval_runs (application_id, dataset_id, id DESC);

    CREATE TABLE IF NOT EXISTS eval_results (
        id serial PRIMARY KEY,
        run_id integer NOT NULL REFERENCES eval_runs(id) ON DELETE CASCADE,
        -- NOT a foreign key to eval_dataset_cases, deliberately. A case can be
        -- edited or deleted after a run; the result must survive it, because
        -- the run's `snapshot.cases` is what the scorecard renders and the
        -- result is the evidence for that row. A CASCADE here would delete a
        -- finished run's scores when somebody tidied the dataset.
        dataset_case_id integer NOT NULL,
        -- Likewise not a foreign key: the snapshot carries the dimension the
        -- score was produced against, and the library row may have moved on.
        dimension_id integer NOT NULL,
        status varchar(32) NOT NULL,
        native_score double precision,
        normalized_score double precision,
        target_met boolean,
        -- The judge's own answer: `{ "score": <number>, "reason": "<text>" }`
        -- on success, `{ "error": "<reason>", "raw": "<model text>" }` when the
        -- model did not answer in the schema. The RAW text is kept because a
        -- judge failure is a prompt problem, and a prompt problem cannot be
        -- diagnosed from the word "error".
        verdict jsonb NOT NULL DEFAULT '{}'::jsonb,
        -- What was scored: the case input, the agent's output, the expected
        -- output. The reference's case drill-down reads exactly these keys.
        evidence jsonb NOT NULL DEFAULT '{}'::jsonb,
        created_at timestamptz NOT NULL DEFAULT now(),

        CONSTRAINT eval_results_status_check
            CHECK (status IN ('ok', 'error', 'pending_human', 'skipped')),
        -- A result that claims to be a score MUST carry one. Without this a
        -- judge failure that forgot to set the status stores `ok` with a NULL
        -- score, the scorecard averages it as absent, and the run reports a
        -- headline computed over fewer cases than it says it covered.
        CONSTRAINT eval_results_ok_has_scores_check
            CHECK (status <> 'ok'
                   OR (native_score IS NOT NULL AND normalized_score IS NOT NULL)),
        CONSTRAINT eval_results_normalized_range_check
            CHECK (normalized_score IS NULL
                   OR (normalized_score >= 0 AND normalized_score <= 100)),
        CONSTRAINT eval_results_verdict_object_check
            CHECK (jsonb_typeof(verdict) = 'object'),
        CONSTRAINT eval_results_evidence_object_check
            CHECK (jsonb_typeof(evidence) = 'object'),
        -- One score per (run, case, dimension). The orchestrator is
        -- restartable, so a re-queued run WILL re-score cases it already
        -- scored; the uniqueness is what makes that an upsert instead of a
        -- second, contradictory row the average counts twice.
        CONSTRAINT eval_results_target_unique
            UNIQUE (run_id, dataset_case_id, dimension_id)
    );

    -- The scorecard reads one run ordered by (case, id) — the reference's own
    -- ordering, which its truncation-aware pager depends on.
    CREATE INDEX IF NOT EXISTS eval_results_run_case_idx
        ON eval_results (run_id, dataset_case_id, id);

    -- The agent references, added only where `applications` exists, with the
    -- guard 0126 and 0130 use. BOTH are ON DELETE CASCADE, and that is
    -- tenant/0131's decision rather than a new one: 0130 gave
    -- eval_dimensions.application_id a NO ACTION key, and 0131 had to replace
    -- it because an agent carrying an evaluation row could not be deleted at
    -- all and a project delete stopped on the same row. `ON DELETE SET NULL`
    -- is NOT the alternative: a NULL application_id here means "a project-wide
    -- dataset", so SET NULL would quietly PROMOTE one agent's dataset into the
    -- whole project's library — 0130's header rejects the same trade for the
    -- same reason. ALTER TABLE ... ADD CONSTRAINT has no
    -- IF NOT EXISTS, so an unguarded ADD raises 42710 on a re-run and 42P01
    -- where the table was never created. Several integration fixtures apply
    -- the tenant chain to a schema of their own making, and a raise there
    -- fails the WHOLE chain rather than this file.
    IF to_regclass('applications') IS NOT NULL AND NOT EXISTS (
        SELECT 1
          FROM pg_constraint AS con
          JOIN pg_attribute AS att
            ON att.attrelid = con.conrelid
           AND att.attnum = ANY (con.conkey)
         WHERE con.conrelid = to_regclass('eval_datasets')
           AND con.contype = 'f'
           AND att.attname = 'application_id'
    ) THEN
        ALTER TABLE eval_datasets
            ADD CONSTRAINT eval_datasets_application_id_fkey
            FOREIGN KEY (application_id) REFERENCES applications(id) ON DELETE CASCADE;
    END IF;

    IF to_regclass('applications') IS NOT NULL AND NOT EXISTS (
        SELECT 1
          FROM pg_constraint AS con
          JOIN pg_attribute AS att
            ON att.attrelid = con.conrelid
           AND att.attnum = ANY (con.conkey)
         WHERE con.conrelid = to_regclass('eval_runs')
           AND con.contype = 'f'
           AND att.attname = 'application_id'
    ) THEN
        ALTER TABLE eval_runs
            ADD CONSTRAINT eval_runs_application_id_fkey
            FOREIGN KEY (application_id) REFERENCES applications(id) ON DELETE CASCADE;
    END IF;

END
$$;

COMMENT ON TABLE eval_datasets IS
    'A named set of evaluation cases inside one project. Slice 2 of Agent Evaluation.';
COMMENT ON TABLE eval_dataset_cases IS
    'One evaluation case: the input the agent is given, the variables it is given with it, and the expected answer if the author stated one.';
COMMENT ON TABLE eval_runs IS
    'One execution of a dataset against one agent version. `snapshot` freezes the dimensions the run was scored against, so a later edit to the library cannot re-scale a finished run.';
COMMENT ON COLUMN eval_runs.execution_mode IS
    'predict_blocking = one blocking LLM turn per case (no tools, no toolkits, no memory); runtime_plane = the real agent. Recorded so a score cannot claim more than it measured.';
COMMENT ON COLUMN eval_runs.heartbeat_at IS
    'Stamped while the in-process orchestrator works. A `running` row whose heartbeat is older than the TTL is re-queued at startup — a goroutine dies with its process, and started_at cannot tell a dead run from a slow one.';
COMMENT ON TABLE eval_results IS
    'One score per (run, dataset case, dimension). status=error is what a judge that did not answer in the score schema produces — never a silent 0.';
