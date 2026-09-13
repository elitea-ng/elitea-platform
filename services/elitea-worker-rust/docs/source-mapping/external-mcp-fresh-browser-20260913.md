# Fresh-browser external MCP acceptance

The check starts a fresh local Playwright browser context with installed Chrome.
It signs in through the rehearsal identity selector. Personal Tokens creates a ten-minute PAT through the UI.
The independent Python client uses only `Authorization: Bearer <PAT>` for MCP calls. It does not read browser cookies.

| Export | Result marker | Result |
| --- | --- | --- |
| Toolkit 31, echo operation | `RUST_PAT_ATOMIC_TOOLKIT_20260913` | Pass |
| Agent 20, attached echo and linked skill | `RUST_PAT_ATOMIC_AGENT_20260913` | Pass |
| Pipeline 18, autonomous terminal output | `RUST_PAT_ATOMIC_PIPELINE_20260913` | Pass |

Each call returns one text block. The client checks initialization, unique exported names, argument schemas, error status, and the expected marker.
The client source is `services/elitea-worker-rust/tests/acceptance/external_mcp_client.py`.
The token is revoked with HTTP 204. Toolkit 31 sharing returns to its original disabled state, verified after reload.
The browser closes after cleanup. No credentials or browser session state are exported to tracked files.

Main uses the atomic-admission deployment documented in [prepared command recovery](toolkit-prepared-command-recovery.md).
Rust remains on the compatible instruction-enabled recovery image documented in [takeover authority](toolkit-takeover-authority.md).

## Remaining external reconnect contract

`services/elitea-main/internal/api/v2/mcp/handler.go` declares a stateless HTTP transport without an MCP session identifier.
`services/elitea-main/internal/api/v2/mcp/execute.go` assigns a new UUID as the direct toolkit admission key on each call.
Therefore, these successful calls do not prove recovery of the original execution after a lost HTTP response.
Repeated calls cannot be treated as continuation without a durable client request identity or a supported event replay contract.
A JSON-RPC request ID alone is insufficient across unrelated stateless clients.
The remaining implementation must preserve actor and project isolation, argument conflicts, and exact execution identity during reconnect.
Do not close gate 3d on the normal-path evidence above.


## Toolkit admission and observation separation

The current platform reference is `projects/centry/pylon_main/plugins/elitea_core/routes/mcp_sse.py::_handle_mcp_request` in the umbrella workspace.
It checks project access and refuses GET with HTTP 405.
Its optional SSE POST mode does not provide the required restart recovery contract.

The new implementation separates toolkit admission from result observation in `services/elitea-main/internal/application/toolkitexecution/execute.go`.
`Admit` returns after the existing frozen input and durable command admission succeed.
`AdmittedCurrentReadTool` retains private execution and tool identity fields without arguments or credentials.
`Wait` observes that invocation and verifies the original tool identity before returning its result.
The existing `Execute` method composes both steps and preserves its caller contract.
Cancellation stops observation without creating or cancelling durable work.

Tests cover admission without waiting, cancelled observation, timeout, repeated observation, changed current toolkit data, and mismatched results.
The toolkit application tests and MCP handler tests pass.
The toolkit race tests and focused vet check also pass.
These are component checks. They do not establish external reconnect acceptance.

This change introduces no migration, proto change, or worker command change.
The transport still needs a durable actor/project/scope/request binding and standard SSE cursor replay.
The admitted handle is process-local and is not a durable recovery receipt.
A resumed GET must load trusted durable invocation metadata; it must never construct authority from client-supplied identifiers.
The transport must publish the resume cursor after admission and before waiting for the result.
A subsequent POST remains a new call; JSON-RPC IDs are not global idempotency keys.

Protocol reference: [MCP Streamable HTTP resumability](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports).
Gate 3d remains open until an independent client resumes the same invocation across disconnection and service restart.
