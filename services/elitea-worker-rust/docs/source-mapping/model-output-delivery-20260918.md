# Durable delivery of large model answers

Status: focused checks and deployed browser acceptance pass on 2026-09-18.
Gate 4 remains open.

## Source mapping

| Source | Behavior | New implementation |
| --- | --- | --- |
| Current UI `src/components/Chat/hooks.js` stream handlers | Show model deltas, completed thinking steps, and the final answer. | Web `features/chat-messages/lib/chatStreamTurnFrames.ts` and `chatStreamThinkingFrames.ts` assemble bounded fragments. |
| Rust `transport/openai_compatible_facade.rs` and `anthropic_facade.rs` | Collect model text and reasoning. | A completed output has a separate 4 MiB ceiling. It no longer uses the browser event ceiling. |
| Rust `agents/events.rs` | Project model output into browser events. | Split large completed steps and final answers into UTF-8 fragments of at most 8,192 bytes. |
| Existing `tool_output_chunk_v1` contract | Validate offsets, lengths, digests, and exact redelivery. | `libs/proto/elitea/runtime/v1/node_event.proto` defines equivalent model and final-answer contracts. |
| Main `infra/db/repos/agent_trace.go` | Persist thinking steps in the existing trace records. | Store text and reasoning assembly metadata in the existing attributes. |
| Main `infra/db/repos/agent_stream_text.go` | Persist provisional answer text under execution and generation fences. | `agent_result_chunks.go` replaces intermediate text and saves assembly state in existing message metadata. |
| Main `infra/db/repos/agent_execution_results.go` | Commit the terminal answer and settle the response group. | Resolve and verify a final-result reference before removing provisional text and committing success. |
| Main `api/v2/mcp/execute.go::readTurnState` | Return the persisted answer after execution settles. | The external MCP reader continues to read the same final text rows. |

## Failure and correction

A completed 58,452-byte answer fails during event projection in chat 589.
JSON escaping expands the text to 61,001 bytes before event metadata.
The complete event exceeds its 61,440-byte browser limit.
The model context indicator does not measure this transport limit.

Keep the existing browser and protobuf event limits.
Use fragments for completed model text, reasoning, and final answers.
Verify the complete digest before accepting the final fragment.
Reject gaps, changed digests, changed lengths, and conflicting redelivery.

The first final-answer fragment replaces provisional intermediate output.
The terminal event carries a length and digest reference instead of repeating the large answer.
Main resolves that reference inside the terminal transaction, after inbox deduplication.
The transaction commits one final answer in the existing message tables.
No application database migration or new table is required.

Deploy Main and Web before the worker emits the new events.
Old inline events remain valid.

## Verification

- All 34 Rust event-projection checks pass.
- A projection check reconstructs escaped Unicode output and a 700,000-byte answer from bounded frames.
- All 42 provider-facade checks pass, including the separate completion, stream-byte, and event-count bounds.
- Focused Go repository checks pass with isolated PostgreSQL databases.
- A PostgreSQL test resumes an 825,000-byte answer through a fresh pool and repository instance.
- That test checks replay, incomplete assembly, wrong-generation access, and one final persisted answer.
- All 97 browser reducer and fragment checks pass. Web type checking passes.
- Rust formatting and Clippy pass with warnings denied.
- Output application, node-event codec, and external MCP unit packages pass.

The broader repository suite finds an existing internal-tool catalogue mismatch in deferred builder changes.
The SQL catalogue includes `skill_builder` and `project_context_builder`; the authorable catalogue does not.
This record does not claim that the broader repository suite passes.

## Deployed browser acceptance

Main and Web deploy before the worker. All three services retain their existing configuration, mounts, and networks.
The database remains `elitea_cutover_20260909_current`.
Fresh headed Playwright creates private synthetic chat 591 and sends the request through the UI.
The real OpenAI-compatible Haiku route returns 450 numbered records and a final marker.
No request is mocked.

The browser receives 75,629 answer bytes through ten final-result fragments.
It shows the final marker once and reports no page or execution errors.
A fresh browser verifies all 450 records in order, without duplicates.
Reload preserves the same visible answer.
PostgreSQL contains one settled answer row, with 75,629 bytes and no error flag.
The saved screenshot confirms the last records, final marker, and normal answer controls.

The first harness waits for `full_message` after the UI closes its stream on `agent_response`.
The corrected harness uses that terminal response. The verification does not repeat the model request.
This harness correction does not change product code.

| Service | Verified image digest |
| --- | --- |
| Main | `sha256:b809597b7942590335a15ac5f8331f7c3bbb2dbb79725abf4aa5a290bfbeac51` |
| Web | `sha256:e198c182cad3b78dda62e09634bc339e2d493589316fca1e21b8661acf8d98a5` |
| Rust worker | `sha256:109e0801e80aea97607ee96c492175f2748300a9a48740682c477ffb6219cfa8` |

## Remaining boundaries

The 4 MiB assembly ceiling is not proof of full-window capacity.
Provider request, stream, event, and worker-checkpoint limits remain separate acceptance gates.
Browser reconnection during incomplete final-answer assembly requires a separate check.
Full-mode million-token input and large dedicated-summary input remain open.
The live compaction schema failure is separate from this output-delivery correction.

Compaction follows context usage, not user-turn boundaries.
Repeated compaction uses the prior structured snapshot and new covered records.
Active instructions and exact source revisions remain outside the summary.
Compaction does not deactivate skills or authorize new work.
