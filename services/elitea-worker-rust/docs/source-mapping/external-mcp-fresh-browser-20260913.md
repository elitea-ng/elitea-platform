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


## Toolkit replay and deployed external-client acceptance

Toolkit calls now use the same SSE resume transport as agent calls.
`ReadToolResultReference` identifies the original frozen result without carrying arguments or credentials.
`CurrentReadToolExecutionService.WaitForResult` verifies result identity and never performs another admission.
The reference is not permission. The MCP cursor authenticates its provenance; GET rechecks caller, project, scope, and current export.
The cursor accepts either an agent reference or a toolkit reference, never both.
No new schema, migration, or worker command is required.

Temporary result-observation errors close the stream without a final JSON-RPC response.
The priming cursor remains usable for GET retry.
Terminal toolkit failures still return an error result.
The existing JSON-only call path retains its prior behavior.

Source owners:

- `services/elitea-main/internal/application/toolkitexecution/execute.go`: admitted references and repeatable observation.
- `services/elitea-main/internal/api/v2/mcp/execute.go`: admission observers and resumable agent polling.
- `services/elitea-main/internal/api/v2/mcp/resume_cursor.go`: mutually exclusive result references.
- `services/elitea-main/internal/api/v2/mcp/resume_stream.go`: shared agent/toolkit response replay.
- `services/elitea-worker-rust/tests/acceptance/external_mcp_client.py`: independent SSE and GET acceptance client.

Main deploys as `elitea-main:mcp-resume-20260913`, image `sha256:2a35928cf102a7850e9a0310bae897b6457f66eaf622d37d5944e3fdd27b3be5`.
The replacement retains all six mounts and the existing environment, network, and resource settings.
The product database and Rust worker image remain unchanged.

A fresh visible Chrome session creates a temporary PAT through Personal Tokens.
The independent Python client receives only that PAT through its environment.
It initializes MCP, verifies unique names and argument schemas, and sends one tools/call POST per fixture.
It closes the POST connection after the priming cursor.
All continuation requests use GET with `Last-Event-ID`.

| Export | Disconnect | Main restart | Result |
| --- | --- | --- | --- |
| Toolkit 31, echo operation | After priming cursor | Yes | One text block with `RUST_PAT_RESUME_TOOLKIT_20260913` |
| Agent version 20, attached echo and linked skill | After priming cursor | Yes | One text block with `RUST_PAT_RESUME_AGENT_20260913` |
| Autonomous pipeline version 18 | After priming cursor | No | One text block with `RUST_PAT_RESUME_PIPELINE_20260913` |

Each initial cursor replays the same JSON-RPC result after completion.
Each completed cursor returns HTTP 204 without another response.
These calls do not resubmit tools/call after disconnection.
The PAT is revoked with HTTP 204.
Toolkit 31 returns to its original disabled-sharing state, verified after reload.
The browser closes after cleanup.

Focused MCP/toolkit tests, race tests, and vet pass.
Tests verify service replacement, original result identity, zero admission on GET, and transient observation without a final response.
The external-client result closes the normal autonomous transport-replay proof for these three fixtures.
It does not prove interrupted provider effects, every provider failure, restricted-user access, or all mixed authorization combinations.
Those remaining gate requirements require their own evidence before overall point 3 closure.

## Restricted caller and failed-result replay

A fresh headed Playwright session creates a dedicated restricted identity through the local OIDC form.
Database inspection confirms that this identity has no project membership or administrator role.
The browser creates a temporary PAT through Personal Tokens.
An independent Bearer-only client requests the private project MCP endpoint.
Tool discovery, saved-agent invocation, and internal skill creation each return HTTP 403.
This proves private-project isolation for a nonmember.
It does not prove individual operation restrictions for a project member.
The PAT is revoked with HTTP 204. The dedicated identity remains available for later tests.

A separate headed Playwright session enables Toolkit 31 sharing and creates another temporary PAT.
The independent client calls its echo operation without the required marker argument.
The client disconnects after the priming cursor and resumes through GET.
The terminal result contains `isError=true` and the expected safe failure text.
The initial cursor replays the exact error response. The completed cursor returns HTTP 204.
The acceptance client now supports `--expect-error` to verify terminal failures explicitly.
This case does not restart Main and does not prove saved-agent or pipeline failure behavior.
The PAT is revoked with HTTP 204. Toolkit sharing returns to disabled after reload.

Local evidence logs are `elitea-visible-restricted-mcp.log` and `elitea-visible-pat-failure-acceptance.log`.
Both runs use visible Chrome through Python Playwright.
A separate Computer Use check opens recovered chat 567 in a fresh Chrome tab.
The complete persisted response includes `MAIN_CHECKPOINT_ORCHARD_FINISHED_20260913`.
This confirms the rendered result after recovery; it does not repeat the crash experiment.
Saved-agent and pipeline failure and mixed-guard proofs remain open.

## Independent pipeline pause refusal

A fresh headed Playwright session creates a temporary PAT through Personal Tokens.
The independent client invokes `Hitl_node` through the pipeline version 8 endpoint.
The task includes `RUST_EXTERNAL_PAUSE_20260913` and requests a joke for review.
The client disconnects after the priming event and resumes through GET.
The response sets `isError=true` and identifies the human-approval boundary.
The original cursor replays the exact response. The completed cursor returns HTTP 204.
The client does not approve, reject, or resume the pipeline interrupt.
The PAT is revoked with HTTP 204.
The local evidence log is `elitea-visible-pat-pause-acceptance.log`.
This proves pause refusal through the independent transport client.
It does not prove runtime failure, mixed guards, or result replay after a later browser decision.

## Saved-agent runtime failure and imported-schema deletion

A temporary saved agent uses the existing model reference with temperature 99.
A headed Playwright session opens its editor before the independent MCP call.
Execution `74c85ef3271974b084fe28c119c03ffb` reaches durable `FAILED` state.
The external client disconnects after priming and receives `isError=true` through GET.
The original cursor replays the exact error. The completed cursor returns HTTP 204.
This proves saved-agent runtime failure delivery, not a provider outage or pipeline failure.
The temporary PAT is revoked with HTTP 204.

Cleanup initially returns HTTP 500 for application 20 and version 24.
The imported database has non-cascading application, version, variable, and tag foreign keys.
Main deletion incorrectly assumes that the native schema's cascading behavior applies to imported tenants.
`internal/infra/db/repos/applications.go` now locks the application and deletes owned rows in one transaction.
This includes optional legacy tool rows, variables, tag associations, versions, and the application.
A failed parent deletion rolls back all preceding changes.
No product schema or migration changes are required.

The current functional source is Core `api/v2/application.py`, method `delete`, which delegates application deletion to its shared service.
The native API keeps its existing authorization and published-version checks.
This Main fix supports cleanup of saved entities exercised through Rust execution; it does not change Rust checkpoint ownership.
`applications_imported_delete_postgres_integration_test.go` reproduces SQLSTATE 23503 before the fix and passes afterward.
The test also proves rollback when another reference blocks deletion.
Existing PostgreSQL deletion tests pass against the native schema.

Main deploys as `elitea-main:imported-delete-final-20260913` with all six existing mounts and unchanged deployment settings.
Its image is `sha256:95bb2e49469f24e9b31ecbd41dd3df51e54cc23ac762e54e48ab995bf233713a`.
Fresh headed Playwright verification confirms DELETE 204, subsequent GET 404, and absence from the rendered agent list.
The fixture is removed. The local cleanup log is `elitea-visible-fixture-cleanup.log`.
An initial browser login attempt fails with `invalid state cookie` before deletion.
Using the established settings login entry point succeeds; the login-path difference remains unclassified.

## Saved-pipeline runtime failure

A temporary pipeline copies the existing HITL fixture structure and changes its entry point to a nonexistent node.
Its saved model reference remains unchanged.
A fresh headed Playwright session opens the pipeline editor before the independent MCP call.
Execution `224b13fb04098f591baa9dc385ea9905` reaches durable `FAILED` state.
The client disconnects after priming and receives an error through GET.
The original cursor replays the exact error. The completed cursor returns HTTP 204.
This proves pipeline configuration-failure delivery through the runtime and external transport.
It does not prove an interrupted provider effect or a provider outage.
Application 21 and version 25 are removed through the normal API with HTTP 204.
The temporary PAT is revoked with HTTP 204.
The local evidence log is `elitea-visible-pipeline-runtime-failure.log`.

The remaining mixed-guard requirement is separate from these failure cases.
TG-02 describes independent parallel HITL, sensitive-tool, and delegated-authorization guards.
TG-10 requires external results to remain errors while those guards prevent autonomous completion.
Individual HITL refusal and terminal error replay do not prove this combined behavior.

## Mixed-guard component verification

The exact Rust test `agents::pipeline_tests::pipeline_agent_node_resumes_parallel_mixed_sensitive_and_authorization_guards` passes.
It checks separate descendant identities and zero protected calls before decisions.
After both decisions, each child performs one call and the parent completes without replanning.
Its model and MCP connectors are doubles. This is component evidence, not deployed acceptance.
The initial short exact filter selects zero tests and is excluded from evidence.

A real PostgreSQL projection test exposes a reporting gap for both saved agents and pipelines.
When both guard metadata fields exist, Main reports only human approval.
The result already remains an error and does not expose partial text.
`mcp/execute.go` now identifies human approval and tool authorization together.
It directs the user to resolve each pending request in the conversation.
It does not add external interrupt decisions or change worker checkpoint ownership.
`TestExternalAgentAndPipelineReportBothPendingGuardKinds` fails before this change and passes afterward.
Its two subtests verify both guard descriptions, the execution ID, and absence of partial text.
Admission and worker execution remain doubles in this PostgreSQL test.
The current business sources remain the sensitive and delegated guard mappings in `agent-runtime.md`.
The new Main projection preserves these distinct Rust events in the external error description.
A deployed combined-guard run remains required.
Main deploys as `elitea-main:mixed-guard-report-20260913`.
Its image is `sha256:c3068f0573b0b6d283fc439f75c5697f1d249c86978bef5fad94fe426b70ec9f`.
The replacement retains all six mounts and existing runtime settings.

## Live combined-guard failure

A fresh headed Playwright session creates two child agents and a parent that invokes both in parallel.
One child uses the synthetic OpenAPI echo. The other uses a new delegated MCP toolkit.
Main's administration guardrail API temporarily marks the OpenAPI echo as sensitive.
The worker fallback policy does not override Main's stamped policy; an earlier fallback-only attempt produces only delegated authorization.

Execution `56c18ebef68895b695e53255134b317f` invokes both child agents.
Message group 5904 records the echo call and the separate MCP authorization call.
The worker publishes the sensitive interrupt, then returns `agent_pause.mixed_guardrails_unsupported`.
The execution eventually reaches `FAILED`; the browser projection reports `The runtime operation failed.`
This is not a passing combined-guard result.

The blocker is `AgentPauseAccumulator::finish` in `src/execution/native_agent_lifecycle.rs`.
It explicitly rejects a nonempty sensitive/HITL set together with a nonempty authorization set.
The passing nested-agent component test does not cross this lifecycle boundary.
The next implementation must preserve both sets through terminal publication and Main projection.
It must retain exact child identities and existing browser continuation authority.

Main's original guardrail values are restored through its API with HTTP 200.
Applications 25, 26, and 27 and toolkit 38 are deleted with HTTP 204.
The temporary PAT is revoked with HTTP 204.
The earlier fallback-policy test also removes all fixtures and restores the worker policy file byte-for-byte.
The live failure log is `elitea-live-mixed-authoritative.log`.
The reusable local fixture script is `elitea-live-mixed-guards.py`.

## Mixed-pause lifecycle implementation

Rust `AgentPauseAccumulator::finish_mixed` now retains both guard sets in one terminal event.
The event keeps the existing paused-HITL terminal state and includes the existing authorization metadata.
The authorization terminal identity remains bound to its last request; each sensitive interrupt retains its own identity.
The existing total limit of 16 cards applies across both sets.
No new protobuf state or database migration is introduced.

Main `agent_execution_results.go` validates both existing metadata shapes against the bound terminal artifact.
It rejects a combined set above the limit and identities duplicated across guard kinds.
`agent_mixed_pause.sql` persists both sets in one update under the existing terminal transaction.
It retains unrelated response metadata and stops streaming without marking the pause as an execution error.
The existing single-kind writers remain unchanged.
The SQL generator produces the new query binding and interface entry.

The lifecycle regression now expects both sets instead of the previous explicit rejection.
The bound-artifact regression verifies decoding and selection of the mixed writer.
`TestMixedPauseSQLPreservesBothGuardSets` passes against isolated PostgreSQL projection columns.
These checks establish component and SQL behavior. Deployed runtime and browser verification remain pending.

## Deployed mixed-pause acceptance

Main deploys as `elitea-main:mixed-pause-20260913` with image `sha256:3134d5fd03d5fe09f5c69695c769b31199a99e45a7d286d65727f0188ee78085`.
Rust deploys as `elitea-worker-rust:mixed-pause-20260913` with image `sha256:aa57b19e80d837881d1939d2c7d290859846e9ce3a5ee440f8b807ea3603696d`.
Both replacements retain their environment, mounts, networks, and resource limits.

A fresh headed Playwright session creates the PAT through account settings.
The independent MCP client invokes temporary parent agent 30, version 34, with both child agents attached.
Execution `d82323e078d858a7596ea25d04a46fcf` successfully persists the pause in message group 5906, conversation 575.
The infrastructure job is `SUCCEEDED`; the requested agent work remains paused.
The MCP result has `isError: true` and identifies human approval and tool authorization together.
The initial cursor replays the exact result. The completed cursor returns HTTP 204.
The client receives one text block and performs no external approval or authorization decision.

A separate fresh headed browser opens conversation 575 after persistence.
It renders `Authorize`, `Skip Auth`, `Approve`, and `Reject` controls together.
The screenshot is inspected. This verifies rendering, not successful browser continuation after either decision.
The local evidence files are `elitea-live-mixed-deployed.log`, `elitea-mixed-chat-ui.log`, and `elitea-mixed-chat-ui.png`.

The original administration guardrails are restored with HTTP 200.
Temporary applications 28, 29, and 30 and toolkit 41 are deleted with HTTP 204.
The temporary PAT is revoked with HTTP 204.
The saved-pipeline combined-guard variant and the remaining point 3 checklist still require verification.
Pipeline HITL history and participant UI remain outside this fix.

## Saved-pipeline combined-guard acceptance

A temporary saved pipeline wraps the same parallel child-agent fixture through the existing `agent` graph node.
Its graph alias matches the attached saved application name.
Rust `graph/application_tests.rs` defines this existing node contract; `agents/application_tools.rs` resolves the frozen alias.
No new graph node or pipeline-history behavior is added.

The independent client invokes pipeline 34, version 38, through `/app/2/mcp/pipeline/38`.
Execution `8fb7ec321ea390427566e0f5cb94731a` persists both guards in group 5908, conversation 576.
The MCP response reports both pending guard kinds as one error result.
Initial-cursor replay matches the result; the completed cursor returns HTTP 204.
No external approval or authorization decision occurs.

A separate headed Playwright browser opens the saved conversation.
Both authorization controls and both sensitive-decision controls render. The screenshot is inspected.
This verifies nested control presentation, not direct pipeline decision history or post-decision execution.
Evidence files are `elitea-live-mixed-pipeline.log`, `elitea-mixed-pipeline-ui.log`, and `elitea-mixed-pipeline-ui.png`.

The administration guardrails are restored with HTTP 200.
Temporary applications 31 through 34 and toolkit 44 are deleted with HTTP 204.
The UI-created PAT is revoked with HTTP 204.
Both saved-agent and saved-pipeline combined-guard external refusal and replay now have deployed evidence.
Toolkit Test active-invocation replacement remains an independent point 3 verification requirement.
