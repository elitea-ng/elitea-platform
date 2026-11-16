-- 0251_conversation_skills.sql — skills pinned to a conversation (ADR-0050
-- amendment 1, A1.2; skills epic 2169 story S3).
--
-- A conversation skill is a skill, or a Marketplace listing version, that
-- applies to EVERY run of the conversation, whichever agent or model answers.
-- It is an attachment, never a participant: it has no avatar, no address and no
-- voice, and it grants nothing (a skill is text, ADR-0044 amendment 1).
--
--   skill_id            a project skill (a base version is read each turn)
--   listing_id +        a Marketplace listing version pinned BY REFERENCE: the
--   listing_version_id  text is read from the listing version when each turn
--                       starts, and nothing is forked into the project
--
-- Exactly one of the two forms per row (the CHECKs). A conversation holds at
-- most five rows; the API enforces it under the conversation's row lock, which
-- a CHECK cannot. The same skill or listing version is pinned once.
--
-- `pinned_by` is not a foreign key, as chat_conversation_archives.archived_by
-- is not: user identity is central (auth.User), never a tenant-schema key.
-- A deleted conversation or project skill takes its pins with it (ON DELETE
-- CASCADE), so no delete flow has to remember them. The listing side has no
-- foreign key (a cross-schema reference would block an unpublish); a pin whose
-- listing version was yanked or withdrawn refuses the next turn instead.
--
-- Guarded like 0135 and 0151 on the parents: some fixtures apply the tenant
-- chain to a schema with no chat_conversations or skills. IF NOT EXISTS keeps
-- it idempotent. No BEGIN/COMMIT: the ledgered runner wraps each file.
DO $$
BEGIN
    IF to_regclass('chat_conversations') IS NULL OR to_regclass('skills') IS NULL THEN
        RETURN;
    END IF;

    CREATE TABLE IF NOT EXISTS conversation_skills (
        id serial PRIMARY KEY,
        conversation_id integer NOT NULL REFERENCES chat_conversations (id) ON DELETE CASCADE,
        skill_id integer REFERENCES skills (id) ON DELETE CASCADE,
        listing_id bigint,
        listing_version_id bigint,
        pinned_by bigint NOT NULL,
        pinned_at timestamp NOT NULL DEFAULT (now() AT TIME ZONE 'UTC'),
        CONSTRAINT conversation_skills_one_form_check CHECK (
            (skill_id IS NOT NULL AND listing_id IS NULL AND listing_version_id IS NULL)
            OR (skill_id IS NULL AND listing_id IS NOT NULL AND listing_version_id IS NOT NULL)
        ),
        CONSTRAINT conversation_skills_listing_check CHECK (
            listing_id IS NULL OR (listing_id > 0 AND listing_version_id > 0)
        )
    );

    CREATE UNIQUE INDEX IF NOT EXISTS conversation_skills_skill_key
        ON conversation_skills (conversation_id, skill_id) WHERE skill_id IS NOT NULL;
    CREATE UNIQUE INDEX IF NOT EXISTS conversation_skills_listing_key
        ON conversation_skills (conversation_id, listing_id, listing_version_id) WHERE listing_id IS NOT NULL;
    CREATE INDEX IF NOT EXISTS conversation_skills_conversation_idx
        ON conversation_skills (conversation_id, id);
END
$$;
