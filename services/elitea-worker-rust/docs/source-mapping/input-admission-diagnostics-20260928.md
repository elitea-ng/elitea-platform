# Input admission diagnostics

Status: focused checks and deployed browser acceptance pass. The message-capacity correction below awaits deployed verification.

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


## Deployed browser acceptance

Main deploys before Rust from revision `92f089835`.
Main image: `sha256:f025f46d30c88779d9462a90616b0523d59d890e1f50fd2563af7f4f7cc38bc4`.
Worker image: `sha256:9a974ad42aa7ec427e6fe06fff68d8779b0716f45e6c0c51751bc8479767f441`.
Deployment retains six Main mounts, five worker mounts, credentials, databases, networks, and resource limits.

Fresh headed Playwright submits a synthetic 70 KiB message in chat 723.
Main admits the message, but Rust's generic 64 KiB JSON-string bound rejects it before model invocation.
Execution `445e051391a027326557130024ebf3fd` emits `EXECUTION_INPUT_LIMIT` with the registered message.
The browser shows the specific guidance, error code, message reference, and operator-only diagnostics guidance.
Reload preserves the failure. Browser page errors remain empty and browser responses are not mocked.
The screenshot is inspected. Worker logs use ERROR and include the safe validation reason and execution identity.
No model-request event occurs during the isolated check.
Evidence uses `elitea-input-admission-ui-*` in the local temporary directory.
Strict Rust library and test Clippy checks also pass.

### Separate admitted-message mismatch

Main `application/agentexecution/start.go` allows user messages up to 256 KiB.
Rust `agents/protocol.rs` parses user input with a generic 64 KiB decoded-string bound.
Rust `agents/assembly.rs` already allows text up to 512 KiB.
The test proves the new diagnostic path, not an acceptable long-message contract.
Align Rust parsing with the admitted data-plane content before closing this remaining limit issue.
Preserve metadata bounds and model-context admission; do not increase arbitrary control strings.

## Admitted user-message capacity correction

Main continues to admit user messages up to 256 KiB.
Rust now parses root user text against its existing 512 KiB assembly capacity.
Both boundaries use decoded UTF-8 byte lengths.
Escaped JSON can exceed 256 KiB without exceeding the admitted decoded message size.
The complete encoded execution input remains bounded at 8 MiB.
Object metadata and nested array values retain their 64 KiB string bound.
Model context admission and compaction remain unchanged. No database or provider configuration changes are required.

`agents/request.rs` owns the shared user-text capacity.
`agents/protocol.rs::parse_user_input` applies that capacity only to root text.
`agents/assembly.rs::validate_common_profile` uses the same constant.
The existing source mapping above supplies the current-platform business behavior reference.
This correction concerns the replatform transport boundary, which must preserve a message already admitted by Main.

Contract tests cover the inclusive worker boundary, Main's boundary, escaped text, multibyte text, oversize rejection, and unchanged metadata rejection.
Deployed acceptance must regenerate chat 723 with its original 70 KiB user message.
