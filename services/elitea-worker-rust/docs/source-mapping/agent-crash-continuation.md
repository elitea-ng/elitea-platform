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
