-- name: CancelCurrentAgentExecution :one
WITH target AS MATERIALIZED (
    SELECT response.id AS response_message_group_id,
           response.reply_to_id AS question_message_group_id,
           job.execution_id,
           job.generation,
           response.is_streaming,
           (
               response.meta ? 'hitl_interrupt'
               OR response.meta ? 'hitl_interrupts'
               OR response.meta ? 'authorization_requests'
           ) AS has_pause_projection,
           (response.meta ->> 'execution_generation')::text
               AS client_execution_generation,
           CASE
               WHEN question_author.entity_meta ->> 'id' ~ '^[1-9][0-9]*$'
               THEN (question_author.entity_meta ->> 'id')::bigint
           END AS question_author_id
    FROM chat_message_group AS response
    JOIN chat_conversations AS conversation
      ON conversation.id = response.conversation_id
    JOIN chat_message_group AS question
      ON question.id = response.reply_to_id
     AND question.conversation_id = conversation.id
    JOIN chat_participants AS question_author
      ON question_author.id = question.author_participant_id
     AND question_author.entity_name = 'user'
    JOIN elitea_runtime.agent_execution_jobs AS binding
      ON binding.client_message_id = response.uuid::text
     AND binding.execution_id = response.task_id
     AND binding.client_execution_generation
         = response.meta ->> 'execution_generation'
    JOIN elitea_runtime.execution_jobs AS job
      ON job.execution_id = binding.execution_id
     AND job.generation = binding.generation
     AND job.capability_id = binding.capability_id
    WHERE response.uuid = sqlc.arg(response_message_id)::uuid
      AND job.tenant_id = sqlc.arg(project_id)::integer::text
      AND job.resource_project_id = sqlc.arg(project_id)::integer
      AND job.projection_project_id = sqlc.arg(project_id)::integer
      AND job.capability_id IN (
          'agent.execute.application.v1',
          'agent.execute.adhoc.v1'
      )
      AND (
          conversation.author_id = sqlc.arg(actor_user_id)::bigint
          OR (
              question_author.entity_meta ->> 'id' ~ '^[1-9][0-9]*$'
              AND (question_author.entity_meta ->> 'id')::bigint
                  = sqlc.arg(actor_user_id)::bigint
          )
      )
), cancelled AS (
    UPDATE elitea_runtime.execution_jobs AS job
    SET desired_state = 'CANCELLED'
    FROM target
    WHERE job.execution_id = target.execution_id
      AND job.generation = target.generation
      AND (
          (
              job.desired_state IN ('RUNNING', 'SUSPENDED')
              AND (
                  job.state IN (
                      'PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING'
                  )
                  OR (
                      job.state IN ('SUCCEEDED', 'FAILED', 'CANCELLED')
                      AND target.is_streaming
                  )
                  OR (
                      job.state = 'SUCCEEDED'
                      AND target.has_pause_projection
                  )
              )
          )
          OR job.desired_state = 'CANCELLED'
      )
    RETURNING job.execution_id, job.generation
), recorded AS (
    -- The question is deleted when the turn has no output, so the replay
    -- check cannot resolve its author later: record it now, on the binding.
    UPDATE elitea_runtime.agent_execution_jobs AS binding
    SET stop_question_author_id = target.question_author_id
    FROM target
    JOIN cancelled
      ON cancelled.execution_id = target.execution_id
     AND cancelled.generation = target.generation
    WHERE binding.execution_id = target.execution_id
      AND binding.generation = target.generation
      AND binding.stop_question_author_id IS DISTINCT FROM target.question_author_id
    RETURNING binding.execution_id
)
SELECT target.response_message_group_id,
       target.question_message_group_id,
       target.execution_id,
       target.generation,
       target.client_execution_generation
FROM target
JOIN cancelled
  ON cancelled.execution_id = target.execution_id
 AND cancelled.generation = target.generation;

-- name: ProjectCurrentAgentStop :one
WITH target AS MATERIALIZED (
    SELECT response.id AS response_message_group_id,
           question.id AS question_message_group_id,
           item_count.value AS item_count,
           COALESCE(conversation.source = 'editor_test', FALSE)
               AS keep_empty_editor_test,
           latest_trace.text AS latest_trace_text
    FROM chat_message_group AS response
    JOIN chat_message_group AS question
      ON question.id = response.reply_to_id
     AND question.conversation_id = response.conversation_id
    JOIN chat_conversations AS conversation
      ON conversation.id = response.conversation_id
    CROSS JOIN LATERAL (
        SELECT count(*)::bigint AS value
        FROM chat_message_items AS item
        WHERE item.message_group_id = response.id
    ) AS item_count
    LEFT JOIN LATERAL (
        SELECT trace.text
        FROM chat_message_trace_step AS trace
        WHERE trace.message_group_id = response.id
          AND trace.kind = 'thinking_step'
          AND trace.text IS NOT NULL
          AND btrim(trace.text) <> ''
        ORDER BY trace.finished_at DESC NULLS LAST, trace.id DESC
        LIMIT 1
    ) AS latest_trace ON TRUE
    WHERE response.id = sqlc.arg(response_message_group_id)::integer
      AND question.id = sqlc.arg(question_message_group_id)::integer
      AND response.task_id = sqlc.arg(execution_id)::text
      AND response.meta ->> 'execution_generation'
          = sqlc.arg(execution_generation)::text
    FOR UPDATE OF response, question
), retained AS (
    UPDATE chat_message_group AS response
    SET is_streaming = FALSE,
        meta = response.meta
            - 'hitl_interrupt'
            - 'hitl_interrupts'
            - 'authorization_requests'
            - 'node_recovery_required_v1',
        updated_at = clock_timestamp()
    FROM target
    WHERE response.id = target.response_message_group_id
      AND (
          target.item_count > 0
          OR target.keep_empty_editor_test
          OR COALESCE(target.latest_trace_text, '') <> ''
      )
    RETURNING response.id
), salvaged_item AS (
    INSERT INTO chat_message_items (
        uuid, item_type, order_index, meta, message_group_id
    )
    SELECT gen_random_uuid(),
           'text_message',
           0,
           '{}'::jsonb,
           target.response_message_group_id
    FROM target
    WHERE target.item_count = 0
      AND COALESCE(target.latest_trace_text, '') <> ''
    RETURNING id, message_group_id
), salvaged_text AS (
    INSERT INTO chat_messages_text (id, content)
    SELECT salvaged_item.id, target.latest_trace_text
    FROM salvaged_item
    JOIN target
      ON target.response_message_group_id = salvaged_item.message_group_id
    RETURNING id
), deleted_response AS (
    DELETE FROM chat_message_group AS response
    USING target
    WHERE response.id = target.response_message_group_id
      AND target.item_count = 0
      AND NOT target.keep_empty_editor_test
      AND COALESCE(target.latest_trace_text, '') = ''
    RETURNING response.id
), deleted_question_items AS (
    DELETE FROM chat_message_items AS item
    USING target, deleted_response
    WHERE item.message_group_id = target.question_message_group_id
    RETURNING item.id
), deleted_question AS (
    DELETE FROM chat_message_group AS question
    USING target, deleted_response
    WHERE question.id = target.question_message_group_id
      AND (SELECT count(*) FROM deleted_question_items) >= 0
    RETURNING question.id
)
SELECT EXISTS (SELECT 1 FROM deleted_response) AS deleted,
       EXISTS (SELECT 1 FROM salvaged_text) AS salvaged,
       EXISTS (SELECT 1 FROM retained) AS retained,
       EXISTS (SELECT 1 FROM deleted_question) AS question_deleted;

-- name: IsCurrentAgentCancellationReplay :one
-- A replay admits the same principals CancelCurrentAgentExecution does: the
-- conversation's author and the question's author. A stop of a turn with no
-- output deletes the question and the answer, so neither is read from the
-- message rows: the conversation author is resolved through the binding's
-- client_stream_id, which admission pins to the conversation uuid, and the
-- question author is the stop_question_author_id the stop recorded (shared
-- 0154). The job's actor is NOT the question author for a regeneration (the
-- conversation's owner may regenerate another member's question); it stands
-- in only for a binding no stop through this route recorded.
SELECT EXISTS (
    SELECT 1
    FROM elitea_runtime.execution_jobs AS job
    JOIN elitea_runtime.agent_execution_jobs AS binding
      ON binding.execution_id = job.execution_id
     AND binding.generation = job.generation
     AND binding.capability_id = job.capability_id
    LEFT JOIN chat_conversations AS conversation
      ON conversation.uuid::text = binding.client_stream_id
    WHERE binding.client_message_id = sqlc.arg(response_message_id)::text
      AND job.tenant_id = sqlc.arg(project_id)::integer::text
      AND job.resource_project_id = sqlc.arg(project_id)::integer
      AND job.projection_project_id = sqlc.arg(project_id)::integer
      AND (
          conversation.author_id = sqlc.arg(actor_user_id)::bigint
          OR binding.stop_question_author_id = sqlc.arg(actor_user_id)::bigint
          OR (
              binding.stop_question_author_id IS NULL
              AND job.actor_id = sqlc.arg(actor_user_id)::bigint::text
          )
      )
      AND job.capability_id IN (
          'agent.execute.application.v1',
          'agent.execute.adhoc.v1'
      )
      AND job.desired_state = 'CANCELLED'
);
