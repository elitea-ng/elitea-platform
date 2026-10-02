-- name: FinalizeCurrentAgentMixedPause :execrows
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = meta || jsonb_build_object(
        'thread_id', sqlc.arg(thread_id)::text,
        'hitl_interrupt', sqlc.arg(hitl_interrupt)::jsonb,
        'hitl_interrupts', sqlc.arg(hitl_interrupts)::jsonb,
        'authorization_requests', sqlc.arg(authorization_requests)::jsonb,
        'is_error', FALSE,
        'error', '',
        'invoked_skills', sqlc.arg(invoked_skills)::jsonb
    ),
    updated_at = clock_timestamp()
WHERE id = sqlc.arg(message_group_id)::bigint;
