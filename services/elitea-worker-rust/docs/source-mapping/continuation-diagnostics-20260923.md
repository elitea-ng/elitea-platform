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
| Pipeline | Nested pipeline | Propagate failure; do not release partial state as success | Open |
| Pipeline Agent node | Pipeline application | Propagate failure to the owning graph | Deployed continuation code and downstream suppression pass; exact cause remains open |
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
