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

Deployment, new-category browser acceptance, and a copyable UI correlation control remain open.
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
