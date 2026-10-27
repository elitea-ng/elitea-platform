-- Mandate-bound unattended runs (ADR-0039, issue 1122).
--
-- With enterprise credential governance on, a pipeline schedule, an inbound
-- pipeline trigger and an index schedule run only under an explicit,
-- approved, revocable mandate (shared 0181, elitea_identity.mandates). The
-- schedule author, the trigger creator and the project system token are not
-- an authority for a governed run. This file stores which mandate each one
-- runs under, and nothing else.
--
--   * `pipeline_schedules.mandate_id` and `pipeline_triggers.mandate_id` —
--     the attached mandate's id (a uuid, kept as text: the mandate lives in
--     the shared schema, so no foreign key can reach it). Written only by
--     the attach route, which is mounted only with the flag on. NULL on every
--     existing row and on every row a flag-off deployment writes, so a
--     flag-off deployment reads and runs exactly as before. Saving a schedule
--     never writes it: the save keeps the attachment, and the author reset
--     (0133) is unchanged.
--
--   * `index_schedule_mandates` — the same attachment for an index schedule.
--     Index schedules have no table of their own: they live in
--     `elitea_tools.meta.indexes_meta[<index>].schedules`, a document the
--     index schedule routes rewrite whole. An attachment kept inside it would
--     be lost to the next schedule save, so it is a row keyed by the toolkit
--     and the index name instead. Empty on a flag-off deployment.
--
--   * The schedule outcome vocabulary gains the three governed skips. A
--     governed fire that finds no mandate, an unusable one, or one whose
--     scope no longer covers the schedule records one of them and does not
--     run; `last_run` is not stamped (0133's rule for every skip).
--
-- No permission: the attach route is gated on the central
-- `models.agent_identity.mandates.manage`, which shared 0181 grants. No data.
-- Same guard as 0138: a tenant schema that has not reached 0133 has no table
-- here.
DO $$
BEGIN
    IF to_regclass('pipeline_schedules') IS NOT NULL THEN
        ALTER TABLE pipeline_schedules ADD COLUMN IF NOT EXISTS mandate_id text NULL;
        ALTER TABLE pipeline_schedules DROP CONSTRAINT IF EXISTS pipeline_schedules_last_result_check;
        ALTER TABLE pipeline_schedules
            ADD CONSTRAINT pipeline_schedules_last_result_check
            CHECK (last_result IS NULL OR last_result IN (
                'dispatched',
                'skipped_overlap',
                'skipped_unauthorized',
                'skipped_missing_version',
                'skipped_mandate_required',
                'skipped_mandate_not_usable',
                'skipped_mandate_scope',
                'failed'
            ));
    END IF;

    IF to_regclass('pipeline_triggers') IS NOT NULL THEN
        ALTER TABLE pipeline_triggers ADD COLUMN IF NOT EXISTS mandate_id text NULL;
    END IF;

    CREATE TABLE IF NOT EXISTS index_schedule_mandates (
        toolkit_id integer NOT NULL,
        index_name text NOT NULL,
        mandate_id text NOT NULL,
        attached_by integer NOT NULL,
        attached_at timestamptz NOT NULL DEFAULT now(),

        CONSTRAINT index_schedule_mandates_pkey PRIMARY KEY (toolkit_id, index_name),
        CONSTRAINT index_schedule_mandates_index_name_check
            CHECK (index_name <> '' AND octet_length(index_name) <= 256)
    );

    -- Guarded as 0133 guards its own keys: a fixture schema that builds a
    -- partial tenant chain has no elitea_tools.
    IF to_regclass('elitea_tools') IS NOT NULL AND NOT EXISTS (
        SELECT 1
          FROM pg_constraint AS con
         WHERE con.conrelid = to_regclass('index_schedule_mandates')
           AND con.contype = 'f'
    ) THEN
        ALTER TABLE index_schedule_mandates
            ADD CONSTRAINT index_schedule_mandates_toolkit_id_fkey
            FOREIGN KEY (toolkit_id) REFERENCES elitea_tools(id)
            ON DELETE CASCADE;
    END IF;
END
$$;

COMMENT ON TABLE index_schedule_mandates IS
    'The mandate a governed index schedule runs under (ADR-0039, issue 1122). Empty with credential governance off.';
