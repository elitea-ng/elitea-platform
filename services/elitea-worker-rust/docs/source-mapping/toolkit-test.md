# Toolkit Test source mapping

Status: `partial`. Gate `TKTEST-RUST-01` remains open.

Rust must support the existing toolkit Test functionality. A read-only subset
is a delivery stage, not the final compatibility target.
Indexing and artifact-backed operations keep their separate activation gates.
Those gates do not defer the entire toolkit Test entry point.

## Evidence and ownership

The source review date is 2026-09-08. The incoming platform revision is
`0286138a`. The legacy sources below define behavior, not implementation structure.

| Current source | Required behavior | Replatform source and status |
| --- | --- | --- |
| EliteaUI `features/toolkits/ui/test-tools/TestTools.jsx` and `api/toolkits.js` | Select one operation, edit arguments, run it, and show its result. | Web `features/toolkits/ui/test-tools/TestTools.tsx` and `api/toolkitTestRun.ts`; Main supplies the new request path. |
| Core `api/v2/test_toolkit_tool.py` and `rpc/application.py::test_toolkit_tool_sio` | Validate project access and correlate the request, events, and result. | Main `internal/api/v2/toolkits/handler.go`, `internal/api/v2/toolkitrun`, and `internal/application/toolkitcalltool`. |
| Indexer `methods/indexer_test_toolkit.py::_indexer_test_toolkit_tool_task` | Forward runtime context, model settings, authorization data, callbacks, and generation identity. | Python worker `handlers/toolkit_call_tool.py` and Rust `execution/toolkit_delivery_processor.rs` adopt the shared command. |
| SDK `runtime/clients/client.py::test_toolkit_tool` | Invoke one named tool with explicit arguments. Preserve tool failures as test outcomes. | Python worker `agents/sdk_adapter.py`; Rust must reuse native toolkit execution without an extra planning turn. |
| Core `methods/runtool.py` and `utils/mcp_service.py` | Execute an externally published toolkit operation directly. | Main MCP service supports the new Test contract and the existing Rust read contract. These contracts are distinct. |

Main adds `ToolkitCallToolCommandV1` and `ToolkitCallToolResultV1` in
`libs/proto/elitea/runtime/v1/toolkit.proto`. The capability is
`toolkit.call_tool.v1`. These messages complement the legacy Test flow.

Rust currently implements `toolkit.execute.read.v1` through
`src/protocol/command.rs`, `src/toolkits/direct_execution.rs`, and Main's
`internal/application/toolkitexecution`. The shared Test command now uses the same claim, lease, native toolkit, spool, and settlement machinery.
Protocol decoding alone does not close this gate.

All workers use the same language-neutral protobuf schema. There is no
language-specific encoding or alternate toolkit Test contract.
Rust must adopt `toolkit.call_tool.v1`; its existing read capability is not a substitute.
The two capability names describe different admitted operations, not different worker languages.

Main keeps Python toolkit Test dispatch operational. An explicit Rust
external-MCP read refusal must not fall through to Python and bypass Rust policy.

## Closure requirements

1. Admit the exact toolkit, revision, operation, and immutable input references.
2. Reuse native configured toolsets and credential redemption.
3. Preserve explicit selection, project access, and sensitive-operation policy.
4. Preserve delegated authorization and safe structured Skip outcomes where the entry point supports continuation.
5. Preserve tool errors separately from runtime failures.
6. Preserve empty, null, scalar, list, and object results without invented placeholders.
7. Enforce cancellation, deadlines, claim fencing, replay safety, and bounded output.
8. Preserve supplied model settings for tools that use models internally.
9. Prove each supported HTTP, chat, agent, pipeline, and external-MCP invocation path separately.
10. Keep effectful operations closed until durable effect receipts prevent unsafe replay.

The Test UI must not add a planning turn merely to select an already named tool.
A tool can still call a model internally.

## Verification plan

Add cross-language fixtures for the command, result, and immutable input bindings.
Add component tests for exact dispatch, credentials, policy refusal, and tool errors.
Add property tests for malformed envelopes, result shapes, and replay identity.
Add system tests for lease replacement, cancellation, and output recovery.
Run browser proofs through the toolkit Test panel and the affected chat paths.

Use the OAuth emulator for delegated and client-credentials tests.
Use the MCP emulator for DCR tests. OpenAPI credentials do not gain a DCR type.

Agent-only parsers still reject `ToolkitCallTool`.
The production execution parser routes the shared command through the toolkit delivery processor.

## Implementation history: 2026-09-09

The source baseline is SDK `ecf49dfac73cd096da4c2297f3d91d13e526395a` and Core `b701a00aeff0af1a416916c4a537bfdd4b7d8337`.
SDK `runtime/clients/client.py::test_toolkit_tool` defines explicit tool selection, arguments, model configuration, and tool outcomes.
Core `rpc/application.py::test_toolkit_tool_sio` defines the current Test request boundary.

Rust `protocol/command.rs` authenticates the shared command without translating its language-neutral wire contract.
`protocol/control.rs` validates immutable settings, arguments, and required runtime context entries after the claim.
`execution/toolkit_delivery_processor.rs` fetches each entry under the monitored claim and invokes exactly one native tool.
`toolkits/direct_request.rs` verifies the exact saved toolkit identifier and configuration type.
It rejects absent policy context and retains validated model settings for native model-independent operations.
`toolkits/direct_runtime.rs` passes claim-materialized token context to the existing configured and MCP materializers.

`toolkits/direct_execution.rs` preserves selection, policy, authorization, and read-only checks immediately before invocation.
Effectful operations remain closed until durable effect receipts support safe replay.
Provider tool failures become safe `TOOL_ERROR` summaries.
Lease, deadline, policy, and runtime failures remain typed runtime failures.
Unknown operations and unsupported families keep separate shared result statuses.

The shared result preserves null, empty strings, booleans, numbers, lists, and objects.
Oversized inline values set `truncated` and omit the entire JSON value.
The worker never emits partial JSON.
Spool replay checks the exact settings and arguments digests against the current claim.
Settlement and Redis retirement reuse the existing fenced delivery path.

New component tests cover exact result shapes, safe tool failures, selection, mandatory policy, and effect refusal.
Model-dependent toolkit families remain unavailable until their native adapters consume the retained model settings.
Authenticated transport, worker replacement, and browser Test proofs remain verification requirements.
This delivery does not claim full toolkit Test parity or close `TKTEST-RUST-01`.

## Verification: 2026-09-09

`cargo clippy --all-targets --offline -- -D warnings` passes.
`cargo test --lib toolkit --offline` passes with 393 tests and no ignored tests.
The test run requires permission to bind local HTTP fixture listeners.
The initial sandbox run passes 388 tests and refuses three listener tests.
The authorized run includes two additional artifact transport tests added during integration.
These tests do not prove a deployed Main-to-worker request or browser flow.

## Shared Test authorization: 2026-09-09

The Test REST client currently sends tool names and arguments only.
The Test panel displays model controls, but `useToolkitChatDispatch.hooks.ts` does not forward these settings to that REST client.
Core accepts `llm_model` and `llm_settings`; the SDK constructs a model before toolkit initialization.
Rust retains validated model selection and settings without adding a model call for model-independent native operations.
The retained context remains part of the immutable input binding.

The shared schema adds `AUTHORIZATION_REQUIRED` and `ToolkitAuthorizationRequiredV1`.
The challenge contains exact toolkit identity and bounded public OAuth metadata.
It contains no bearer token, client secret, tool arguments, or provider body.
The worker derives the challenge only from an existing delegated authorization guard or a validated authorization error.
It preserves the current claim and input bindings during terminal publication.
A Test authorization outcome completes the refused attempt; it does not report a successful protected operation.
Main owns fresh authorized retry admission and saved-token redemption.
Skip must not dispatch a worker command.

The native OpenAPI test proves that another configuration's token does not satisfy the frozen authorization requirement.
The same test rebuilds the native toolset only with its exact claim-materialized token key.

Authorization follow-up verification passes with 401 toolkit tests and no ignored tests.
All-target Clippy passes after the model and authorization changes.
OpenAPI server-variable substitution now stops after 128 expansions.
A self-referential default cannot block synchronous claim materialization indefinitely.
