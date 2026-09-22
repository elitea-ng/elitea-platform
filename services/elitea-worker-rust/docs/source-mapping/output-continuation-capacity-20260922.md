# Output continuation capacity

## Current-platform functional reference

Core revision `6c59068503cab7adecfce6bebc19ed8bfaf1af90` uses `utils/token_limit_continuation.py` and `rpc/chat_all.py` for explicit output continuation.
It reads persisted visible output and requests the missing ending.
It separates output exhaustion from HITL and MCP authorization resumes.
An empty visible answer remains eligible when reasoning exhausts the output allowance.
The original request constraints remain applicable across continuation calls.

UI revision `d067a728a6a0a26c1f07d304fe066bd38bf39d98` handles completion metadata in `src/components/Chat/hooks.js`.
SDK revision `966526e8334354366dd161b606d73fe8e204b850` separately handles nested output in `runtime/tools/llm.py::_continue_nested_output`.
That nested behavior requires its own Rust parity check; increasing admission capacity does not establish that parity.

## Confirmed mismatch

Completed model answers support 4 MiB, but Main and Rust reject continued partial answers above 64 KiB.
New tests reproduce rejection at 65,537 bytes before the correction.
The failure does not indicate that the model context is full.

| Boundary | Implementation |
| --- | --- |
| Persisted visible answer and continuation identity | Main `internal/db/queries/agent_chat.sql`, output-limit resolution query |
| Admission and immutable request | Main `internal/application/agentexecution/continue.go`, `input_bundle.go` |
| Protobuf and scalar decoding | Rust `src/agents/protocol.rs` |
| Supported continuation profile | Rust `src/agents/assembly.rs` |
| Missing-ending model instruction | Rust `src/agents/session.rs::output_continuation_prompt` |
| Saved prefix, overlap handling, and recovery projection | Rust `src/agents/events.rs` |

Main and Rust now accept a partial answer up to the existing 4 MiB completed-answer limit.
Rust decodes continuation text as a bounded scalar instead of applying the generic control-field JSON limit.
This does not widen unrelated control fields or permit arbitrary nested JSON.
The complete encoded input retains its separate 8 MiB limit, including history and other fields.
JSON escaping and other request content still count toward that aggregate limit.
Model context admission and compaction remain separate from byte-capacity admission.
No migration, table, or protobuf field is added.

## Verification boundary

Eight Rust continuation checks and all 17 agent-input wire checks pass.
The Main application package passes. Rust Clippy passes with warnings denied.
They cover large partial admission, byte limits, malformed text, Unicode, wire round trips, and exact recovery-prefix reconstruction.
The wire suite's obsolete 1 MiB assertion now uses the existing 8 MiB input contract.
The correction requires deployment and fresh browser verification of Continue, appended output, and reload.
This record does not claim deployed acceptance or completion of point 4.

## Rehearsal deployment

The corrected Main image is `sha256:9bfea8353ac09b53a6a9cb6bbd670789f826339aae3c7f86cd88cc997615c3c8`.
The corrected Rust image is `sha256:8c11be66261e0e7e0885a13335cd5483a56316693dd28c85c1e76234358f7031`.
Main retains its deployed source baseline with only the continuation-capacity change.
Rust uses commit `9012713f` through the auditable release build.
The replacement preserves environment, mounts, networks, and resource limits.
No execution holds an active claim during either replacement.

Initial synthetic chats 622 and 623 fail admission because the fixture selects temperature zero.
The current Main admission contract requires a positive temperature.
These requests do not execute a model or test continuation.
The corrected fixture uses temperature 0.2 and starts chat 624.
The real Haiku route produces an 81,472-byte partial answer and requests Continue.
Fresh browser reload preserves that partial answer exactly.
Continue returns HTTP 422 before another model execution.
This is failed end-to-end acceptance, despite passing capacity component checks.

Read-only database inspection finds Haiku and 16,000 output tokens on the user participant.
The assistant participant has no saved model settings.
`ResolveCurrentAdhocTurn` reads `target_mapping.entity_settings`, which refers to that assistant participant.
The initial request supplies model settings explicitly; `currentContinuationInput` supplies an empty settings object.
`currentAdhocSnapshot` then rejects the missing model name.
The current context-policy restoration occurs later and restores only context settings and the summary model.
The repair must recover the admitted task-model selection and reauthorize it before continuation assembly.
Do not populate the assistant settings merely to bypass this reproduction.
Reuse chat 624 for the corrected Continue check instead of regenerating its partial answer.

The original execution is `bda319ba17e5e73ef47f6882f5e1f9da`.
Browser evidence uses the local `elitea-continuation-live` prefix.
The failure screenshot is inspected visually.

## Separate nested-output audit

Rust `application_tools.rs::drain_child` selects final child text without inspecting the output-limit finish reason.
`model_scope.rs::successful_terminal` also lacks an explicit output-limit exclusion.
The current SDK's nested continuation behavior requires a dedicated reproduction and durable implementation review.
Do not treat the child compaction and crash-recovery proofs as proof of output-exhaustion continuation.

## Admitted model selection correction

Main now loads the digest-verified original input before continuation assembly.
`FrozenContextPolicy.TaskLLM` carries the original runtime selection from existing immutable input storage.
`continuationAdhocSettings` selects only the model name, project, output allowance, reasoning effort, and temperature.
It does not restore credentials or cached provider transport settings.
The normal model catalog and freezer reauthorize that selection for the current caller.
A different catalog fallback model or project is rejected.
Changed participant settings cannot add new generation options to the continued request.
Context settings and the dedicated summary model retain their original restoration behavior.

The application package passes regressions for empty participant settings, changed settings, denied models, and catalog identity drift.
Both PostgreSQL policy tests pass against isolated databases.
They verify exact model recovery, actor/project/conversation/generation isolation, and digest corruption refusal.
The Main deployment is `sha256:59e8a6d182f4c19003962bf50472833c85ecc5c19b83033bd42160f0b0a3dfdf`.
A fresh browser retries chat 624 without changing its participant settings or regenerating its partial answer.
Continue returns HTTP 200, and execution `984a788894e6d87ddb9fbb3dee2c9dcd` completes the missing output.
Fresh saved-history inspection finds 700 ordered records and one terminal marker in a 117,629-byte rendered answer.
The live answer incorrectly shows only the 215 newly completed record headings.
This identifies a separate browser fragment-projection defect.
The initial harness also includes the Continue button label in its partial-text comparison.
The corrected harness removes that control label before comparing answer text.

Web `features/chat-messages/lib/chatStreamTurnFrames.ts` replaces interim output when final-result fragments arrive.
For continuation, it must prepend the fixed answer prefix captured at `agent_start`.
`convertMessagesToChatHistory.ts` now carries that transient prefix separately from the fragment buffer.
Exact fragment replay does not duplicate the prefix, and regeneration clears it.
All 97 reducer tests and TypeScript checking pass.
Go vet passes for the changed application and repository packages.
Deployed UI acceptance passes after the saved-history correction below.


## Exact saved-history continuation

Main `infra/db/repos/conversations.go::ListMessages` previously inserted a newline between all text items.
Streaming and external MCP concatenate continuation fragments without this separator.
The saved `output_limit_sequence` identifies output-continuation groups.
Their text items now concatenate exactly, with deterministic item ordering.
Ordinary message groups retain their existing newline separator. No schema change is required.
The PostgreSQL regression fails before the correction and passes afterward for both cases.

Chat 625 uses Haiku with a 16,000-token output allowance.
Execution `90fc91cae7924f016f76198390038322` stops at the output limit with 81,460 visible bytes.
Reload preserves that partial answer. Continue returns HTTP 200.
Execution `b6e898422763e99bc80853f42413f0e9` completes with 117,625 visible bytes and a normal stop.
The live answer contains 700 ordered records, the exact original prefix, and one ending marker.
A fresh headed Playwright browser verifies exact saved-history equality after trimming trailing DOM formatting.
It does not normalize internal whitespace or the continuation seam.
The rendered screenshot confirms the final records and ending marker without a runtime error.

Main deployment: `sha256:48f9bd6c0f350dc2914ade7894f35a4d9898c8841f6ccc7552cb76dc0b09f406`.
Web deployment: `sha256:06eb02816b7618fdeceb68053656f888e39965504f3392d2e2f3ffee5b4fbf03`.
Worker deployment: `sha256:8c11be66261e0e7e0885a13335cd5483a56316693dd28c85c1e76234358f7031`.
The Main replacement preserves its environment, six mounts, networks, and resource limits.
This proves direct chat output continuation and reload. Replacement during continuation and nested output-exhaustion recovery remain unverified.

## Nested output exhaustion investigation

Current SDK reference: `966526e8334354366dd161b606d73fe8e204b850`.
`elitea_sdk/runtime/tools/llm.py::_continue_nested_output` detects a nested response stopped by length.
It requests continuation, validates the answer boundary, and returns the completed response to the graph.
It rejects no-progress responses and invalid continuation boundaries after bounded retries.
These are behavioral requirements. The Rust implementation must also preserve claim-fenced recovery.

Rust `agents/application_tools.rs::ApplicationAgentTool::drain_child` currently accepts the truncated child response as its final result.
`agents/model_scope.rs::successful_terminal` also previously generated a completion receipt for `FinishReason::MaxTokens`.
A new regression reproduces that false receipt before the correction.
The local correction accepts only an absent legacy finish reason or `FinishReason::Stop` for a completion receipt.
All 12 model-scope tests pass, including the PostgreSQL child-scope takeover test.
This correction is not deployed and does not implement automatic continuation.

ADK 2.2.0 `vendor/adk-agent/src/llm_agent.rs` ends its loop when the response has no function calls.
Its per-chunk after-model callbacks do not provide a continuation-loop decision.
The Rust `ModelCheckpointWriter::restore_validated` rejects a model checkpoint followed by persisted model content.
Therefore a truncated response requires an explicit durable continuation boundary before another model request.
Do not weaken this recovery check or restart the child task from its original input.

The next implementation must retain the accumulated answer, pending continuation request, and attempt identity through existing session storage.
It must apply the same model authorization, context budgeting, compaction, cancellation, and claim fencing to each continuation request.
Only the completed answer becomes the parent tool result and child completion receipt.
Verify interruption before checkpoint commit, after checkpoint commit, and after final completion before parent delivery.
Verify both visible truncation and reasoning-only output exhaustion.
Deployed browser acceptance and worker-loss verification remain required.

## Automatic child continuation candidate

`agents/model_scope_output.rs` wraps the already authorized child model through ADK's `Llm` interface.
It intercepts output exhaustion before ADK returns the child result.
Ordinary tool-call completion markers remain intact.
Only accepted continuation text extends the final child answer.
The next response must repeat a bounded exact tail before adding text.
The validator handles UTF-8 and tails split across streamed chunks.
A mismatched tail, missing terminal response, or no visible progress fails explicitly.
Reasoning-only exhaustion receives a bounded request for visible output.

`agents/model_checkpoint/output.rs::OutputContinuation` stores the accepted prefix and continuation round.
`ModelCheckpointWriter` includes this optional field with the next prepared request in the existing checkpoint event.
Ordinary checkpoints omit the field, preserving their previous serialized shape.
Older workers reject checkpoints with this new field. Do not roll back an active continuation to an older reader.
No table or protobuf change is required.
The normal context preparation path still applies budgeting, compaction, and claim fencing before every request.
Recovery restores the exact pending request and accepted prefix before provider dispatch.
The parent receives only the completed child answer.

The continuation uses the child's admitted model-turn allowance.
It does not impose the SDK's separate four-continuation ceiling or a cumulative 64,000-output-token cap.
The existing four-MiB completed-answer and durable-storage bounds remain enforced.
The provider facade also retains its admitted request-count limit.

The private durable-completion adapter now exposes the combined answer to scope persistence.
This matters because ADK reconstructs streamed text after the scope's persistence hook.
The provider's own snapshot contains only the latest segment.
Do not repeat the full answer on the live terminal chunk to repair persistence.

Component verification covers ordinary delegation, repeated continuation, exact seams, reasoning-only exhaustion, and admitted turn limits.
A PostgreSQL test replaces the root claim after the continuation checkpoint commits.
The old writer is rejected. The replacement calls the model once for the pending continuation.
The ADK test returns one completed answer after multiple continuations and verifies its durable receipt content.
All 401 agent tests pass with PostgreSQL enabled.

The candidate is deployed below. Browser acceptance remains open.
Live provider seam behavior, worker loss during a continuation request, and parent delivery still require rehearsal verification.
The current composition attaches this wrapper through `ScopedModelCheckpoint`.
Explicitly disabled legacy context plans bypass that scope; their automatic continuation remains an open composition check.

## Nested browser failure and transport snapshot ownership

Commit `0103d7c3` deploys as worker image `sha256:eeb175ea2c3c9e2499b5dc297233b80e8aa0d2e1abff14578b42ed80dda14866`.
The replacement retains its environment, five mounts, networks, and resource limits.
Fresh headed Playwright runs chat 627 with parent application 46 and child application 45.
The parent allows 4,000 output tokens. The child allows 512 and must return 120 ordered records.
Execution `36b811a2859dd1980678fd63fa1e52ef` fails after saving continuation round one.
The worker reports `model_gateway.completion_reused` from the child transport.
The browser displays the terminal error. No completed child result reaches the parent.

The OpenAI-compatible transport treats every recorded answer as a final completion.
Its second segment cannot replace the previous output-limited snapshot.
The native Anthropic transport has the same completion guard.
Tests must cover both real stream parsers across repeated length terminals and the final stop.
A final stop must still reject a duplicate completion.
This failure does not concern the compaction trigger or context capacity.

The transport correction marks output-limited completion snapshots as replaceable.
A subsequent segment replaces that snapshot; the scoped adapter still owns the combined durable answer.
A normally completed or consumed snapshot remains protected against replacement.
This preserves direct chat truncation snapshots and existing completion-consumption rules.
The OpenAI-compatible regression reproduces `model_gateway.completion_reused` before this correction.
Both transport regressions parse two output-limited segments, a final stop, and a rejected duplicate final response.
The correction requires a new image and browser rerun before acceptance.

All 47 provider-facade tests pass after the transport correction.
The suite also corrects two stale summary tests that assumed the retired two-call summary ceiling.
They now verify three independent summaries without consuming the ordinary chat model allowance.
Strict Clippy passes for the transport correction. Browser rerun remains pending.

## Boundary verification follow-up

Commit `4772fa43` deploys as worker image `sha256:e1848e8f9380d1fe4d5afb78733e28106ff4a7bdd13813d6ab1b9b83906e3671`.
Fresh headed Playwright runs chat 628 with the same saved parent and child.
Execution `23c7cacc903b8dee27679ab1c3fb25d9` passes the previous provider snapshot failure.
Its child persists continuation round two and accepted records through 81.
The subsequent response fails with `model.output_continuation_failed`.
No final child receipt or successful parent answer is claimed.
The rejected provider text is not persisted, so its exact boundary mismatch remains unconfirmed.

The SDK reference uses an explicit anchor-copy instruction and bounded boundary repair.
The Rust instruction now states that an anchor can start or end inside a word.
It requires exact copying before any new text.
Mismatch diagnostics report byte counts, never provider text or the accepted anchor.
Anchor derivation and checkpoint interpretation remain unchanged for in-flight recovery.
Browser acceptance, boundary repair, and interruption testing remain open.
