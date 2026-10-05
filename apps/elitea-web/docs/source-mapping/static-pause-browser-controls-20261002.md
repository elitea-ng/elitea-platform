# Static pipeline continuation browser mapping

This layer consumes the versioned static occurrence contract. It uses the existing continuation route and trace store.

## Source contracts

The authoritative wire source is `libs/proto/elitea/runtime/v1/node_event_static_pause_v1.md`.
The owning Main consumer is the frozen static consumer, with the separate multiple-batch inventory amendment.

| Existing boundary | Browser implementation |
| --- | --- |
| Main `currentContinuationBody`, static arm | Closed OpenAPI root and selected-leaf request schemas |
| Main persisted `pipeline_static_v1` | `staticPipelinePause.ts::staticPauseBinding`, exact root occurrence |
| Main persisted `pipeline_static_tools_v1` | Same parser, bounded original leaf inventory and `(batch, ordinal)` uniqueness |
| Original response `meta.execution_generation` and `thread_id` | `convertMessagesToChatHistory.ts`, restored control identity |
| Signed root `full_message` metadata | `chatStreamStaticPause.ts`, validated static pause projection |
| Terminal pause stream handling | `chatStreamTurnEnd.ts`, closes only validated root static pauses |
| Main static continuation endpoint | Owning Orval `continueStaticPipeline` endpoint and schemas |
| Existing continuation transport | `resumeDetailed`, retains server refusal without socket fallback |
| Main chat and Pipeline Test | Shared `StaticPipelineContinuation` composition in `ChatBox.tsx` |
| Original Editor Test receipt | Exact response/generation, project/conversation, PAUSED phase and `can_control` checks |

The request echoes only the original response, project, conversation, thread and public pause selectors.
Generation remains a local control fence. Main derives its authority from the persisted original response and command.
The request never sends checkpoint, state, version, dependency, credential or grant selectors.

Root controls display the authored before/after boundary. Saved-pipeline controls select an explicit subset and retain unselected occurrences.
Dynamic approval fields remain separate. Static continuation creates no synthetic HITL history.
Descendant model events cannot clear the root static inventory. Root model replay clears previous controls.

Mounting or restoring controls submits no work. Superseded, streaming, terminal and read-only receipts expose no working controls.
Main still validates frozen versions, exact occurrences, authorization and atomic consumption.

## Required integration gates

This private browser slice does not prove deployed static execution.
The worker coordinator remains gated until its typed family contracts pass assembled tests.

The frozen Main inventory consumer permits an ordinary Agent root only.
Graph → ordinary Agent → saved-pipeline static leaves require an explicit root-kind and authority extension.
This browser rejects pipeline-root tool inventory until that extension is implemented and tested.
That route remains required Gate 5 work.

Required runtime acceptance includes before-resume once, after-resume without repeated effects, and loop occurrences.
Nested static pauses need exact descendant checkpoints and frozen version verification.
Selected leaves across original model batches must retain untouched static and dynamic siblings.
Restore and Run History selection must create no execution. Historical or superseded controls must refuse continuation.
Stop must retain the original Test response and generation while making its controls unavailable.
