-- Per-message like/dislike feedback with an optional comment (#880).
--
-- WHY A NEW TABLE, NOT THE EXISTING social_feedbacks (internal/api/v2/social/
-- handler.go's CreateFeedback/ListFeedbacks). That table is real and mounted
-- (/social/feedbacks/default/{projectID}), but its shape does not fit a chat
-- MESSAGE's like/dislike/comment control:
--
--   * No unique constraint on (entity_name, entity_id, user_id) at all — a
--     second CreateFeedback call from the same user on the same entity
--     inserts a SECOND row rather than replacing the first. A thumbs control
--     needs "change your mind" to overwrite, not accumulate duplicate rows
--     ListFeedbacks would then have to de-duplicate on every read.
--   * `rating INTEGER` (nominally 1-5, unenforced — the column carries no
--     CHECK) is a star-rating shape, not a binary like/dislike one. Reusing
--     it for thumbs would mean inventing a private convention (1=dislike,
--     5=like, say) on a column that already means something else wherever
--     the entity-rating feature is used.
--   * `entity_id INTEGER` cannot hold a message's real identifier: a chat
--     message the client and this route both address by
--     chat_message_group.uuid (see conversations/handler.go's GetMessage/
--     DeleteMessage — "{messageID}" is that UUID, not a row id).
--
-- So this is a NEW, purpose-built table rather than a bent-shape reuse: one
-- row per (message, user), upsertable, with a hard uniqueness guarantee the
-- application does not have to enforce by convention.
--
-- SHAPE
--
--   chat_message_feedback
--     id                  bigserial   PRIMARY KEY
--     message_group_uuid  uuid        NOT NULL REFERENCES chat_message_group(uuid) ON DELETE CASCADE
--     user_id             integer     NOT NULL
--     rating              smallint    NOT NULL CHECK (rating IN (-1, 1))  -- -1 = dislike, 1 = like
--     comment             text        NULL
--     created_at          timestamp   NOT NULL DEFAULT now()
--     updated_at          timestamp   NOT NULL DEFAULT now()
--     UNIQUE (message_group_uuid, user_id)
--
-- `rating` is `smallint` with a CHECK rather than a `boolean` "is_like" flag:
-- the sign reads directly as "which way" in a `SUM(rating)`/`COUNT(*) FILTER`
-- aggregate query, and a CHECK constraint is the same backstop
-- wrapEvalWrite's own header explains for tenant/0132 — a write the handler's
-- validation failed to catch is refused by the table rather than silently
-- accepted.
--
-- `user_id` is NOT a foreign key to any users table: neither
-- `social_feedbacks` above nor `chat_participant_mapping` constrains it
-- either — user identity in this schema is authenticated centrally
-- (auth.User.ID) and never stored as a tenant-schema foreign key.
--
-- ON DELETE CASCADE from chat_message_group: deleting a message (DeleteMessage,
-- the same route this feature's UI sits under) must not leave orphaned
-- feedback rows an aggregate query would still count.
--
-- Guarded on the PARENT with to_regclass, matching 0125/0126/0127/0129: a
-- handful of integration fixtures apply the tenant chain to a schema that
-- never created chat_message_group, and a REFERENCES clause against a missing
-- relation raises 42P01 and fails the whole chain.
DO $$
BEGIN
    IF to_regclass('chat_message_group') IS NULL THEN
        RETURN;
    END IF;

    CREATE TABLE IF NOT EXISTS chat_message_feedback (
        id bigserial PRIMARY KEY,
        message_group_uuid uuid NOT NULL REFERENCES chat_message_group(uuid) ON DELETE CASCADE,
        user_id integer NOT NULL,
        rating smallint NOT NULL,
        comment text,
        created_at timestamp NOT NULL DEFAULT now(),
        updated_at timestamp NOT NULL DEFAULT now(),
        CONSTRAINT chat_message_feedback_message_user_uc UNIQUE (message_group_uuid, user_id),
        CONSTRAINT chat_message_feedback_rating_check CHECK (rating IN (-1, 1))
    );

    CREATE INDEX IF NOT EXISTS ix_tenant_chat_message_feedback_message_group_uuid
        ON chat_message_feedback (message_group_uuid);
END
$$;
