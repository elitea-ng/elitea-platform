-- name: GetCurrentAgentTaskStatus :one
-- The read half of the legacy `application_task` surface. It resolves the
-- durable execution from the RESPONSE MESSAGE, exactly the way
-- CancelCurrentAgentExecution does, so a caller can never name a foreign
-- execution id and read another project's run state.
--
-- The ownership predicate is byte-for-byte the cancel query's: the caller owns
-- the conversation, or the caller wrote the question this response answers.
-- A reader that could see more than the canceller could stop would be a
-- disclosure the cancel path already refuses.
SELECT job.state,
       job.desired_state,
       job.settled_at,
       job.terminal_error_code
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
  );
