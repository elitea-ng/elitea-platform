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


## Agent response cursors and GET replay

`services/elitea-main/internal/api/v2/mcp/resume_cursor.go` encrypts bounded admission metadata with a deployment-key-derived AEAD key.
The key derivation uses a distinct MCP response domain.
Cursors expire after at most 24 hours and survive replacement with the same deployment key.
They contain no prompts, credentials, arguments, or result content.
No new database table or migration is required.
Key changes invalidate existing cursors; there is no process-local key fallback.

`resume_stream.go` emits a priming SSE cursor after agent admission, before result observation.
A final SSE event preserves the original JSON-RPC request ID.
A GET with `Last-Event-ID` authenticates through the existing router and checks current run permission.
It also verifies the actor, project, exact endpoint scope, application version, and current MCP export.
It reads the original response message without starting another invocation.
A completed cursor returns HTTP 204 after the same access checks.
An observer disconnect stops waiting without cancelling the durable execution.

`execute.go` adds an admission observer to the existing agent start path.
`server.go` selects SSE for compatible agent calls and handles resumable GET requests.
`internal/api/mcp_resume.go` and `router.go` compose the codec from the existing deployment master key.
Deployments without that key retain their existing JSON response behavior.
Toolkit and internal-builder calls still use their existing response path.

The current platform route remains the functional reference identified above.
Its GET refusal is not copied because the new platform requires crash recovery.
Rust continues to own execution checkpoints; this change only reconnects the external caller to Main's existing durable result projection.

Verification passes for cursor tampering, expiry, replacement keys, request identity, SSE framing, and caller/scope/export isolation.
Focused MCP tests, race tests, and vet pass.
`TestMCPAgentResponseResumesAfterHandlerReplacementPostgres` runs against an isolated real PostgreSQL database and passes.
It discards the original response, replaces the handler and codec, and returns the exact original answer through GET.
The replacement has no admission service. The test records one original admission.
This test uses an admission double and does not prove a worker or Main process restart through an external client.

Deployment and independent-client acceptance remain pending.
Toolkit resume still requires the matching durable observation path.
Responses that pause for human input require separate policy and replay verification; no external resume-of-interrupt feature is introduced.
Gate 3d remains open.
