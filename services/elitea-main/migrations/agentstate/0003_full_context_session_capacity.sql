-- Model recovery state can contain the complete prepared request.
-- Preserve the 1 MiB application and user state bounds.
ALTER TABLE elitea_runtime.agent_sessions
    DROP CONSTRAINT agent_sessions_state_size;
ALTER TABLE elitea_runtime.agent_sessions
    ADD CONSTRAINT agent_sessions_state_size CHECK (
        octet_length(state) BETWEEN 2 AND 8388608
    );

-- Events can contain both recovery state and model content.
ALTER TABLE elitea_runtime.agent_session_events
    DROP CONSTRAINT agent_session_events_size;
ALTER TABLE elitea_runtime.agent_session_events
    ADD CONSTRAINT agent_session_events_size CHECK (
        octet_length(event_payload) BETWEEN 2 AND 16777216
        AND payload_bytes BETWEEN 2 AND 16777216
    );
