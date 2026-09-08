# Delegated authorization during active execution

Status: partial. The OpenAPI direct-pipeline-node path has component proof.
Agent tool loops, remote MCP expiry, and deployed browser recovery remain open.

## Business evidence

The SDK source was checked at `da1d9e4db920170ba0f2765a4aede5c015f145a8` on 2026-09-08.

| Current source | Required behavior | Rust source |
| --- | --- | --- |
| `configurations/openapi.py` and `tools/openapi/__init__.py` | Bind delegated authorization to the toolkit configuration and protected resource. | `toolkits/families/openapi/config.rs` |
| `runtime/tools/function.py::_build_mcp_auth_interrupt` | Pause the direct Toolkit/MCP node with toolkit identity and Authorize/Skip actions. | `agents/graph/direct_tool.rs` |
| `runtime/tools/function.py::_build_mcp_auth_skipped_termination` | Stop dependent nodes after Skip. Clear the unavailable node's declared outputs. | `agents/graph/direct_tool.rs::mcp_authorization_stopped` |
| `runtime/tools/function.py::_build_mcp_auth_refresh_termination` | Stop if authorization does not produce an available tool. | The same bounded Rust direct-node continuation |
| `tools/openapi/api_wrapper.py` | Distinguish HTTP authentication failures from successful tool results. | `toolkits/families/openapi/client.rs` |

The inspected SDK OpenAPI operation wrapper returns a structured HTTP error for 401.
It does not establish active-run delegated reauthorization for this operation path.
Rust deliberately improves this path through the existing direct-node authorization contract.
It does not copy provider response bodies into the authorization guard.

## Implemented boundary

An OpenAPI client retains its frozen delegated requirement after it receives an access token.
A later resource 401 returns this requirement through the existing typed authorization signal.
The tool policy wrapper preserves the signal. The direct node creates its normal checkpointed pause.

The requirement retains the toolkit identifier, configuration identifier, resource, and configured consent metadata.
It contains no access token, stored client secret, provider response body, or invocation arguments.
The worker performs no automatic credential exchange or protected-call retry.

Authorize uses the existing continuation validator and a rebuilt, claim-authorized client.
Only the paused node resumes. Completed nodes remain complete.
Following nodes can use that rebuilt client without another authorization guard.
Skip stops the pipeline without dispatching the paused operation or dependent nodes.
A second 401 after authorization stops the pipeline instead of creating an unlimited guard loop.

Anonymous authentication failures do not create delegated guards.
HTTP 403, rate limits, and server errors retain their distinct failure categories.
Client-credentials and static-header clients have no delegated requirement.

## Verification

`toolkits/families/openapi/tools_error_tests.rs` exercises the real OpenAPI client,
operation adapter, and policy wrapper with a controlled HTTP transport.
The expiry regression fails before the error-mapping correction.
It succeeds after the correction and checks identity, secret exclusion, and absence of automatic retries.

`tools_pipeline_tests.rs` runs a three-node native ADK graph through Runner.
It uses in-memory sessions and checkpoints, plus the same real OpenAPI adapters.
Node one succeeds. Node two receives 401. Node three remains unexecuted during the pause.
The test verifies these outcomes:

- Authorize resumes node two with the replacement token, then executes node three.
- The original node-one request does not repeat.
- Skip executes neither node two nor node three again.
- A token key for another configuration cannot resolve the checkpoint.
- Another 401 after authorization terminates without another guard or downstream request.

The existing generic direct-node tests also include OpenAPI beside MCP and SharePoint.
These are component tests, not browser, live-provider, or cross-process restart proofs.

The full locked Rust run passes 893 library tests and 84 integration/contract tests.
No tests are ignored. PostgreSQL tests use isolated databases on the rehearsal service.
Formatting and strict all-target, all-feature Clippy checks pass.

## Required new-UI proof

Component tests do not close the browser gate. Use Playwright against the
replatform UI in the Private project after the data-preserving deployment.
Do not use the legacy UI deployment procedure for this environment.

| Browser scenario | Required evidence | Status |
| --- | --- | --- |
| Direct OpenAPI node rejects a previously accepted delegated token | Toolkit identity and enabled Authorize/Skip controls belong to the paused node. No dependent node executes. | Pending |
| Authorize succeeds | The exact paused operation resumes with the replacement token. Completed nodes do not repeat. Downstream nodes finish. | Pending |
| User selects Skip | The pipeline reports the unavailable node and stops dependent work without a generic runtime failure. | Pending |
| Replacement token is rejected | The run terminates with a clear result, without repeated guards or an automatic retry loop. | Pending |
| Reload, later turn, and regeneration | History survives reload. A prior Skip does not permanently decline authorization. New runs use the correct input and participants. | Pending |
| Multiple tabs | Authorization and logout state remain consistent. Old tabs cannot resume a different or already resolved guard. | Pending |

Correlate each browser action with Main execution identity, Rust checkpoint and
settlement evidence, and emulator request counts. A visible final answer alone
does not prove exact-node resume or absence of duplicate protected calls.

A read-only Playwright readiness check on 2026-09-08 reached the authenticated
Private-project chat page. It did not start an execution or exercise this fix.
The running worker predates this commit, and the migration conflict below remains.
The check also observed HTTP 429 responses from the notification event endpoint
with existing browser tabs open. Track this separately from execution failures;
the check does not establish the cause of a worker or authorization failure.
The temporary inspection tab was closed. Existing tabs and chats were retained.

## Remaining boundaries

- Recover active agent and LLM-node tool loops through their authorization tools.
- Prove remote MCP expiry during discovery and protected operation execution.
- Prove parallel in-flight calls, cancellation, and concurrent token replacement.
- Verify browser refresh and reauthorization without reusing rejected credentials.
- Repeat the scenarios after process replacement with PostgreSQL checkpoints.

The rehearsal database still has conflicting shared migrations 111 and 112.
The read-only ledger check confirms this state on 2026-09-08.
This slice changes no deployment, migration ledger, chat, or emulator state.
Complete the documented data-preserving cutover before merged-service browser verification.
