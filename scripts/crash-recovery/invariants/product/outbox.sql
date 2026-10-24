-- I4/I7: command outbox identity. Digests may not change; publish_attempts may grow (class I). Bound: :'exec_id'.
SELECT coalesce(json_agg(r ORDER BY r.generation), '[]'::json) FROM (
  SELECT o.outbox_id, o.generation, o.stream_name, encode(o.prepared_signed_envelope_digest, 'hex') AS prepared_digest,
         encode(o.published_envelope_digest, 'hex') AS published_digest, o.prepared_key_id, o.publish_attempts,
         o.published_at, o.authority_granted_at, o.retired_at, o.retirement_code, o.last_error_code
    FROM elitea_runtime.command_outbox o
   WHERE o.execution_id = :'exec_id'
) r;
