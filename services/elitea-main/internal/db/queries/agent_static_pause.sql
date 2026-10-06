-- Static pauses use the original command input and exact persisted response.
-- Checkpoint state stays in the worker; this query carries only its public proof.
-- name: ResolveCurrentStaticContinuation :one
SELECT conversation.uuid AS conversation_uuid,
       question.uuid AS question_id,
       response.author_participant_id AS target_participant_id,
       question_text.content::text AS user_input,
       COALESCE((response.meta -> 'pipeline_static_v1')::text, '')::text AS static_pause_json,
       COALESCE(response.meta ->> 'thread_id', '')::text AS thread_id,
       COALESCE(response.meta ->> 'execution_generation', '')::text AS execution_generation,
       entry.content_bytes, entry.content_digest
FROM chat_message_group AS response
JOIN chat_conversations AS conversation ON conversation.id = response.conversation_id
JOIN chat_message_group AS question
  ON question.id = response.reply_to_id AND question.conversation_id = conversation.id
JOIN chat_participants AS question_author
  ON question_author.id = question.author_participant_id AND question_author.entity_name = 'user'
JOIN chat_participants AS response_author
  ON response_author.id = response.author_participant_id AND response_author.entity_name = 'application'
JOIN chat_participant_mapping AS actor_mapping ON actor_mapping.conversation_id = conversation.id
JOIN chat_participants AS actor_participant
  ON actor_participant.id = actor_mapping.participant_id AND actor_participant.entity_name = 'user'
 AND (actor_participant.entity_meta ->> 'id')::bigint = sqlc.arg(actor_user_id)::bigint
JOIN LATERAL (
    SELECT text_item.content FROM chat_message_items AS item
    JOIN chat_messages_text AS text_item ON text_item.id = item.id
    WHERE item.message_group_id = question.id AND item.item_type = 'text_message'
    ORDER BY item.order_index DESC, item.id DESC LIMIT 1
) AS question_text ON TRUE
JOIN elitea_runtime.execution_jobs AS job
  ON job.execution_id = response.task_id
 AND job.tenant_id = sqlc.arg(project_id)::integer::text
 AND job.resource_project_id = sqlc.arg(project_id)::integer
 AND job.projection_project_id = sqlc.arg(project_id)::integer
 AND job.actor_id = sqlc.arg(actor_user_id)::bigint::text
 AND job.capability_id = 'agent.execute.application.v1'
JOIN elitea_runtime.agent_execution_jobs AS binding
  ON binding.execution_id = job.execution_id AND binding.generation = job.generation
 AND binding.input_bundle_id = job.input_bundle_id
 AND binding.client_stream_id = conversation.uuid::text
 AND binding.client_message_id = response.uuid::text
 AND binding.client_execution_generation = response.meta ->> 'execution_generation'
JOIN elitea_runtime.input_bundle_entries AS entry
  ON entry.input_bundle_id = job.input_bundle_id AND entry.entry_id = binding.request_entry_id
 AND entry.semantic_role = 'agent.execution_request'
WHERE conversation.uuid = sqlc.arg(conversation_uuid)::uuid
  AND response.uuid = sqlc.arg(response_message_id)::uuid
  AND NOT response.is_streaming
  AND (conversation.author_id = sqlc.arg(actor_user_id)::bigint
       OR (question_author.entity_meta ->> 'id')::bigint = sqlc.arg(actor_user_id)::bigint)
  AND (response_author.entity_meta ->> 'project_id')::integer = sqlc.arg(project_id)::integer
  AND jsonb_typeof(response.meta -> 'pipeline_static_v1') = 'object'
  AND COALESCE(response.meta ->> 'thread_id', '') <> ''
  AND COALESCE(response.meta ->> 'execution_generation', '') <> ''
  AND NOT (response.meta ? 'hitl_interrupt') AND NOT (response.meta ? 'hitl_interrupts')
  AND NOT (response.meta ? 'authorization_requests') AND NOT (response.meta ? 'output_limit_reached');

-- name: ResumeCurrentAgentStatic :one
WITH resolved AS MATERIALIZED (
    SELECT response.id, response.uuid
    FROM chat_message_group AS response
    JOIN chat_conversations AS conversation ON conversation.id = response.conversation_id
    JOIN chat_message_group AS question
      ON question.id = response.reply_to_id AND question.conversation_id = conversation.id
    JOIN chat_participants AS question_author
      ON question_author.id = question.author_participant_id AND question_author.entity_name = 'user'
    JOIN chat_participants AS response_author
      ON response_author.id = response.author_participant_id
     AND response_author.id = sqlc.arg(target_participant_id)::integer
     AND response_author.entity_name = 'application'
    JOIN chat_participant_mapping AS actor_mapping ON actor_mapping.conversation_id = conversation.id
    JOIN chat_participants AS actor_participant
      ON actor_participant.id = actor_mapping.participant_id AND actor_participant.entity_name = 'user'
     AND (actor_participant.entity_meta ->> 'id')::bigint = sqlc.arg(actor_user_id)::bigint
    JOIN chat_participant_mapping AS application_mapping
      ON application_mapping.conversation_id = conversation.id AND application_mapping.participant_id = response_author.id
    JOIN application_versions AS application_version
      ON application_version.id = sqlc.arg(application_version_id)::integer
     AND application_version.id = (application_mapping.entity_settings ->> 'version_id')::integer
     AND application_version.application_id = sqlc.arg(application_id)::integer
     AND application_version.application_id = (response_author.entity_meta ->> 'id')::integer
    JOIN elitea_runtime.execution_jobs AS job
      ON job.execution_id = response.task_id AND job.tenant_id = sqlc.arg(project_id)::integer::text
     AND job.resource_project_id = sqlc.arg(project_id)::integer AND job.projection_project_id = sqlc.arg(project_id)::integer
     AND job.actor_id = sqlc.arg(actor_user_id)::bigint::text AND job.capability_id = 'agent.execute.application.v1'
    JOIN elitea_runtime.agent_execution_jobs AS binding
      ON binding.execution_id = job.execution_id AND binding.generation = job.generation
     AND binding.input_bundle_id = job.input_bundle_id AND binding.client_stream_id = conversation.uuid::text
     AND binding.client_message_id = response.uuid::text AND binding.client_execution_generation = sqlc.arg(execution_generation)::text
    JOIN elitea_runtime.input_bundle_entries AS entry
      ON entry.input_bundle_id = job.input_bundle_id AND entry.entry_id = binding.request_entry_id
     AND entry.semantic_role = 'agent.execution_request' AND entry.content_digest = sqlc.arg(input_digest)::bytea
    WHERE conversation.uuid = sqlc.arg(conversation_uuid)::uuid AND response.uuid = sqlc.arg(response_message_id)::uuid
      AND question.uuid = sqlc.arg(question_id)::uuid AND NOT response.is_streaming
      AND response.meta -> 'pipeline_static_v1' = sqlc.arg(static_pause_json)::jsonb
      AND response.meta ->> 'execution_generation' = sqlc.arg(execution_generation)::text
      AND response.meta ->> 'thread_id' = sqlc.arg(thread_id)::text
      AND NOT (response.meta ? 'hitl_interrupt') AND NOT (response.meta ? 'hitl_interrupts')
      AND NOT (response.meta ? 'authorization_requests') AND NOT (response.meta ? 'output_limit_reached')
      AND (conversation.author_id = sqlc.arg(actor_user_id)::bigint
           OR (question_author.entity_meta ->> 'id')::bigint = sqlc.arg(actor_user_id)::bigint)
      AND (response_author.entity_meta ->> 'project_id')::integer = sqlc.arg(project_id)::integer
    FOR UPDATE OF response
), updated AS (
    UPDATE chat_message_group AS response
    SET meta = response.meta - 'pipeline_static_v1', is_streaming = TRUE,
        task_id = sqlc.arg(execution_id)::text, updated_at = clock_timestamp()
    FROM resolved WHERE response.id = resolved.id RETURNING response.id, response.uuid
)
SELECT updated.id AS response_message_group_id, updated.uuid AS response_message_id FROM updated;

-- Static pauses use the original command input and exact persisted response.
-- Checkpoint state stays in the worker; this query carries only its public proof.
-- name: ResolveCurrentStaticToolContinuation :one
SELECT conversation.uuid AS conversation_uuid,
       question.uuid AS question_id,
       response.author_participant_id AS target_participant_id,
       question_text.content::text AS user_input,
       COALESCE((response.meta -> 'pipeline_static_tools_v1')::text, '')::text AS static_tools_json,
       COALESCE(response.meta ->> 'thread_id', '')::text AS thread_id,
       COALESCE(response.meta ->> 'execution_generation', '')::text AS execution_generation,
       entry.content_bytes, entry.content_digest
FROM chat_message_group AS response
JOIN chat_conversations AS conversation ON conversation.id = response.conversation_id
JOIN chat_message_group AS question
  ON question.id = response.reply_to_id AND question.conversation_id = conversation.id
JOIN chat_participants AS question_author
  ON question_author.id = question.author_participant_id AND question_author.entity_name = 'user'
JOIN chat_participants AS response_author
  ON response_author.id = response.author_participant_id AND response_author.entity_name = 'application'
JOIN chat_participant_mapping AS actor_mapping ON actor_mapping.conversation_id = conversation.id
JOIN chat_participants AS actor_participant
  ON actor_participant.id = actor_mapping.participant_id AND actor_participant.entity_name = 'user'
 AND (actor_participant.entity_meta ->> 'id')::bigint = sqlc.arg(actor_user_id)::bigint
JOIN LATERAL (
    SELECT text_item.content FROM chat_message_items AS item
    JOIN chat_messages_text AS text_item ON text_item.id = item.id
    WHERE item.message_group_id = question.id AND item.item_type = 'text_message'
    ORDER BY item.order_index DESC, item.id DESC LIMIT 1
) AS question_text ON TRUE
JOIN elitea_runtime.execution_jobs AS job
  ON job.execution_id = response.task_id
 AND job.tenant_id = sqlc.arg(project_id)::integer::text
 AND job.resource_project_id = sqlc.arg(project_id)::integer
 AND job.projection_project_id = sqlc.arg(project_id)::integer
 AND job.actor_id = sqlc.arg(actor_user_id)::bigint::text
 AND job.capability_id = 'agent.execute.application.v1'
JOIN elitea_runtime.agent_execution_jobs AS binding
  ON binding.execution_id = job.execution_id AND binding.generation = job.generation
 AND binding.input_bundle_id = job.input_bundle_id
 AND binding.client_stream_id = conversation.uuid::text
 AND binding.client_message_id = response.uuid::text
 AND binding.client_execution_generation = response.meta ->> 'execution_generation'
JOIN elitea_runtime.input_bundle_entries AS entry
  ON entry.input_bundle_id = job.input_bundle_id AND entry.entry_id = binding.request_entry_id
 AND entry.semantic_role = 'agent.execution_request'
WHERE conversation.uuid = sqlc.arg(conversation_uuid)::uuid
  AND response.uuid = sqlc.arg(response_message_id)::uuid
  AND NOT response.is_streaming
  AND (conversation.author_id = sqlc.arg(actor_user_id)::bigint
       OR (question_author.entity_meta ->> 'id')::bigint = sqlc.arg(actor_user_id)::bigint)
  AND (response_author.entity_meta ->> 'project_id')::integer = sqlc.arg(project_id)::integer
  AND jsonb_typeof(response.meta -> 'pipeline_static_tools_v1') = 'object'
  AND COALESCE(response.meta ->> 'thread_id', '') <> ''
  AND COALESCE(response.meta ->> 'execution_generation', '') <> ''
  AND NOT (response.meta ? 'output_limit_reached');

-- name: ResumeCurrentAgentStaticTools :one
WITH resolved AS MATERIALIZED (
    SELECT response.id, response.uuid
    FROM chat_message_group AS response
    JOIN chat_conversations AS conversation ON conversation.id = response.conversation_id
    JOIN chat_message_group AS question
      ON question.id = response.reply_to_id AND question.conversation_id = conversation.id
    JOIN chat_participants AS question_author
      ON question_author.id = question.author_participant_id AND question_author.entity_name = 'user'
    JOIN chat_participants AS response_author
      ON response_author.id = response.author_participant_id
     AND response_author.id = sqlc.arg(target_participant_id)::integer
     AND response_author.entity_name = 'application'
    JOIN chat_participant_mapping AS actor_mapping ON actor_mapping.conversation_id = conversation.id
    JOIN chat_participants AS actor_participant
      ON actor_participant.id = actor_mapping.participant_id AND actor_participant.entity_name = 'user'
     AND (actor_participant.entity_meta ->> 'id')::bigint = sqlc.arg(actor_user_id)::bigint
    JOIN chat_participant_mapping AS application_mapping
      ON application_mapping.conversation_id = conversation.id AND application_mapping.participant_id = response_author.id
    JOIN application_versions AS application_version
      ON application_version.id = sqlc.arg(application_version_id)::integer
     AND application_version.id = (application_mapping.entity_settings ->> 'version_id')::integer
     AND application_version.application_id = sqlc.arg(application_id)::integer
     AND application_version.application_id = (response_author.entity_meta ->> 'id')::integer
    JOIN elitea_runtime.execution_jobs AS job
      ON job.execution_id = response.task_id AND job.tenant_id = sqlc.arg(project_id)::integer::text
     AND job.resource_project_id = sqlc.arg(project_id)::integer AND job.projection_project_id = sqlc.arg(project_id)::integer
     AND job.actor_id = sqlc.arg(actor_user_id)::bigint::text AND job.capability_id = 'agent.execute.application.v1'
    JOIN elitea_runtime.agent_execution_jobs AS binding
      ON binding.execution_id = job.execution_id AND binding.generation = job.generation
     AND binding.input_bundle_id = job.input_bundle_id AND binding.client_stream_id = conversation.uuid::text
     AND binding.client_message_id = response.uuid::text AND binding.client_execution_generation = sqlc.arg(execution_generation)::text
    JOIN elitea_runtime.input_bundle_entries AS entry
      ON entry.input_bundle_id = job.input_bundle_id AND entry.entry_id = binding.request_entry_id
     AND entry.semantic_role = 'agent.execution_request' AND entry.content_digest = sqlc.arg(input_digest)::bytea
    WHERE conversation.uuid = sqlc.arg(conversation_uuid)::uuid AND response.uuid = sqlc.arg(response_message_id)::uuid
      AND question.uuid = sqlc.arg(question_id)::uuid AND NOT response.is_streaming
      AND response.meta -> 'pipeline_static_tools_v1' = sqlc.arg(static_tools_json)::jsonb
      AND jsonb_typeof(sqlc.arg(static_pause_ids)::jsonb) = 'array'
      AND jsonb_array_length(sqlc.arg(static_pause_ids)::jsonb) BETWEEN 1 AND 16
      AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(sqlc.arg(static_pause_ids)::jsonb) AS selected(value)
          WHERE NOT EXISTS (SELECT 1 FROM jsonb_array_elements(response.meta -> 'pipeline_static_tools_v1' -> 'pauses') AS pause(value)
              WHERE pause.value -> 'proof' -> 'pause_id' = selected.value))
      AND response.meta ->> 'execution_generation' = sqlc.arg(execution_generation)::text
      AND response.meta ->> 'thread_id' = sqlc.arg(thread_id)::text
      AND NOT (response.meta ? 'output_limit_reached')
      AND (conversation.author_id = sqlc.arg(actor_user_id)::bigint
           OR (question_author.entity_meta ->> 'id')::bigint = sqlc.arg(actor_user_id)::bigint)
      AND (response_author.entity_meta ->> 'project_id')::integer = sqlc.arg(project_id)::integer
    FOR UPDATE OF response
), updated AS (
    UPDATE chat_message_group AS response
    SET meta = (response.meta - 'pipeline_static_tools_v1') || CASE
        WHEN EXISTS (SELECT 1 FROM jsonb_array_elements(response.meta -> 'pipeline_static_tools_v1' -> 'pauses') AS pause(value)
            WHERE NOT (sqlc.arg(static_pause_ids)::jsonb @> jsonb_build_array(pause.value -> 'proof' -> 'pause_id')))
        THEN jsonb_build_object('pipeline_static_tools_v1', (response.meta -> 'pipeline_static_tools_v1') || jsonb_build_object('pauses',
            (SELECT jsonb_agg(pause.value ORDER BY pause.ordinality) FROM jsonb_array_elements(response.meta -> 'pipeline_static_tools_v1' -> 'pauses') WITH ORDINALITY AS pause(value, ordinality)
             WHERE NOT (sqlc.arg(static_pause_ids)::jsonb @> jsonb_build_array(pause.value -> 'proof' -> 'pause_id')))))
        ELSE '{}'::jsonb END, is_streaming = TRUE,
        task_id = sqlc.arg(execution_id)::text, updated_at = clock_timestamp()
    FROM resolved WHERE response.id = resolved.id RETURNING response.id, response.uuid
)
SELECT updated.id AS response_message_group_id, updated.uuid AS response_message_id FROM updated;
