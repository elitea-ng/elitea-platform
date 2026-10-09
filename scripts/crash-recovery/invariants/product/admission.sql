-- I1: one admission for the client question (no replacement execution). Bound: :'exec_id'.
-- The (stream, message) pair is read from the execution's own admission row, then counted.
SELECT json_build_object(
  'rows', coalesce((SELECT json_agg(json_build_object(
            'execution_id', a.execution_id, 'generation', a.generation,
            'client_stream_id', a.client_stream_id, 'client_message_id', a.client_message_id,
            'client_execution_generation', a.client_execution_generation))
          FROM elitea_runtime.agent_execution_jobs a WHERE a.execution_id = :'exec_id'), '[]'::json),
  'same_question_executions', (
    SELECT count(DISTINCT o.execution_id) FROM elitea_runtime.agent_execution_jobs o
      JOIN elitea_runtime.agent_execution_jobs a
        ON a.client_stream_id = o.client_stream_id AND a.client_message_id = o.client_message_id
     WHERE a.execution_id = :'exec_id'));
