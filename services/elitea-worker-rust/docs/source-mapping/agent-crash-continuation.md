# Agent crash continuation

Status: implementation required. This document defines the next recovery change, not a passing gate.

## Checkpoint recording implementation

`src/agents/model_checkpoint.rs` records the ordinary agent's model/tool boundary through ADK 2.2 callbacks.
`src/agents/session.rs` installs these callbacks during ordinary agent assembly.
The existing claim-bound session service persists the checkpoint under `elitea.agent.recovery.v1`.
The checkpoint contains execution, generation, definition digest, invocation ID, and phase.

Before model dispatch, it stores the model request and tool declarations separately.
ADK excludes `LlmRequest.tools` from its normal serialization. Saving only that serialization would lose available tool schemas.
Before tool dispatch, the checkpoint becomes `tool_may_have_started` and removes the replayable model request.
The next model step records its request, including completed tool results from ADK history.
Existing session limits and writer fencing apply. A failed checkpoint write stops the corresponding dispatch.

This change records recovery evidence. It does not grant Main recovery authority.
Specialized guard-resume assembly and pipeline node assembly do not install these callbacks yet.
No migration or product schema change is added. The change is not deployed as a completed crash recovery feature.

The Runner test checks persisted state inside both the model and tool implementations.
It verifies two model steps, one tool execution, preserved tool declarations, and the completed result in the next request.
A second test removes the checkpoint store and verifies that the model receives no invocation.
The agent suite passes 306 tests before the additional definition-digest field is added.
Focused checkpoint checks validate the final field layout separately.
Strict Clippy passes for all targets and features. The new module passes formatting checks.
Whole-crate formatting still reports an unrelated pending change in `src/agents/graph/compiler_tests.rs`.

## Explicit model checkpoint restoration

`assemble_ordinary_native_from_checkpoint` provides a separate Rust assembly entry point.
Ordinary assembly never infers recovery permission from session state. Later turns and repeated nodes retain their normal behavior.
The recovery entry point requires a matching execution, generation, definition digest, supported checkpoint version, and pending model phase.
It also requires the corresponding durable checkpoint event. A subsequent completed model event prevents replay.
Missing checkpoints, uncertain tool phases, and changed tool declarations cause refusal before model dispatch.
Recovery loads an existing session. It cannot create a session or apply regeneration cleanup.

ADK's before-model callback substitutes the saved request exactly once. Later model calls consume the restored session's normal tool results.
The Runner receives empty continuation input. The session wrapper omits this empty user event from durable history.
The callback also removes empty continuation input from subsequent provider requests.
This preserves the original user turn without resubmitting its text.

The component test completes one tool, interrupts the following model response after partial text, and constructs a new Runner.
The restored request contains the completed tool result. The tool executes once, and durable history contains one user event.
The agent suite passes 308 tests. Three focused checkpoint tests cover recording, failed persistence, and invalid recovery evidence.

The live claim path does not dispatch to this recovery entry point yet. Claim authority, partial-output replacement, and deployed crash acceptance remain unfinished.
These component results do not close the live continuation gate.

## Confirmed failure

The [Main crash check](chat-restart-observer.md#main-process-crash-acceptance) separates browser observation from runtime continuation.
The browser reconnects correctly. The interrupted agent becomes a failed execution after reclaim.

The failure does not require a worker crash. Main also carries the active model transport used by this deployment.
The worker reports a model stream failure when Main stops. Control supervision loses contact at the same time.

## Current source mapping

| Owner | Source | Current contract |
| --- | --- | --- |
| Main | `internal/infra/db/repos/claims.go` | A running invocation beyond `PREPARING` receives ambiguous recovery authority. |
| Rust | `src/execution/agent_delivery.rs` | Output recovery receives no business input and cannot construct a fresh invocation. |
| Rust | `src/execution/agent_delivery_processor.rs::process_output_recovery` | A running ambiguous invocation becomes an internal terminal failure. |
| Rust | `src/agents/session.rs::assemble_ordinary_native_with_sessions_and_runtime_catalogs` | Restores ADK session history, then constructs an ordinary Runner invocation. |
| Rust | `src/agents/session.rs::RunnerSessionService` | Enriches completed model events before durable persistence. Partial model output is not a completed event. |
| Rust | `src/agents/replay_history.rs` | Normalizes exact call/result pairs and rejects conflicting replay history. |
| Rust | `src/agents/replay_history/recovery.rs` | Repairs one unavailable tool selection before semantic output. It does not recover a crashed invocation. |
| Rust | `src/agents/graph/turn_checkpointer.rs` | Retains graph checkpoints and isolates fresh turns from another execution's frontier. |
| Rust | `src/state/postgres_session.rs` | Persists sessions and events with writer fencing in the existing runtime state database. |
| ADK 2.2 | `adk-runner/src/runner.rs::run_with_config` | Accepts new user content and creates a new invocation ID. Reusing this entry point alone is not exact crash resume. |

The legacy platform remains the business reference for chat, tools, and pipeline behavior.
It does not supply the requested crash-continuation guarantee. Do not copy its restart limitations into this implementation.

## Required implementation boundary

Keep output-only recovery for completed output and uncertain external effects.
Add a separate, typed continuation path for a validated durable execution checkpoint.
Do not turn every ambiguous claim into an ordinary accepted claim.

Use the existing ADK session and graph checkpoint stores. Do not introduce a parallel product database schema.
Bind each recovery checkpoint to the execution, generation, frozen input revision, and current claim authority.
Persist the execution position before opening a model request or invoking a tool.
Preserve completed call/result pairs by their existing IDs. Never repeat them to reconstruct session history.

Distinguish these positions:

- A model request is pending. Restart that model step from its durable inputs, without resubmitting the user turn.
- A tool result is committed. Continue from that exact result.
- A tool invocation has an uncertain outcome. Require its recovery receipt or reconciliation contract before proceeding.
- A graph node is complete. Resume the saved graph frontier without running the completed node again.
- A terminal result is durable. Recover its publication and settlement without invoking the model.

Preserve authoritative skills, project context, persona, and frozen model settings during reconstruction.
Use a new output attempt identity to replace unfinished provisional text. Do not append a regenerated answer to its partial predecessor.
Do not confuse an execution replay cursor reset with replacement of model text.

An explicit inspection claim permits immutable input hydration. Worker checkpoint validation and one-use authorization must precede resumed model invocation.
Cancellation, deadline expiry, foreign scope, stale writers, and unsupported checkpoints must refuse execution.
Keep recovery attempts bounded by durable execution policy, not by an in-memory retry counter alone.

## Acceptance order

1. Interrupt a model step after partial text. Verify one user turn and one final answer after recovery.
2. Complete a tool call, then interrupt the next model step. Verify the tool executes exactly once.
3. Interrupt an in-flight tool. Verify no automatic duplicate effect occurs.
4. Replace the worker with no local spool. Verify the same checkpoint and output identity rules.
5. Repeat with a saved agent and an autonomous pipeline through an external MCP client.
6. Verify cancellation, stale claim rejection, and terminal output recovery during each transition.

The external MCP transport must also reconnect to the original execution. A repeated POST is a separate invocation today.
Browser reconnection, session persistence, and safe terminal failure remain separate evidence from successful continuation.

## Model recovery authority component

Main does not own checkpoints. Rust stores and validates checkpoint contents through the existing ADK state service.
Main records only an inspection mode and a SHA-256 authorization receipt on its execution claim.

| Source | Implementation |
| --- | --- |
| `libs/proto/elitea/runtime/v1/control.proto` | Adds opt-in checkpoint inspection and a separate authorization RPC. |
| Main `internal/application/execution/model_checkpoint_authority.go` | Validates the fence and digest before the repository call. |
| Main `internal/infra/db/repos/claims.go` | Selects inspection only for opted-in agent claims after ambiguous invocation. |
| Main `internal/infra/db/repos/model_checkpoint_authority.go` | Locks the current claim and job; grants one model restoration attempt. |
| Main `internal/transport/runtimegrpc/control/model_checkpoint_authority.go` | Authenticates the worker and rejects malformed wire evidence. |
| Main `migrations/shared/0131_agent_model_checkpoint_claim.sql` | Adds authority metadata to existing runtime claims. Product tables remain unchanged. |
| Rust `src/protocol/control.rs` | Keeps opt-in disabled and rejects the new disposition until dispatch integration is complete. |

The runtime migration is necessary to distinguish inspection from invocation across Main restarts and concurrent requests.
It stores no model request, prompt, tool result, or checkpoint body. It creates no new table.
Ordinary invocation remains fenced by `MAY_HAVE_STARTED`.
A duplicate authorization returns `ALREADY_AUTHORIZED`, which does not permit another model attempt.

Real PostgreSQL tests cover application and ad-hoc recovery claims, concurrent authorization, digest changes, stale workers, expiry, and cancellation.
They also reject checkpoint authority for legacy agent workers, toolkits, and indexing jobs.
The test database applies the migration. The rehearsal deployment does not apply this change yet.

The broader repository test run still reports an unrelated pending catalogue mismatch for `project_context_builder` and `skill_builder`.
Focused claim tests and control tests pass. These results do not establish live crash continuation or MCP reconnection.

## Rust authorization transport

`src/transport/control_grpc.rs` calls the dedicated checkpoint authorization RPC through the generated tonic client.
It preserves workload metadata, request and response size bounds, and the configured deadline.
It sends one request. A timeout or transport error does not cause an automatic retry.
The response preserves `ALREADY_AUTHORIZED`; transport success alone never grants invocation permission.
Adapters without checkpoint support return an explicit unimplemented response.

Transport tests cover connection failure, timeout, exact metadata, and the already-authorized response.
These are component tests. They do not start an independent Main process.

The remaining coordinator change must address an ordering difference:

1. Ordinary preparation calls `BeginExecution` before claim-bound input materialization.
2. Ordinary authorization creates session and runtime-context authority before native assembly.
3. Checkpoint inspection must obtain session access before restored invocation authorization.
4. The recovery path must validate the saved model request before it obtains a model submission permit.

The relevant boundaries are `agent_preparation.rs`, `agent_invocation.rs`, `native_agent_lifecycle.rs`, and `agents/ordinary.rs`.
Do not route an inspection claim through ordinary `BeginExecution` or infer fresh authority from its hydrated inputs.
Recovery opt-in remains disabled until this distinct path and partial-output replacement are implemented.

## Rust inspection claim boundary

`src/protocol/model_checkpoint_inspection.rs` separates checkpoint inspection from fresh invocation authority.
Its parser requires the explicit recovery disposition and an authenticated agent command.
It shares identity, lease, manifest, and immutable revision validation with the ordinary claim parser.
It does not change the received disposition or expose a fresh claim to its caller.

Consuming the inspection claim issues one claim-bound ADK session authority and retains a separate pending authorization claim.
This operation creates no runtime credential grant, model submission permit, output cursor, or settlement permission.
Rust and ADK retain checkpoint ownership. Main receives no checkpoint contents.

The focused test rejects fresh claims, changed generations, changed workload sessions, expired leases, cancellation, changed input revisions, and missing manifests.
It also checks that session access preserves the execution ID, claim attempt, lease epoch, and fence token.

This boundary is not routed into production yet. Lease supervision, checkpoint evidence binding, and restored invocation dispatch remain required.

## Validated checkpoint authorization

`src/agents/model_checkpoint.rs` creates `ValidatedModelCheckpoint` only after validating the durable model checkpoint.
The evidence contains the execution ID, generation, and SHA-256 digest of the validated checkpoint JSON.
It contains no prompt, model request, tool result, or credential. Checkpoint contents stay in ADK storage.

`InspectedModelCheckpointClaim::authorize` consumes the evidence and the pending inspection claim.
It rejects a different execution or generation before calling Main.
It sends the digest through the dedicated bounded authorization transport.
Only `AUTHORIZED_NOW` creates a model submission permit, output authority, and runtime-context authority.
An already-authorized response, malformed response, or transport failure creates no permit.
The returned failure retains the consumed claim and exposes no retry method.

Five focused tests pass. They cover checkpoint validation, digest binding, inspection, first authorization, duplicate authorization, and transport loss.
The authorization tests use a component RPC double. They do not prove recovery through independently deployed services.
Session access still precedes authorization; runtime credential access follows successful authorization.
The coordinator must connect these values to lease supervision, restored assembly, and partial-output replacement before enabling recovery.

## Inspection, authorization, and Runner release

`NativeSessionBackend::inspect_model_checkpoint` opens the claim-bound session and validates the checkpoint before runtime credential redemption.
It constructs no model, toolset, Runner, or new user turn. Missing state and regeneration requests cannot enter recovery.

The coordinator can then authorize the validated digest and obtain runtime-context authority.
`assemble_ordinary_native_from_checkpoint` returns `PendingRecoveredAgentInvocation`, which has no start method.
Post-authorization assembly validates the saved checkpoint again. The pending Runner compares this evidence with the authorization receipt.
Only an exact execution, generation, and digest match releases the assembled Runner.
This second check detects a checkpoint change between inspection and assembly.

| Rust source | Responsibility |
| --- | --- |
| `src/agents/session.rs::NativeSessionBackend::inspect_model_checkpoint` | Reads worker-owned checkpoint evidence before credential redemption. |
| `src/protocol/model_checkpoint_inspection.rs::AuthorizedModelCheckpoint` | Retains the authorized evidence beside the one-use model permit. |
| `src/agents/runtime.rs::PendingRecoveredAgentInvocation` | Prevents Runner start until assembled evidence matches authorization. |
| `src/agents/session_tests.rs::interrupted_model_restores_saved_request_without_repeating_tool_or_user_turn` | Exercises inspection, authorization, restored assembly, and model completion. |

The component test first rejects authority for another checkpoint digest without invoking the model.
It then inspects state, authorizes the digest, assembles recovery, and completes the pending model step.
The completed tool runs once. The restored model runs once. Durable history retains one original user event.
The test uses injected ADK storage and an RPC double. It does not establish deployed crash recovery.

The Redis delivery coordinator still requires recovery routing, lease supervision, and partial-output replacement.
External MCP reconnect remains a separate unfinished gate. Recovery opt-in remains disabled.

## Lease-supervised checkpoint inspection

`ClaimLeaseMonitor::start_checkpoint_inspection` uses the existing bounded lease actor without calling ordinary `BeginExecution`.
The monitor retains a pending inspection value and the exact claim lease handle.
`activate_checkpoint_inspection` consumes that value after renewal and desired-state observation succeed.
Only the resulting `LiveModelCheckpointInspection` can issue session access in production.
The ordinary activation method cannot release checkpoint inspection as fresh invocation authority.

The monitor continues renewal after activation. Its existing state probe governs ADK state writes and cooperative cancellation.
Cancellation or lease loss during the first poll releases no session access.
Repeated activation fails. Shutdown preserves a latched lease-loss error.

Source owners are `src/execution/agent_lease.rs` and `src/protocol/model_checkpoint_inspection.rs`.
All 15 lease component tests pass, including the new inspection activation, cancellation, and expiry cases.
These tests use an RPC double. They do not prove deployed recovery routing or worker replacement.

The next integration must connect the delivery route, immutable input materialization, and output position to this supervised inspection value.
Recovery opt-in remains disabled until the complete path is ready for deployment verification.

## Frozen request materialization during inspection

`src/protocol/control.rs::AcceptedAgentClaim` now owns the shared private manifest-entry binding helper.
Ordinary lease-monitored execution and `LiveModelCheckpointInspection` borrow the same validated content identity, immutable version, digest, length, claim, and fence.
`src/transport/input_content.rs::InputContentClient::fetch_checkpoint_request` uses the existing bounded HTTP/2 materializer, including source identity and response digest verification.
It accepts only the live inspection type; raw and pending inspection claims cannot fetch through this API.
It does not issue session access, runtime credentials, or model invocation permission.

All 11 input-content component tests pass, including a recovery request test that verifies the exact route and fence headers and rejects a changed source digest. Strict library/test Clippy passes.
The test bypasses lease activation only to construct its transport fixture; cancellation and expiry at activation are covered by the lease tests above.
This is new crash-recovery behavior, not a port of the current platform's restart behavior. Checkpoints remain Rust/ADK-owned; Main serves the original immutable input through its existing content contract.
No migration is added for this change. Recovery routing and output replacement remain unfinished, and no deployed recovery claim is made.

## Agent-only checkpoint delivery routing

`src/protocol/model_checkpoint_inspection.rs::AgentControlClient::claim_agent_checkpoint_delivery` explicitly advertises checkpoint recovery for a verified agent command. Its result distinguishes a validated inspection claim from the existing ordinary claim decisions. The generic claim parser and direct toolkit entry point still reject the checkpoint disposition.
`src/execution/agent_delivery.rs::AgentDeliveryRouter::route_checkpoint_verified` preserves the exact Redis delivery, signed command, and inspection authority together. Ordinary dispositions reuse the existing fresh, replay, settlement, and retirement routing; inspection itself performs no BeginExecution, invocation authorization, or Redis acknowledgment.

This is an opt-in coordinator entry point, not an enabled production recovery path. The existing processor still uses `route_verified` until restored assembly, output replacement, and terminal ownership are connected. This preserves the distinction between an implemented routing component and successful deployed continuation.

The claim test checks explicit opt-in, ordinary accepted fallback, checkpoint inspection, and rejection of a mismatched producer fence. Existing agent control and delivery contracts verify unchanged ordinary and terminal routing. These remain component checks; mandatory UI restart verification and external MCP reconnection are pending.

## Recovery request preparation

`src/execution/agent_preparation.rs::prepare_checkpoint_input` now connects supervised inspection input loading to the existing canonical agent request parser. Fresh and recovery preparation share `input_binding_from_parts`, preserving the bundle identity/digest and request entry version/digest.
`LiveModelCheckpointInspection::matches_command` checks the exact authenticated command binding before any content request. The input fetch runs through the existing lease cancellation boundary, followed by an explicit lease observation and deadline recheck. The caller retains inspection and lease ownership on failure for subsequent terminal/no-ACK policy; this helper neither begins nor authorizes an invocation.

All 17 preparation component tests pass. The recovery case verifies normal parsing, malformed input, cancellation observed after materialization, pre-fetch deadline expiry, input-service failure, and a different signed command. It asserts no BeginExecution or AuthorizeInvocation calls, and no input fetch for rejected command/deadline cases. Strict Clippy verification accompanies the change.

No current-platform business contract or product schema changes here: the ordinary parser remains the behavior reference, and crash continuation is new worker functionality. The production processor still awaits recovery lifecycle and output integration. These checks do not replace mandatory deployed UI and external-client restart verification.

## Native assembler checkpoint inspection

`src/agents/runtime.rs::NativeAgentAssembler::inspect_checkpoint` supplies the coordinator with a credential-free inspection entry point. Assemblers without checkpoint support reject it explicitly. `src/agents/native_runtime.rs` dispatches inspection according to the same frozen runtime kind used for assembly; it does not reinterpret a pipeline as an ordinary agent.
The ordinary implementation in `src/agents/ordinary.rs` uses its configured `NativeSessionBackend` and the shared pure `admit_ordinary_plan` helper. Normal assembly and inspection therefore construct the same profile, definition digest, session identity, and continuation mode. Inspection supplies the frozen attachment metadata for plan construction; the restored model request itself remains the durable checkpoint's authority.

All 309 agent component tests pass after sharing admission. The new ordinary assembler test rejects absent checkpoints in both invocation-local and injected session backends with zero runtime credential redemptions and zero model calls. Existing restoration tests remain the evidence for checkpoint replay; this new test establishes the assembler boundary only. Pipeline inspection uses the explicit unsupported default until pipeline checkpoint support is implemented.

No product schema, proto, or current-platform behavior changes in this step. Restored assembly, output lifecycle integration, and deployed browser/external MCP restart acceptance remain required.

## Restored native assembler path

`NativeAgentAssembler::assemble_checkpoint` returns `PendingRecoveredAgentInvocation`, preserving the exact checkpoint authorization requirement through `NativeRuntimeAssembler` completion mapping. Unsupported assemblers reject restoration explicitly.
`OrdinaryNativeAgentAssembler` shares runtime redemption and provider/tool/session setup between ordinary and restored assembly. Recovery uses the existing checkpoint-specific session assembler and does not reread attachments; the durable model request already contains resolved document content. It does not fall back to creating a fresh session or synthesizing another user turn.

The ordinary assembler integration test runs an initial model transport failure, inspects its persisted checkpoint without another credential redemption, authorizes its digest, assembles the pending Runner, checks the exact authorization, and completes the restored model. The gateway receives identical message arrays for the initial and restored requests, and no second model call occurs during assembly. The suite of 310 agent tests passes, with strict library/test Clippy verification. Final browser-output projection is also asserted by the focused integration test.

Source owners: `src/agents/ordinary.rs`, `src/agents/runtime.rs`, `src/agents/native_runtime.rs`, and `src/agents/ordinary_tests.rs`. This extends the worker-owned restart capability; no current-platform business behavior, schema, or proto changes are added here. The test uses injected session storage and gateway/control doubles. Delivery supervision, output replacement, and live UI/external MCP restart acceptance remain unfinished.

## Shared authorized lifecycle integration

`AuthorizedModelCheckpoint::into_lifecycle_parts` projects one successful authorization into the existing submission/output/runtime/session grants plus a private, one-use `CheckpointAssemblyAuthorization`. Only successful checkpoint authorization can construct that proof. It carries the exact validated digest, not checkpoint contents or model credentials.
`AuthorizedAgentRun::from_checkpoint` retains those grants with the original delivery, request, lease, and prepared output. Progress-publisher binding and cleanup preserve the checkpoint proof. `CursorBoundAuthorizedAgentRun::assemble_native` consumes it to select `assemble_checkpoint`, validates the restored digest, and rejoins the existing lease-raced start/progress/terminal lifecycle. Ordinary runs retain the ordinary assembler path. Runner start refuses any remaining unconsumed checkpoint assembly proof.

The full library run passed 979 tests with loopback access enabled; seven local HTTP-fixture tests had failed in the sandbox. An additional focused lifecycle test passes, proving recovery selects the checkpoint assembler and retains the run for no-ACK cleanup after dependency failure, releasing admission capacity. The real assembler restoration test now exercises the split lifecycle proof and still completes the restored model. Strict Clippy validation accompanies this change.

Source mapping: `src/protocol/model_checkpoint_inspection.rs`, `src/agents/runtime.rs`, and `src/execution/agent_preparation.rs`; verification in ordinary assembler and execution preparation tests. This is new worker recovery orchestration rather than a change to current-platform business logic. No product schema or protocol change is added in this step. Production intake, partial-output replacement, and real browser/external MCP restart verification remain pending.

## Checkpoint output spool preparation

`AgentOutputPreflight::prepare_checkpoint` opens the same execution/producer-scoped encrypted spool under the checkpoint claim's output transport identity. Empty output is ready for the shared lifecycle. A single pending progress frame is reconciled only after signed-command/identity validation and only when its sequence is covered by the authenticated claim handoff watermark. The reconciled spool is reopened because a reconciled spool handle intentionally cannot publish new frames.
Uncovered progress and terminal frames remain on disk and return no ready output; they still require exact-frame replay before model recovery. Malformed frames and mismatched output transport identities fail closed. This function does not contact Main, invoke a model, or acknowledge Redis.

The two focused tests pass: empty and covered-progress spools accept the next output sequence after preparation, uncovered progress and terminal output survive repeated inspection, and a mismatched producer is rejected. Strict Clippy validation accompanies the change.
Source owners: `src/execution/output_delivery.rs`, `src/execution/agent_delivery.rs`, and `src/protocol/model_checkpoint_inspection.rs`; tests are in `src/execution/output_delivery_tests.rs`.

This reuses the new platform's existing spool and output-watermark contracts; no current-platform business behavior or schema changes are introduced. Main owns the accepted output watermark, while Rust/ADK still owns model checkpoint state. Production coordinator intake, pending-frame replay integration, partial browser-output replacement, and deployed UI/external MCP restart proof remain pending.

## Process-owned checkpoint recovery job

`src/execution/checkpoint_recovery.rs` composes lease activation, frozen request preparation, credential-free checkpoint inspection, and one-attempt checkpoint authorization under the existing invocation supervisor. `AgentInvocationCoordinator::submit_checkpoint` transfers the unpolled future and admission reservation synchronously; rejected submissions start neither a lease actor nor an RPC. An accepted job remains owned after its caller drops the completion waiter. After authorization it hands the restored run to the shared native lifecycle.

`AuthorizedAgentLifecycle::inspect_checkpoint` delegates through the native assembler. Preparation/inspection failures use the existing canonical failure publisher when output authority remains valid; lease loss and uncertain authorization retain the Redis delivery for recovery. The authorization RPC is deliberately not raced against caller cancellation. Main still receives only checkpoint identity/digest evidence; Rust/ADK owns checkpoint contents.

The focused supervisor test passes for both accepted work after waiter disconnection and rejected work after shutdown. It verifies one checkpoint authorization, no ordinary BeginExecution/AuthorizeInvocation calls, supervisor drain, and admission release. `cargo check --lib` and strict library/test Clippy also pass. These are component checks using control/session doubles, not deployed restart acceptance.

Source mapping: `src/execution/checkpoint_recovery.rs`, `agent_coordinator.rs`, `agent_invocation.rs`, `native_agent_lifecycle.rs`, and `src/protocol/model_checkpoint_inspection.rs`; verification in `src/execution/output_delivery_tests.rs`. This adds production orchestration for the new platform's recovery contract, rather than porting current-platform restart behavior. No schema or proto changes are introduced in this step. Production intake remains on the ordinary route until pending-frame replay and partial-output replacement are integrated; browser and external MCP crash/reconnect gates remain open.

## Exact retained-progress replay before recovery

`AgentOutputPreflight::replay_checkpoint_progress` reuses `AgentProgressConnector` and its owned replay session for one exact retained progress frame. It validates the checkpoint claim's transport identity, signed command, and frame identity before opening a network replay, then explicitly closes the replay session on every observed outcome. Terminal frames remain reserved for terminal replay/settlement. The method consumes the inspection delivery and returns no model or output authority: the next attempt must claim again to obtain Main's updated watermark, even after a successful replay.

The focused checkpoint suite passed 27 tests. The new replay case verifies exact-frame replay, zero fresh progress sends, session close on authorization failure, retained-frame retry, and an empty spool after ACK. A focused rerun verifies the consuming inspection interface; strict library/test Clippy passes. Source mapping: `src/execution/agent_delivery.rs` shares structural output validation; `src/execution/output_delivery.rs` owns replay; `src/execution/output_delivery_tests.rs` provides transport-double evidence. This uses existing Rust output durability contracts without changing current-platform business logic or database schemas.

Intake wiring, terminal recovery integration, partial-answer replacement, and live browser/external-client continuation remain outstanding. This component is not deployed or advertised as completed crash recovery.

## Opt-in whole-delivery composition

`AgentDeliveryProcessor::process_checkpoint_verified` now joins the recovery claim route to bounded invocation admission, output preflight, exact retained-progress replay, and `AgentInvocationCoordinator::submit_checkpoint`. Ordinary claim decisions share `process_route` with the existing processor. Ready output and its inspection delivery enter the supervised checkpoint job together. Pending progress consumes the inspection through exact replay and returns no-ACK for a fresh claim; it never proceeds to model authorization with the old watermark. Pending terminal output remains reserved for terminal settlement integration. Admission is released on all return paths.

`AgentOutputPreflight::prepare_checkpoint` now borrows the inspection delivery so the processor retains ownership when preflight identifies pending output. It returns only prepared output; the processor must supply the same retained delivery at supervisor submission. Production bootstrap still invokes the ordinary route, so this explicit opt-in method is not yet enabled in the deployment.

Verification: 28 checkpoint tests and all 64 output-delivery tests pass, with strict library/test Clippy. The new processor test checks both empty and pending-progress spools: one checkpoint authorization/lifecycle call for empty output; one exact replay and zero authorization/lifecycle calls for pending output; no BeginExecution, ordinary authorization, or Redis retirement in either recovery case; admission capacity returns after drain. The existing ordinary application/ad-hoc retirement tests also pass through the shared route. These tests use control and lifecycle doubles and do not prove live model recovery.

Source mapping: `src/execution/agent_delivery_processor.rs`, `src/execution/output_delivery.rs`, and `src/execution/output_delivery_tests.rs`. This is new-platform recovery composition using existing worker contracts; no current-platform business behavior, proto, or database schema changes. Partial browser-answer replacement, terminal-output recovery integration, deployment enablement, and mandatory browser/external MCP crash/reconnect verification remain open.

## Ordinary restart projection and interrupted reasoning

The existing Rust `AgentEventProjector::start` already emits `agent_start` before native execution, and the shared lifecycle durably publishes that start before polling the model. The ordinary restoration test now verifies the recovered event has `should_continue: false`. In the UI, `chatStreamTurnFrames.ts` already resets answer content on that event and retains tool actions. No new output protocol is needed for ordinary answer replacement.

A focused regression exposed a remaining UI defect: an interrupted `<think>` block left the message's synthetic reasoning action open after `agent_start`. The next recovered answer was consumed into that old reasoning sink, leaving visible content empty. The restart reducer now removes only the synthetic reasoning action via the existing `reasoningActionId`; all other tool actions remain. Explicit `should_continue` retains its prior behavior.

Source mapping: Rust `src/agents/events.rs::AgentEventProjector::start`, restored assembly in `src/agents/session.rs`, and its assertion in `src/agents/ordinary_tests.rs`; browser projection in `apps/elitea-web/src/features/chat-messages/lib/chatStreamTurnFrames.ts` and regression in `chatStreamReducer.test.ts`. This builds on the existing new-UI start/continuation contract and adds no current-platform business behavior, proto, or schema change.

Evidence: the regression failed before the fix with an empty answer instead of recovered text. Afterwards 105 focused reducer/reasoning tests, UI TypeScript checking, the Rust restored-assembler test, and strict Rust library/test Clippy pass. The test retains the completed tool entry and a single assistant message. These are component checks; this UI change is not deployed yet, and mandatory fresh-browser crash recovery remains pending. Explicit output-continuation prefix recovery still needs separate verification, as do terminal replay, deployment enablement, and external MCP reconnect.

## Retained terminal recovery under inspection claims

`AgentOutputPreflight::prepare_checkpoint_pending` separates retained progress from a durable terminal. The latter is validated against the signed agent command, original request version/digest, exact output identity, and next sequence after the claim watermark. `ModelCheckpointInspection::terminal_replacement` changes only its fence and handoff watermark; consuming the inspection discards input/model authority and retains only the existing terminal recovery claim.

`PreparedOutputSpool::replace_pending_agent_terminal_recovery` uses the existing recovery-rebind validator and exact durable compare-and-swap. A changed payload, timestamp, settlement outcome, or logical identity is refused. A different fence must have strictly newer claim attempt and lease epoch, a changed token, and the same producer; result bytes remain unchanged. This follows the established direct-tool terminal recovery mechanism without its deadline-outcome substitution. Main retains cancellation/deadline decision authority during output acceptance.

The processor passes the prepared terminal to `recover_accepted_terminal`, which supervises the lease, replays the exact terminal, obtains the bound settlement receipt, and only then retires Redis. It never loads a model checkpoint or authorizes invocation on this branch. Progress still replays separately and requires a fresh claim.

Source mapping: `src/protocol/model_checkpoint_inspection.rs`, `src/execution/agent_delivery.rs`, `src/execution/output_delivery.rs`, `src/execution/agent_delivery_processor.rs`, and `src/transport/output_grpc.rs`. The functional reference for this new-platform durability work is the existing `src/execution/toolkit_output.rs` terminal rebind/settlement contract; the current Python platform does not supply this crash-continuation behavior. No schema or proto changes are introduced.

Verification: all 64 output-delivery tests, three terminal-rebind tests, and strict library/test Clippy pass. The processor test covers empty output, pending progress, same-claim terminal, and newer-claim terminal. Terminal cases preserve the exact final frame under the accepted fence, perform settlement and Redis retirement, and make zero checkpoint/model authorization calls. The transport test refuses result mutation and stale compare-and-swap while retaining durable bytes. Production recovery remains disabled pending continued-output prefix behavior, remaining recovery-boundary checks, and deployed fresh-browser/external MCP crash/reconnect acceptance.

## Frozen prefix replay for recovered output continuation

Restored ordinary assembly marks its projector for checkpoint recovery after checkpoint validation and before Runner start. `AgentEventProjector::start` then emits the existing non-continuing `agent_start` reset, followed by the frozen continuation prefix as bounded `agent_llm_chunk` events. The shared lifecycle publishes the entire start batch before starting the model. The interrupted attempt's appended suffix is therefore removed, while the original prefix is reconstructed once. Normal, non-recovered Continue runs retain their previous start semantics.

Prefix chunks reuse the existing 8 KiB inline-text bound and split only at UTF-8 boundaries. They are output projection only, not extra user/model input. The context's existing 64 KiB prefix bound remains unchanged. Event construction completes before committing the projector's started state. Final result projection retains the original Continue semantics so the existing suffix-overlap handling still applies; no new event type, proto field, or database schema is needed.

Source mapping: `src/agents/session.rs::assemble_ordinary_with_checkpoint_mode`, `src/agents/events.rs::AgentEventProjector::start`, and `src/agents/events_tests.rs`; UI contract verification is in `apps/elitea-web/src/features/chat-messages/lib/chatStreamReducer.test.ts`. This extends the new platform's existing output-continuation protocol rather than copying current-platform restart behavior.

Verification: all 31 Rust event-projection tests, the restored-assembler test, 93 UI reducer tests, UI TypeScript checking, and strict Rust library/test Clippy pass. The prefix test covers a short answer and a large Unicode/escaped prefix, exact reconstruction, and refusal to start the same projector twice. The UI sequence replaces an interrupted suffix and preserves one final saved-prefix-plus-recovered-suffix answer. These component checks do not substitute for deployed fresh-browser restart acceptance. Recovery enablement, live crash/replacement verification, and external MCP reconnect remain pending.

## Deployment opt-in routing

`RuntimeDeployConfig.agent_model_checkpoint_recovery` is an optional boolean defaulting to false. Validation refuses enabled recovery without `agent_checkpoint_connection_path`. Production composition passes this setting to `AgentDeliveryProcessor`, whose normal verified delivery entry point selects the checkpoint-aware route only when enabled. The generic execution multiplexer retains the same capability dispatch and now explicitly requires the existing progress-replay connector.

Source mapping: `src/config.rs`, `src/execution/production.rs`, `src/execution/agent_delivery_processor.rs`, and `src/execution/execution_delivery_processor.rs`; fixture adjustment in `src/transport/redis_connector.rs`. The processor regression now enters through ordinary configured delivery processing rather than directly calling the recovery helper. All 29 checkpoint-filtered tests and strict library/test Clippy pass. The configuration test checks default-off behavior, explicit enablement, and rejection without durable storage.

This adds no new migration. Rehearsal inspection confirms shared history is at 130; the previously reviewed shared/0131 runtime-claim migration is required before enabling recovery. Tenant schema migrations are not required for this rollout. Image builds are in progress; this section does not claim deployed enablement or browser/external-client acceptance.
