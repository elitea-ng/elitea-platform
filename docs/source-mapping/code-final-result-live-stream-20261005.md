# Code final result live stream

## User outcome

The live chat shows one final answer without a reload.
The observer keeps the same execution through final result projection.
The final result releases Stop and retains the question link and continuation metadata.
The global Disconnected badge keeps its existing connection meaning.

## Source mapping

| Current behavior | Source | Candidate behavior |
| --- | --- | --- |
| Rust emits success progress before the result frame. | `services/elitea-worker-rust/src/agents/events.rs`, `finish_after_eos` | Preserve the final batch order. |
| Python emits the same success progress and result sequence. | `services/elitea-worker-python/src/elitea_worker/handlers/agent_events.py`, `emit_completed_response`, `emit_terminal` | Apply the same durable completion rule. |
| Main hides the result frame until terminal projection commits. | `services/elitea-main/internal/infra/db/repos/replay_events.go` | Keep the observer open through that delay. |
| Web closes at `pipeline_finish` or terminal `agent_response`. | `apps/elitea-web/src/features/chat-messages/model/useChatStreamTransport.ts` | Close the generation-bound observer at a root result, failure, or validated pause. |
| Web ignores ordinary `full_message`. | `apps/elitea-web/src/features/chat-messages/lib/chatStreamReducer.ts` | Reduce the complete result and its metadata into one answer. |
| Web requires an existing answer for final snapshots and result chunks. | `apps/elitea-web/src/features/chat-messages/lib/chatStreamTurnFrames.ts` | Create a missing answer from an identified final snapshot or valid result chunk. |
| Web can retain the optimistic answer identity. | `apps/elitea-web/src/features/chat-messages/lib/chatStreamShared.ts` | Resolve the final answer to its durable response identity. |

The candidate uses `chatStreamFinalResult.ts` for result reduction and root ownership checks.
Generation identifies a durable execution attempt. It does not identify the Worker language.
Legacy frames without a generation retain their terminal marker behavior.
Reconnects keep the same execution URL and last delivered cursor.
No execution lifetime timer, new admission, or transport polling is added.

## Verification

The regression fixtures follow current Worker source. They are not captured Run C SSE frames.
The saved baseline fails 12 of 14 new regression checks.
The candidate passes all 185 checks in seven focused files.
The checks include missing and existing answers, final batch replay, continuation metadata, generation fencing, and child ownership.
The checks include 75 seconds of quiet preparation and a same-execution reconnect during delayed projection.
Typecheck and focused lint pass.
These checks use an EventSource double and mocked HTTP. They do not prove browser or deployment behavior.

## Limits

Run C's exact streaming failure remains unproven because its receipt lacks SSE and browser callback evidence.
Referenced final results use one bounded read from the existing conversation history API.
The read requires the captured project, conversation, response ID, and generation.
The saved row must match that generation and have a settled streaming state.
The saved content must match the result reference size and SHA-256 digest.
The observer rejects late history responses after its run ownership changes.
An unavailable or unauthorized snapshot releases the observer and shows saved-result reload guidance.
The read never admits a new execution or polls for completion.
Main owns the replay cursor visibility barrier. Web cannot repair an already skipped hidden cursor.
Live browser acceptance and cohort pinning remain required.
