-- The admission transaction already locks the conversation and consumes the
-- exact pending decision with ResumeCurrentAgentHITL. The repository rechecks
-- direct scope from that statement's pre-update metadata before this write.
-- name: SegmentCurrentPipelineHITL :one
WITH paused AS MATERIALIZED (
    SELECT response.id, response.uuid, response.conversation_id,
           response.author_participant_id, actor.id AS actor_participant_id
    FROM chat_message_group AS response
    JOIN chat_participant_mapping AS actor_mapping
      ON actor_mapping.conversation_id = response.conversation_id
    JOIN chat_participants AS actor
      ON actor.id = actor_mapping.participant_id
     AND actor.entity_name = 'user'
     AND actor.entity_meta ->> 'id' = sqlc.arg(actor_user_id)::bigint::text
    WHERE response.id = sqlc.arg(paused_response_id)::integer
      AND response.task_id = sqlc.arg(execution_id)::text
      AND response.is_streaming
    FOR UPDATE OF response
), finalized AS (
    UPDATE chat_message_group AS response
    SET is_streaming = FALSE,
        task_id = sqlc.narg(previous_task_id)::text,
        updated_at = clock_timestamp(),
        meta = response.meta || jsonb_build_object('pipeline_hitl_resolution', jsonb_build_object(
            'interrupt_id', sqlc.arg(interrupt_id)::text,
            'node_name', sqlc.arg(node_name)::text,
            'action', sqlc.arg(decision_action)::text,
            'decision_message_id', sqlc.arg(decision_id)::uuid::text,
            'continuation_message_id', sqlc.arg(continuation_id)::uuid::text))
    FROM paused WHERE response.id = paused.id
    RETURNING response.id
), removed_provisional AS (
    DELETE FROM chat_message_items AS item USING finalized
    WHERE item.message_group_id = finalized.id AND item.item_type = 'text_message'
      AND item.meta -> 'runtime_stream_provisional' = 'true'::jsonb
), review_item AS (
    INSERT INTO chat_message_items (uuid, item_type, order_index, meta, message_group_id)
    SELECT gen_random_uuid(), 'text_message',
           (SELECT count(*)::integer FROM chat_message_items WHERE message_group_id = finalized.id),
           jsonb_build_object('kind', 'pipeline_hitl_prompt', 'status', 'resolved',
               'interrupt_id', sqlc.arg(interrupt_id)::text, 'node_name', sqlc.arg(node_name)::text,
               'action', sqlc.arg(decision_action)::text), finalized.id
    FROM finalized RETURNING id
), review_text AS (
    INSERT INTO chat_messages_text (id, content)
    SELECT id, sqlc.arg(review_message)::text FROM review_item
), decision_group AS (
    INSERT INTO chat_message_group (uuid, author_participant_id, conversation_id,
                                   sent_to_id, meta, is_streaming, created_at)
    SELECT sqlc.arg(decision_id)::uuid, paused.actor_participant_id, paused.conversation_id,
           paused.author_participant_id,
           jsonb_build_object('pipeline_hitl_decision', jsonb_build_object(
               'interrupt_id', sqlc.arg(interrupt_id)::text, 'node_name', sqlc.arg(node_name)::text,
               'action', sqlc.arg(decision_action)::text)), FALSE, clock_timestamp()
    FROM paused JOIN finalized ON finalized.id = paused.id
    RETURNING id, conversation_id, sent_to_id, created_at
), decision_item AS (
    INSERT INTO chat_message_items (uuid, item_type, order_index, meta, message_group_id)
    SELECT gen_random_uuid(), 'text_message', 0,
           jsonb_build_object('kind', 'pipeline_hitl_decision', 'interrupt_id', sqlc.arg(interrupt_id)::text,
               'node_name', sqlc.arg(node_name)::text, 'action', sqlc.arg(decision_action)::text), id
    FROM decision_group RETURNING id
), decision_text AS (
    INSERT INTO chat_messages_text (id, content)
    SELECT id, sqlc.arg(decision_text)::text FROM decision_item
), continuation AS (
    INSERT INTO chat_message_group (uuid, author_participant_id, conversation_id,
                                   reply_to_id, meta, is_streaming, created_at, task_id)
    SELECT sqlc.arg(continuation_id)::uuid, decision_group.sent_to_id, decision_group.conversation_id,
           decision_group.id,
           jsonb_build_object('thread_id', sqlc.arg(thread_id)::text,
               'execution_generation', sqlc.arg(execution_generation)::text,
               'pipeline_hitl_parent', jsonb_build_object('interrupt_id', sqlc.arg(interrupt_id)::text,
                   'prompt_message_id', paused.uuid::text,
                   'decision_message_id', sqlc.arg(decision_id)::uuid::text)),
           TRUE, decision_group.created_at + interval '1 microsecond', sqlc.arg(execution_id)::text
    FROM decision_group JOIN paused ON paused.conversation_id = decision_group.conversation_id
    RETURNING id, uuid
)
SELECT id AS response_message_group_id, uuid AS response_message_id FROM continuation;
