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

## Exact suffix validation

Commit `e333b49e` deploys as image `sha256:33b4603ac90ea227343662ee9986412e2c7eea5979808bbeb064e0cc16caa9de`.
Chat 629, execution `60a2198537a36cb60af2fa821f502dde`, still rejects a later boundary.
The safe diagnostic reports zero matched bytes for the 256-byte anchor when a three-byte chunk arrives.
The stronger instruction alone does not establish acceptance.

SDK `_merge_anchored_continuation` permits exact suffix overlap and an exact final-line overlap.
Rust now verifies this behavior before releasing the initial continuation bytes.
It prefers the longest exact overlap and rejects incoming preambles or changed boundary text.
Longer suffix matches must start at a word boundary. The final-line fallback requires four alphanumeric characters.
The validator never modifies the accepted prefix or normalizes whitespace.
It buffers the initial response until the anchor length is available, or until the response ends.
Later chunks pass through without an additional copy.
Existing checkpoint fields and anchor derivation remain unchanged.
Tests cover split chunks, repeated anchors, suffix-only responses, invalid edits, and preservation of the full accepted prefix.
Live acceptance and bounded repair of responses without an exact overlap remain open.

The final candidate passes all 405 agent tests with PostgreSQL enabled. Strict Clippy and formatting checks pass.
This is component proof. The candidate still requires a new image and fresh browser acceptance.

## Continuation request-history correction, 2026-09-23

Image `sha256:04dc8b70a30942e09decee4530b22da430e86172653546692fe8e519518a70e4` contains commit `769909a9`.
Chat 630, execution `9b39dca5a6da3c01fbe1efeeef6673f8`, persists round three and records through 119.
Its accepted prefix ends with the partial text `RECORD`.
A later response ends after 71 bytes without an exact overlap, so the worker rejects it.

An authenticated gateway probe reuses the synthetic saved request and child instructions.
Haiku returns `120: Cedar archive verification remains complete.` and the ending marker, without an anchor or leading space.
Direct concatenation would produce `RECORD120`, changing the requested format.
A comparison probe replaces earlier continuation exchanges with one accumulated answer and the latest protocol instruction.
That response includes the exact anchor, correctly completes record 120, and ends with the required marker.
This comparison supports correcting request construction rather than weakening boundary verification.

SDK `_continue_nested_output` builds each continuation from original messages, accepted output, and one current protocol instruction.
Rust `OutputContinuation::request` now collapses its own prior continuation exchanges into one assistant content entry.
It retains the original history and keeps the latest continuation instruction only.
The round count bounds removal of protocol exchanges, including checkpoints from the earlier implementation.
It does not reinsert accepted text already removed from model history by compaction.
The separate durable prefix still preserves the full user-visible answer.
Tests cover repeated request construction and preservation of compacted history.
The correction requires deployed browser verification before acceptance.

The request-history correction passes 407 agent tests with PostgreSQL enabled, strict all-target Clippy, and formatting checks. The two focused history tests pass. Browser acceptance remains pending.

## Continuation parity audit, 2026-09-23

Current SDK reference `elitea_sdk/runtime/tools/llm.py::_continue_nested_output` uses four automatic continuation rounds, one bounded retry for an invalid join, and explicit word-target/sentence-boundary completion. It never increases the caller's configured output allowance. Each request carries original messages, the accepted answer, and one latest continuation instruction. Provider errors, no progress, invalid joins, and exhausted attempts raise `OutputContinuationExhausted` with partial output.

Current UI reference `src/[fsd]/features/chat/lib/helpers/continuationError.helpers.js::normalizeContinuationError` recognizes `output_continuation_exhausted`, preserves partial output, and provides an explicit incomplete-response message.

The Rust request-history correction does not complete this parity requirement. Remaining checks and implementation are:

- Resolve the automatic-round policy explicitly: the candidate currently follows admitted model-step capacity rather than the SDK's separate four-round bound.
- Preserve the configured output cap, constrain the prompt to the remaining original task, and verify completion without repeated or expanded content.
- Add bounded invalid-boundary repair without accepting or persisting an unverified suffix.
- Project continuation exhaustion and no progress as an explicit incomplete-response result with accepted partial output, rather than the generic runtime error.
- Prove the nested answer and completion receipt after normal execution and worker restart through a fresh browser.

These are open requirements; component test success is not continuation acceptance.

Commit `645ae550` is deployed as worker image `sha256:ea21cb72ececf63c2192869ffe21107bd4cde00368776267fa90a49d40c5d649`. Deployment preserves the existing environment, five mounts, networks, and resource limits. Fresh headed-browser chat 631 exercises the same 512-token child and 120-record request. Execution `ed3c901ec0f75142aba1234e8769f6f0` passes the headed-browser check: all 120 records appear once, the ending marker appears once, no terminal failure occurs, and reload preserves the rendered answer. PostgreSQL contains one completed child receipt with all 120 records and one marker. The parent adds an introductory sentence; its complete response is therefore not byte-identical to the child result. The child report itself is intact. The rendered final screenshot was inspected. Worker-restart verification remains open.

### Worker loss during output continuation

Fresh headed-browser chat 632, execution `f17862e94cbd79e0ca7db48a8ce2b900`, confirms output continuation round two in PostgreSQL before killing the rehearsal worker with SIGKILL and starting it again. The original browser session receives the recovered final answer: records 001–120 occur once in order, the ending marker occurs once, there are no browser errors or terminal execution failures, and reload preserves the rendered result. The final screenshot was inspected. PostgreSQL contains one completed child receipt with all 120 records and one marker. The parent adds an introductory sentence here too; the child report remains intact.

This proves recovery after a persisted continuation boundary for this nested-agent fixture. It does not prove recovery from every provider-stream position or close the remaining continuation parity items above. No database schema change was required.

## Registered incomplete-continuation failure

Legacy business reference: SDK `OutputContinuationExhausted` and UI `normalizeContinuationError` distinguish an unfinished response from a generic runtime failure. The replatform adds `RUNTIME_ERROR_CODE_V1_OUTPUT_CONTINUATION_EXHAUSTED` (13) to `libs/proto/elitea/runtime/v1/errors.proto`, using the existing error envelope without changing persistence schemas.

Rust `ApplicationEventFailure::OutputContinuation` preserves the static error identity through the nested fatal channel. `native_agent_lifecycle::model_failure` maps it to a dedicated terminal kind. `protocol/output.rs` registers its safe message and recognizes it during durable output restoration. The code is not valid as a control-plane rejection.

Main `transport/runtimegrpc/output::runtimeFailurePolicyFor` accepts the registered code/message pair and projects `OUTPUT_CONTINUATION_EXHAUSTED` through the existing failure path. The browser's `runtimeFailureReason` and `recordStreamFailure` already show this message while retaining streamed content. Detailed provider text is not added to the safe message. This does not yet guarantee that every failed child's unstreamed partial output is delivered to its parent.

Deployment order: deploy Main with the new registered error before a worker that can emit it; an older Main deliberately rejects unknown codes. Generated Go/Python bindings use the pinned repository generator versions. Rust build dependencies explicitly include each input proto to avoid stale bindings during incremental development.

Component tests and fresh deployed-browser failure acceptance are tracked separately. Bounded join repair, round-policy alignment, and failed-child partial-result handling remain open.

Validation of the registered-error candidate: 408 PostgreSQL-enabled agent tests pass, including preservation through the nested fatal channel. The focused Rust continuation suite passes 20 tests (its standalone PostgreSQL case was separately covered by the enabled full agent suite), including canonical failure restoration and rejection of injected message text. Main output transport/application suites pass. The UI settle suite passes nine tests, including preservation of streamed partial content with the explicit incomplete-response message. Strict all-target Clippy and formatting pass. The candidate is not yet deployed; live failure rendering and reload remain unverified.

### Registered-error deployment and fixture correction

Commit `cac24c2e` is deployed to Main (`sha256:d2ae5d06ffe364d0730fc32aea9c9e015fec9d28bc8a7e93b7162ae5ecefad7b`) before the worker (`sha256:3dacf295f95c930bade05c281f5b7ade3468e9c29c5c489b93715588e906f5c1`). Existing environments, mounts, networks, and limits are preserved.

Chat 633 completed its 120-record task rather than failing: its child saved `meta.step_limit=1` does not override the admitted parent's 30-step limit. `OrdinaryNoToolProfile` child construction intentionally uses `fallback.step_limit`; the fixture therefore did not establish exhaustion. This is not failure-path acceptance. Chat 634 instead sets the admitted parent/conversation limit to one, with the same 512-token child output cap.

Chat 634, execution `4aa270fc357b13d045ac77660a1244ce`, passes the registered failure path in a fresh headed browser. SSE carries `OUTPUT_CONTINUATION_EXHAUSTED` with the registered sentence and `retryable=false`; the sentence is visible in chat and unchanged after reload. No browser errors occur. The rendered screenshot was inspected. PostgreSQL contains one child model session and zero completed-child receipts. The final parent response contains the error sentence only: this verifies error projection and persistence, not delivery of the failed child's partial answer.

## One durable boundary-repair attempt

SDK functional reference: `_continue_nested_output` permits one invalid-seam retry and strengthens `_build_output_continuation_prompt` for that retry. Rust `model_scope_output::generate` now rejects the malformed prefix before releasing it, drops that provider stream, and prepares one stricter request through the ordinary budget/compaction/checkpoint hook. It keeps the accepted prefix unchanged and does not insert rejected text into history. The retry consumes an admitted model call.

`OutputContinuation.repair_used` records the consumed allowance together with the prefix, round, and exact pending request. Older checkpoints default this field to false; a second boundary mismatch after recovery fails with the registered incomplete-continuation result. Readers predating this optional field reject repaired checkpoints because their checkpoint structs reject unknown fields; avoid rolling back a worker while such invocations are pending. No database migration is added.

`DurableModelCompletion::discard_unaccepted` explicitly clears an unaccepted provider snapshot before retry. Native Anthropic and OpenAI-compatible adapters implement it, refusing to reset an already consumed completion. The default refuses the operation for an adapter that has not opted in. Ordinary completed-response reuse remains rejected.

Tests cover successful repair without rejected text, repeated mismatch failure, exact request restoration with the repair allowance already consumed, and provider replacement only after explicit discard. Live browser/restart acceptance of the repair path remains pending. Automatic-round policy and failed-child partial-result handling remain separate open items.

Component validation: 411 PostgreSQL-enabled agent tests pass, including claim takeover after a persisted repair request. The replacement restores the exact request and the consumed repair allowance; the old writer is fenced out. Both provider facade suites pass 49 tests, including explicit discard/replacement and refusal to reset a consumed completion. Live repair-path acceptance remains pending.

## Four-call continuation policy (2026-09-23)

The user selected the current SDK's four automatic continuation calls, including
one possible boundary repair, rather than coupling retries to the agent step limit.
The initial answer call is not one of those four calls. Each call retains the
configured output cap and context admission checks. No-progress and a second
invalid boundary still terminate early with the explicit incomplete-response error.

Rust `agents/request.rs::MAX_OUTPUT_CONTINUATION_CALLS` owns the fixed allowance.
`agents/model_scope_output.rs` enforces it for both normal continuation and repair.
The existing durable round is restored after interruption: recovery at round four
allows only that pending fourth call, not a fresh allowance. Older saved rounds
above four fail explicitly before provider dispatch; checkpoint decoding remains
compatible. No persistence schema change is needed.

`agents/application_tools.rs::LazyNestedAgent::bind_model` reserves four additional
provider calls for checkpointed child final-answer continuation. ADK's logical
`max_iterations(step_limit)` stays unchanged. The shared provider admission ceiling
is correspondingly the maximum logical steps plus four, used by both native
Anthropic and OpenAI-compatible routes. Continuation cannot introduce tool calls;
it completes the child's final answer rather than adding agent/tool iterations.
Explicitly disabled context-management children still bypass this scoped path;
that pre-existing coverage gap is not closed by this policy change.

Focused tests cover completion on the fourth continuation, no fifth dispatch,
repair consuming the same allowance, and recovery retaining only the remaining
allowance. Deployment and fresh browser acceptance of this policy are still pending.

### Repair browser evidence correction

Chat 635 (execution `c7206765a9b8f6edc024b3d5c0489eb9`) on the prior repair image
completed all 120 records and survived reload, but did not emit a boundary mismatch
or repair-checkpoint event. The synthetic refusal instruction did not exercise
repair. This is ordinary continuation regression evidence only, not live repair
acceptance. The PostgreSQL repair takeover tests remain component evidence.

## Four-call policy rehearsal acceptance

Commit `31e5a5ab` passes 413 PostgreSQL-enabled agent tests, 154 transport tests,
and strict all-target Clippy. Rehearsal worker image
`sha256:7f015015797b13f4275b8c9e3e36f6469ec4ee37b4326790f6e053eedc7063ed`
contains that committed source; the replacement preserved all five mounts,
credentials, networks, and resource settings.

Fresh headed Playwright chat 636 (execution
`5e37f59bb1702fda8e0fe1484ea310d0`) uses Haiku, a 256-token child output cap,
and a two-step agent limit. It completes after two additional continuation calls
(three child model-context measurements). All 40 ordered records and one ending
marker reach the parent, with no browser errors and exact persisted reload.
PostgreSQL contains one completed child-result receipt with those 40 records.
The parent adds an introductory sentence; its full response is not byte-identical
to the child result. The worker does not spend the unused third/fourth continuation
allowances. Artifacts: `elitea-continuation-four-result.json`,
`elitea-continuation-four-durable-proof.json`, and
`elitea-continuation-four-complete.png` in the local temporary verification directory.

The current SDK reference `elitea_sdk/runtime/tools/llm.py::_continue_nested_output`
also exits when the provider no longer reports an output-length finish. Four is a
hard upper bound, never a required number of calls. The Rust scoped loop returns
on a successful terminal response using the same behavioral rule.

Fresh headed Playwright chat 637 (execution
`ad2ad8b77b7443a557f89318b4704c66`) requests 120 records with the same 256-token
child output cap and two-step agent limit. Five child model-context measurements
show the initial call plus all four allowed continuations. The run stops with
`OUTPUT_CONTINUATION_EXHAUSTED` and the specific incomplete-response sentence,
without a browser error. The error survives reload. PostgreSQL contains one child
session and zero completed child-result receipts. Artifacts:
`elitea-continuation-cap-result.json`, `elitea-continuation-cap-durable-proof.json`,
and `elitea-continuation-cap-complete.png` in the local temporary directory.
Both screenshots were inspected. These cases accept early completion and cap
exhaustion; they do not close live repair or failed-child partial-result delivery.

## Incomplete child output remains inspectable

Current SDK `966526e8334354366dd161b606d73fe8e204b850` carries accepted partial
text on `OutputContinuationExhausted` in `runtime/exceptions.py` and
`runtime/tools/llm.py`. Indexer `c048daabef6c59106f96a7a36d094fe79fc149e8`
projects that text for the user in `utils/funcs.py`. UI
`2a3b14f93aa6c821f9ca13f06ea15730c2d59ee7` renders an expandable partial response
in `features/chat/ui/error-trace/ContinuationError.jsx` under its `[fsd]` tree.
Core `6c59068503cab7adecfce6bebc19ed8bfaf1af90` explicitly strips partial text
from parallel-parent error messages; its parallel-dispatch test requires this.
Therefore, inspectable partial evidence must not become a successful child tool
result or an instruction for the orchestrator to complete the failed answer.

Rust `agents/events.rs::preserve_incomplete_output` snapshots already accepted
visible text from active model projections, recursively preserving descendant
call identity. It uses the existing chunked `thinking_steps` persistence contract
and labels the generation `Incomplete response`. It emits neither an agent answer
nor a tool result; completed sibling steps remain unchanged. Repeated local
finalization emits no duplicate snapshot. Reasoning text and rejected seams are
not added to the snapshot.

`execution/native_agent_lifecycle.rs` publishes and acknowledges these trace
frames before the registered continuation failure. Failed publication does not
acknowledge terminal completion, so the existing claim/replay path retains
ownership. No application-table migration or new wire field is introduced.
The existing UI execution-step renderer is used; the top-level failure remains
explicit and no child completion receipt is synthesized.

Two focused tests cover descendant identity, explicit incomplete labeling,
no successful-answer projection, idempotence, and bounded UTF-8 fragments.
The PostgreSQL-enabled agent suite (415 tests), execution suite (143 tests), and
strict all-target Clippy pass. Deployed browser verification remains pending.

The new UI's `chatStreamTurnFrames.ts` previously discarded a child ending step
unless an action placeholder already existed; child start/chunk frames intentionally
cannot create a parent answer. It now creates the missing child step by its stable
run identity, preserving hierarchy and parent streaming state. `applyThinkingStep`
also updates `toolOutputs`, which the nested-step renderer reads live. A regression
test checks that duplicate end frames do not duplicate the step or alter the parent.
This is rendering-only ownership: no incomplete content returns to the parent model.

### Non-streaming child handoff correction

Fresh browser chat 638 correctly failed at the continuation cap but did not expose
the new partial step. This failed acceptance revealed that nested ADK invocations
use `StreamingMode::None`: their accepted deltas never reach the outer projector.
It is not accepted UI evidence.

`agents/model_scope_output.rs` now retains the accepted prefix at each completed
output-limited response (at most five bounded snapshots), including the last
accepted segment when the cap is reached. `agents/model_scope.rs` forwards that
snapshot as a partial presentation event immediately before propagating the
continuation error. It never appends a successful result/receipt to the child
session. Rejected seam text is excluded. A non-streaming ADK runner regression
proves that all five accepted segments are inspectable before the failure and no
completed receipt exists. The outer lifecycle persists the resulting incomplete
step before publishing its terminal error.

The corrected worker in chat 639 preserved 5,071 characters of accepted partial
output. Read-only browser inspection confirmed it in the persisted trace. The
first harness still searched the older `Thought for` control; this trace uses
`Execution details`. Its label also exposed a real rendering gap:
`PersistedMessageTrace.tsx` ignored `attrs.response_metadata.tool_name`, where Main
already preserves thinking-step display names. It now reads that existing field
before falling back to the model name. No Main change or schema addition is needed.
The two persisted-trace UI tests and TypeScript checking pass.

### Partial-output browser acceptance

Chat 640, execution `df3a8a2510712014fb722dba892d4581`, verifies the complete
handoff with worker `d4cc90cc` / image
`sha256:d4578aa5781d46f7e6336cccf40d55d4d537184c4e7c0bfb5b284744716a8317`
and UI `06cbedca` / image
`sha256:c877109a0372a7e73b46fca03d0cd6b7e8708a92cbf3286b59663cac299f29c0`.
Fresh headed Playwright sends the request, observes the registered terminal error,
and opens the live incomplete child step. The accepted 5,071-character text is
identical when reopened through persisted `Execution details`. The live row title
is separate presentation text and is excluded from content equality. The first
combined harness flagged that title difference; a read-only browser follow-up
compared the actual text successfully without another model invocation.
The response contains no completion marker and the explicit failure stays visible.
No browser runtime errors occurred. PostgreSQL confirms one child session and zero
completed child-result receipts. Screenshots were inspected.

Artifacts in the local temporary verification directory:
`elitea-incomplete-result.json`, `elitea-incomplete-durable-proof.json`,
`elitea-incomplete-partial-live.png`, `elitea-incomplete-partial-reloaded.png`.
Earlier failed acceptance artifacts remain separately identified as chats 638/639.
Final component coverage: 416 PostgreSQL-enabled agent tests, 143 execution tests,
249 chat-message reducer tests, two persisted-trace UI tests, strict Clippy and UI
TypeScript checking. No unrelated deferred HITL work was included.

This accepts partial-output visibility for the bounded nested-child failure.
It does not claim crash injection during partial-trace publication, live boundary
repair, disabled-compaction child continuation, or bare pipeline-LLM continuation.
Those boundaries remain separate from this browser acceptance.


### Continuation when compaction is disabled

Current SDK reference remains `966526e8334354366dd161b606d73fe8e204b850`.
`elitea_sdk/runtime/tools/llm.py::_continue_nested_output` detects truncated nested output independently of summarization settings.
It stops when the answer completes. Four additional calls remain the maximum, including boundary repair.

`agents/application_tools.rs::LazyNestedAgent` now installs the child checkpoint regardless of the compaction switch.
The provider allowance includes four continuation calls. ADK retains the configured logical step limit.
`agents/model_scope.rs` accepts an optional compaction plan and optional provider budget metadata.
An enabled compaction plan still requires budget metadata. Disabled compaction never constructs or invokes a summarizer.
The existing model checkpoint owns accepted prefixes, pending requests, claim fencing, and completed child receipts.
No database schema or wire contract changes are required.

Authorized synthetic replay now records the current invocation's conservative tool boundary before dispatch.
It does not record the synthetic response as a pending provider request or grant permission to replay effects.
This fixes nested authorization and confirmation paths exposed by enabling checkpoints without compaction.

Regression coverage checks early completion without summarization and PostgreSQL claim takeover with compaction enabled and disabled.
All 418 PostgreSQL-enabled agent tests pass, including nested confirmation and authorization replay regressions.
Strict all-target Clippy also passes. Deployed browser acceptance remains pending for this change.


### Disabled-compaction browser acceptance

Worker commit `0e7a9de5` runs as image `sha256:3d6df3e2fb2b77df9c858acc6046c75dd92740b002b9ee22a5da091a04654dbc`.
Fresh headed Playwright verifies chat 641, execution `dd07530708e81db43fd6dbddbdabd36d`.
The persisted conversation disables both context management and summarization.
The existing child uses Haiku, a 256-token output allowance, and two logical steps.
It completes 40 ordered records with one final marker after the initial call and two continuation calls.
No compaction or browser error occurs. Reload preserves the displayed answer exactly.
PostgreSQL confirms one completed child receipt with all 40 records and one final marker.
The parent adds an introduction, so its full response differs from the child result.

Local evidence: `elitea-disabled-result.json`, `elitea-disabled-durable-proof.json`,
`elitea-disabled-policy.json`, `elitea-disabled-complete.png`, and `elitea-disabled-ending.png`.
The context tooltip incorrectly states unconditional automatic compaction.
`ContextBudgetPanel.tsx` now qualifies the threshold with “When enabled”.
This wording correction does not change the saved policy or runtime behavior.

The tooltip correction passes all 57 context-budget UI tests.
UI commit `bada7cde` runs as image `sha256:9d39544fce7bc383772824186d8a962dd34889e05833f6e136d73df62446f5b4`.
A fresh read-only headed browser verifies the corrected text and the persisted disabled policy.
Screenshot `elitea-disabled-tooltip.png` records this check. No additional model call is made.


### Pipeline LLM-node continuation

The current SDK reference `_continue_nested_output` explicitly completes truncated graph-node output before returning it to the graph.
Rust `agents/pipeline.rs` now installs the existing model-scope checkpoint and continuation wrapper for each LLM node.
Compaction remains optional. The provider allowance includes four continuation calls beyond the logical node step allowance.
The node retains its output limit on each call and stops when the answer completes.
ADK structured-output validation receives the completed answer before graph output projection.
Graph data and deterministic nodes do not become compaction subjects.

`agents/graph/llm.rs` waits for model completion before writing node output or scheduling downstream nodes.
The existing bounded event bridge in `agents/graph/node_events.rs` carries a typed continuation-failure signal.
This preserves `model.output_continuation_failed` across ADK graph error wrapping.
Accepted partial text stays presentation evidence; exhaustion does not produce successful graph output.
No new database schema or wire contract is introduced.

New regressions cover a two-continuation answer followed by a deterministic node, and failure after four continuation calls.
All 420 PostgreSQL-enabled agent tests pass, including both new pipeline regressions.
Strict all-target Clippy passes. Deployed pipeline browser acceptance remains pending.
