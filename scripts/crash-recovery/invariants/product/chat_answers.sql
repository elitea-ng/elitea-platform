-- I1/I8/I9: the chat transcript holds exactly one assistant answer group for the execution. The response
-- group is written at admission with task_id = execution_id (queries/agent_chat.sql). Its uuid is the
-- "Message ID" the failure reference shows (agent_execution_jobs.client_message_id). Only flags, codes,
-- counts and lengths are read. Bound: :'exec_id', identifier :"chat_schema" (p_<projection_project_id>).
SELECT coalesce(json_agg(r ORDER BY r.id), '[]'::json) FROM (
  SELECT g.id, g.uuid::text AS message_id, g.is_streaming, g.meta ->> 'execution_generation' AS execution_generation,
         coalesce((g.meta ->> 'is_error')::boolean, false) AS is_error, g.meta ->> 'error_code' AS error_code,
         (SELECT count(*) FROM :"chat_schema".chat_message_items i WHERE i.message_group_id = g.id
            AND i.item_type = 'text_message'
            AND coalesce(i.meta -> 'runtime_stream_provisional', 'false'::jsonb) <> 'true'::jsonb) AS final_text_items,
         (SELECT coalesce(sum(length(t.content)), 0) FROM :"chat_schema".chat_message_items i
            JOIN :"chat_schema".chat_messages_text t ON t.id = i.id
           WHERE i.message_group_id = g.id AND i.item_type = 'text_message') AS text_chars
    FROM :"chat_schema".chat_message_group g
   WHERE g.task_id = :'exec_id'
) r;
