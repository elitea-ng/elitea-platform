# Input admission diagnostics

Status: implementation in verification. Deployment and browser acceptance remain open.

## Source mapping

The [runtime diagnostic mapping](continuation-diagnostics-20260923.md) records current SDK and UI behavior for safe failures and operator references.
That behavior requires an understandable failure reason without exposing private provider or request data.
The replatform adds a durable input-admission boundary before model authorization.

| Boundary | Owning source |
| --- | --- |
| Input materialization and protocol validation | Rust `execution/agent_preparation.rs::PreInvocationTerminalCause` |
| Canonical public failure and durable replay | Rust `protocol/output.rs::runtime_error_policy` and `canonical_runtime_failure` |
| Versioned public error identity | `libs/proto/elitea/runtime/v1/errors.proto` |
| Worker receipt validation | Main `transport/runtimegrpc/output/server.go::runtimeFailurePolicyFor` |
| Browser projection and preserved signed receipt | Main `application/output/runtime_failure.go::IngestFailure` |

## Problem and change

Previously, input admission and resource exhaustion during execution share `RESOURCE_EXHAUSTED`.
Main replaces that code's message with generic processing-limit guidance.
That message does not identify whether a model request starts.

Add `EXECUTION_INPUT_LIMIT`, protobuf enum value 31.
Map only input-content and input-protocol resource failures before invocation to this code.
The public message identifies input admission, distinguishes model token limits, and suggests reducing input or using the support reference.
The message does not expose input values, provider bodies, or internal paths.
Exact field and validator reasons remain in operator diagnostics.
Do not claim this message identifies each individual field in the UI.
Input size failures emit ERROR with their existing safe reason and execution span.
Other failures keep their existing classification and logging behavior.

The existing generic code and message remain valid for historical receipt replay.
Canonical validation rejects arbitrary replacement messages.
Main preserves the signed worker payload while projecting the specific new message.
No database schema, runtime size limit, model budget, or dependency changes.
The repository generator also refreshes existing instruction and context-accounting comments in generated Go bindings.

## Rollout and verification

Deploy Main before Rust. Older consumers do not admit the new code.
The current web client displays the supplied safe message through its existing failure block.
Browser verification is still required for live failure, support reference, and reload.
Check that no provider invocation occurs for the rejected request.
This change does not close other gate 4 accounting or field-level diagnostic work.

Focused verification passes 19 Rust preparation tests and three canonical failure-policy tests.
Main output transport and projection suites pass, including unchanged signed receipt bytes.
Main vet passes for both affected packages.
