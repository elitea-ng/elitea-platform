# Toolkit Test recovery before execution ID receipt

Status: implemented and verified through deployed headed Chrome on 2026-09-14.

## Functional reference and ownership

Current Core `pylon_main/plugins/elitea_core/api/v2/test_toolkit_tool.py` executes one selected tool and returns its result or task ID.
The route supports synchronous waiting and asynchronous task references.
Its timeout path attempts to stop the task.
It does not provide a browser request reference for recovery before receiving that task ID.
The new platform preserves the selected-tool behavior and adds durable observation without automatic resubmission.

Rust `execution/toolkit_delivery_processor.rs` retains toolkit execution and terminal output ownership.
Rust `toolkits/` retains provider invocation and policy enforcement.
Main retains admission identity and persisted result projection.
This change adds no Rust execution engine, checkpoint ownership, worker command field, table, or migration.

## Request and result contract

The Toolkit Test browser creates an opaque request key before sending its POST.
It stores only project ID, toolkit ID, request key, and lookup mode in the existing session-storage namespace.
Arguments, results, credentials, and authorization references remain absent from this record.
Logout clears the namespace. Disabled session storage limits recovery to the surviving page.

`Idempotency-Key` opts into a stable identity scoped to project, actor, and toolkit.
Main hashes these identifiers into the existing admission key.
The admission ledger independently checks the immutable input digest.
Reusing the same key with changed inputs returns HTTP 409 instead of admitting another execution.
Callers that omit this header retain the previous `request_id` behavior.
A deliberate new call, including an authorized retry, receives a new key.

`GET /elitea_core/test_tool/prompt_lib/{project_id}/{tool_id}/{request_key}?lookup=request` resolves an existing admission.
The default lookup continues to accept an execution ID.
Both forms use the existing result reader and its frozen actor, project, tenant, capability, and toolkit checks.
The GET never resolves current settings, admits work, or dispatches a tool call.
An accepted pending request returns the actual execution ID for subsequent observation.

A lost connection or gateway failure starts result lookup, never another POST.
A missing request returns an inconclusive UI state and preserves the reference for reload.
Absence during lookup does not prove that a concurrent admission cannot complete.
The UI does not repeatedly poll an unaccepted request or automatically rerun its tool.
Reset or a deliberate new run replaces the latest tab-local receipt.

## Source mapping

| Owner | Source | Responsibility |
| --- | --- | --- |
| Main admission | `internal/application/toolkitcalltool/request_recovery.go`, `service.go` | Stable scoped request key; legacy compatibility |
| Main result | `internal/application/toolkitcalltool/result.go` | Resolve the request and read its original outcome |
| Main repository | `internal/infra/db/repos/toolkit_call_tool_result_reader.go` | Reuse `GetToolkitCallToolAdmissionByIdempotency` and existing ownership validation |
| Main HTTP | `internal/api/v2/toolkitrun/response.go`, `toolkits/test_tool_result.go` | Header, conflict response, and request lookup |
| Browser | `features/toolkits/api/toolkitTestRecovery.ts`, `toolkitTestRun.ts` | Bounded receipt and read-only recovery |
| Browser flow | `features/toolkits/ui/test-tools/useToolkitTestToolRun.ts`, `TestToolResultPanel.tsx` | Save before submission; show pending or inconclusive results |

Main paths are relative to `services/elitea-main/`.
Browser paths are relative to `apps/elitea-web/src/`.
OpenAPI and its Go and TypeScript bindings include the additive header and lookup parameter.
The Orval operation override supplies a complete result fixture because pending and completed responses have different required fields.
Generated files use the repository generators.

## Verification

Focused Go tests cover stable identity, changed-input conflict identity, invalid keys, lookup ownership, and no admission during result reads.
Real PostgreSQL tests cover original admission lookup, actor and project refusal, toolkit binding, and conflicting input under an existing key.
Focused MCP and shared toolkit-execution tests pass with the default execution-ID lookup unchanged.
UI tests cover storage before POST, lost response across remount, original-result retrieval, and inconclusive lookup across reload.
Type checking and lint pass after the generated fixture correction.
The deployed browser evidence is recorded below.

External MCP SSE result replay remains a separate transport contract.
Elicitation and sensitive-tool approval are deferred in `external-mcp-elicitation-deferred.md`.


## Deployed headed Chrome acceptance, 2026-09-14

Main image: `sha256:f0355d2c5cc912409a0e2fa3a3978c506f9adc01eadc5b760c388a719c5222dc`.
Web image: `sha256:6f6767ebb1dacfd2a5dd409aa05648b8e244059f65d1673eed587140ab59e893`.
Rust retains image `sha256:5d024c10addbb14f58be732f3e48059df1abaf1ad5dc4d46fcca5588c04ce78d`.
Main retains all six mounts and its existing environment, including deployment secrets, database configuration, and durable TLS trust.
No OAuth emulator restart occurs.

A fresh headed Chrome session creates temporary MCP toolkit 68 and completes real DCR-public OAuth consent.
Playwright forwards the selected tool POST to Main but withholds its successful response from the page.
Rust execution `2a8c55d9af18f2b833db01723ffa24fa` returns `RUST_REQUEST_RECOVERY_BROWSER_20260914`.
The browser reloads before receiving the response or execution ID.
One request-key GET retrieves the original result and displays the marker.
The accepted request receipt is then cleared.
Observed counts are one browser POST, one result GET, and one provider resource call.
The provider count comes from the isolated emulator's actual invocation counter.
This fault injection proves response-loss recovery. It does not claim a worker crash during this call.

A second deliberate call receives a synthetic HTTP 503 before delivery to Main.
Request lookup returns 404 and the UI shows an inconclusive state.
Reload preserves the exact receipt and checks it again.
No automatic POST occurs and the provider invocation count remains unchanged.
The UI neither claims success nor claims that the unconfirmed execution is running.
Temporary toolkit deletion returns HTTP 204.

Evidence files are `elitea-request-recovery-browser.py`, `elitea-request-recovery-browser.log`, and `elitea-request-recovery-evidence.json`.
Screenshots are `elitea-request-recovery-success.png` and `elitea-request-recovery-unconfirmed.png`.
The browser suite exits successfully.
All 36 focused UI tests pass. Go unit, real PostgreSQL, MCP, and shared toolkit-execution checks pass.
The PostgreSQL run has no skips.

The read-only database check confirms one job for the accepted request and zero jobs for the undelivered request.
The reusable acceptance script is `tests/acceptance/toolkit_test_request_recovery.py` in this Rust package.
It takes explicit deployment, OAuth fixture, project, toolkit template, test subject, and output-directory arguments.

The packaged script also passes in a second fresh browser session with temporary toolkit 69.
Execution `0421389bd80e780f4cb26c3f94c52eed` returns `RUST_REQUEST_RECOVERY_BROWSER` after one POST and one provider call.
Both response-loss and unaccepted-request checks pass again. Cleanup returns HTTP 204.
Evidence is `elitea-request-recovery-packaged.log` and the script output directory.


## Final deployment verification

The final Main image is `sha256:1111a68e414a4331a4b3448af440411f58dec5dd8d7b4cb2ef7889ea4d11a9d8`.
It includes the complete current generated API contract and preserves the earlier uncommitted contract work.
The Web and Rust image identities remain as recorded above.
The packaged browser suite passes again against this final deployment.
Temporary toolkit 70 produces execution `14a039e867c310a0a4d98ddcf7e5b0f6`.
One POST, one result GET, and one provider call recover the exact marker after reload.
The undelivered-request case also passes. Cleanup returns HTTP 204.
Evidence is `elitea-request-recovery-final.log` and its screenshot directory.
