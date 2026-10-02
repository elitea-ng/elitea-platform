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

## Successful worker-loss continuation

Fresh headed Playwright opens chat 616 after the rehearsal worker replacement.
The worker image is `sha256:62314f31dc934d2c3a090801f914ecf84d2407d85fe7137d19b0e0f4ac985d56`.
Its Rust source matches commit `8d92e672`; the build retains auditable release metadata.
Main and UI images remain unchanged.
The deployment preserves worker secrets, five mounts, networks, and resource limits.
The test sends real browser requests without interception.

Execution `83190c68ced8e968dbd089b778eb3807` delegates to child call `tooluse_q3O5V2d3ip5CQoKVYqhlxo`.
After the first child compaction completes, the test kills the worker with SIGKILL and starts it again.
The replacement worker resumes the same execution and child identity.

| Observation | Estimated input tokens |
| --- | ---: |
| First child compaction, before | 151459 |
| First child compaction, after | 1051 |
| First child measurement after worker recovery | 1051 |
| Second child compaction, before | 152123 |
| Second child compaction, after | 1323 |
| Parent final measurement | 780 |

The execution settles as `SUCCEEDED`.
Claim attempt 1, lease epoch 1, ends with `LEASE_EXPIRED`.
Claim attempt 2, lease epoch 2, ends with `SETTLED`.
The durable tool ledger contains one logical `elitea_agent_40_v_47` call and 24 `read_compaction_record` calls.
All records have `is_error = false`.
The final answer retains CEDAR-731, teal, archive verification, the handoff step, and the 24-read count.
Playwright records no page errors and confirms exact answer equality after reload.
The final screenshot shows one complete answer and no failure banner.

Local evidence files use the `elitea-child-recovery-crash` prefix.
The build manifest uses the `elitea-child-recovery-build-source` prefix.
The read-only claim and tool-count query uses `elitea-child-recovery-check.sql`.

This run closes the observed waiting-parent recovery failure from chat 615 for one saved child agent.
It proves continued child work, repeated compaction, parent completion, and browser reload after whole-worker loss.
It does not prove concurrent sibling recovery, deeper delegated recovery, graph recovery, or external-effect reconciliation.
The completed-child delivery race has component and PostgreSQL evidence, but no separate deployed failure-injection case yet.

## Scoped delegation recovery

`src/agents/application_tools.rs::build_uncached` now binds each child's admitted agent names to its model-scope storage configuration.
This configuration shares the root claim but does not share child histories or pending calls.
`ScopedModelCheckpoint` applies the same delegation boundary with the child's durable session identity.
`ScopedDelegationModel` uses the restored scope writer after the before-model callback initializes it.
It replays the stored grandchild call through normal ADK dispatch.
Leaf agents keep their existing model path.
Guard replay retains its separate control path.

The new scoped ADK Runner test interrupts a child while its own delegated call waits.
Recovery emits the same call, retains the child's prepared history, and requests only the final provider response.
The PostgreSQL-enabled agent suite passes 386 tests; strict Clippy passes.
These checks do not replace the pending deployed deeper-chain and concurrent-sibling crash test.

## Deeper-chain verification blocked before crash injection

Fresh headed Playwright chat 617 uses a parent with concurrent saved-agent children:
a short report child and a child that delegates to the compaction fixture agent.
The deployed worker image is `sha256:9f1417a046c2d0063ea525438fb5e96e7cc004409dd48e3dac66023b86df26bd`;
its source matches `0878db6e`.
Execution `e4a3f98da2f33f7279e3e043c6792300` fails before the planned worker kill.
The short child has a durable completion receipt, both waiting ancestors have
`delegation_pending` checkpoints, and the leaf has a `context_pending` checkpoint.

The leaf reports estimated input of 151486 tokens at its first compaction.
The worker logs `context_summary_reference` at summary validation, attempts one
correction, and exits with the same code. This validation rejects a reference
value that cannot be matched to source records. The exact rejected value is not
present in the inspected diagnostics; its cause remains unconfirmed.
No completed compaction or worker restart occurs in this run.
This is not evidence of successful deeper-chain or concurrent-sibling recovery.
The earlier chat 616 parent-and-child crash proof remains valid.

Evidence uses the local `elitea-deep-recovery` prefix: browser result, screenshots,
execution metadata, checkpoint query, and worker log. The next acceptance run
must retain this failed evidence and verify both completed-child reuse and the
unfinished grandchild after an actual worker interruption.

## Streamed source reference correction

Chat 618 repeats the pre-crash failure with execution `9b53e75bcdbe02828f137da95613e5ba`.
Worker diagnostics confirm that the grandchild inherits Luna for summarization and uses Haiku for task execution.
The summary and its correction again fail source-reference validation.
No worker interruption occurs in this run.

A separate streaming provider probe reproduces a source-matching defect using verified fictional records from chat 617.
All twelve tool outputs contain only fixture fields and repeated fictional evidence text.
The probe returns `Pass 1: Reading indexes 1-12` as a reference.
That text spans adjacent model text parts: `Pass 1: Reading` and ` indexes 1-12**`.
The previous validator searches each string separately and rejects the contiguous displayed text.
The original failed summaries are not retained, so this probe does not identify their exact rejected values.

`src/agents/context_summary.rs::contains_reference` now recognizes adjacent text parts within one content message.
The matcher retains a reference-sized suffix instead of copying the full source for each reference.
Tool, reasoning, and message boundaries remain separate. Invented or paraphrased references remain invalid.
Focused tests cover the reproduced text, Unicode splits, and prohibited cross-boundary combinations.
The existing [context summary mapping](context-summary-compatibility-20260918.md) provides current-platform and ADK source references.
This correction changes only Rust summary validation. It changes no schema, execution authority, or checkpoint ownership.
The PostgreSQL-enabled agent suite passes 388 tests with no skips. Strict Clippy and formatting checks pass.
The deployed acceptance below verifies this correction with nested worker recovery.

## Concurrent sibling and grandchild recovery acceptance

Fresh headed Playwright chat 619 runs against worker image `sha256:f216f359c9318c5dc93dc914f35189ef3c1b30fe29b9e52c675417b87eea8272`.
The image contains `0878db6e` and the streamed-reference correction above.
Execution `fc4b014836d598bece46efaf0928606d` starts both root children in the same parallel batch.
The short child completes while its sibling waits for a grandchild's tool work.
The test captures checkpoint phases and completion receipts, then kills the worker after the first grandchild compaction.

| Observation | Estimated input tokens |
| --- | ---: |
| First grandchild compaction, before | 151467 |
| First grandchild compaction, after | 1341 |
| First grandchild measurement after recovery | 1341 |
| Second grandchild compaction, before | 152405 |
| Second grandchild compaction, after | 1328 |
| Waiting child final measurement | 787 |
| Root final measurement | 985 |

The execution settles as `SUCCEEDED` after claim 1 expires and claim 2 settles.
The short child's completion receipt remains byte-for-byte identical across recovery.
Its receipt event is `elitea-child-2_llm_1790097752086520092`.
Each of the three delegated agents has one logical tool record and no error.
The tool ledger contains 24 successful `read_compaction_record` calls.
The fixture server independently records exactly 24 physical calls, with each index requested twice.
The final answer includes `SHORT-CEDAR-OK`, CEDAR-731, teal, archive verification, the handoff step, and the 24-read count.
The browser records no page errors and retains the exact answer after reload.
Visual inspection confirms one complete result without a failure banner or duplicated answer.

This run proves completed-sibling reuse and two-level delegated recovery after whole-worker loss.
Each unfinished ancestor resumes its saved delegation instead of making another provider selection.
It does not prove graph recovery, arbitrary external-effect reconciliation, or every possible crash boundary.
Local evidence uses the `elitea-stream-reference-recovery` prefix.
The image source manifest uses `elitea-stream-reference-build-source`.
