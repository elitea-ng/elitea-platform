# Nested agent compaction acceptance

## Settings and ownership

`src/agents/assembly.rs::from_nested_version_for_type` inherits the admitted parent context policy and dedicated summary-model selection.
It recalculates the budget against the child's authorized task-model limits and output reserve.
A nested child's saved context policy does not override this inherited policy.
This follows the earlier inheritance agreement; explicit child overrides remain a separate product decision.

`src/agents/model_scope.rs` gives each invocation separate history and checkpoint identity.
The identity includes the execution, generation, child session, and agent identity.
`src/agents/application_tools.rs::drain_child` forwards execution events to the trace and returns the selected child answer to its caller.
The parent does not receive the child's entire tool history as its tool result.
The child can compact repeatedly within one delegated task.

The current functional reference is `projects/elitea-sdk/elitea_sdk/runtime/tools/application.py`.
Its `client.application` call constructs the saved child application separately.
This reference defines delegated application behavior; it does not define the new compaction algorithm.
The existing [durable compaction mapping](durable-context-compaction.md) records ADK and PostgreSQL ownership.

## Live rehearsal

Fresh headed Playwright opens chat 614 and sends a task to saved parent 41, version 48.
The parent delegates once to child 40, version 47.
Both use the configured Haiku task model, with a 128,000-token window and 4,000 reserved output tokens.
The caller selects Full mode and the dedicated Luna summary model, with 8,192 maximum output tokens.
The child has no separate saved compaction policy.
The fixture serves fictional, read-only records over the existing trusted rehearsal TLS route.
No browser requests are intercepted.

Execution `c98e88dde9166d84c6de435d5f972537` settles as `SUCCEEDED`.
The durable tool ledger contains one `elitea_agent_40_v_47` call and 24 `read_compaction_record` calls.
All 25 records have `is_error = false`.

The child's context events carry the same nested call identity for both compactions:

| Scope | Before | After |
| --- | ---: | ---: |
| Child, first compaction | 151466 | 1137 |
| Child, second compaction | 152203 | 1206 |
| Parent, initial/final measurement | 354 | 766 |

These values are estimated input tokens, not provider usage counters.
The child events retain Full mode, 122,720 usable input tokens, and a 110,448-token trigger.
The parent emits no compaction event.
Its final answer preserves CEDAR-731, teal, archive verification, the handoff step, and the 24-read count.
The browser reports no page errors, and its final answer remains exactly equal after reload.
The final screenshot is inspected visually.

The run uses worker image `sha256:0c1c3a9673a0855142988cc5856f82f1ee297a68925fac34fd27d2b3070705ac`.
It uses UI image `sha256:84af8476b3ff188e2909552a748d180c622e0d2ac328f115331ee4924dc495a8`.
Local evidence files use the `elitea-nested-compaction` prefix.

## Limits

An earlier chat 613 selected the child directly and did not test nesting.
It is not counted as nested acceptance.
This run proves one nested child, repeated compaction, parent isolation, and stable final-answer reload.
It does not prove sibling concurrency, nested worker replacement, pipeline-model recovery, or a million-token provider route.

## Worker-loss follow-up

Fresh chat 615 tests worker loss after the first completed child compaction.
Execution `93f16e11a8683e19b5cfca6aa9d36de7` uses the same saved parent and child.
The browser observes child input fall from 151,482 to 1,077 estimated tokens.
The test sends SIGKILL to the rehearsal worker and starts that container again.
Redis redelivers the original command after the recovery interval.
The replacement worker emits `execution.failed` with code `INTERNAL`; the browser displays that failure.
This is a failed recovery test, not successful continuation.

Read-only inspection of the agentstate database confirms both persisted boundaries:

- The parent checkpoint phase is `tool_may_have_started`.
- The child model-scope checkpoint phase is `model_pending`.
- Both sessions retain their compaction-state key.

`src/agents/model_checkpoint.rs::before_tool` writes the parent boundary before delegation.
`restore_validated` admits only `ModelPending` or admitted `ContextPending` states.
`src/agents/session.rs::inspect_model_checkpoint` therefore rejects the parent before runtime reconstruction.
`src/execution/checkpoint_recovery.rs` converts that inspection failure into terminal output.
The worker log records `agent_delivery.authorization_terminal_retired` for the redelivery.
This corrects the initial suspicion that Main terminated the execution before worker recovery.

The repair requires coordinated parent and child recovery evidence.
Persist the exact delegated call identity and its child model scope before child execution.
On recovery, validate that evidence under the replacement root claim and current authorization.
Resume the existing child checkpoint, then deliver its result to the waiting parent call.
Do not restart the child from its original task or replay arbitrary unfinished tools.
Keep pending side-effect tool boundaries closed unless their own durable receipts permit recovery.
Verify completed-child delivery, interrupted-child continuation, stale writers, and unchanged sibling calls.
Then repeat the fresh browser crash test and inspect the durable tool ledger for duplicate reads.
Local failure evidence uses the `elitea-nested-crash` prefix.

## Child request restoration foundation

The first repair separates authorized crash recovery from ordinary scope loading.
`src/agents/ordinary.rs::prepare_runner_inputs` enables pending-model restoration only for the checkpoint-recovery assembly path.
Fresh calls, regeneration, and human-approval resumes do not enable it.
`ModelScopeSessions` carries this mode through the existing child-scope factory.
For an existing scope without a guard-replay marker, `ScopedModelCheckpoint::writer` restores the validated model checkpoint.
The next model preparation uses the saved request, including its compacted history and exact tool declarations.
It persists the replacement invocation marker before provider dispatch.
Unfinished ordinary tool boundaries remain rejected.
New child scopes still start normally; scope loading alone does not grant recovery permission.

Two focused tests verify exact request restoration without another summary call and refusal of unfinished tools.
The ordinary scope-reload and guard-replay tests remain unchanged.
The PostgreSQL-enabled agent suite passes 378 tests, with no failures or ignored tests.
Strict Clippy passes.
This foundation does not admit the parent's unfinished delegation boundary.
Parent/child coordination and repeated browser crash acceptance remain required before deployment acceptance.

## Completed child delivery

`src/agents/model_scope.rs` stores a completion receipt with the successful terminal event.
The existing session transaction commits the event and receipt together under the root writer fence.
The receipt binds the execution, generation, definition, agent, invocation, and event identities.
The existing completion adapter fills an empty streamed terminal event before storage.
No database migration or duplicate response store is required.

Authorized recovery validates the receipt against its stored event before it restores an unfinished model request.
A later model request supersedes an earlier completion receipt.
An interrupted, failed, partial, or pending-tool event cannot establish successful completion.
Ordinary invocation and guard replay do not consume these receipts.

`ModelScopeAgent::run` returns the saved content before it invokes the child runtime.
The delivery uses the replacement invocation and branch identities.
It does not replay old state changes, provider usage, or provider metadata.
The parent continues to receive the selected child answer through `ApplicationAgentTool::drain_child`.
The legacy application reference remains unchanged; this recovery behavior extends the existing delegated-result contract.

Tests cover completed delivery through an ADK Runner, superseded completion, interruption, changed definitions, and detached receipts.
The PostgreSQL test also checks completion delivery after writer takeover and rejects the stale writer.
The PostgreSQL-enabled agent suite passes 383 tests, with no failures or ignored tests.
Strict Clippy, formatting, and whitespace checks pass.
These component checks do not close the parent delegation recovery boundary or replace the browser crash gate.

## Waiting parent recovery

`src/agents/model_checkpoint/delegation.rs` adds a private `delegation_pending` checkpoint phase.
Before saved-agent dispatch, it stores the exact parent request and the model's complete call batch.
The batch retains call IDs, arguments, and tool declarations.
Only batches containing admitted saved-agent calls receive this recovery permission.
Mixed ordinary-tool batches keep the existing unfinished-tool boundary.
Pipeline delegation still requires its graph recovery path.

`src/agents/events.rs::agent_tool_names` obtains eligible tools from the runtime application catalog.
`src/agents/session.rs::build_runtime_agent` binds these names after fresh runtime authorization.
Recovery validates the stored execution, generation, definition, event marker, tool schemas, and current catalog membership.
It writes the replacement delegation marker before dispatch.
A one-shot model adapter emits the original call batch through normal ADK tool execution.
The adapter follows the existing direct-approval replay pattern in `src/agents/direct_hitl.rs`.
It does not ask the provider to select another child.
ADK retains the restored parent request before it processes the child result.

The child call ID selects the same child model scope.
An unfinished child restores its model request; a completed child returns its validated completion receipt.
The parent requests its final answer after the child result returns.
The existing claim fence controls these writes; Main does not own the checkpoints.

The current-platform reference remains `projects/elitea-sdk/elitea_sdk/runtime/tools/application.py`.
Its delegated task and final-result contract remains the behavioral reference.
Worker-loss continuation extends that behavior using the existing ADK and PostgreSQL boundaries.

An ADK Runner test interrupts a waiting parent and checks the exact replayed call ID and arguments.
It rejects removed agent bindings and changed tool declarations before resumed dispatch.
It verifies one original provider selection and one final provider response.
Batch validation rejects duplicate IDs and mixed ordinary tools.
The PostgreSQL-enabled agent suite passes 385 tests; strict Clippy passes.
Fresh worker-crash browser acceptance remains required.
