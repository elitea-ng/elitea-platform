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
| Indexer `methods/indexer_test_toolkit.py::_indexer_test_toolkit_tool_task` | Forward runtime context, model settings, authorization data, callbacks, and generation identity. | Python worker `handlers/toolkit_call_tool.py` and its delivery adapter. Rust support remains planned. |
| SDK `runtime/clients/client.py::test_toolkit_tool` | Invoke one named tool with explicit arguments. Preserve tool failures as test outcomes. | Python worker `agents/sdk_adapter.py`; Rust must reuse native toolkit execution without an extra planning turn. |
| Core `methods/runtool.py` and `utils/mcp_service.py` | Execute an externally published toolkit operation directly. | Main MCP service supports the new Test contract and the existing Rust read contract. These contracts are distinct. |

Main adds `ToolkitCallToolCommandV1` and `ToolkitCallToolResultV1` in
`libs/proto/elitea/runtime/v1/toolkit.proto`. The capability is
`toolkit.call_tool.v1`. These messages complement the legacy Test flow.

Rust currently implements `toolkit.execute.read.v1` through
`src/protocol/command.rs`, `src/toolkits/direct_execution.rs`, and Main's
`internal/application/toolkitexecution`. It does not implement the new Test
command or result. Protocol decoding alone does not close this gate.

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

Existing Rust agent-command tests reject `ToolkitCallTool` explicitly.
This proves a safe unsupported boundary, not toolkit Test parity.
