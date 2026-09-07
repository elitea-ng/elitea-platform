# Delegated OAuth and DCR source mapping

Status: Main's OAuth and DCR proxies are implemented. The existing UI flow can
use them. Local HTTPS grant tests pass. Live provider proof remains required.

This flow supports remote MCP servers and delegated toolkit authentication.
SharePoint and OpenAPI use the same authorization contract.

## Ownership

| Layer | Responsibility |
| --- | --- |
| UI | Consume guard metadata, retain remote MCP discovery, open the popup, create PKCE material, and manage OAuth tokens. |
| Main | Authorize the caller, resolve stored toolkit credentials, proxy token and DCR requests, and return bounded safe responses. |
| Rust worker | Discover public configured OAuth metadata at the guard, emit durable interrupts, and consume claim-scoped tokens after approval. |

Rust never receives a stored OAuth client secret from this browser proxy.
Main expands the secret only for one outbound token request.

## Current-platform evidence

Current-platform evidence was checked on 2026-09-04. The `elitea_core`
revision was `6a036d777ca909fac377ceaec05719f0fa611b6d`.

| Current source | Observable contract | Replatform source |
| --- | --- | --- |
| `models/pd/mcp_oauth.py::McpOAuthTokenRequest` | Accept authorization-code and refresh grants. Preserve `used_dcr` for both grants. | `internal/api/v2/eliteacore/mcp_oauth_proxy.go` |
| `api/v2/mcp_oauth_proxy.py::ProjectAPI.post` | Resolve omitted toolkit credentials. Read SharePoint and OpenAPI nested configuration. | Main resolver and proxy |
| `utils/mcp_oauth.py::{exchange_token,refresh_token}` | Send form data. Accept JSON or form token responses. Apply a 30-second timeout. | Main proxy HTTP boundary |
| `models/pd/mcp_oauth.py::McpDynamicClientRegistrationRequest` | Accept RFC 7591 client metadata. | Main DCR request model |
| `utils/mcp_oauth.py::register_dynamic_client` | Supply current defaults and forward optional registration fields. | Main DCR proxy |
| EliteaUI `mcpAuthFlow.helpers.js` | Run popup, PKCE, DCR, exchange, persistence, and refresh flows. | `apps/elitea-web/src/features/mcps/` |

The current platform prevents stored credentials from replacing DCR-issued
credentials. Main keeps the same `used_dcr` rule.

Main can still resolve an omitted toolkit scope for a DCR request. It never
loads that toolkit's client identifier or secret for the request.

## Main credential boundary

Main reads one actor-visible toolkit through the generated tenant repository.
The repository applies the current folder-access overlay.

Main then uses the existing claim-mode configuration resolver. This resolver
expands configuration references and vault references.

The OAuth handler owns no raw SQL. It also creates no second vault or
configuration expander.

Stored settings can use top-level fields. They can also use
`sharepoint_configuration` or `openapi_configuration`.

Prebuilt MCP settings use the existing catalogue resolver. Main validates the
stored toolkit type before it reads those settings.

## Endpoint safety

Caller-supplied and DCR-issued credentials can use any validated HTTPS token
endpoint. Loopback HTTP remains available for local development.

Main sends a stored toolkit secret only to a bound endpoint. An exact stored
`token_endpoint`, `token_url`, or `oauth_token_endpoint` creates that binding.

An `oauth_discovery_endpoint` also creates a same-origin path binding. This
supports the configured Azure tenant base URL used by SharePoint.

Main rejects a stored secret for an unbound endpoint before transport. This is
an intentional security correction over the current unrestricted proxy.

Redirects must keep the original origin. TLS verification remains enabled.

## Wire and failure contract

OAuth requests are limited to 64 KiB. Provider responses are limited to
512 KiB. Both proxies use a 30-second request context.

The token proxy supports `authorization_code` and `refresh_token`. It forwards
PKCE, scope, client data, and the required grant value.

The token proxy accepts JSON and form-encoded provider responses. It returns
only token fields used by the UI.

The proxy never returns a stored `client_secret`. Provider errors contain only
bounded sanitized descriptions.

The DCR proxy forwards RFC 7591 fields. It supplies these defaults:

- `grant_types`: `authorization_code`, `refresh_token`;
- `response_types`: `code`;
- `token_endpoint_auth_method`: `none`;
- `application_type`: `web`.

DCR can return a provider-issued client secret. The UI needs that secret for a
later token exchange and refresh.

## Verification

Main tests cover nested OpenAPI credentials, SharePoint scopes, DCR isolation,
bound endpoints, unbound endpoints, and actor identity.

Main tests also cover JSON and form responses, RFC 7591 fields, redirects,
request bounds, response bounds, and redacted failures.

Application tests prove claim-mode resolution and absent-row behavior. The
runtime composition reuses the sqlc repository and existing resolver.

The replatform UI already has focused DCR, exchange, refresh, and `used_dcr`
tests. A live provider test remains required before production activation.

## Durable authorization display

The current SDK treats authorization as a durable interrupt.
`elitea_sdk/configurations/sharepoint.py::_build_mcp_authorization_required` also supplies OAuth discovery metadata.
Indexer normalizes this metadata in `utils/funcs.py::_mcp_auth_error_to_metadata`.
Its `methods/agent_common.py` projects the pause as `action_required`.
EliteaUI renders the authorization decision through `ChatContinue.jsx`.

The replatform display must preserve an exact Skip decision when discovery metadata is absent.
`apps/elitea-web/src/entities/message/lib/authorizationActions.ts` retains this durable action.
It requires `mcp_auth`, an interrupt identifier, and an explicit `skip` action.
An ordinary discovery failure does not become a durable pause.

`ChatContinue.tsx` shows the missing-metadata explanation and preserves Skip.
It disables authorization until discovery metadata and the modal integration are available.
It never fabricates an authorization server or reports a successful login.
`SubAgentAccordion.tsx` serializes structured output before rendering it.
This prevents authorization metadata from crashing the nested view.
The original structured object remains available for the exact resume operation.

The regression test normalizes two persisted child interrupts and renders the real nested panel.
It checks separate Skip controls and the exact selected interrupt identifier.
The first nested rendering test reproduces React's invalid-object-child failure before the fix.

Live execution `159381af6f06a6df13bb6432a073dee6` pauses on one Surname Resolver authorization request.
Main persists its interrupt identifier and parent hierarchy, but no authorization-server list.
The worker settles the outer delivery and releases its claim.
This is a durable pause, not successful execution of the protected SharePoint operation.
The selected Name Resolver version does not require authorization in this configuration.

## Parallel result ownership and resume

The parent waits for every dispatched child to return a result or a durable pause.
A completed child keeps its result while another child waits for authorization.
The parent must not request its final model response until the pending child resumes.
Authorization or Skip resumes that child without rerunning the completed sibling.

`agents/application_tools.rs::ApplicationCallBatch` tracks pending calls and suppresses interrupted results.
`ApplicationEventStreamingAgent` stops before the parent consumes an incomplete result set.
`pipeline_parent_waits_for_single_child_authorization_before_final_answer` checks both Authorization and Skip.
It checks model request counts and both child results in the parent's final request.

The single-decision parser previously treated nested OAuth decisions as direct pipeline tool decisions.
`agents/graph/resume.rs` now keeps delegated decisions in their typed application route.
Exact checkpoint validation still rejects incomplete parallel decision sets before model or protected tool execution.

Current EliteaUI `src/components/Chat/hooks.js` stores `AgentLlmChunk` text in the owning execution step.
The replatform previously appended all child chunks to the parent answer.
Main also persisted those chunks as provisional parent text.
This displayed a child's answer while the actual parent remained paused.

`chatStreamTurnFrames.ts` now keeps child model events separate from parent answer state.
Child completion updates execution steps but cannot finish the parent turn.
Main's `agent_stream_text.go` excludes child chunks from provisional parent text.
The existing trace projection retains child execution records.
`ApplicationAnswer.tsx` displays named authorization controls before answer content.
This change does not redesign the deferred thinking-step panel.

Focused tests cover live frame ownership, parent completion, child step retention, and PostgreSQL provisional text.
The PostgreSQL test preserves parent text across failure without adding a child's answer.
Live browser resume and reload checks remain required for this slice.

## Shared configured authorization and exact replay

The SDK evidence was checked again on 2026-09-07 at revision `4b7691c7ddc4bc2a8e4f1b5f7da620d845b55b0e`.
This contract applies to every delegated-auth toolkit, not only SharePoint.

| Current source | Business behavior | Rust or UI source |
| --- | --- | --- |
| SDK `runtime/toolkits/tools.py::_build_deferred_mcp_auth_tools` | Raise an agent guard when the model invokes the proxy. Return a structured result for Skip. | `agents/session.rs`, `agents/direct_hitl.rs`, `toolkits/delegated_auth.rs` |
| SDK `configurations/sharepoint.py::_build_mcp_authorization_required` | Supply public endpoints, client identifier, scopes, and configuration identity. | `toolkits/delegated_auth/discovery.rs`; SharePoint and OpenAPI configuration adapters |
| SDK `runtime/tools/llm.py::_append_completion_dedup` | Preserve one provider call/result pair for each call identifier. | `agents/replay_history.rs` |
| SDK `runtime/tools/llm.py` parallel guard aggregation | Retain completed siblings and the original pending call batch. | `agents/application_tools.rs`, `agents/direct_hitl.rs`, pipeline resume tests |
| EliteaUI configuration-scoped OAuth storage | Keep separate credentials for separate configurations on one issuer. | `authorizationActions.ts`, `useChatBoxHandlers.authorization.ts`, Rust requirement matching |

Agent authorization remains inside the tool-calling loop.
Direct pipeline nodes raise the same guard when the node executes.
Public metadata discovery does not grant authorization or execute the protected operation.

The worker limits discovery to five seconds and 64 KiB.
It keeps TLS verification enabled and refuses redirects.
It sends no authorization header or cookies.
It projects only approved public metadata fields.
The client-secret marker is the constant `********`, not the stored secret.
Main resolves that marker through the actor-visible toolkit when it exchanges a code.

The UI disables configured authorization when required endpoints are absent.
It preserves the exact Skip control and does not synthesize OAuth endpoints.
Opening or cancelling the dialog does not resume the run.
Only the successful OAuth callback submits Authorization.

The browser submits tokens only for the decided authorization cards.
Rust matches configuration-scoped keys against the persisted requirement.
A token for another configuration cannot satisfy the decision.
OpenAPI also resolves the same scoped key when it rebuilds its client.

ADK checks all batch confirmations before executing a call.
Replaying the whole batch with one decision can therefore pause again without recording that decision.
Rust first replays the exact decided call and persists its result.
It then admits the remaining batch through the existing policies.
The provider projection removes duplicate replay calls without changing durable ADK events.
It rejects changed arguments, repeated results, and reused completed call identities.

Tests cover Authorization and Skip for application and ad-hoc calls with one or three pending calls.
They verify zero protected calls after Skip and no provider replanning of the pending batch.
Nested tests validate complete provider histories and retain completed sibling results.
Bounded generated cases check projection idempotence for batches up to 64 calls.
Transport tests cover fixed and chunked response bounds, malformed responses, redirects, and cancellation.

The full local suite passes 866 library tests and 83 integration or contract tests.
PostgreSQL-backed tests run against isolated test databases; no Rust test is ignored.
The expanded chat, message, and MCP suite passes 874 UI tests, and TypeScript checking passes.
These results do not prove a live authorization-code or refresh grant.

## Local browser proof: 2026-09-07

The rehearsal uses Private project 2 and conversation 528.
The saved pipeline calls Full Name Resolver, which calls Name Resolver and Surname Resolver.
The selected Surname Resolver version requires delegated authorization.
These checks resume an existing participant. They do not prove attachment through the composer menu.
The stored participant identifies application 10, version 16, but lacks `agent_type` in its conversation settings.
The UI groups this participant under Agents. The visible group does not establish the execution type.

Regeneration execution `6a5164dc944e86fab7050736b03dd09c` reaches the durable authorization pause.
The authorization card has public discovery data and survives page reload.
Authorize opens the real OAuth dialog with configured client settings and scopes.
The dialog does not request the stored client secret.
Cancelling the dialog creates no continuation execution.
The persisted parent answer remains empty while the child waits.

Skip execution `d5dc334f066811a1a1354af86a55ca36` completes successfully.
Session event 76 closes the original `get_files_list` call with `mcp_auth_decision`, status `declined`.
No protected SharePoint read runs for that call.
The completed Name Resolver does not run again.
Surname Resolver returns at event 78; the parent emits its final answer at event 79.

The saved answer contains both names and has 1,247 characters.
It has no error, no pending authorization request, and one resolved authorization identifier.
The answer survives page reload.
The worker restart count remains zero throughout both executions.

This browser proof covers regeneration, public discovery, dialog opening, cancellation, Skip, sibling reuse, parent ordering, and persistence.
It does not cover a real account login or the subsequent token exchange.

An unrelated old toolkit delivery still reports `agent_output.invalid_durable_state`.
Its execution identifier is `15f6d2c70254eb8573094fa4d1cc839e`.
This slice does not delete its state or change its retirement behavior.

### Composer attachment and execution identity

Separate browser checks use conversations 529 and 530 in Private project 2.
The operator selects Resolve Name through the Pipelines submenu without direct API writes.
Both conversations persist application 10, version 12, with `agent_type: pipeline`.

Conversation 529 starts from an empty chat.
Execution `d6bd750889f28c390e143daebc5bdeec` invokes Full Name Resolver through `pipeline:Agent1:0`.
Its model requests Name Resolver and Surname Resolver in one tool-call batch.
Name Resolver completes. Surname Resolver pauses for delegated authorization.
The parent answer stays empty. The enabled authorization controls survive reload.
Authorize opens the configured dialog; cancellation starts no grant.

Skip execution `e4f97c8cdfcdec22065cb2841f7c3874` records `mcp_auth_decision: declined` at event 13.
The completed Name Resolver does not run again.
Surname Resolver returns at event 15. The parent then requests Surname Resolver again at event 16.
This new call reaches another authorization pause. This test does not prove terminal pipeline completion.
The outer delivery settles; the paused conversation remains available for investigation.

Conversation 530 has a completed ordinary model turn before attachment.
The participant POST returns HTTP 200 and preserves the earlier messages.
The next composer send starts execution `f5cc80b66458b5a60e2a3b8769a103d0`.
Durable event 4 invokes `elitea_agent_9_v_15` with call ID `pipeline:Agent1:0`.
Event 6 records its result, and event 7 finishes the root pipeline.
The saved answer has 151 characters, both child-agent names, no error, and no authorization pause.
This launch test asks for configured child names without invoking them.
It proves attachment to an existing conversation and saved-pipeline routing, not protected-tool execution.

## Child resume task preservation: 2026-09-07

Conversation 531 uses Full Name Resolver as a direct agent, not Resolve Name as a pipeline.
Execution `49c3d5e8c7fbae0af7947f6b5c505107` completes Name Resolver and pauses Surname Resolver for authorization.
Skip execution `f7e0f75ec1f25f6d17efc52b88b97865` closes the original tool call with status `declined` at event 10.
Surname Resolver emits the literal `?` at event 11. Its parent receives that result at event 12.
The parent emits its final answer at event 13.
The placeholder is a native child model result, not a browser-only replacement.
Conversation 533 reproduced the same result after a single explicit Skip.
Both traces show that the child resumes before its parent produces the final answer.

The provider-request regression then identified a missing input boundary.
ADK 2.2.0 `adk-agent/src/llm_agent.rs::build_request` replaces the last user entry with the current input.
`adk-runner/src/runner.rs::run_with_config` first appends that current input to session history.
Our custom child context bypassed Runner and did not append the replay input.
ADK therefore replaced the original child task with the private replay marker.
The provider projection removed that marker, leaving no original user task.
This also removed an intermediate orchestrator's task in a pipeline resume.

`agents/application_tools.rs::ApplicationToolInvocationContext::with_resume` now appends the current input before running ADK.
It preserves the original task without exposing replay markers to the provider.
The exact-child invocation and fenced session ownership remain unchanged.
This implements the SDK's resume behavior using ADK's input contract, not a byte-for-byte SDK port.

The failing regression observed zero user messages in the resumed child request.
The fixed tests verify the exact task once, no control marker, and both sibling results.
They cover direct agents and pipeline Agent nodes, Authorize and Skip, and either sibling completing first.
The deployed task-preservation fix restored a meaningful Surname response, but exposed two further collection limits.

### Complete child results and logical event limits

In conversation 533, child event 37 contained 965 characters after Skip.
Its parent received only the last streamed fragment: ` with these tasks?**`.
The child result collector expects a complete event, but direct replay supplied an SSE run configuration.
`ApplicationToolInvocationContext::with_resume` now uses `StreamingMode::None` for fresh and resumed child tools.
It retains the replay's exact confirmation decisions and fingerprints.
It returns the entire accumulated child result instead of its final delta.
The regression uses character-sized provider chunks and fails with `d` instead of `resolved child` before the fix.

Pipeline LLM nodes retain their live SSE events.
`graph/llm.rs` collects their final output by stable model-turn event identity.
Their private replay transcript also receives complete model content, not the terminal delta alone.
Fresh LLM-node sessions append the current input before ADK runs, preserving previous user turns.
The pipeline tests now use fragmented provider responses across ordinary, authorization, clarification, block, and nested-subgraph paths.

Conversation 532 then failed at child event 46 with 285 text parts, 3,023 characters, and finish reason `Stop`.
The failure was the 256-parts projection bound, not a provider token stop or agent-step limit.
ADK retains one part per provider delta in an accumulated response.
`agents/events.rs::bounded_logical_parts` now counts adjacent text or thinking fragments as one logical block.
It does not alter persisted content or reasoning signatures.
The 256 logical-block cap, 60-KiB text bound, 40-KiB tool-value bound, and tool-call bounds remain in force.
A 61,440-fragment scan bound also rejects empty-fragment floods.
Generated cases cover Unicode text with 1 through 2,048 fragments, including the 256/257 boundary.
Negative cases retain byte, logical-block, and empty-fragment limits.
These limits remain independent of model max-output tokens and need separate large-output transport proof.

### Terminal direct-agent and pipeline proof

The latest Rust image completes both conversations after one explicit Skip each.
Conversation 533 starts execution `16081c20f26dc7a030ff57da525509eb` and resumes through `b012b11332242b6c5726fb592d24b0f4`.
Name Resolver completes at event 45 and remains available through the pause.
Surname Resolver returns 1,014 characters at event 52. The parent receives all 1,014 characters at event 53.
The parent completes at event 54. No repeated authorization request follows.

Conversation 532 starts execution `067cf4ccba6b375647f51f8c45a31a09` and resumes through `60f3060e4c3504f297895a9c444c2fcd`.
Name Resolver completes at event 56 and does not run again during resume.
Surname Resolver returns 2,757 characters at event 64. Full Name Resolver receives all characters at event 65.
Full Name Resolver returns 1,509 characters at event 66. The pipeline receives all characters and finishes at event 68.
Both saved answers survive reload, contain no error, and retain one resolved authorization identifier without a pending guard.
These proofs cover Skip, not a real OAuth grant.

The same-page regeneration check exposes a separate browser identity defect.
Resume events can carry `question_id: null`, while the resume request identifies only the existing answer.
The old transport passes null through and erases that answer's question link.
The next Regenerate then falls back to the legacy route and receives HTTP 400.
Reload restores the persisted link and hides the defect.
`useChatStreamTransport.ts` now normalizes null and empty links to absence, preserving the existing question identity.
The transport regression covers null, empty, and omitted links through resume, summary, and terminal frames.
The null and empty cases fail before the fix.
After deployment, same-page regeneration returns HTTP 200 through `agent.regenerate.v1` for both conversations after Skip.
The browser does not reload between the decision and Regenerate.
The affected UI files also pass the repository lint command after typed batch cleanup.

A repeat pipeline run reveals a separate guard-cardinality gap.
Execution `03de21c5f7cb6f4fb32b040920c8def4` requests `get_files_list` and `get_lists` together at event 76.
Resume `f518b52ba11c060588ee45bd6e197e51` records the first Skip at event 85.
The second original operation raises a new guard at event 87.
This is not duplicate execution of the resolved call or premature parent completion.
The next Skip completes the pipeline. However, requiring a second toolkit decision differs from the SDK's toolkit-level proxy behavior.
This reproduction defines the toolkit-wide authorization regression addressed below.

The [runtime limit inventory](runtime-limits.md) distinguishes this event bound from cumulative session limits and execution policy.

### Toolkit authorization tools and run-scoped Skip

The SDK baseline was checked again on 2026-09-07 at revision `c0bca04c5a3ef53608932eeb925e4a473175e5f2`.
The user confirms demand-driven authorization for agents and LLM nodes, not preauthorization before the model runs.

| Current SDK source | Required behavior | Rust source |
| --- | --- | --- |
| `runtime/toolkits/tools.py::_infer_proxy_tool_names` | Expose one authorization tool for an unavailable toolkit. | `toolkits/delegated_auth/model_tools.rs` |
| `runtime/toolkits/tools.py::_build_deferred_mcp_auth_tools` | Hide protected operations until authorization succeeds. | `bind_authorization_model_tools` and the existing toolkit materializers |
| `runtime/toolkits/tools.py::_make_mcp_auth_control_tool` | Retain a structured Skip decision without another prompt for each operation. | `DelegatedAuthorizationCatalog::decline` and `DeclinedTool` |
| Deferred toolkit handling in `runtime/toolkits/tools.py` | Keep direct-node authorization separate from model tool selection. | Existing direct Toolkit/MCP node guards remain unchanged. |

An unauthorized toolkit now exposes only its local authorization tool to the model.
The tool name identifies the frozen toolkit configuration without putting a resource URL in the name.
Selected protected declarations remain hidden, including distinct operations requested together.
Private operation placeholders remain available for exact legacy replay. They cannot dispatch protected work.

Authorize validates the stored continuation and rebuilds the toolkit with claim-scoped credentials.
The resumed authorization tool returns a local structured result. It does not execute a protected operation.
The next model request sees the admitted operations and selects its next action.
A sensitive-action policy still requires its own approval.

Skip covers the bound toolkit's unavailable operations for the current run.
Sequential and parallel retries return structured `declined` results without protected dispatch or another authorization interrupt.
The private checkpoint retains this scope if another guard suspends the run.
A fresh user turn does not inherit this private Skip scope.
Different toolkit configurations on the same resource do not inherit each other's decisions.

Direct agents, nested agents, and pipeline LLM nodes use the shared model boundary.
Pipeline replay stores the decisions in its existing checkpoint envelope.
Direct Toolkit/MCP nodes retain their node-start authorization path.
This slice changes neither Main nor the browser wire contract.

Removing the authorization declaration exposed a model-facade history defect.
The facade required historical tool names to remain in the current declaration list.
It now validates historical names and payload bounds without treating that history as current execution authority.
Both model facades still reject new calls outside the current tool declarations.

Verification passes 871 library tests and 93 integration or contract tests, with no ignored tests.
The run includes PostgreSQL session, checkpoint, fencing, and recovery tests.
Component cases cover MCP, OpenAPI, and SharePoint with 1 through 16 protected operations.
Runtime tests cover application and ad-hoc execution, nested siblings, pipeline LLM nodes, Authorize, Skip, and separate sensitive guards.
Both model facades preserve retired-tool history and reject a new call to that retired tool.
Formatting, strict Clippy, and strict rustdoc checks pass.
Real provider authorization-code, DCR, refresh, and concurrent-tab proofs remain separate gates.

### Deployed authorization-tool proof

The release image `sha256:3148e1cb30f0c54fd5a57bb6e371b7d77ecc5020ddea03345f5188460edfc19e` passes both Private-project browser flows.
Only the idle Rust worker is replaced. Main, UI, and database containers remain unchanged.

Full Name Resolver in conversation 533 starts execution `5cd7464300b90d18b20e8e033eeef617`.
Surname Resolver calls the authorization tool at event 83, before any protected operation.
Name Resolver completes at event 85 while Surname Resolver waits.
The browser shows one authorization guard with an enabled Authorize button.
Skip resumes execution `f2f36365db6963cb10272262b7ce7969` and records `declined` at event 90.
Surname Resolver completes at event 91. The parent completes at event 93, without another Name Resolver invocation.
The saved answer contains 1,283 characters and one resolved authorization identifier.

Resolve Name in conversation 532 regenerates through HTTP 200 and starts execution `0b3976ed76b28f1ed7397b947fc36b0a`.
Surname Resolver calls the authorization tool at event 131. Name Resolver completes at event 133.
One Skip returns HTTP 200 and resumes execution `a7a063f2ffda600be03ea91c37455cd2`.
The authorization result is `declined` at event 140. Surname Resolver completes at event 141.
Full Name Resolver completes at event 143. The pipeline completes at event 145.
The saved answer contains 1,516 characters and one resolved authorization identifier.
Neither flow invokes protected SharePoint operations or raises a second guard after Skip.
Both saved answers survive reload with no pending guard or error.
These browser results prove Skip and parent ordering, not a successful provider login.

### Local OAuth protocol components

The current Core source is checked again on 2026-09-07 at revision `09d8d5d8e10cddd0d2b8e59d18866f021fc006cb`.
`utils/mcp_oauth.py` remains the behavioral baseline for code exchange, refresh, and registration.
The replatform keeps form token responses, optional token fields, DCR credential precedence, and the existing HTTP routes.

The tests use temporary HTTPS servers with verified test certificates.
One server acts as an OAuth issuer. A separate server exposes a protected OpenAPI-style resource.
No test uses Aha, SharePoint, a real user grant, or a stored production credential.

| Evidence | Owning source | Verified boundary |
| --- | --- | --- |
| DCR public and confidential clients | Main `mcp_oauth_protocol_test.go::TestMCPOAuthProtocolTLSGrantsAndProtectedOpenAPI` | Forward registration metadata. Preserve issued client identifiers and secrets. Do not replace them with stored OpenAPI credentials. |
| Stored OpenAPI client | Same Main test | Resolve one toolkit under the caller's project and actor. Send its credentials only to its bound initial endpoint. |
| Code exchange and S256 | Same Main test | Forward the RFC 7636 Appendix B verifier. Preserve rejection of wrong verifiers, callback URIs, client credentials, and consumed codes. |
| Refresh rotation | Same Main test | Return replacement access and refresh tokens. Omit code, callback, and verifier fields from refresh requests. Preserve rejection of the consumed refresh token. |
| Separate protected resource | Same Main test | The fixture rejects an absent Bearer token. Issued and rotated access tokens permit resource reads. |
| Cross-origin redirect | Main `TestMCPOAuthProtocolDoesNotForwardCredentialsAcrossOriginRedirect` | Reject 302, 307, and 308 redirects before the second server receives a request. |
| Request cancellation | Main `TestMCPOAuthProtocolCancellationReachesProviderRequest` | Carry caller cancellation into the provider request. |
| Malformed provider success | Main `mcp_oauth_validation_test.go` | Reject missing, empty, or wrongly typed credentials, conflicting error fields, duplicate form tokens, and oversized responses. |
| Error redaction | Same Main test file | Remove PKCE verifiers, codes, refresh tokens, and client secrets before truncation. Handle short and overlapping values. |
| Parallel Rust operations | Rust `openapi_tests.rs::delegated_openapi_rotated_tokens_stay_bound_during_parallel_resource_calls` | Keep two configuration-and-issuer token bindings separate. Rebuilt clients use rotated tokens. No refresh or client-credential exchange occurs in these resource calls. |
| Browser state logic | UI `oauthFlow.test.ts` and `tokenLifecycle.test.ts` | Retain the existing DCR, exchange, refresh, and token-state unit contracts. |

The regression tests fail before the Main correction.
Main now validates credential response fields and adds `Cache-Control: no-store` and `Pragma: no-cache` to both proxy responses.
Main also redacts complete grant values before truncating provider errors.
These corrections change neither database queries nor the successful browser request contract.
The Rust change adds a component test. It does not change runtime token ownership.
The coding guidelines keep this slice limited to verified defects and the existing transport test seam.

The protocol references are [RFC 6749 sections 4.1.3, 5, and 6](https://www.rfc-editor.org/rfc/rfc6749),
[RFC 7591 section 3](https://www.rfc-editor.org/rfc/rfc7591),
and [RFC 7636 section 4.6 and Appendix B](https://www.rfc-editor.org/rfc/rfc7636).
The fixture validates PKCE and grants. Main forwards them; Main is not the authorization server.

Verification passes:

- Main: 22 focused tests and 42 subtests under the race detector, with no skips.
- Rust: 336 toolkit library tests, with no ignored tests.
- UI: 32 OAuth and token-lifecycle unit tests.
- Go vet for the handler package, strict all-target Rust Clippy, and Rust formatting.

The broader Main handler package also runs against the rehearsal PostgreSQL service.
All 227 tests and 195 subtests pass, with no skips.
Each database fixture creates and removes its own named database. The fixtures do not modify rehearsal application data.
No deployment, real browser authorization, database migration, or production capability activation occurs in this slice.

These are component proofs, not a continuous browser-to-Rust run or complete OAuth/MCP certification.
The resource request in the Main test uses a test client. The Rust test uses the existing transport seam.
Compatibility still permits an omitted `token_type`, form token responses, and partial legacy registration metadata.
This slice does not prove `client_secret_basic`, private-key JWT, registration management, provider consent, discovery interoperability, or concurrent-tab logout.

## Remaining gates

### Open verification

- Prove one configured remote MCP DCR flow through the browser and Rust resume.
  Local public and confidential DCR grant components pass. They do not close the browser-to-Rust gate.
- Prove one stored SharePoint or OpenAPI delegated flow through both grants.
- Prove the configured authorization-code and refresh grants through the deployed dialog.
  The verified dialog and Skip path do not prove either grant.
- Extend the deployed authorization-tool proof to a real provider grant.
  Local tests cover operation unlocking and repeated parallel Skip calls. Browser proofs currently cover Skip.
- Prove logout and concurrent-tab behavior against the replatform stack.
  A second tab on conversation 533 temporarily retained active guard controls after the deciding tab completed.
  The controls later cleared; immediate collaborator synchronization is not proven.
- Repeat the earlier conversation 529 scenario if it recurs with complete child results.
  The latest direct-agent and pipeline proofs complete after one Skip without a repeated guard.
- Publish both proxy schemas in Main's OpenAPI document.
- Add load and Kubernetes evidence before production capability registration.
