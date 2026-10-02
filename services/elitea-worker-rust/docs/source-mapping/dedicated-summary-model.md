# Dedicated summary models

Status: component implementation, 2026-09-16. Point 4 and deployed acceptance remain open.

## Functional source mapping

SDK revision: `18704a4070d098761fd1d35897dc53e412b4cbcc`.
`runtime/clients/client.py::_inject_summarization` selects a low-tier model and independent output settings, with a 4,000-token default.
The separate selection and output controls form the functional reference.
The new implementation uses the existing authorized catalogue and ADK model boundary.

| Current contract | New owner |
| --- | --- |
| Summary model differs from the task model. | Main `application/agentexecution/summary_model.go` authorizes selection and freezes limits. |
| Summary output settings remain independent. | Rust `agents/assembly.rs::SummaryModelProfile` validates the frozen model and explicit output cap. |
| Context middleware receives the selected model. | `transport/model_facade.rs::bind_with_summary` binds the selected provider without changing the task binding. |
| Nested execution applies context middleware locally. | `application_tools.rs` and `pipeline.rs` inherit selection while retaining separate histories and model sessions. |

## Implementation

`AgentExecutionInputV1.summary_model`, field 66, carries `SummaryModelSnapshotV1`.
The snapshot contains model settings and its own `ModelContextLimitsV1`.
Main removes authored snapshots and resolves explicit selections through the project catalogue, including authorized shared models.
Unavailable selections fail; they never silently become the catalogue default.
Output-only settings select the authorized task model.
The default summary output cap is 8,192 tokens, bounded by the selected model maximum.
Explicit caps must be positive and must fit that maximum.

The snapshot excludes credentials, reasoning controls, tools, and task instructions.
Rust validates the complete snapshot before credential redemption.
The selected adapter retains the existing execution claim and resource-project billing context.
It uses the summary model's owner, model identity, output cap, and input limits.
An inherited immutable profile uses `Arc`; each consumer retains independent summary state.
Absent snapshots preserve isolated same-model summarization and old-input compatibility.

The summary model has independent completion capture and call limits.
Summary admission includes the complete provider request and fails before network dispatch when capacity is exhausted.
Failure does not consume a chat model turn or replace the captured task answer.
OpenAI-compatible chat can use a native Anthropic summarizer, and the reverse.
No application schema change, migration, or new table is required.

The larger input snapshot crosses the rejected-authorization cleanup future limit.
The coordinator and its stop-race test now box that cleanup future without changing ownership or cleanup order.

## Verification

- Main agent execution: 221 tests pass without skipped cases; Go vet passes.
- Rust agents: 344 tests pass, including isolated PostgreSQL replacement and fencing checks.
- Provider adapters: 42 regression tests pass. Two additional selection tests cover cross-provider routing and independent capacity refusal.
- Input protocol: 15 tests pass, including field 66 round trips and missing-limit rejection.
- Child and mixed-pipeline fixtures use the selected summary model. Deterministic graphs make no summary call.
- Rust formatting and strict library/test Clippy pass.
- Buf reports no breaking changes against the preceding commit. The authorization stop-race cleanup test passes.

Evidence uses the `elitea-point4-summary-model-*` and `elitea-point4-summary-selection-*` temporary log prefixes.
These component fixtures do not establish live-provider summary quality or browser acceptance.

## Remaining integration

Main now resolves selections for Start and Regenerate. Continue restores the admitted snapshot from immutable execution input.
The [settings delivery mapping](context-policy-delivery.md) records component evidence. Deployed selection remains open.
Complete UI controls, context status, representative summary quality, and recovery acceptance.
Deploy compatible workers before Main emits field 66; older strict readers reject unknown fields.
Large histories that require multiple summary requests remain separate point 4 work.
