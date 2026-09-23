# Model failure reasons across the runtime contract

## Problem and behavior

The worker distinguishes model gateway failures internally but maps most of them to `INTERNAL`.
Main then requires the generic message `The runtime operation failed.`
Users cannot determine whether to change input, wait, or ask an administrator for help.

This change preserves ten known reasons with registered messages and retry flags.
Operator failure logs use the same explanation alongside the upstream code and execution identity.
Unknown failures keep the existing internal-error fallback.
Raw provider bodies and exception strings never become public messages.

## Current-platform behavior reference

SDK revision: `966526e8334354366dd161b606d73fe8e204b850`.

`elitea_sdk/runtime/tool_outcome.py::ToolOutcome` carries a message, error class, and retry information.
`classify_provider_error_category` leaves unknown categories unclassified instead of guessing.
This supplies behavioral evidence for explicit failure classification, not a model-protocol implementation to copy.
`runtime/middleware/strategies.py::ExceptionContext` retains operator exception information.
Model-assisted explanation is not implemented by this change.

## Source mapping

| Owner | Source | Responsibility |
| --- | --- | --- |
| Contract | `libs/proto/elitea/runtime/v1/errors.proto` | Append codes 14–23; preserve all existing numbers. |
| Worker | `src/transport/openai_compatible_facade.rs` and `anthropic_facade.rs` | Existing typed gateway failure codes. |
| Worker | `src/execution/native_agent_lifecycle.rs::model_failure` | Map known failures without parsing provider text. |
| Worker | `src/protocol/output.rs::runtime_error_policy` | Canonical explanation, retry flag, and replay validation. |
| Worker | `src/protocol/control.rs::runtime_rejection` | Reject output-only model codes in control responses. |
| Main | `services/elitea-main/internal/transport/runtimegrpc/output/server.go::runtimeFailurePolicyFor` | Accept only registered code/message/retry combinations. |
| Main | `services/elitea-main/internal/application/output/runtime_failure.go` | Existing durable projection of validated failure messages. |
| UI | `apps/elitea-web/src/features/chat-messages/lib/chatStreamSettle.ts` | Existing display of the server-provided safe message. |

Worker source paths are relative to `services/elitea-worker-rust`.
Other paths are relative to the repository root.
Generated Go and Python bindings use the pinned repository generator.
Rust bindings use the existing build-time generator.
No database migration or new table is required.

## Registered model reasons

| Code | Retryable | User explanation |
| --- | --- | --- |
| `MODEL_TIMEOUT` | true | The model did not respond in time. Try again; if this continues, ask an administrator to check the model connection. |
| `MODEL_RATE_LIMITED` | true | The model service reached its request limit. Wait briefly and try again, or select another model. |
| `MODEL_ACCESS_DENIED` | false | The model service denied access. Ask an administrator to check the model credentials and project permissions. |
| `MODEL_BUDGET_EXHAUSTED` | false | The model budget is exhausted. Ask an administrator to check the project budget or provider billing before retrying. |
| `MODEL_REQUEST_REJECTED` | false | The model service rejected this request. Check the selected model and its settings, or ask an administrator to inspect the execution logs. |
| `MODEL_RESPONSE_INVALID` | true | The model returned an incomplete or invalid response. Try again or select another model. Report repeated failures to an administrator. |
| `CONTEXT_BUDGET_EXCEEDED` | false | The model input exceeds the available context budget. Enable compaction, reduce attached content, or select a model with a larger context window. |
| `MODEL_REQUEST_TOO_LARGE` | false | The model request exceeds the transport size limit. Reduce attached content or tool results. A larger token window alone may not resolve this. |
| `MODEL_UNAVAILABLE` | true | The model service could not be reached or is temporarily unavailable. Try again; ask an administrator to check the connection if it persists. |
| `MODEL_PROVIDER_FAILURE` | true | The model service reported an error while generating the response. Try again or select another model. An administrator can inspect the execution logs. |

Retryability describes the failure; it does not authorize replay of completed tool effects.
A response stream error can leave visible partial output.
This change does not mark that output complete or repeat tool calls.

## Deployment and verification boundary

Deploy Main before the worker emits these new codes.
Older Main binaries reject unknown runtime codes.
Drain affected executions before rolling Main back.
Existing persisted code/message pairs remain unchanged and valid.

The shared fixture is `testdata/proto/runtime/v1/model_failure_policies.json`.
Rust verifies canonical replay and rejects injected messages.
Main tests cross the output handler, preserve reason and retryability, and reject unregistered text.
Taxonomy tests cover known gateway categories and retain the unknown-error fallback.

Main and worker deployment is complete (rehearsal images below). New-category browser acceptance and a copyable UI correlation control remain open.
Existing generic configuration and non-model failures still require further classification.
This slice does not complete OBS-RUST-01 or Gate 4.

## Local verification

- Rust taxonomy: five tests pass.
- Rust output policy and replay: two tests pass.
- Strict all-target Rust Clippy passes.
- Main runtime output transport and application output packages pass.
- Protobuf lint, build, and pinned binding generation pass.
- The shared fixture verifies ten new message policies in both languages.

These checks use component boundaries, not deployed browser traffic.

## Rehearsal deployment and rejected fixtures

On 2026-09-23 Main was deployed first, followed by the worker, retaining existing
environment, mounts, and runtime limits. Images:

- Main `elitea-main:model-reasons-20260923`, image `a5bbe878da7d`.
- Worker `elitea-worker-rust:model-reasons-20260923`, image `9d9d095d1be1`.

Fresh headed browser fixtures reached earlier guards instead of the intended
model-failure boundary. They are explicitly **not acceptance** of the new codes:

| Chat | Execution | Observed boundary |
| --- | --- | --- |
| 655 | `04ee12df18663fa2fb5d7b94ec2bc21f` | Invalid temperature rejected as `INVALID_INPUT`. |
| 656 | None | Large submitted message rejected with HTTP 413 before execution. |
| 657 | `5459e9abc6216394fff7965432dca58f` | Large saved pipeline rejected with `RESOURCE_EXHAUSTED` before invocation. |
| 658 | `93d3a0feb34cfdbde94564215c713782` | Synthetic toolkit pipeline rejected as `INVALID_INPUT`; a repeat reaches native assembly, not preparation validation. |

The preparation investigation found that `PreInvocationTerminalCause` retains a
safe reason, but `pre_invocation_terminal` only recorded its broad code on the
span. The worker now emits `agent_preparation_terminal` with `error_code` and
`failure_reason` in the existing execution span. It uses the data-free `Display`
implementation, never `Debug`, request content, or the transport source chain.
This is operator logging; the persisted public failure contract is unchanged.
Commit `4182b419` was deployed as worker image
`elitea-worker-rust:preparation-reasons-20260923` (`fc36e2bdab59`). All five mounts,
environment, networks, and resource limits were preserved; no active claim was
interrupted. Eighteen preparation tests and strict all-target Clippy pass.

Fresh headed browser execution `1eacec3702927e4e580798e3fdc3d352` in chat 657
reproduces the resource rejection. The new warning includes the execution ID,
`agent_input.resource_exhausted`, and the exact safe reason:
`agent input validation failed: the agent JSON input exceeds the approved limit`.
The public error survives reload unchanged, with zero browser runtime errors.
The screenshot was visually inspected. This verifies the preparation diagnostic,
not the new model taxonomy or an improved public resource-limit explanation.

Evidence: `/private/tmp/elitea-preparation-resource-live-result.json`,
`/private/tmp/elitea-preparation-resource-live-complete.png`, and
`/private/tmp/elitea-preparation-resource-live.log` (local rehearsal artifacts).
The earlier chat 658 repeat (`5b1fe85e31a09e08e190d21432fbd096`) passes preparation
and fails native assembly with `native_agent.invalid_input`; its generic public
error also survives reload. Assembly cause visibility remains a separate open
diagnostic boundary and must not be attributed to this preparation fix.

The assembly follow-up adds `agent_native_assembly_failed` and `failure_reason`
to `src/execution/native_agent_lifecycle.rs` at the failed assembly boundary.
`src/agents/runtime.rs::NativeAgentAssemblyError` stores only a static message;
its `Display` is safe for this log. No model/provider payload or source chain
is formatted. This supplements the existing error code and execution span,
without changing failure persistence, retryability, or authorization order.
The 34 assembly-related component tests and strict all-target Clippy pass.
Commit `8cd212a0` was deployed as `elitea-worker-rust:assembly-reasons-20260923`
(`3918796d6906`), retaining the previous environment and five mounts after the
active-claim check passed.

Fresh headed browser execution `ae540572b826d4b2ab86a9d4e0b2f06e` in chat 658
verifies the warning and stable error reload, with zero browser runtime errors.
The reason is `a pipeline LLM node references a tool outside its frozen scope`.
The synthetic fixture had used `entity_type: pipeline` for its tool relation;
the verified application/pipeline relation contract uses `entity_type: agent`.
The fixture relation was corrected without changing production authorization or
relaxing frozen-scope validation. This is diagnostic acceptance, not a model
failure acceptance. Evidence:
`/private/tmp/elitea-assembly-reasons-live-result.json` and
`/private/tmp/elitea-assembly-reasons-live.log`.

## Pipeline model failure propagation

The corrected fixture reaches the model in execution
`ca6c2c7eea7388d05ab8b5ff0444884e`, but ends as `INTERNAL`. Source inspection
identified an independent propagation gap: `graph/llm.rs::run_pipeline_llm`
replaced start/stream ADK errors with `LlmExecutionError::Unavailable` before
the graph wrapper and lifecycle classifier could see their codes. Only
`model.output_continuation_failed` had an explicit side-channel bridge.

The existing invocation-owned channel in `graph/node_events.rs` now forwards
the static ADK error code for both start and stream failures. It reconstructs
a data-free error, with no provider message or source, for the outer lifecycle
to classify. Known codes use the same canonical policy as ordinary agents;
unknown codes still fail as Internal. The graph stops on the error; no retry,
extra model call, or fabricated successful node output is introduced.

The bridge regression covers context exhaustion, rate limiting, provider
failure, continuation exhaustion, and an unknown static code. All 77 graph
component tests pass. Deployed verification of the context-error propagation
change passes as recorded below. This is worker-owned error transport rather than a literal current-SDK
port; the current platform's user-facing failure behavior remains the reference
described at the start of this mapping.

Source mapping: `src/execution/agent_preparation.rs::pre_invocation_terminal`
consumes the existing `PreInvocationTerminalCause` produced by typed input
validation/materialization; its safe contracts are
`src/protocol/error.rs::ProtocolError` and
`src/transport/input_content.rs::InputContentError`. No legacy behavior is
being ported here: this fills a diagnostic gap in the new worker's admission
boundary. Main's own validation and HTTP error UX/logging remain a separate
platform follow-up; this worker work does not establish coverage of them.


## Deployed pipeline context failure verification

The feature baseline `dc7190cf` was deployed as
`elitea-worker-rust:pipeline-model-errors-20260923`, image
`77e685e1212097381e9d5d0651caa86a0070efef41e201ebbcec2323e803d8a0`.
Only the worker was replaced, after checking for active claims and retaining
its environment and all five mounts. Main remained on `model-reasons-20260923`.

Fresh headed Playwright execution `1aa474d030babf37f53f890957473052`, chat 658,
reached the context guard through real model calls and synthetic tool records.
The pipeline used Haiku with a 128,000-token Full window, 4,000 output reserve,
1,280 safety margin, and 122,720 usable input tokens. Compaction was intentionally
disabled for this failure test. The saved toolkit relation used `entity_type=agent`.

The browser received `CONTEXT_BUDGET_EXCEEDED` with the canonical actionable
message. The partial response remained visible and both response and error
survived reload exactly. No browser runtime errors were observed; the screenshot
was visually inspected. The earlier execution `ca6c2c7eea7388d05ab8b5ff0444884e`
at this boundary returned `INTERNAL` before the bridge fix.

Evidence: `/private/tmp/elitea-pipeline-model-errors-result.json`,
`/private/tmp/elitea-pipeline-model-errors-complete.png`, and
`/private/tmp/elitea-pipeline-model-errors-live.log`.
This proves the deployed pipeline context-error category, not every provider
error or execution against the later merged backend. The merged frontend also
rendered this persisted result and retained it after reload against the unchanged
rehearsal backend; see `main-integration-20260923.md`.

Remaining diagnostic work includes continuation-specific partial-output controls,
live provider-category checks beyond context errors, and origin-level async
ancestry. Current opt-in captures contain active runner span names and a native
stack at the capture boundary, not complete suspended-future stacks.
