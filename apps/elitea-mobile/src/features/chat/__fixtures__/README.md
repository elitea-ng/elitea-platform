# Execution event-stream fixtures

These `.sse` files are test **data**: byte streams in the server's
`text/event-stream` framing (`id: <cursor>`, `event: <type>`, `data: <JSON>`,
blank line; `: heartbeat` comments). The chat reducer and the stream tests are
driven from them.

## Provenance (read this before trusting them)

They are **synthesised, not captured**. When they were written (2026-10-04)
no Elitea stack with a model was running on the authoring machine, so the
frames were composed by hand from the server behaviour observed by reading
(never copying) the elitea-platform sources at `e7585fee2`:

- framing, cursor ids and heartbeats: the execution events handler
  (`services/elitea-main/internal/api/v2/executions/events.go`);
- the five event names and the `execution.failed` / `execution.replay_reset`
  bodies: the durable replay repositories;
- NodeEvent field names and the turn-ending rules (delta chunks, the whole
  reply in `agent_response`, `finish_reason`, terminal vs fan-out-child
  pauses, `mcp_authorization_required` with `authorization_requests`): the
  worker's NodeEvent codec and the web client's documented stream semantics.

All content is placeholder text (`[tool output]`, `Hello, café ☕ world.`);
ids are invented. The multi-byte characters are deliberate (UTF-8 split tests).

**LIVE-VERIFY:** replace or extend them with real transcripts and re-run the
reducer tests. In particular confirm that `agent_llm_chunk.content` is a delta
and `agent_response.content` is the whole reply.

## Capturing a real transcript

Against a local stack or the LAN model rig, with a personal access token:

```sh
# 1. start a turn (prints events_url)
curl -sS -X POST \
  -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' \
  "$ORIGIN/api/v2/elitea_core/messages/prompt_lib/$PID/$CONV_UUID?execution_contract=agent.execute.application.v1" \
  -d '{"payload":{"user_input":"hi"},"project_id":'$PID',"participant_id":'$PARTICIPANT',"conversation_uuid":"'$CONV_UUID'","question_id":"'$(uuidgen | tr A-Z a-z)'"}'

# 2. record the stream from the start (the server never closes it: stop with
#    Ctrl-C after the terminal frame)
curl -sS -N -H "Authorization: Bearer $PAT" -H 'Accept: text/event-stream' \
  "$ORIGIN$EVENTS_URL" | tee capture.sse
```

Before committing a capture, scrub it: replace every `content`, `thinking`,
tool input/output and reference with placeholders, and replace user,
project, conversation and message identifiers with invented ones.
