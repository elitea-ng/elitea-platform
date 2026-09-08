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

### Session reuse across execution requests

The current UI baseline is checked at revision `8fc59c63a5409060db4208454f87bd9c620ff56d`.
`src/common/messagePayloadUtils.js` includes session token maps for ordinary and application messages.
`mcpAuth.helpers.js` retains credential-scoped tokens until expiry or toolkit logout.
`mcpAuthFlow.helpers.js` keeps DCR-issued credentials separate from stored toolkit credentials during refresh.
The replatform preserves these outcomes without copying the background timer design.

Three boundaries previously prevented reuse:

- The UI omitted session tokens from new turns and sent an empty map during regeneration.
- Main rejected non-empty session token maps on both routes.
- Rust treated any non-empty token map as an interrupt continuation.

| Current-platform evidence | Replatform source | Behavior |
| --- | --- | --- |
| `messagePayloadUtils.js` | UI `executionTokens.ts`, `useChatBoxSend.ts`, and `useChatBoxSend.helpers.ts` | Await refresh before submission. Include only current-project access tokens and session identifiers. |
| Browser token lifecycle | UI `refreshOwnership.ts` and `tokenLifecycle.ts` | Share one active refresh grant per credential key within a tab. Preserve rotation. Reject stale refresh writes after logout or later authorization. |
| Toolkit logout and cross-tab markers | UI `storage.ts` and `logoutSync.ts` | Keep the token-store logout operation. A remote logout marker invalidates an in-flight refresh result. The new OpenAPI toolkit form still lacks its logout control. |
| Current message input contract | Main `agentexecution/route.go`, `start.go`, `adhoc.go`, `regenerate.go`, and `mcp_tokens.go` | Accept bounded token objects after route authorization. Place them in the existing encrypted execution input, not chat history or Redis fields. |
| SDK session-token input | Rust `agents/runtime.rs` and `agents/assembly.rs` | Distinguish session credentials from explicit resume actions. Permit reuse on fresh turns and regeneration. Keep output-continuation authority closed. |

Toolkit admission and credential matching remain mandatory before resource calls.
The UI does not send refresh tokens or client secrets in execution requests.
The shared tab refresh owner prevents duplicate rotation within that tab.
It does not prove concurrent refresh coordination between separate browser tabs.

The isolated fixture is documented in `deploy/oauth-emulator/README.md`.
Its stored OpenAPI mode supports delegated authorization and client credentials, not DCR.
Its separate MCP modes support public and confidential DCR protocol tests.

The initial browser proof uses rehearsal toolkit 27 and chat 535.
After consent, execution `5909dfba363e061df56ce8557c0ff2be` calls the protected echo operation and settles.
The result contains marker `RUST_OAUTH_FIXED_20260907` and grant generation 1.
This proves the initial code grant and protected resource call.

The fixture initially returned `invalid_grant` after consent.
Chrome blocked the callback redirect because the form policy allowed only the fixture origin.
The corrected policy includes only the configured callback origin.
The fixture test pins this behavior without weakening grant validation.

Verification for session reuse includes 339 UI tests and the focused Main route and application suites under the race detector.
Rust passes 276 agent tests and strict all-target Clippy.
The tests cover fresh and regenerated agents and pipelines, exact resume separation, rotation, project filtering, and logout races.
Direct-node authorization sequencing requires separate deployed evidence.

### Ordinary turns after a completed guard

The first ordinary turn after a completed guard exposed another history defect.
ADK persists both the original tool call and its replay, but only one result.
The previous projection normalized explicit resume requests, not later ordinary turns.
A regression test reproduced two copies of one call and one result in the next provider request.

`agents/replay_history.rs::provider_model` now applies the same projection at the shared provider boundary.
`agents/session.rs`, `agents/pipeline.rs`, and `agents/application_tools.rs` use this boundary.
Replay adapters remain outside it because they need the original pending batch.
Durable events stay unchanged. The projection does not execute tools or grant authorization.
Conflicting calls and duplicate results remain invalid.

The sensitive-tool test now checks the next ordinary turn after approval.
The delegated tests check the next ordinary turn after authorization for application and ad-hoc runs.
Each provider request retains one complete call/result pair per identifier.
The next turn uses its new user input and new tool-call identifiers.

### Deployed code grant, refresh, and first-turn reuse

The deployed browser uses the same Private-project conversation 535.
Execution `83d44878f068e732e42d8de12de8b3d6` reaches a new authorization guard after an earlier Skip.
The browser completes the emulator consent and authorization-code exchange with S256 PKCE.
Resume `3a55bf5830094a373af52244746c1a71` calls the protected resource and returns `RUST_OAUTH_AFTER_SKIP_E`.

The next ordinary turn runs as `de31106bb1393d2a1d762824bcc430ca`.
It returns `RUST_OAUTH_FIRST_TURN_F` with grant generation 2, without regeneration or another consent prompt.
The network sequence shows a successful refresh grant before the execution request.
That request contains only `access_token` and `session_id` in its credential entry.
It contains no refresh token or client secret.

Earlier executions `d8daf28888599921cccc0739f2570a5d` and `62d1bf826b4130148bd207e7ebc60ad7` prove regenerated and ordinary protected calls with refreshed credentials.
These are browser-to-Main-to-Rust emulator proofs, not real-provider interoperability or production activation.

Manual removal of this tab's test token exposes a separate stream failure before the next guard.
Executions `ef1981035ddc7172b345bc38eaffbaec` and `9171b068431fb977a8aa4426132642fd` fail after the gateway accepts the request.
Regeneration later reaches a guard. The token-removal failure remains under investigation.
The lifecycle now logs the static upstream error code without provider messages, bodies, or credentials.
The new UI still lacks the OpenAPI toolkit logout control present in the current application.
Manual token removal does not prove toolkit logout or cross-tab invalidation.

A later token-removal check runs as `1931d9ed98711e49e3a37dfb304151a8`.
It settles and displays a confirmation instruction as text, without an actionable authorization card.
This does not close the token-removal recovery gate.

### Token-removal recovery and historical control messages

Execution `119ca8f9999196ba1d48e6d66ecb91b4` identifies the rejected operation through the worker's static error code.
Its code is `model_gateway.tool_call`. The model selects a tool outside the current declaration list.
The facade rejects that call before tool dispatch. HTTP 200 from the gateway does not imply a valid model response.

The repair keeps this admission boundary intact.
Both model facades now report `model_facade.tool_not_admitted` for this exact failure.
`agents/replay_history/recovery.rs` permits one corrective model request before any semantic output leaves the facade.
It retains the current tool declarations and original user task.
It does not synthesize authorization, execute the rejected call, or retry other errors.
The extra request consumes the existing invocation model-turn budget.
Partial text, reasoning, tool calls, or terminal output disable repair to prevent duplicate output or dispatch.
Dropping the stream cancels owned work. No detached retry task exists.

ADK 2.2.0 `adk-runner/src/context.rs::conversation_history_for_agent_impl` includes saved confirmation text in later model history.
The regression reproduces this behavior after a completed sensitive-tool guard.
`agents/runner_history.rs` removes confirmation content from the Runner's loaded view, using the typed event action.
It preserves stored events, confirmation identities, actions, state, and branch metadata.
`agents/session.rs::RunnerSessionService` applies this view to ordinary, resumed, and pipeline Runners.

The current SDK `runtime/toolkits/tools.py::_build_deferred_mcp_auth_tools` scopes Skip to the current run.
Rust's Skip result now names that scope and returns `use_other_tools_or_report`.
It omits discovery URLs, challenges, client settings, and authorization context.
Those fields remain on the durable guard, where the browser needs them.
The authorization tool description tells the model to serve the current task without asking users to name internal tools.
The provider-history projection corrects only the exact retired Rust Skip directive; it does not rewrite stored records.

Tests cover later-turn guards after Skip, repeated same-run calls, exact call/result history, and minimal decision payloads.
Recovery tests cover one repair, repeated rejection, unrelated errors, partial output, and cancellation before and during repair.
The PostgreSQL-backed test run passes 883 library tests and 83 integration or contract tests, with no ignored tests.
Formatting and strict Clippy checks pass. The release image builds successfully.

### Deployed token-removal recovery

The rehearsal worker uses image configuration `sha256:85722825e1236f694807887484ea07c40a77f7a15c3da108aa830fc82c1e2df7`.
The test retains conversation 535 and its earlier failed and declined turns.
No test clears chat history, resets the emulator, or changes another toolkit's token.

| Browser action | Execution | Result |
| --- | --- | --- |
| Request the protected operation with no token | `6bea7bd26842fdb01bc3fb44b7df0772` | One actionable authorization guard. No protected dispatch before consent. |
| Complete stored-client consent | `c0cac6a01806b65642d48db3f2179d32` | Authorization resumes. `echo_marker` returns marker `RUST_OAUTH_RECOVERY_I`, mode `stored`, generation 1. |
| Submit the next ordinary request | `a3f3085aff33b942048c3faaa1defd01` | Refresh completes before dispatch. The protected call returns `RUST_OAUTH_REUSE_J`, generation 2, without another consent prompt. |
| Regenerate that request | `2c24be587e95c35ec126f6d68738656b` | HTTP 200. The protected call returns the same marker with generation 3. The answer survives reload. |
| Remove only the fixture token and submit a normal task | `5fea061dbeec625540b5d7002b1488ed` | The first attempt raises one enabled authorization guard, without regeneration. |
| Skip the current run | `432a910cb17596d84b73fcff09c862a9` | The minimal result contains `scope: current_run`, without authorization context or discovery settings. No protected call occurs. |
| Request the toolkit again in ordinary language | `681b485b33b37f8394821af5a6b84a94` | A new actionable guard appears. The user does not name the internal authorization tool. |
| Authorize that later request | `df3280b9c641a7f6490a27d61c4c5e6e` | The protected call returns `RUST_OAUTH_AFTER_SKIP_L`, mode `stored`, generation 1. |

The execution request after refresh carries the access token and session identifier, without refresh tokens or client secrets.
Database event metadata confirms authorization precedes protected dispatch and the regenerated turn executes `echo_marker`.
These live runs do not trigger the corrective model retry. Deterministic component tests prove that branch.
An earlier marker-only answer does not count as tool execution proof.
This closes the reproduced between-turn token-removal path, not mid-stream token expiry, real-provider interoperability, or cross-tab logout.
DCR browser verification remains a separate gate.

### Remote MCP catalogue contract

The DCR browser check finds no Remote MCP option in the create form.
Main's type catalogue contains eight built-in types, without `mcp`.
The web menu already supports this key and labels it Remote MCP.

SDK revision `da1d9e4db920170ba0f2765a4aede5c015f145a8` supplies the connection-field baseline in `runtime/toolkits/mcp.py::McpToolkit.toolkit_config_schema`.
Main `internal/api/v2/toolkits/handler.go` now supplies those connection fields and retains runtime discovery for operation schemas.
Only the server URL is required. DCR does not require a stored client identifier or secret.
Client secrets use the existing password field. Native execution still requires verified TLS and its existing credential admission rules.
The change does not activate effectful operations or remote servers by itself.

The catalogue tests fail before the change and pass after it.
The affected toolkit and agent-execution packages pass race tests and vet.
The deployed form creates Private-project MCP fixture 28 through the browser.
This creation result does not prove a successful DCR grant or Rust resume.

### Configured MCP consent scopes

The first launch of MCP fixture 28 fails as execution `1caea520377a2a7899f2b8c7e1f0ef66`.
Rust rejects its configured scope list as unowned credentials before the authorization guard.
The toolkit has no inline client identifier or secret.

`toolkits/mcp.rs` now validates scopes as bounded consent inputs, not credentials.
It accepts at most 64 scope tokens and 4,096 bytes in total.
Each token must use the OAuth scope-token character set.
The guard supplies these consent defaults and retains the discovered authorization-server metadata, including DCR registration.
Inline client identifiers and secrets remain rejected at this native configuration boundary.
Scope settings do not authorize a toolkit or unlock protected operations.

The scoped-guard test fails before the change and passes after it.
Negative cases reject malformed scope types, invalid characters, oversized lists, and unowned clients before connecting.
The PostgreSQL-backed suite passes 884 library tests and 83 integration or contract tests, with no ignored tests.
Strict all-target Clippy passes.

### DCR browser blockers after scope validation

The deployed worker uses image configuration `sha256:52bb266ac6d01fc0362ca73d2517f7997b92b6f75b71bad398b273806374467a`.
The deployment preserves the worker's environment, trust mounts, and durable state.
Fixture 28 retains its original URL, scopes, and empty operation selection.

Regeneration before reload sends temporary answer ID `1d6a7d9b-ec6d-473a-9002-be8ff74fd256` and receives HTTP 422.
The legacy fallback then receives HTTP 400.
The original admission returns persisted answer ID `b8e3b11f-a88b-5dde-a45a-cbf3b64777f7`.
Reload uses that persisted ID, and regeneration returns HTTP 200.
No data is cleared during this check.

`useChatStreamTransport.ts` now retains the accepted answer identity when recording an early failure.
`chatStreamSettle.ts` uses that identity instead of creating a temporary identifier.
The regression test fails before the change and passes after it.
All 634 focused chat and MCP UI tests pass. TypeScript checking and targeted lint also pass.

The deployed UI uses image configuration `sha256:9ce95e019b48df6ffcf47b808a7d0554f92ff7b302999750481a39e1cc3c1afd`.
A new ordinary turn fails as execution `bd630544a1e849a13ca1cccd194cd268` before any progress frame.
Without a page reload, Regenerate sends persisted answer ID `72248a56-12a6-56a4-9a62-d730b139f5cd` and receives HTTP 200.
Execution `84b9a66f4636b1f8d8dd03ac12085430` starts through the current route, without the legacy fallback.
The separate MCP bootstrap failure remains visible. This check proves regeneration identity, not successful DCR.

Execution `7c20139bc33f0adcbd9069ce8b1e50d4` reaches Rust and fails with `native_agent.authorization_failed` during toolset assembly.
The remote server requires authorization before discovery. The saved operation selection is empty.
At that revision, `materialize_mcp_toolsets` creates authorization placeholders only for known selected names.
Without those names, it returns the authorization error instead of creating a toolkit-level guard.
This identifies a runtime bootstrap gap, not a failed code grant.

The browser's MCP editor shows Load Tools, but clicking it sends no request.
The shared toolkit form accepts discovery callbacks, but `ConfigurationTab.tsx::formSlots` supplies no MCP discovery integration.
That launcher gap also remains open.
No DCR registration, consent, token exchange, or protected resource call occurs in these checks.

Additional direct-node cases remain explicit:

- Reuse a valid toolkit authorization from a prior node without another guard.
- Preserve separate interrupt identities when parallel branches reach an unauthorized toolkit before either decision completes.
- Resume the owning child after each decision. Do not wake the parent before its required children complete.
- Keep authorization and Skip scoped to the bound toolkit and run contract.

### Authorization before remote operation discovery

The current-platform baseline is `elitea-sdk/elitea_sdk/runtime/toolkits/tools.py::_build_deferred_mcp_auth_tools`.
It supplies a toolkit authorization operation when remote discovery requires consent.
`runtime/toolkits/mcp.py` treats an empty selection as all discovered operations.
These contracts do not require invented remote operation names.

Rust `toolkits/delegated_auth.rs` now stores discovery requirements independently from protected operations.
`toolkits/delegated_auth/model_tools.rs` exposes one authorization proxy for each bound requirement.
`toolkits/mcp.rs` adds this requirement when remote discovery requires authorization and the selection is empty.
No protected operation is exposed before consent. An admitted token allows real discovery on rebuild.
Existing selection, exclusion, policy, and credential admission checks remain in place.

The regression first fails on the original unauthorized-discovery response and passes after the change.
Additional tests cover catalog merge, exact binding, current-run Skip, a fresh run, and internal-name collisions.
The rehearsal PostgreSQL suite passes 887 library tests and 83 integration or contract tests, with zero ignored tests.
Strict all-target Clippy passes.

Regeneration of the existing failed conversation starts execution `266b4de2b0c44bb5ce56728d6923f8de`.
It reaches a durable authorization pause at terminal sequence 15.
The browser presents the guard and successfully registers a public DCR client.
The first consent attempt then fails with `invalid_target` because the authorization URL has no resource audience.
This proves toolkit authorization before discovery. It does not yet prove a completed DCR grant or Rust resume.

### MCP OAuth resource audience

The [MCP authorization contract](https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization) requires the protected resource in authorization and token requests.
The current-platform OAuth helper does not supply this parameter. Its omission is not a compatibility requirement.

Web `features/mcps/lib/oauthFlow.ts` now carries the protected MCP URL separately from the browser storage key.
The consent URL and code exchange use this audience. Token metadata retains it for refresh.
`authModalHelpers.ts` supplies the actual MCP resource when a configuration-specific storage key differs.
Delegated OpenAPI and SharePoint flows retain their existing request shape.
Main `internal/api/v2/eliteacore/mcp_oauth_proxy.go` validates and forwards the optional resource for both grants.
It rejects malformed resources, user information, fragments, and disallowed plain HTTP before transport.

Regression tests fail before audience propagation and pass after it.
All 288 MCP UI tests pass, with TypeScript checking and targeted lint.
Main OAuth and DCR race tests pass.
The broader MCP lint invocation reports an existing unsafe access in `useMcpTokenChange.test.tsx`; the edited files pass.
### Public DCR browser grant and refresh

The scoped rehearsal deployment preserves all original environment values and mounts.
It does not reset the emulator, conversation history, or durable runtime state.
The image configuration identifiers are:

- Rust worker: `sha256:6cc009ec1784c624d81f53e99d4df9a5667dfd80dc876c1d086dfe91a33668e5`.
- Main: `sha256:4494adf2e3c8a822dec57f9ef127a7570445a1b99fc51441f6c0ceab1a9b4e73`.
- Web: `sha256:ced2f7f2574441f3c2fc4d2b0b28ad94bc0c2bbfdc2eb0653e5de27420e947ea`.

Conversation 536 reloads its saved authorization guard after Main and worker replacement.
The browser registers the public client, presents consent with the MCP resource audience, and exchanges the code.
Execution `be8b2e98f68306fd1fc4291b9f07decb` resumes the pending authorization call.
Rust discovers the real operations and returns the fixture marker with mode `dcr-public` and generation 1.
The run settles, acknowledges terminal sequence 36, and retires its delivery.

A later ordinary task refreshes the token without another registration or consent prompt.
Execution `7c0135406ab05484f01a6094f2249a23` returns the new marker with mode `dcr-public` and generation 2.
It also settles and retires its delivery.
This closes the public DCR browser-to-Rust and between-turn refresh proof for the local emulator.
It does not prove a real provider, the confidential variant, active-run expiry, or editor discovery.

DCR registers the client automatically; it does not grant user access by itself.
The authorization-code flow still uses browser consent. Later valid-token use and refresh do not require another consent prompt.
Client credentials is a separate application-access grant, not a shortcut around delegated user authorization.

### Confidential DCR browser grant

The browser creates Private-project MCP fixture 29 without an inline client identifier or secret.
Conversation 537 uses this fixture on its first turn.
Execution `babf91ac84c1d030667e30188b90b095` pauses for toolkit authorization before operation discovery.
The browser registers the confidential client and completes user consent and the code exchange.
The token endpoint accepts the registration-issued client credentials.
Execution `c4c2fa2c0150eb1a78ac753c83ccc36a` resumes the pending call and returns the marker with mode `dcr-secret` and generation 1.
It settles, acknowledges terminal sequence 41, and retires its delivery.
No protected call occurs before consent. No operator-supplied client secret is added to the toolkit.
The next ordinary task refreshes the confidential client token without another consent prompt.
Execution `da5679e973c46ee972d805a4a81cd6b7` returns the new marker with mode `dcr-secret` and generation 2.
It settles and retires its delivery. Real-provider checks remain open.

### Protected MCP discovery from the create and edit pages

The current-platform business evidence is Indexer's
`methods/indexer_mcp_sync_tools.py`: it uses the exact resource token or a prebuilt
toolkit alias and returns `requires_authorization` with consent metadata.
Current UI `features/toolkits/ui/form/ToolBase/ToolActionsSelector.jsx` connects
Load Tools to the discovery hook and authorization modal.

Main `internal/mcpregistry/authorization.go` preserves a bounded Bearer challenge
without retaining the provider response body. `eliteacore/mcp_sync_auth.go`
resolves public protected-resource and authorization-server metadata, validates
the exact resource and issuer, and projects only consent fields. Metadata reads
receive no invocation credentials or custom headers. Existing proxy validation
rejects cross-origin redirects; TLS verification remains enabled. Discovery
uses only the exact bound access token or an admitted prebuilt alias, never a
refresh token. The existing request-body bound also applies to tool discovery.

Web `pages/toolkits/lib/useMcpDiscoverySlot.tsx` composes the existing MCP hook and
modal into both `CreateToolkit` and `EditToolkit`. No sideways feature import is
introduced. `ConfigurationTab` forwards the slot, and `ToolBase` displays the
discovered names even when the registry schema has an empty tool enum. Empty
selections initialize from discovery; existing nonempty selections are retained.
Discovered metadata and credentials are not saved as toolkit settings.

`useGetRemoteMcpTools.ts` refreshes session tokens before discovery, selects only
the bound resource, rejects a missing project, ignores responses for a changed
form, and retries once after successful authorization without a timer.
`useMcpAuthModal.ts` stores remote MCP grants by resource rather than issuer.
`discoveryMetadata.ts` retains the advertised token-endpoint authentication
methods so confidential DCR requirements reach the consent flow.

Verification on 2026-09-08:

- 307 focused MCP and toolkit-page tests pass, including both real page
  compositions, consent metadata, token isolation, refresh, stale responses,
  and the post-consent retry. The creation fixture explicitly supplies the
  real platform-settings route, avoiding a generated random visibility flag.
- Main's focused OAuth, DCR, discovery, and challenge-parser race tests pass.
  Negative cases cover resource/issuer mismatch, oversized bodies, invalid
  endpoints, cross-origin redirects, and credential-free metadata reads.
- TypeScript checking, targeted UI lint, Go vet, and the full UI container
  build pass. No dependency or database schema changes are needed.
- Public DCR discovery passes through both the edit page and a new, unsaved
  MCP form: automatic registration, explicit fixture consent, code exchange,
  automatic discovery retry, and the visible `echo_marker` operation.
- Confidential DCR editor discovery passes the same sequence on fixture 29
  without operator-entered client credentials.

The initial discovery-proof Main image configuration is
`sha256:88512b0b082e45a8d35664e72494afad33cd783e889f09f88e616c2d1018cdac`.
The final Web image configuration is
`sha256:ec20fe53674fb2b9dbc33f6e4c28b92d1cbbbad1e4631e90499610b8b8d5507f`.
Scoped replacement preserves environment values and original mounts. No
conversation, emulator state, or test toolkit is deleted by this slice.

### Refresh must not expand the authorized scope

The confidential editor's next Load Tools request exposed a Main defect:
`mcp_oauth_proxy` returned HTTP 400 with the safe error `invalid_scope`.
The browser omitted `scope` on refresh, but Main populated it from the toolkit's
configured defaults. Those included `offline_access`, while the completed consent
granted only `records.read`. The emulator rejected that scope expansion before
issuing a refreshed token. Registration and initial consent were not the failure.

[RFC 6749 section 6](https://www.rfc-editor.org/rfc/rfc6749#section-6) defines an
omitted refresh scope as the original grant's scope. Main
`mcp_oauth_proxy.go::resolveMCPOAuthCredentials` now leaves it absent on refresh,
including when stored client credentials must be resolved. An explicit scope is
forwarded unchanged; the authorization server still enforces its grant bound.
Code-grant defaults are unchanged. Four regression cases cover stored-client and
DCR refresh with omitted or explicit scope. Both omitted-scope cases fail before
the change; all four pass after it, along with the focused OAuth/discovery race
suite and vet.

The same browser document and confidential grant were retained for the deployed
refresh retest. After replacing only rehearsal Main, cancelling the unnecessary
consent prompt and pressing Load Tools returned HTTP 200 from both the OAuth
proxy and discovery endpoint. The response contained `echo_marker`. Emulator
confidential-client refresh count advanced from 1 to 2 and initialization/list
counts from 3 to 4; registration, consent, and code-exchange counts all remained
at 2. No new authorization was performed. Main reports healthy with deployed
image `sha256:3d85a6997df216be4951de1488f7fdac034cf66ce23c0a16579e3b529a95e453`.
This is a same-document refresh proof, not confidential-client reload recovery.

## Merged-stack verification: 2026-09-08

Main, web, gateway, and Rust now deploy source `0f789b57` together on migrated rehearsal database copies.
See [the integration record](main-sync-20260908.md#browser-proof-on-the-merged-deployment) for image identities, migration preservation, and execution correlation.

Playwright verifies public DCR registration, consent, code exchange, durable resume, and a real MCP result in the Private project.
After reload and access-token expiry, another turn refreshes the token without registration or consent.
Regeneration executes the tool again. A further reload preserves the regenerated result.
Emulator counters and committed execution settlements confirm these results.
This refreshes the public-client proof on the merged stack. It does not close confidential-client reload recovery or real-provider gates.

The merged UI hides an attached MCP despite enabled visibility settings.
The conversation API and Rust execution retain the attachment.
The chat page omits the participant panel's `isMcpVisible` prop, whose default is false.
This is a UI integration gap, not missing runtime toolkit binding.

## Remaining gates

### Open verification

- Wire the chat participant panel to the existing MCP visibility hook.
  Cover both enabled and disabled settings through the mounted page. Verify the saved MCP in the deployed browser.
- Repeat DCR against a real provider.
  Both variants now pass the local emulator grant, durable resume, tool call, and between-turn refresh proof.
  Editor discovery and toolkit authorization before operation names are known
  now pass local browser checks.
- Repeat stored delegated code and refresh grants against a real provider.
  The deployed OpenAPI emulator proves both grants and first-turn session reuse.
- Extend the deployed authorization-tool proof to a real provider grant.
  Local tests cover operation unlocking and repeated parallel Skip calls. Browser proofs cover Skip and the emulator's stored-client grant.
- Extend token-removal recovery to expiry or revocation during an active tool-calling run.
  The existing-conversation, between-turn removal path passes the deployed proof above.
  [OpenAPI direct-node recovery](delegated-auth-expiry.md) now has component proof.
  Agent loops, remote MCP, and deployed active-run recovery remain open.
- Prove logout and concurrent-tab behavior against the replatform stack.
  A second tab on conversation 533 temporarily retained active guard controls after the deciding tab completed.
  The controls later cleared; immediate collaborator synchronization is not proven.
- Close confidential DCR credential ownership across a document reload.
  `clientSecretVault.ts` intentionally retains issued client secrets only in
  memory. The same-document refresh proof does not demonstrate reload recovery;
  any durable solution must keep client secrets server-side, not put them back
  into browser storage or reuse an unrelated toolkit OAuth client.
- Review consent scope defaults: the editor currently prefers resource-advertised
  scopes over configured scopes. A configured `offline_access` scope is therefore
  not automatically selected when only `records.read` is advertised. The emulator
  issues refresh tokens in this case; that does not prove a real provider will.
- Repeat the earlier conversation 529 scenario if it recurs with complete child results.
  The latest direct-agent and pipeline proofs complete after one Skip without a repeated guard.
- Publish both proxy schemas in Main's OpenAPI document.
- Add load and Kubernetes evidence before production capability registration.

### Deferred diagnostic follow-up

Return to [OBS-RUST-01](agent-runtime.md#obs-rust-01-detailed-runtime-diagnostics) after the current functional compatibility gaps close.
It tracks richer errors, async span traces, stack backtraces, safe UI correlation, and bounded operator diagnostics.
The current static upstream error code does not complete that follow-up.
