-- Record, on each inbound trigger, what it was issued for and what it admits
-- (legacy issue 6656, review findings on the agent trigger).
--
-- Legacy issue 6656 opened the inbound trigger to ordinary AGENT versions.
-- Three facts that the inbound path must decide on were not stored, so they
-- could not be enforced:
--
--   * `target_kind` — the kind of version the credential was ISSUED for,
--     `pipeline` or `agent`. `agent_type` can change on a version update.
--     Before 6656, minting and running both required a pipeline, so a trigger
--     that outlived a version edited into an agent was refused. With agents
--     admitted, that row would start an agent run that nobody issued a
--     credential for: the sender's payload as the prompt, the toolkits of the
--     version, and the creator's identity. The inbound path now refuses a row
--     whose `target_kind` differs from the version's current kind, and a
--     rotation (an explicit re-issue by a writer) records the current kind.
--
--     The DEFAULT and the backfill are `pipeline`. Every row that exists
--     before this migration was minted through a route that required a
--     pipeline version, so `pipeline` is a true statement about each of them.
--
--   * `event_filter` — the provider event names this trigger admits
--     (`X-GitHub-Event`, `X-Gitlab-Event`). NULL admits every event, which is
--     every existing row and every pipeline trigger unless its writer sets
--     one. A new AGENT trigger with a provider preset gets a short default
--     list, so a star, a fork or a comment on a public repository does not
--     start a paid model call.
--
--   * `allow_variable_overrides` — whether a caller may re-value the agent's
--     declared variables through the body's `variables`. A variable is
--     substituted into the instructions, so the default is FALSE: the holder
--     of the URL may send a message, and may change instruction text only
--     when the trigger opts in.
--
-- No table and no permission, so no shared sibling. Same guard as 0138: a
-- tenant schema that has not yet reached 0133 has no table here.
DO $$
BEGIN
    IF to_regclass('pipeline_triggers') IS NULL THEN
        RETURN;
    END IF;

    ALTER TABLE pipeline_triggers
        ADD COLUMN IF NOT EXISTS target_kind varchar(16) NOT NULL DEFAULT 'pipeline';
    ALTER TABLE pipeline_triggers
        ADD COLUMN IF NOT EXISTS event_filter text[];
    ALTER TABLE pipeline_triggers
        ADD COLUMN IF NOT EXISTS allow_variable_overrides boolean NOT NULL DEFAULT false;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('pipeline_triggers')
          AND conname = 'pipeline_triggers_target_kind_check'
    ) THEN
        ALTER TABLE pipeline_triggers
            ADD CONSTRAINT pipeline_triggers_target_kind_check
            CHECK (target_kind IN ('pipeline', 'agent'));
    END IF;

    -- An empty list would admit nothing, and nobody means that: a trigger
    -- that should admit nothing is revoked instead. NULL is "every event".
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('pipeline_triggers')
          AND conname = 'pipeline_triggers_event_filter_check'
    ) THEN
        ALTER TABLE pipeline_triggers
            ADD CONSTRAINT pipeline_triggers_event_filter_check
            CHECK (event_filter IS NULL OR cardinality(event_filter) > 0);
    END IF;
END
$$;
