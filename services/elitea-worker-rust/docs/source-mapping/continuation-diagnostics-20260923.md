# Continuation failure diagnostics

## Behavior reference

Current SDK source: elitea_sdk/runtime/tools/llm.py, _continue_nested_output and parallel application result handling.
The SDK reports continuation attempts, failure reason, stop reason, and partial-output availability.
Parallel children return this information in an error ToolMessage.
The sequential tool loop raises OutputContinuationExhausted.
These paths do not have identical parent recovery behavior.

## Rust mapping

src/agents/model_checkpoint/output.rs owns payload-free typed continuation causes.
src/agents/model_scope_output.rs attaches a cause at each failed continuation condition.
src/agents/application_tools.rs preserves the typed cause through the nested fatal channel.
src/agents/runtime.rs exposes only the typed cause to logging.
src/execution/native_agent_lifecycle.rs logs unrecovered start and stream failures at ERROR.
Each event includes execution identity, generation, stable codes, a cause message, and the existing diagnostic stack.
Handled authorization challenges return before this terminal error log.
Recoverable retries retain their existing severity.

Raw provider messages and arbitrary error chains remain excluded.
The public error code and database schema do not change.
Release line tables remain enabled.

## Validation and remaining work

Focused continuation tests cover the four-call limit, repair, partial output, and graph recovery.
A cause test verifies safe extraction through wrapping and rejects arbitrary provider text.
The nested channel test verifies that the original typed cause reaches its receiver.

This change does not complete observability.
Original failure-site stacks and suspended asynchronous ancestry need further work.
General user error guidance and copyable UI correlation identity also remain open.

The user clarified that child-local failures must not terminate a healthy orchestrator.
Add a structured child error result, with recovery guidance and partial-output availability.
Distinguish parent recovery from permission to repeat the same operation.
Do not convert cancellation, authorization, or execution-wide limits into recoverable tool data.
The ordinary nested-agent continuation path now returns an error report instead of sending the root fatal signal.
The report includes the cause, available partial output, recoverable=true, and retryable=false.
Recovery means revising the task, not permission to repeat the same call or its side effects.
The error field remains a string for the existing tool-error UI projection.
The structured failure field gives the parent explicit recovery guidance.
A failed report has no successful response field.
The failed child does not create a completed model receipt.
The parent can persist the error tool result through its existing checkpoint flow.
Direct root continuation failure still uses the terminal failure contract.
Other child failure categories and graph propagation require separate classification and acceptance.

## Pipeline boundary clarification

An LLM orchestrator can reason about failed child output.
A direct graph Agent node cannot treat that output as valid downstream state.
The invocation-owned pipeline application marker distinguishes these callers.
A marked Agent node retains the typed fatal continuation signal and stops the graph.
The existing direct LLM-node path also stops on continuation exhaustion.
The focused suite includes downstream-state protection and the new Agent-node classification.
Pipeline-as-child propagation remains a separate verification item.

## Error propagation acceptance matrix

Use the execution boundary, not the application type name, to select behavior.

| Caller | Failed operation | Required result | Evidence |
| --- | --- | --- | --- |
| Chat orchestrator | Agent child continuation | Return a failed tool report; let the parent decide | Local tests and deployed browser acceptance pass |
| Direct pipeline | LLM node continuation | Stop before downstream state is written | Existing focused graph test passes |
| Direct pipeline | Agent node calling an Agent application | Stop with the typed failure | Local classification and deployed graph acceptance pass |
| Pipeline | Saved child pipeline through an Agent node | Propagate failure; do not release partial state as success | Chat 677 verifies typed direct-tool failure and unchanged downstream state |
| Pipeline Agent node | Pipeline application | Propagate failure to the owning graph | Chats 665 and 677 verify continuation and direct-tool failure paths; other categories require separate proof |
| Chat orchestrator | Pipeline child | Stop the child graph; report its contained failure to the parent | Deployed continuation failure report and parent completion pass |
| Any scope | Cancellation or execution-wide limit | Preserve control semantics; do not offer automatic retry | Existing handling retained; combination tests remain |

The legacy platform does not establish acceptance for all nested combinations.
Verify each path against the intended state and error contract.
Keep graph expansion in gate 5, but track error propagation under gate 4.
Do not claim that all child error categories are handled by the continuation-specific correction.

## Deployed acceptance on 2026-09-23

Worker revision d705fa9a is deployed to rehearsal with existing configuration and mounts.
A fresh headed browser runs the original chat 637 without response fixtures.
The child exhausts continuation. The parent receives its error report and completes normally.
The parent explains the four-call limit, partial-output availability, and the prohibition on an identical automatic retry.
The response remains after reload. No root execution failure or browser error occurs.
The rendered screenshot is inspected.
Local evidence: elitea-nested-report-live.json, elitea-nested-report-live.png, and elitea-nested-report-frames.json.

The worker emits ERROR for nested_application_failed.
Its cause_message states that the answer still reached the output limit after four continuation calls.
The event includes application, version, invocation, tool-call identity, and the diagnostic trace.

The full PostgreSQL-enabled worker suite passes 1,212 tests before the pipeline-boundary refinement.
The refinement passes 31 focused continuation tests and strict all-target Clippy.
The refinement includes the direct Agent-node failure classifier and existing downstream graph-state checks.
The later direct Agent-node acceptance appears below. Wider nested-pipeline acceptance remains open.

## Direct pipeline Agent-node acceptance

Fresh headed-browser chat 663 runs a pipeline Agent node bound to the capped Agent application.
Execution 50d5318156ddd7c45c073ec3e69aee95 stops with OUTPUT_CONTINUATION_EXHAUSTED.
The friendly error persists after reload. No browser error occurs.
PostgreSQL contains only the initial graph checkpoint at step zero, with delegate pending.
The answer and final_text fields remain empty. The downstream finish node does not run.
Local evidence: elitea-pipeline-agent-failure-live.json and elitea-pipeline-agent-failure-live.png.

Direct-tool rejection tests also pass.
Reject and block_with_comment do not execute the protected call.
Authorization Skip reaches END with the pipeline-stopped explanation.
These are component tests, not new browser acceptance of authorization controls.

## Nested pipeline verification follow-up

Conversation 665 exposes a child checkpoint scope mismatch before the child model starts.
See [checkpoint scope mapping](nested-pipeline-checkpoint-scope-20260923.md) for the correction and verification status.

Fresh headed-browser conversation 666 checks an ordinary orchestrator calling an LLM-only pipeline.
The parent remains active and reports a child failure.
The report contains only a generic LLM-node explanation.
The expected continuation-specific reason is absent, so diagnostic acceptance fails.
The screenshot is `elitea-orchestrator-pipeline-failure-failed.png` in the local test evidence directory.
Do not count parent survival alone as complete error-contract verification.

## Typed pipeline continuation cause

The graph event bridge previously retained only the stable model error code.
It discarded the typed continuation reason before the root error projector received it.
The bridge now carries the safe continuation enum beside the code.
It never forwards arbitrary provider messages or source text.

An ordinary orchestrator's pipeline tool now returns the existing structured continuation failure contract.
The failed child graph stops. The parent receives retry and recovery guidance.
Direct pipeline execution still propagates the failure as a terminal event.
The tool drains queued node causes before accepting a generic graph error.
This preserves the specific cause when the graph wraps the model error in the same poll.

Source owners: `agents/graph/node_events.rs`, `agents/graph/llm.rs`, and `agents/application_pipeline.rs`.
The tool reuses `application_tools.rs::child_continuation_report` instead of defining a second failure format.
Current-platform orchestration behavior remains the reference described in the propagation matrix.
Deployed acceptance of this continuation follow-up passes below.

The follow-up passes all 1,215 worker library tests with PostgreSQL available.
Clippy passes for the library and tests with warnings denied.
The bridge regression checks typed cause preservation and provider-message exclusion.

## Deployed typed pipeline cause acceptance

Worker revision `18982ecb` runs as image `sha256:360c9ab9a1aa1633d61d3dc08a484d4b46564f2f711950b0e5c5777f60ad28f1`.
The worker retains its environment, limits, networks, and five mounts.

Fresh headed-browser conversation 665 retains terminal continuation failure and downstream suppression.
Fresh headed-browser conversation 666 completes its parent response after receiving the child failure report.
The parent explains the four-call limit, retryable=false, recoverable=true, and revise_task guidance.
It reports no available partial output and does not repeat the child call.
Both checks use real responses and pass reload verification without browser errors.
The screenshots are inspected.

Worker logs record both boundaries at ERROR level.
The cause_message states that output still reached the limit after four continuation calls.
The direct pipeline's public message remains the registered generic continuation explanation.
More detailed public error guidance and other failure categories remain separate gate 4 work.

Evidence files: `elitea-pipeline-nested-failure-live.json` and `elitea-orchestrator-pipeline-failure-live.json` in the local test evidence directory.

## Public error reference

The current UI reference is `projects/EliteaUI/src/[fsd]/features/chat/ui/error-trace/ErrorTrace.jsx`.
It provides expandable diagnostics with copy and download controls.
The replatform adds a separate public reference in `apps/elitea-web/src/features/chat-messages/ui/error-trace/FailureReference.tsx`.
`ApplicationAnswer.tsx` mounts this reference for ordinary errors and incomplete continuation responses.
The reference contains the error code, when available, and the response message ID.
It excludes conversation content, provider payloads, and internal stack traces.

Operators can resolve the message ID through `elitea_runtime.agent_execution_jobs.client_message_id`.
The existing `agent_cancel.sql::IsCurrentAgentCancellationReplay` query shows this execution binding.
This change adds no persistence fields, migrations, or worker contracts.

The focused ApplicationAnswer suite passes 24 tests. TypeScript checking passes.
A fresh headed Playwright browser reads real conversation 665 through the development UI and rehearsal backend.
The copied reference remains identical after reload. The browser reports no page errors.
The screenshot is inspected. The test uses no mocked requests and starts no additional model calls.
Evidence files are `elitea-error-reference-browser.json` and `elitea-error-reference-browser.png` in the local test evidence directory.
This verification uses the development UI. Container deployment is a separate acceptance step.

The follow-up deploys UI revision `0e4557e4` as image `sha256:2303350b4a02b07d224816df6c539da0903886b187cc782783ebfaf9494d4f00`.
Deployment preserves the existing environment, networks, and resource limits.
Fresh headed Playwright verification passes on the deployed conversation 665, including clipboard contents and stable reload.
There are no page errors or mocked requests. The deployed screenshot is inspected.
Evidence files are `elitea-error-reference-deployed.json` and `elitea-error-reference-deployed.png` in the local test evidence directory.

### Operator lookup

The UI explains that detailed diagnostics are available to operators in service logs.
It does not offer a stack-trace viewer or grant log access to ordinary users.
The error reference is a locator, not an access credential.

Use the copied message ID with the affected project and actor in an authorized, read-only database session.
The following psql query uses supplied variables:

```sql
SELECT binding.execution_id, binding.generation
FROM elitea_runtime.agent_execution_jobs AS binding
JOIN elitea_runtime.execution_jobs AS job
  ON job.execution_id = binding.execution_id
 AND job.generation = binding.generation
 AND job.capability_id = binding.capability_id
WHERE binding.client_message_id = :'message_id'
  AND job.resource_project_id = :'project_id'::integer
  AND job.actor_id = :'actor_id';
```

Filter retained worker logs by the returned execution ID and generation.
Inspect `error_code`, `upstream_error_code`, `cause_message`, `failure_reason`, and `failure_diagnostic` where present.
Use the log platform's event view to retain multiline diagnostic text.
Do not publish raw log records into chat or support tickets without review.

`ELITEA_RUST_FAILURE_DIAGNOSTICS=on` enables bounded stack capture before the failure occurs.
Disabled or rate-limited capture does not contain a stack. Enabling capture later cannot reconstruct an earlier stack.
Container replacement can remove local logs. Production operators need retained logs for historical lookup.
An authorized operator trace viewer remains a separate UI improvement.

Read-only rehearsal verification resolves message `f92c39ca-4578-5f94-932f-2143432a000d` to execution `085934c4898fb780acc1f8b580a7bec0`, generation 1.
The retained worker log contains the matching cause and diagnostic fields.

The operator-only explanation is deployed from `bfbebfd4` in UI image `sha256:6300dc7b7064053e3b352d2b4a6fedb506de7df201b3b81b7f356da70271497c`.
Fresh headed browser verification checks the visible explanation, exact clipboard reference, and stable reload in conversation 665.
The screenshot is inspected. The browser reports no page errors and uses no mocked requests.
Evidence: `elitea-operator-guidance-deployed.json` and `elitea-operator-guidance-deployed.png` in the local test evidence directory.

## Known child model failures, 2026-09-24

Ordinary child-agent model failures previously published a fatal application event, except for continuation exhaustion.
This could terminate the orchestrator after a child received a rate limit or denied model access.

`src/protocol/output.rs::model_failure` now owns the shared root-and-child classification.
The lifecycle imports this existing classification instead of maintaining another table.
`application_tools.rs::child_failure_report` returns the canonical code, safe explanation, retryability, recovery action, and available partial text.
`application_pipeline.rs` uses the same report for known child-pipeline model failures.
No raw provider message enters the report.

Recoverable means that the orchestrator can handle the report. It does not guarantee that the failed child can retry successfully.
Access and budget failures require administrator action. Transient failures require verification before retry. Input and continuation failures require task revision.
No automatic child retry is added. Prior tool effects require verification before another action.
Direct pipeline Agent nodes still fail terminally and now retain the known model code on their fatal event.
Unknown failures, cancellation, authorization controls, and invalid runtime state keep their existing terminal or control behavior.

Current-platform references are `elitea_sdk/runtime/tools/application.py` and `elitea_sdk/runtime/tool_outcome.py`.
The former preserves graph interrupts as control flow. The latter defines typed tool outcomes with retry information.
The Rust implementation follows those distinctions through its existing report and event contracts.
No database schema or protobuf changes are required.
Deployed acceptance of these additional child failure categories remains open.

The assembled-agent regression injects HTTP 429, 403, and 503 at the child model gateway boundary.
Each case completes the parent, preserves the expected report, and makes exactly three model requests without a child retry.
The report excludes a provider-body secret sentinel.
Additional checks preserve terminal direct-node handling and reject conversion of cancellation, authorization, or invalid-state errors.
All 1,217 Rust library tests pass with PostgreSQL available and no ignored tests.
Formatting and strict library-and-test Clippy checks pass.

## Tool-result projection boundary, 2026-09-24

Live child-failure verification in conversation 667 stops at `agent_event.resource_exhausted` before the expected child model failure.
The persisted child result contains about 49 KB. It fits the transport frame but exceeds the 40 KiB inline-value bound.
The previous chunk decision checks only the rendered frame. The subsequent inline check rejects this valid result.

`src/agents/events.rs::completed_tool_entry` now chunks results that exceed either bound.
The existing chunk protocol, digest, total-result limit, and exact tool identity remain unchanged.
No schema change or resource-limit increase is required.
The replatform Python reference is `services/elitea-worker-python/src/elitea_worker/handlers/agent_events.py::_chunk_tool_output`.
It preserves tool results through bounded transport chunks. Rust also enforces its explicit inline-value limit.
This transport correction does not change current-platform tool behavior.

The new regression reproduces the failure with a 48 KiB text result before the fix.
After the fix, it verifies exact chunk reassembly and the tool-end event.
All 42 event-projection tests pass, with no ignored tests.
Browser acceptance and the original child-model-failure verification remain open until the repaired worker runs.


### Deployed child-failure acceptance

Worker image `sha256:c6c0cc33b1eea7966ba6443eabc0f1fa271cbd1b58a315d99421deca38477e34` contains the projection fix from `8bee608b`.
Conversation 668 completes with execution `b524870c517a17919e726686319f7e24` in state `SUCCEEDED`.
The child reports `CONTEXT_BUDGET_EXCEEDED`, `retryable: false`, and `recovery_action: revise_task`.
The parent explains the child failure and completes without another delegation.
Captured events contain one child invocation, twelve record reads, and twenty-four output chunks.
A fresh headed browser verifies the persisted report after reload. It reports no page errors and uses no response mocks.
The screenshot is inspected. Local evidence is `elitea-child-model-failure-accepted.json` and `elitea-child-model-failure-observed.png`.
The original browser assertion expected the request marker, but the parent follows its saved final-marker instruction instead.
The follow-up assertion checks that saved marker and the actual failure report. No second execution is submitted.
This proves the context-budget case. Deployed rate-limit and provider-access cases remain separate acceptance work.

### Output-delivery failure explanation

The user reports that the generic resource-limit message gives no useful recovery guidance.
A new registered code, `OUTPUT_DELIVERY_LIMIT` (24), identifies terminal event-projection capacity failures.
Its message explains the delivery failure, possible missing results, and administrator support reference.
It does not tell users to repeat actions that may already have effects.
`execution/native_agent_lifecycle.rs::projection_failure` selects this code.
The same boundary now logs at ERROR with execution ID, generation, internal error code, and safe failure reason.
`protocol/output.rs` and Main's `internal/transport/runtimegrpc/output/server.go` register the exact public contract.
The shared fixture verifies Rust replay and Main ingestion. Unregistered message text remains rejected.
Existing `RESOURCE_EXHAUSTED` records retain their original contract for durable replay.
The protobuf generator updates Go and Python bindings. Rust generates its binding during compilation.
No database migration is required. Deploy Main before a worker that can emit the new code.
The existing UI displays the registered explanation and its support reference without a new component.
This is a new replatform delivery distinction. Legacy business behavior does not define this worker transport boundary.
Deployment and browser verification of the new explanation remain open.

Main output-boundary tests pass. All three Rust output-policy tests pass, including historical replay and untrusted-message rejection.
Strict Rust Clippy, formatting, and protobuf generation checks pass.


### Output-delivery deployment, 2026-09-24

Main image `sha256:aa69047c6b68f2c809ae73f72edc5476adcee0116e59a28840c6f74a39f7075a` is deployed before the worker.
Worker image `sha256:3b2a75b4d1575c5b22c7fe0d9d9e91d256728d3e7ec418013378ee192e4171f1` follows it.
Both images use source commit `e66c99eb6`.
The deployment retains service environments, networks, resource settings, and all six Main mounts and five worker mounts.
The deployment checks that no execution claim is active before replacement.
Conversation 668 still displays its persisted child-failure report in a fresh browser without response mocks.

A separate controlled browser test verifies the new explanation, support panel, clipboard reference, and reload.
It replaces one historical failure response in browser memory. It does not change persisted messages.
Evidence is `elitea-output-guidance-rendering.json` and `elitea-output-guidance-rendering.png` in the local evidence directory.
The screenshot is inspected, and the final browser run exits successfully without page errors.
This is rendering evidence only. It does not prove live emission of `OUTPUT_DELIVERY_LIMIT` or its corresponding operator log.
Those live acceptance checks remain open. Protocol tests separately prove registered Main ingestion and Rust replay.
The screenshot also shows an invalid negative thinking duration in historical chat 667. Record that UI issue for separate investigation.


### Public application metadata boundary, 2026-09-24

Live output-limit testing exposes a separate assembly rejection in conversation 671.
The saved pipeline contains 48 KB of synthetic fixed arguments. Its public application details exceed the 32 KiB metadata limit.
The assembly failure occurs before tool execution. It does not verify `OUTPUT_DELIVERY_LIMIT`.
Earlier fixture attempts 669 and 670 use the wrong relation type and provide no runtime proof.
Saved pipeline toolkit relations use `entity_type: agent`, matching `apps/elitea-web/src/features/agents/lib/toolRelation.ts`.
The MCP-backed fixture requires a direct `mcp` node. Both the relation type and exact toolkit identity must match.

`src/agents/session.rs::public_application_details` previously copied application instructions into terminal display metadata.
The function now removes root and version instructions from that public copy.
Execution retains the original frozen definition. Skill identities, version identities, names, and agent type remain available.
The existing removal of skill bodies and project context remains unchanged.
The public metadata bound remains 32 KiB. This change does not raise execution or transport limits.

Rust emits the metadata in `agents/events.rs::finish_after_eos` through `full_message`.
Main consumes `application_details.agent_type` or `version_details.agent_type` in `agent_execution_results.go::decodeCurrentAgentFullMessage`.
Current and new UI source searches find no reader of application instructions from this message field.
This is a replatform projection correction, not a change to legacy instruction execution or persistence.
The regression supplies large root and version instructions, checks retained identities, and verifies that the original definition remains intact.
Deployment and renewed live acceptance remain open for this correction.

All 24 session tests pass without ignored tests. Strict library-and-test Clippy and formatting checks pass.


### Public metadata deployment and live test outcome, 2026-09-24

Worker image `sha256:3680886f9d49fc80f6cbc6f998fbf1d9ebc778d21cb854c671bea84b8ff6540d` is deployed from source `a4331b8fe`.
The release build passes. The replacement preserves all five mounts and the existing environment, networks, and limits.

Fresh headed Playwright conversation 672 uses application 84, version 91, with the corrected MCP relation.
Execution `1379c1f6fef463a2bf03ae35b391ef68` reaches execution and fails.
The worker records ERROR with `native_agent.event_failed`, upstream `agent.legacy`, and the generic runtime explanation.
The browser assertion for `OUTPUT_DELIVERY_LIMIT` fails. No response is mocked.
Do not count this run as delivery-limit acceptance.

The fixture is unsuitable for the intended projection test.
`agents/graph/direct_tool.rs::invoke_and_project` invokes the tool and projects graph state without a model-loop tool-start event.
An oversized synthetic argument therefore does not test the model-loop tool-start projector.
The direct-node implementation also collapses several distinct errors into `node_failure`, which becomes a generic graph failure.
The precise failing stage in this execution is not established by the safe operator fields.
Record that diagnostic loss separately; do not infer a provider, context, or delivery limit from it.

The public metadata regression remains verified by its focused tests, and the deployed run passes the earlier assembly boundary.
Live emission and operator-log acceptance of `OUTPUT_DELIVERY_LIMIT` remain open.
Local evidence: `elitea-output-delivery-live-live.json`, `elitea-output-delivery-live-frames.json`, and `elitea-output-delivery-live-failed.png`.


### Direct-tool failure stage preservation, 2026-09-24

Reference SDK commit: `966526e8334354366dd161b606d73fe8e204b850`.
`elitea_sdk/runtime/langchain/langraph_agent.py` binds direct Toolkit and MCP nodes through `FunctionTool`.
`elitea_sdk/runtime/tools/function.py` classifies local failures and records tool outcomes, including retry information.
That implementation can return raw arguments and exception text in messages. Rust does not copy that behavior.
The replatform contract stops a direct pipeline node when its required operation fails.

`agents/graph/direct_tool.rs` now retains a typed cause and static stage until the graph boundary.
Stages distinguish input mapping, identity binding, authorization, confirmation, argument digest, tool execution, and state projection.
The node emits an ERROR event within its execution span. It includes the safe cause and static upstream error code.
The event excludes arguments, results, and provider exception messages.
The graph error retains the safe stage and cause. It does not add retries or change authorization decisions.
The root runtime still maps this graph error to a generic public failure. Public contract propagation remains open.

All 13 direct-tool tests pass without ignored tests.
Tests verify distinct binding and result errors, plus argument-capacity rejection before tool execution.
Strict library-and-test Clippy and formatting checks pass.
Deployment and live diagnostic acceptance remain open.


### Direct-tool diagnostic deployment and acceptance, 2026-09-24

Release image `sha256:03985774d329bfdfcfa0cd46164877e801ddf863a286ae8128c39c73d658ae09` is deployed from `9441cdf42`.
The build succeeds. Deployment preserves the five mounts and existing service configuration.
Conversation 672 runs again through a fresh headed Playwright browser, without response mocks.
Execution `99e73a259bb0d3fcfa5fc97b59154033` reaches FAILED.
Its ERROR event identifies `argument_digest` with cause `the direct-tool node exceeds its resource bound`.
`argument_digest` enforces `MAX_CONFIRMATION_ARGUMENT_BYTES`, which is 40 KiB.
The 48 KB fixture fails before tool invocation. This proves the operator diagnostic, not output-delivery projection.
The placement of the confirmation bound on ordinary calls requires review before changing any capacity limit.

The submission browser check does not observe the expected failure before its assertion times out.
A separate fresh browser readback verifies the latest persisted failure, expanded support reference, and stable reload.
The screenshot is inspected. It shows operator-only diagnostic guidance and a message ID, without internal stack details.
No browser page error occurs during readback. Live streaming acceptance remains open.
Evidence: `elitea-direct-diagnostics-readback-result.json` and `elitea-direct-diagnostics-readback.png`.
The current UI still displays INTERNAL and the generic runtime message.
Propagate the typed direct-node failure through the pipeline event bridge and registered public error contract.
Do not parse arbitrary graph exception text to choose a public error.


### Typed pipeline failure contract, 2026-09-24

Direct-node failures now publish a safe category through the existing invocation-owned pipeline event bridge.
`graph/direct_tool.rs` selects the category from its typed cause and static stage.
`graph/compiler.rs` carries the event sender in `PipelineNodeRuntimes`.
`agents/pipeline.rs` supplies that sender for root and nested pipeline bindings.
`graph/node_events.rs` delivers the failure before the generic graph exception can replace its category.
No exception-text parser or provider-payload projection is introduced.

Protocol codes 25 through 30 distinguish invalid input, input capacity, unavailable tools, failed calls, invalid results, and result capacity.
`protocol/output.rs` registers safe messages and durable replay checks.
Main registers the same messages in `internal/transport/runtimegrpc/output/server.go`.
The shared policy fixture checks exact registration and rejects substituted message text.
Deploy Main before a worker that emits these new codes. No database migration is required.

A direct pipeline failure still stops the graph. An ordinary parent receives the existing structured child failure report.
The report does not permit an automatic identical retry. Pipeline-node callers retain fatal propagation.
Existing approval, authorization, and exact toolkit identity behavior remain unchanged.
The current SDK classification reference remains `FunctionTool` at commit `966526e8334354366dd161b606d73fe8e204b850`.
Rust preserves its useful distinction between failure classes without copying raw exception or argument text into messages.

The new runner test invokes a tool once and rejects its malformed result through the pipeline event bridge.
It verifies `pipeline.result_invalid`, the registered public category, and exclusion of a synthetic private response body.
Deployment, live streaming, and browser acceptance of these public categories remain open.

Validation passes: 14 direct-tool tests, six model-failure contract tests, Main output-transport tests, and strict Rust Clippy.
No tests are ignored in those Rust runs. Formatting and generated protocol checks pass.
An initial narrow contract filter matches zero tests; the corrected `model_failure` filter supplies the six-test evidence above.


### Typed pipeline input-limit browser acceptance, 2026-09-24

Main image `sha256:ff487d7dfa0c2776d15062daab01d30a3f9d9836635da9ec05d050a4946dfb46` deploys before the worker.
Worker image `sha256:056c29a65183678ae0e57a509c30f51a6a9b09b1298efddd4fdc2e215a997e7d` deploys from `810b8ad8c`.
Configuration and all six Main mounts and five worker mounts remain unchanged.

Fresh headed Playwright conversation 673 uses application 85, version 92. No response is mocked.
The browser receives `execution.failed` with `PIPELINE_INPUT_LIMIT` and the registered explanation.
It displays that explanation and the support code before reload. Both remain after reload.
The screenshot is inspected. It explains the input size limit and distinguishes it from model context capacity.
The support panel explains that detailed diagnostics are available to operators. No browser page errors occur.
Evidence: `elitea-pipeline-input-limit-result.json`, `elitea-pipeline-input-limit-frames.json`, and `elitea-pipeline-input-limit.png`.
This closes live input-limit propagation acceptance. It does not prove the other five new categories or output-delivery projection.

### Confirmation capacity ownership correction

The live test identifies a misplaced bound in `graph/direct_tool.rs::argument_digest`.
Hashing applies the 40 KiB confirmation bound to ordinary direct calls before tool invocation.
The digest now uses the existing 512 KiB direct-node value bound. The digest output remains fixed in size.
`sensitive_decision` retains the 40 KiB input bound before it creates or resumes a confirmation.
Ordinary calls do not include the full arguments in a confirmation event. MCP authorization uses the digest for correlation.
This correction does not increase the direct-node value bound or the browser confirmation bound.
It does not change digest contents, authorization decisions, or exact tool identity.

The regression sends 48 KiB through an ordinary node and verifies one tool call.
The same input through a sensitive node fails before tool invocation.
This is a replatform boundary correction; the current SDK is a behavior reference, not the source of these transport bounds.
Deployment and live acceptance of the capacity correction remain open.

All 15 direct-tool tests pass without ignored tests. Strict library-and-test Clippy, formatting, and diff checks pass.


### Direct-tool acceptance and capacity deployment, 2026-09-24

Fresh headed browser cases 675 and 676 run on worker image `056c29a65183678ae0e57a509c30f51a6a9b09b1298efddd4fdc2e215a997e7d`.
Chat 675 calls the read-only fixture with valid input and an invalid output mapping.
Execution `b01672da0d821b564baa69d8d6701c13` fails with `PIPELINE_RESULT_INVALID`.
The live event, visible explanation, support reference, and reload checks pass.
Chat 676 uses a valid mapping and returns the fictional Cedar record.
Execution `d4154b62bb7b82d287f1bab810872d53` succeeds. The live answer and reload checks pass without a failure event.
These cases use no model call or external write.

Worker image `sha256:892896db8db7ca99da1ff16ea730a98713b5a782facf810255457bcc0dd27bd8` then deploys from `caa621d36`.
All five mounts and service settings remain unchanged.
Fresh browser chat 674 submits the 48 KB input previously rejected during digest calculation.
Execution `fdc4eb777c38e1bc7d9f9f66460500ff` reaches `tool_execution`, as verified by the ERROR event.
Its upstream code is `tool.execution.internal`; the browser receives `PIPELINE_TOOL_FAILED`.
The specific explanation, support reference, and reload checks pass.
This proves passage through the corrected digest boundary, not successful execution of a large-input tool.
The fixture accepts only an index and caps HTTP request bodies at 4096 bytes; the oversized synthetic request is deliberately invalid.
The component regression separately proves successful ordinary execution with 48 KiB of input.

All three browser runs use no response mocks and report no page errors. Screenshots are inspected.
Local evidence prefixes are `elitea-result-invalid`, `elitea-direct-success`, and `elitea-tool-failure`.
The result files distinguish the observed live failure events from the successful response.
Output-delivery-limit acceptance and other Gate 4 verification remain open.


### Nested direct-tool error propagation acceptance, 2026-09-24

The deployed worker remains `caa621d36`; no new runtime change is required.
Fresh headed-browser chat 677 runs application 89, version 96, with a saved pipeline child through an Agent node.
The child is application 87, version 94, whose result mapping deliberately requires a missing field.
Execution `2adf6ee5ae037cf311c218caffbb0a03` fails with `PIPELINE_RESULT_INVALID`.
The live browser and reload preserve the exact public category and support reference.
PostgreSQL retains only graph step zero with `delegate` pending. The `answer` and `final_text` fields remain empty.
The downstream state modifier does not run.

Fresh chat 678 runs an ordinary Agent parent with the same child.
Execution `738449893c03ee4c183daff15473378a` succeeds.
The parent reports `PIPELINE_RESULT_INVALID`, retryable=false, `revise_task`, and no available partial output.
It completes with `PARENT_HANDLED_PIPELINE_FAILURE`. Live output and reload checks pass.
Captured events contain one child tool start, one tool error, and a terminal pipeline-finish event.
The parent does not repeat the child call.
Both browser sessions use real responses, no mocks, and report no page errors. Screenshots are inspected.
Evidence prefixes: `elitea-nested-direct-failure` and `elitea-parent-direct-failure` in the local evidence directory.
This proves these caller boundaries for invalid direct-tool results, not every possible nested failure or recovery state.


### Generic resource-limit presentation, 2026-09-24

The legacy message does not identify the exhausted resource or give a useful next step.
Rust `protocol/output.rs::runtime_error_policy` retains its canonical worker message for receipt and replay compatibility.
Python workers and Main transport validation retain the same contract.
Main `application/output/runtime_failure.go::IngestFailure` maps `RESOURCE_EXHAUSTED` to product guidance after binding and fence validation.
The message states that a platform processing limit stopped the run. It warns that the task may be incomplete.
It directs users to share the support reference before repeating actions.
The projection uses this message for browser events, persisted failure details, and the failure observer.
The original worker bytes, digest, error code, and retry policy remain unchanged.
Specific context, provider, pipeline, and output-delivery messages remain unchanged.
This fallback does not claim a token, memory, billing, or output cause without evidence.
Application tests cover presentation and unchanged worker receipts. Transport tests retain canonical message validation.
Deployed browser acceptance remains pending for this presentation change. Existing stored messages are not rewritten.
