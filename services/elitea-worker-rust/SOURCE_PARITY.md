# Rust worker source parity

- Status: capability-disabled production agent runtime with isolated end-to-end proof
- Last verified: 2026-09-04
- Production capability registration: disabled

The previous Rust worker implementation was never committed and no recoverable
Rust source exists in this repository's refs or Git objects. This directory is
the first durable reconstruction checkpoint. Historical reports of ADK-backed
execution, configuration or toolkit implementations, PostgreSQL checkpoints,
and their test counts are not implementation evidence.

The external contract remains the language-neutral protocol under
`libs/proto/elitea/runtime/v1`. ADK-Rust is an internal agent-runtime substrate;
its types must not replace the Elitea command, input, output, settlement, or
browser-event contracts.

## Reconstruction order

1. Establish the tracked Rust shell, strict protocol parsing, and conformance
   fixtures for both `agent.execute.adhoc.v1` and
   `agent.execute.application.v1`.
2. Establish the shared configuration/toolset kernel and source-derived family
   inventory required by agent execution.
3. Implement new-lineage agent execution using reviewed ADK-Rust primitives,
   including sessions, PostgreSQL checkpointing, progressive skills, MCP,
   HITL/continuation, graph parallel nodes, and bounded parallel subagents.
4. Migrate indexing capabilities after the agent track using the indexing
   capability parity matrix.

## Current source-to-Rust mapping

Detailed ledgers:

- [`docs/source-mapping/agent-runtime.md`](docs/source-mapping/agent-runtime.md)
- [`docs/source-mapping/pipeline-nodes.md`](docs/source-mapping/pipeline-nodes.md)
- [`docs/source-mapping/configuration-toolsets.md`](docs/source-mapping/configuration-toolsets.md)
- [`docs/source-mapping/internal-elitea-mcp.md`](docs/source-mapping/internal-elitea-mcp.md)
- [`docs/source-mapping/external-elitea-mcp.md`](docs/source-mapping/external-elitea-mcp.md)
- [`docs/source-mapping/delegated-oauth-dcr.md`](docs/source-mapping/delegated-oauth-dcr.md)
- [`docs/source-mapping/indexing.md`](docs/source-mapping/indexing.md)
- [`docs/adk-rust-2.0.0-audit.md`](docs/adk-rust-2.0.0-audit.md)

Current pipeline-Agent checkpoint: a direct saved participant now streams its
descendant ADK events under the exact pipeline Application call. Sensitive
descendants emit only their hierarchical confirmation cards; a separate
internal graph interrupt binds that complete card set to the pending Agent
node/checkpoint. Exact approve and same-call structured block replay are proven,
including two nested parallel leaves that reuse `call_mcp` without collision and
reject an incomplete decision set before dispatch. This uses the existing
SessionService, Checkpointer and application replay coordinator and adds no
interrupt table or browser-event graph channel.

Current pipeline-LLM checkpoint: a node-selected sensitive tool now uses native
ADK `require_tool_confirmation`, with one bounded private replay envelope joined
to the outer graph checkpoint. The public event remains the masked graph tool
card. Approval supplies the normal result; Reject and Block With Comment supply
the shared `sensitive_tool_blocked` value to the same native tool loop under the
original provider call ID. The regression proves zero real-tool dispatch and no
fresh provider planning turn on resume. A bounded invocation-local bridge now
projects the native node's model/tool start and completion events with
`langgraph_node` identity, keeps provider request payloads out of graph state,
and suppresses the duplicate graph terminal turn. Incremental tool-progress
chunks and approved-effect crash receipts remain explicit activation gates.

Current clarification checkpoint: SDK `ask_user` is a runtime-owned dynamic
toolset, not a configured external toolkit. Application/ad-hoc direct agents,
recursively saved direct agents and a pipeline LLM node may bind it only when
the frozen internal-tool selection and, for a node, exact YAML tool scope both
name it. Native ADK confirmation persists the original call. The browser sees
1-4 normalized questions and only the `answer` action; Main canonicalizes a
bounded object or string answer into the existing protobuf string value. Rust
then returns `User answered...` under the original tool-call ID, marks the
confirmation approved and continues without executing the placeholder,
injecting a user turn or replanning the call. Direct and pipeline checkpointed
E2E tests prove the same-call provider transcript. A nested-parallel E2E also
proves that two calls to the same saved child publish distinct hierarchy-bound
clarification cards, reject a partial decision set, and resume each exact child
with its own answer without replanning. Resume revalidates `ask_user` against
the rematerialized child's frozen internal-tool catalog.

Current delegated-authorization checkpoint: materializer-known remote MCP
challenges now use ADK's native confirmation on the original model-selected
tool in direct application/ad-hoc `LlmAgent` loops and pipeline LLM nodes. The
durable session or graph replay retains the provider call ID and raw arguments;
Authorize rematerializes the exact frozen server with Main's claim-fetched
token and replays without replanning, while Skip supplies the SDK-compatible
`mcp_auth_decision` result under that same call ID without dispatch. An
authorization approval deliberately does not approve a separate sensitive-tool
guard. No synthetic model turn, interrupt table, or extra model-facing control
call is introduced. Direct saved-agent descendants now use that same durable
confirmation as an ordinary `mcp_auth` guardrail: parallel cards retain exact
interrupt/call/hierarchy identity, Authorize rebuilds every named leaf with only
its exact-server token, and Skip returns `mcp_auth_decision` under each original
call ID with zero dispatch. Main now normalizes its exact single-card
authorization continuation into the already-versioned `hitl_decisions` JSON
field, preserving `interrupt_id`, the original `tool_call_id` and
`guardrail_type=mcp_auth`; the standalone Python worker accepts and forwards the
same shared command to the current SDK. Main retains that scalar route and now
also accepts one bounded exact complete authorization decision set, rehydrates
persisted call IDs, and consumes every sibling atomically. Incomplete sets do
not resume the worker. The Rust delivery owner now exposes each same-family pause card as
ordinary progress and binds one bounded ordered exact-ID/call/hierarchy
aggregate as the sole paused terminal after ADK EOS. Independent partial
sibling resume and mixed-guardrail terminal aggregation remain explicit gaps;
no new protobuf field was required. A pipeline Agent node now carries the same exact
parallel Authorize/Skip set through its graph checkpoint: the child hierarchy
remains public, partial sets fail before materialization, Skip closes each
original call with `mcp_auth_decision`, and mixed sensitive/auth leaves resume
atomically. The bounded inline OpenAPI dynamic family, including static secret
headers and schema-aware bounded response search, and the explicitly
selected eight-read delegated SharePoint Graph core now use this exact-resource
flow; their remaining OAuth/content capabilities are listed in the toolset
ledger.

Current OAuth/DCR proxy checkpoint: Main now implements the browser token and
RFC 7591 registration proxies used by the replatform UI. The token proxy
supports authorization-code and refresh grants, PKCE, JSON and form responses,
and the current `used_dcr` isolation rule. It resolves omitted credentials and
scope through one actor-visible toolkit and the existing claim-mode
configuration resolver. Stored SharePoint and OpenAPI nested settings are
supported. Main sends a stored secret only to a toolkit-bound endpoint. This
restriction closes the current platform's caller-selected endpoint exposure.
The UI keeps popup, discovery, token storage, refresh, and logout ownership.
Rust keeps durable authorization interrupts and claim-scoped token use. Live
remote-MCP and delegated-toolkit provider proofs remain gates.

Current provider-tool binding checkpoint: current SDK
`runtime/tools/tool_binding.py` and `runtime/tools/llm.py` retain toolkit
identity when multiple selected toolkits expose the same operation. Rust now
implements that business contract at the common model boundary instead of
deduplicating by bare name. It freezes each admitted toolset once, preserves
already-unique names, creates deterministic bounded toolkit-qualified aliases
only for collisions, and delegates the provider call to the exact original
tool without rewriting its implementation identity. Root application/ad-hoc
agents, recursively nested direct agents and pipeline LLM nodes share this
binding plan. Sensitive and delegated-authorization guards are keyed by the
provider-visible alias but retain exact toolkit identity and the original
provider call ID. Direct Toolkit/MCP graph nodes remain scoped calls and do not
need aliases. Component proofs call two same-named MCP tools in one provider
turn through both direct and pipeline LLM execution; property-style collision
matrices prove deterministic valid names, bounds and reserved runtime-tool
isolation.

Current dependency baseline: every direct and transitive ADK dependency now
resolves to `2.2.0`. The direct-pin upgrade is a model/Runner compatibility
slice, not an MCP prerequisite. The architect's preceding `2.1.0` baseline
already contained the important provider corrections. The Elitea adapters now
match its open tool-turn semantics for OpenAI-compatible and Anthropic calls,
apply the GPT-5.6 Chat Completions tool default, and reject explicit sampling
for current Anthropic models before network I/O. Provider-neutral reasoning
`none` disables native Anthropic thinking by omitting its thinking/effort
fields; Rust does not copy the Python SDK's truthy-string fallback to a 4096
token legacy budget. Provider catalogs, pricing and server-side refusal
fallback remain Main/Bifrost profile concerns rather than implicit worker
policy. The `2.1.0` to `2.2.0` delta adds Vertex Gemini RAG support, exposes
Runner session state through `ReadonlyContext`, and instruments the drained
agent stream; Elitea's transport and authority boundaries remain unchanged.
Full provider, tool-call, session and graph verification is required before
this checkpoint is pushed.

Current observability checkpoint: the worker emits redacted structured logs and
standard OTLP traces across command verification, NATS JetStream delivery, claim,
preparation, native ADK execution, model requests, session/checkpoint writes,
output, settlement and retirement. A valid W3C `traceparent` continues the
upstream trace. `ELITEA_RUST_LOG` and `ELITEA_RUST_TRACE` remain scoped to the
Worker crate and the Elitea libraries it hosts that emit events (`OWNED_TARGETS`
in `src/diagnostics.rs`), so dependency HTTP, SQL and provider payloads cannot be
enabled accidentally.
The panic hook omits panic payloads and full paths, but retains a sanitized
source filename and line for diagnosis. OTLP batching uses the Tokio runtime;
this prevents the exporter-thread reactor panic that previously aborted the
release process. Isolated execution `bc39746696860d52761e5a2470e83c47`
streamed nine events, persisted its reply, exported lifecycle/model/session
spans with `service.name=elitea-worker-rust`, and left `restart_count=0`.
The same stack also projected a UI-created OpenAI credential and model through
Main and Bifrost. Execution `c9b27286118d3e3f999297d97f4f94bd`
selected `gpt-4o-mini`, did not call the mock server, reached the allowlisted
OpenAI endpoint, received an unauthorized provider response, published the
durable browser event `execution.failed`, retired the delivery, and kept the
worker at `restart_count=0`. Credentialed proofs then used the current local
Elitea proxy as an OpenAI-compatible provider. The stronger execution
`868af70730dc53444aeb27b7343f0f51` selected
`eu.anthropic.claude-sonnet-4-6` with `openai_compatible=true`, streamed a
unique provider marker, persisted the assistant reply, retired the delivery,
and left the worker and gateway at `restart_count=0`.
The proof uses the existing `open_ai` credential type. A credential whose
`api_base` names OpenAI stays on Bifrost's native OpenAI provider; a linked
`open_ai` credential with another base routes through the per-key compatible
provider. The resolver reads only the non-secret `api_base`, while the account
still resolves the key per request. The gateway removes one trailing `/v1`
before handing the compatible key to Bifrost because Bifrost adds that segment
itself. Both `/llm` and `/llm/v1` configuration forms therefore reach the same
operations. The standalone smoke reader uses the SSE `event:` name for durable
failures instead of waiting for a `data.type` field that failure frames do not
carry.
Prompts, arguments, results, URLs, credentials, tokens and provider bodies are
never trace or log fields. Backend retention, sampling, metrics and Kubernetes
activation remain deployment gates.

Current configured-tool checkpoint: the Private-project UI created an OpenAPI
credential and the dynamic `rust_openapi_echo` toolkit. Execution
`f25b5e2b95e68575c5656dd9fa577a43` gave the Rust `LlmAgent` one selected
operation. The model called `echo_marker` with the exact marker. Rust executed
the Postman Echo request, returned the result under the provider call ID, ran a
second model turn, reached EOS and retired the delivery. Reloaded history
contained exactly `RUST_OPENAPI_TOOL_E2E_20260828_B`. No mock tool result was
used. Main initially rejected this run because restored policy allowed 16
outstanding requests while rehearsal configuration requested 64. The local
deployment was aligned to 16 without weakening Main's safety check or changing
the restored database.

Existing-chat participant controls now follow the current UI reference.
Toolkit and MCP rows remain open and use checked, pending add/remove switches.
The live rehearsal produced one 204 participant deletion and one 200 addition
for the same toolkit. Agent and pipeline selection uses the available catalog
and activates the selected participant. New-conversation staging and current
UI default-model assignment remain separate parity gaps. The generic OpenAPI
additional-header editor is also absent from the replatform UI. These gaps do
not invalidate the completed configured-tool runtime proof.

Current external Elitea-as-MCP toolkit checkpoint: Main's project-wide and
resource-scoped MCP catalogues continue to publish only `elitea_tools` rows
with `meta.mcp_options.available_by_mcp=true` and only their selected
operations. The descriptor privately pins the exact row and source operation.
Both catalogue scopes apply the current actor-specific `no_access` folder
overlay when the optional social access projection exists. A missing override
table keeps project RBAC behavior. Broken override dependencies fail closed.
On `tools/call`, Main authorizes the caller with the current chat-run
permission, re-reads the current row through the generated tenant repository,
rechecks actor folder access, opt-in, selection and guardrail policy, and
freezes one immutable claim. A signed `toolkit.execute.read.v1` command travels through the existing
Redis claim/fence lifecycle; Rust redeems configuration only after claim,
requires the exact materialized tool to declare itself read-only with no
missing user scope, executes it once, and publishes one typed bounded terminal
result.
Main validates the generation plus toolkit type/name/operation before returning
an MCP result. Thus a catalogue/call race, stale selection, mismatched result,
or missing runtime seam fails closed with a non-empty redacted error. The
dynamic-schema catalogue read remains the bounded current-SQLAlchemy
translation because a tenant schema identifier cannot be a sqlc value; all new
durable execution SQL and the exact current-row re-read use generated sqlc
repositories. A local external client proved list, one OpenAPI call, durable
settlement, exact result return, and hot opt-out without a restart. Agent and
pipeline UI controls now use the existing `mcp` version tag. Effectful or
sensitive toolkit operations, live OAuth/DCR, broader external clients, load,
and Kubernetes proof remain gates.

Current saved-pipeline external-MCP checkpoint: execution
`b13c0d177a1da5f35b6c689e88ed8661` invokes `Full Name Resolver` through the
saved `Resolve Name` pipeline. The compiler normalizes its legacy `Agent 1`
identifier in memory. Public hierarchy labels use application names. The two
leaf agents persist separate authorization pauses, and the graph binds both
interrupt IDs. Main returns the pause; the outer job settles and releases its
claim. Live authorization resume and final completion remain gates. The
[pipeline ledger](docs/source-mapping/pipeline-nodes.md) records the evidence.

Current internal Elitea MCP project-context checkpoint: Main publishes the
current platform's exact GET, PUT, and DELETE builder operations under
`elitea_core/project_context`, with their exact view/edit permissions and tool
names. Internal MCP and REST/UI reuse one handler. The URL project is
authoritative; PUT forwards only content, enabled, and activation description.
The five-field response, enabled-by-default absence semantics, Unicode bounds,
activation-description preserve/remove behavior, deterministic system
configuration identity, and DELETE lifecycle are covered by contract tests and
an isolated PostgreSQL proof. This slice builds stored project context; runtime
progressive-disclosure delivery and model-backed draft generation remain
separate gates. Rust continues to consume this Main-owned surface through its
existing configured HTTP MCP client.

Current internal Elitea MCP secrets checkpoint: Main publishes the current
platform's exact list, create, and update MCP opt-ins under `secrets`, with
their exact permissions and generated tool names. Plaintext read, delete,
hide, administration-vault, and bulk operations remain absent. Internal MCP,
REST/UI, and prebuilt-MCP credential materialization reuse one encrypted-vault
handler; the URL project and authenticated actor are authoritative, update is
value rotation rather than rename, and tool results never contain supplied
secret values. Contract, fuzz, composition, and isolated PostgreSQL lifecycle
tests cover the path. Main's existing lack of default-secret metadata and
conditional default-name suppression remains a shared REST/MCP parity gate.

Current output-continuation checkpoint: OpenAI-compatible model `Auto` preserves
the UI's `max_tokens=-1` sentinel through Main and omits the provider wire limit;
native Anthropic resolves its required limit from the frozen configuration.
An explicitly bounded root response persists its partial text and exact
`output_limit_sequence`, survives reload, and resumes the same response through
`agent.continue.output-limit.v1` with the same conversation, message, question,
thread, generation, and restored ADK session. Main reconstructs ad-hoc model
settings from the selected dummy/model participant rather than the user
participant. Rust removes one meaningful overlap and restores a missing
alphabetic separator at the continuation seam before stream and terminal
projection. Local Private-project UI proof covered a 700-token pause, reload,
one 200 continuation, one start/end marker pair, a clean `with external systems`
seam, durable completion, and a fresh exact follow-up. Root continuation remains
user-triggered; automatic bounded continuation and its future project controls
remain a separate policy slice, as do pipeline/nested automatic continuation
and crash recovery between suffix persistence and graph advancement.

Current regeneration checkpoint: the typed Main route admits
`agent.regenerate.v1` with the original response identity and a new execution
generation. Rust treats `is_regenerate` as a distinct direct-agent start mode,
not as a fresh append or interrupt resume. Under the current claim and session
writer fence, it deletes and recreates only the exact ADK session, seeds Main's
history snapshot from before the replaced response, and runs the original user
turn once. A Private-project UI proof regenerated a bounded response, continued
its 500-token suffix, retained one replacement branch after reload, and removed
the earlier failed response. The resulting agentstate session contains only the
replacement user/model pairs. Saved-pipeline regeneration still requires
coordinated SessionService and Checkpointer rewind semantics.

Current CI checkpoint: `.github/workflows/ci-rust.yml` uses the pinned Rust
toolchain for formatting, Clippy, rustdoc, tests, and a release build.
PostgreSQL 18 activates both durable state component tests. The workflow needs
no provider credentials and does not report a synthetic race-testing claim.

This checkpoint supersedes older broad table wording below that lists
parallel/nested authorization or pipeline-Agent authorization as a generic
open gate. The remaining authorization gates are independent partial sibling
resume, mixed-guardrail aggregation,
live platform OAuth/DCR proof, runtime-discovered post-token model-loop challenges,
remaining SharePoint content/app-only operations,
remaining OpenAPI remote-specification, binary/artifact and production
activation capabilities.

Explicit pipeline parallel-node design checkpoint: the existing
`graph/{yaml,parallel}.rs` code remains a disconnected prototype. The proposed
V1 in `docs/parallel-pipeline-node-design.md` runs two to sixteen owned Agent
nodes with at most eight active branches. Each branch uses the existing saved
Application runtime and one claim-fenced child graph checkpoint. Paused branches
aggregate through the standard outer graph interrupt and require one complete
decision set. The design adds no interrupt table, does not use
`PARKED_CHILDREN`, and does not infer tasks from raw chat. Model-selected
Application fan-out remains a separate current ad-hoc runtime contract. UI,
compiler, PostgreSQL crash, reclaim, tool-call, sensitive-tool, authorization,
and live chat proofs remain activation gates.

Data-driven map/reduce is a separate proposed node. It reads one state list,
repeats one owned worker, and reduces typed outputs in input order.

The map contract mirrors LangGraph `Send`. An explicit upstream LLM can produce
the list from one chat request before item admission starts.

`docs/map-reduce-pipeline-node-design.md` defines the item dispatcher, state
reducer ownership, item checkpoints, aggregate interrupts, bounds, and gates.

| Source evidence | Observable responsibility | Rust target | Proof | Status |
| --- | --- | --- | --- | --- |
| Python worker `config.py::{RuntimeDeployConfig,RuntimeLimits,load_deploy_config,read_regular_file,validate_private_directory}`, `security.py::{RuntimeTrustMaterial,ExactEd25519PublicKeyResolver,_load_redis_password,load_ed25519_keyring}` and `serve.py::{serve_from_config,_serve_deployment_inner}` | Admit one credential-free deployment document, own exact trust and per-generation Redis credentials, construct private-plane clients plus `agentstate`, then compose one application/ad-hoc delivery runtime | `src/{lib,main,config,security,bootstrap}.rs`, `src/transport/nats_jetstream.rs`, `src/execution/production.rs` | Strict config/origin/URL/related-limit and file/symlink/permission unit corpus; exact/duplicate/malformed keyring, Redis profile/error taxonomy, explicit/missing/malformed policy snapshot, CLI and production-build error corpus; live NATS command-bus proof against the chart's permission table passes (`execution/nats_live_tests.rs`); live mTLS/PostgreSQL/process proof remains | Partial capability-disabled executable composition: TLS 1.3 identity, exact-CA roots, command keys and zeroizing secrets feed concrete control/output/content/runtime-context/model channels, a bounded shared PostgreSQL pool and a NATS JetStream consumer bound to the bootstrap-created stream, durable and dead-letter bucket (refusing absence or drift), whose mTLS identity is re-read on every handshake. `serve` preserves the shared Python-compatible deployment document and separately requires one bounded regular-file snapshot containing the current runtime `toolkit_security` dictionary; missing or misspelled policy fields fail closed while an explicit empty dictionary is valid. The multi-thread runtime installs SIGINT/SIGTERM ownership before dependency composition, emits data-free lifecycle/error fields, stops intake, and applies one process-wide shutdown timeout across command delivery and native-agent drain. Capability registration, container/orchestrator projection of the runtime dictionary, atomic policy refresh and live cross-process proof remain gates |
| `libs/proto/elitea/runtime/v1/*.proto` | Language-neutral worker command, input, output, control, and settlement contracts | `build.rs`, `src/protocol/`, `src/transport/` | Generated client compile plus Python-produced command, input, NodeEvent and output-frame binary fixtures; ACK/replay state corpus | Partial: strict agent protocol and one ordered output-session attempt are implemented; control transport, reconnect ownership and settlement are not |
| `services/elitea-worker-python/src/elitea_worker/protocol/codec.py` | Verify exact signed bytes before closed-wire command decode and validate the selected capability | `src/protocol/{command,wire}.rs` | `tests/agent_command_contract.rs` HMAC/Ed25519 vectors and authenticated mutation corpus | Partial: application/ad-hoc command admission is implemented; other capabilities and offline execution envelope are not |
| Python worker `handlers/validation.py` (`ConfigurationValidationHandler`, `_map_errors`), `agents/{sdk_adapter,configuration_registry}.py` (`validate`, `schema`), `execution/delivery.py::ConfigurationValidationDeliveryProcessor`, `protocol/codec.py::{validation_request_from,_validation_result_message}`; pinned SDK `b5113a1` `configurations/*.py` (32 registered pydantic models); Main `runtimecomposition/current_sdk_configuration_catalog_snapshot.json`, `domain/configurations/validation.go` | `configuration.validate.v1`: bind the signed command to the admitted catalog and the selected type's schema digest, then answer whether the stored settings satisfy that type's SDK model. It is a schema verdict (pydantic `model_validate`), never a connection test; the result carries only Main's six-code issue vocabulary with fixed messages, sorted by pointer then code, capped at 64 | `src/validation/{mod,lax,strict_json}.rs`, `src/validation/configuration_rules.json` (generated), `src/execution/configuration_validation.rs`, `ToolkitCommandKind::ConfigurationValidate` in `src/protocol/{command,wire,control,output}.rs`, `src/execution/toolkit_delivery_processor.rs` | The Python worker's cross-language golden vectors (`testdata/proto/runtime/v1/configuration-validation/{valid,invalid,unsupported}`) reproduce byte for byte, success and cancellation (`src/execution/configuration_validation.rs`); `src/validation/tests.rs` replays 1,300+ recorded pydantic 2.12 outcomes (`tests/fixtures/configuration_validation_cases.json`, regenerated by `tests/fixtures/generate_configuration_validation_rules.py`, which refuses to write unless Main's catalog and per-type schema digests reproduce) over all 32 types, plus binding, settings-bound, issue-vocabulary and unsupported-type tests; `shared_toolkit_tests.rs` covers command, claim, result frame, restore and a signed-command round trip for every type | Complete for the 32 types Main routes to the worker (`ado`, `aha`, `azure_search`, `bigquery`, `bitbucket`, `carrier`, `confluence`, `delta_lake`, `figma`, `github`, `gitlab`, `google_places`, `jira`, `langfuse`, `openapi`, `pgvector`, `postman`, `qtest`, `rally`, `report_portal`, `salesforce`, `service_now`, `sharepoint`, `slack`, `sonar`, `sql`, `testio`, `testrail`, `xray`, `zephyr`, `zephyr_enterprise`, `zephyr_essential`). A type outside that table is refused with the registered `UNSUPPORTED_CAPABILITY` failure and its name in the worker log; it is never answered as valid. Validation takes no `AuthorizeInvocation`, so a worker lost mid-validation is simply re-claimed. A VALIDATE-bound Rust worker still opens the agent-state database (`bootstrap.rs`). Live cross-process proof against Main remains a gate |
| Python worker `handlers/indexing.py::IndexIngestDeliveryProcessor`, `execution/delivery.py::_accepted_index_claim`, `protocol/indexing.py`; pinned SDK `b5113a1` `tools/base_indexer_toolkit.py::index_data`, `tools/code_indexer_toolkit.py`, `tools/chunkers/**`, `tools/vector_adapters/**`; Main `application/indexing/{admission,dispatch,resolver}.go`, `application/indexregistry/**`, `infra/db/repos/index_ingest_results.go`, migrations 0162/0164 | `index.ingest.v1` (ADR-0030 I1): one toolkit index run. The claim's bundle names the toolkit, the `index_data` arguments and the embedding model; the run lists the source (`elitea-connectors`: `github`, `gitlab`, `bitbucket`, `ado_repos`, `artifact`; I2: `confluence`, `jira`, see "ADR-0030 I2 Atlassian families" below), diffs it against what `elitea-vector` holds for the registry index's namespace (`ListDocuments`), fetches, extracts and chunks the new and changed documents with the SDK's chunker defaults (`elitea-doc-extract`, `elitea-doc-chunk`), embeds them through the `/llm` gateway on the model facade's channel, writes the points and deletes vanished documents. It reports a typed `IndexIngestSummaryV1` (counts, skips by closed reason, the embedding stamp, a bounded per-document record and the whole index's totals); elitea-main is the only writer of index metadata | `src/indexing/{mod,inputs,source,pipeline,slug}.rs`, `src/transport/embeddings_facade.rs`, `ToolkitCommandKind::IndexIngest` in `src/protocol/{command,wire,control,output}.rs`, `RuntimeFailureKind::ResultRefused` and `OutputProtocolError::ResultRefused`, `src/execution/toolkit_delivery_processor.rs::execute_index_ingest`, `vector_target` and `index_egress_allowlist` in `src/config.rs` | `src/indexing/tests.rs` runs the pipeline against the in-process `elitea-vector` fake, a scripted embeddings gateway and a fixture source (first run, incremental run with unchanged/changed/vanished documents, clean run, the dimension fence before any write, a model mismatch before listing, a gateway refusal, Stop, the whitelist/blacklist and `needs_model` skip, the bounded record); `shared_toolkit_tests.rs` covers the command, the claim's entry rules, the result frame's bindings and Main's bounds, the restore, and a FAILED receipt for a SUCCEEDED proposal; `services/elitea-vector/tests/isolation.rs` proves `ListDocuments` against a real Qdrant | I1 of ADR-0030. Before any point is written the run fails on an embedding model or dimension that differs from the command's expected space; a `RESULT_REFUSED` output rejection (Main's registry refusing a result) is answered with a FAILED terminal. Deviations: the registry list route orders `created_at DESC, index_id DESC` (stable, served by `index_registry_list_idx`), a deliberate difference from Python `index_meta`'s `updated_on DESC, id ASC`; the Indexes tab keeps its selection on the index the user acted on and no longer depends on list order; PDF, Office and e-mail documents are read by the `elitea-doc-extractor` sidecar over a Unix socket (`extractor_socket`, `src/transport/extractor_client.rs`; its own crate and lock because `xberg-native-pdf` needs `smallvec ^1.16`, which `kube-client` 4.2.0 caps `< 1.16`), with typed skips (`encrypted`, `too_many_pages`, `extract_timeout`, `extract_failed`) and `unsupported_type` when no sidecar is configured; progress `NodeEventV1` frames are not published by the index run yet (the registry row and the terminal carry the state); `clean_index` deletes the namespace once the embedding space is proven rather than publishing a generation; agent-called `index_data` is not served inline (an index run is admitted through elitea-main); the other five index tools are served inside the I1 families under the Rust indexing runtime (next row); I2/I3 families are refused with their name |
| SDK `1b335d5` (0.9.68) `tools/sharepoint/api_wrapper.py` (`SharepointApiWrapper._base_loader/_extend_data/_index_tool_params`), `tools/ado/wiki/ado_wrapper.py` (`_iter_wiki_pages`, `_process_document`, `_index_tool_params`), `tools/ado/work_item/ado_wrapper.py` (`_base_loader`, `_process_document`), `tools/ado/test_plan/test_plan_wrapper.py` (`_base_loader`), `tools/non_code_indexer_toolkit.py` | `index.ingest.v1` for the I2 Microsoft families (ADR-0030 I2): `sharepoint`, `ado_wiki`, `ado_boards`, `ado_plans`. Each is a `ContentSource` in `elitea-connectors` that lists everything (continuation tokens, `@odata.nextLink`, WIQL id windows; no page cap), versions documents by what the source gives cheapest and exact, lists dependents (attachments, comments) as documents with a `parent_key` that carry the parent's version, and downloads attachments under the document cap. The family-specific `index_data` arguments reach the connector as the raw argument map | `libs/rust/connectors/src/sharepoint/{config,graph,source}.rs`, `libs/rust/connectors/src/ado/{wiki_source,boards_source,plans_source,text}.rs`, `libs/rust/connectors/src/source/glob.rs`, `src/indexing/source.rs` (the four arms, `SERVED_TOOLKIT_TYPES`), `IndexParameters::arguments` in `src/indexing/inputs.rs`, `parent_id` on points in `src/indexing/pipeline.rs`; `indexcatalog.Families` (Go) and `INDEX_FAMILIES` (agent-runtime) name the four so the index tools are served inside them | The connectors' own suites (recorded fixtures: pagination/continuation, attachments, dependents, 401/403, 429 with `Retry-After`, the SharePoint token renewal and content redirect); `src/indexing/microsoft_family_tests.rs` runs each family through the pipeline against the `elitea-vector` fake (Verification (2): unchanged source writes nothing; one changed document rewrites only its chunks; a removed document's chunks go; a work item field edit re-indexes only the work item document, a new or edited comment only the discussion) | I2 (Microsoft half). Deliberate differences from the SDK, see "ADR-0030 I2: Microsoft families" below |
| Pinned SDK `b5113a1` `tools/base_indexer_toolkit.py::{search_index,stepback_search_index,stepback_summary_index,list_indexes,remove_index}`, `tools/index_params.py`; Main `application/indexregistry/catalog.go`, `domain/indexcatalog`, the two freeze paths that serve index tools (`agentexecution/tools.go`, `toolkitcalltool/resolver.go`); `transport/runtimegrpc/control/remove_index.go`, `application/indexregistry/claim_remove.go` | The five index tools inside the I1 indexing families (ADR-0030 I1): under `ELITEA_INDEXING_RUNTIME=rust` elitea-main freezes the toolkit's `index_registry` rows into the toolkit object as `indexes` (name, namespace UUID, embedding model and dimension); the worker composes the family's toolset with the index tools, which act under the claim's `elitea-vector` token, embed the query through the `/llm` gateway with the index's model and format results as the SDK does (`elitea-vector-index`). The step-back pair call a second binding of the agent's model (non-streaming) in a turn and refuse with a sentence in `toolkit.call_tool.v1`, which binds no model. `remove_index` asks elitea-main over the claim's control plane (`RuntimeControlService.RemoveIndex`): Main checks the run's principal holds the index delete permission, tombstones the registry row and deletes the vectors through its own path or sweeper; the worker never deletes vectors itself | `libs/rust/agent-runtime/src/toolkits/families/index_tools.rs` (names, SDK schemas, `selected_tools`, `CompositeToolset`), `snapshot.rs::indexes`, `materialize.rs::index_toolset_for` (an index-only `selected_tools` is admitted only where the index toolset is composed); `src/indexing/tools.rs::ClaimIndexToolHost` (`QueryEmbedder` over the embeddings facade, `StepbackModel` over the bound ADK model, `remove_index` over `protocol/control.rs::IndexRemovalControl`); `agents/{ordinary,pipeline}.rs`, `toolkits/direct_runtime.rs`, `execution/toolkit_delivery_processor.rs` lend the authority | `agent-runtime/toolkits/index_tools_tests.rs` (SDK schema gate for every I1 family, selection, catalog, the three modes: no catalog, catalog without a store, catalog with a store); `src/indexing/tools_tests.rs` runs the five tools against the `elitea-vector` fake (remove_index against a recording control plane, the store untouched; the non-streaming step-back call) and proves the direct runtime serves them only under Rust indexing, including an artifact toolkit's index tools; Main: `control/remove_index_test.go`, `indexregistry/claim_remove_test.go` | Deviations: `index_data` is not a worker tool; a direct call (`toolkit.call_tool.v1`) and a pipeline node refuse the two step-back tools (no bound model); a pipeline redeems the claim context for its index tools only when a toolkit node calls one on a catalog toolkit (or it has LLM/application nodes); the direct read and discovery paths (`toolkit.execute.read.v1`) serve no index tools and Main freezes no catalog there; nested applications carry none (follow-up); the catalog is frozen at admission, so an index created mid-conversation appears on the next turn; `remove_index` needs a human's approval under the shipped default `sensitive_tools`; Main's vector deletion hook is still deferred (ADR-0031 decision 4), so until elitea-main has an `elitea-vector` client the tombstoned row's points wait for the sweeper |
| `services/elitea-worker-python/src/elitea_worker/protocol/agent.py` | Strict `AgentExecutionInputV1` parsing and result binding | `src/agents/protocol.rs`, `src/agents/result.rs` | `tests/agent_input_contract.rs` application/ad-hoc, limits, mutation and terminal corpus | Partial: input construction and three admitted result states pass; delivery is not implemented |
| Python worker `execution/delivery.py::{AgentExecutionDeliveryProcessor,_process_admitted,_execute_resolved}`, `execution/supervisor.py::ExecutionSupervisor`, `serve.py::ProductionDeliveryProcessor` and process drain | Transfer one raw Redis application/ad-hoc delivery through claim/preparation into authorize-once process ownership, then run the native lifecycle through durable output, settlement and Redis retirement | `src/execution/{agent_delivery_processor,agent_coordinator,agent_invocation,invocation_supervisor,native_agent_lifecycle,production}.rs` | Raw application/ad-hoc delivery-to-retirement E2E, dropped-waiter/drain corpus, stopped-processor no-Begin/no-ACK, stopped-coordinator explicit unstarted cleanup and process-level policy/signal/error tests | Capability-disabled common processor/coordinator and concrete production entrypoint implemented. The processor awaits the exact supervised lifecycle, preserving PEL heartbeat ownership through terminal retirement or explicit no-ACK recovery; direct and pipeline completion share the lifecycle/output owner. The entrypoint consumes the connected bundle plus an explicit security snapshot and owns signals and one bounded drain. Input-free running/ambiguous recovery, capability registration and live process proof remain gated |
| Python worker `handlers/agent.py`, `agents/sdk_adapter.py`; SDK `runtime/clients/client.py`, `runtime/toolkits/application.py`, `runtime/tools/{application,llm,function,router}.py`, graph construction/state utilities and sensitive-tool middleware; EliteaUI `AgentNode.jsx`, `LLMNode.jsx`, `McpNode.jsx`, `ToolkitNode.jsx`, `PrinterNode.jsx`, `RouterNode.jsx`, `DecisionNode/*` and mapping hooks; Main continuation/HITL persistence | Select direct versus saved-pipeline execution, bind frozen model/tool authority, delegate exact child applications, compile active Agent/LLM/Toolkit/MCP/Printer/Router/Decision/control nodes, and pause or authorize exact calls | `src/agents/{request,native_runtime,assembly,ordinary,pipeline,session,application_tools,context_management,sensitive_tools,direct_hitl,events}.rs`, `src/agents/graph/{application,compiler,hitl,resume,state_modifier,llm,node_events,direct_tool,printer,router,decision,agent}.rs`, `src/toolkits/{snapshot,materialize,delegated_auth,mcp,invocation}.rs`, `src/toolkits/families/openapi/`, `src/transport/{model_facade,platform_client,runtime_context}.rs` | Direct/pipeline routing, provider loops, configured-tool/MCP/OpenAPI and child-agent E2E, recursive child-event hierarchy, direct sensitive pause/replay/block-result corpus, direct MCP challenge/authorize/skip/exact-server restart, root and pipeline-LLM delegated authorize/skip/same-ID replay, sensitive-after-auth separation, three-tier parallel nested exact-call resume, root and nested graph HITL plus direct-tool pause/resume/replay, UI-shaped state/Agent/LLM/Toolkit/MCP/Printer/Router/Decision mapping and type/bound fixtures, exact node-tool/application scope, reducers, static Printer checkpoint and public-result projection | Partial capability-disabled runtime. Direct entrypoints use native `LlmAgent`; stored pipelines use the same Runner/session owner plus a Checkpointer. LLM nodes receive only their selected configured/MCP tools. Materializer-known remote-MCP and inline OpenAPI challenges bind native confirmation to the original model-selected call in both execution shapes. Authorize rematerializes and replays exactly without pre-approving a distinct sensitive guard; Skip returns `mcp_auth_decision` under the same call ID with no dispatch or replanning. Direct/ad-hoc saved-agent calls use a typed application-tool adapter over a native child `LlmAgent`; complete Application-only tool sets at the root and nested direct children use native bounded parallel dispatch, and descendant events extend the exact call-bound UI hierarchy before wrapper completion. Native application branches keep descendant events out of the root model's later conversation scope while the root Runner persists the complete hierarchy in the same claim-fenced session. Parallel nested sensitive calls emit and persist distinct real ADK confirmations, preserve their exact UI paths, drain completed siblings, expose no internal pause marker and execute no guarded effect. A complete decision set reconstructs the persisted call hierarchy, re-emits the exact original Application calls at every admitted direct-agent tier and consumes child replay plans by invocation plus call ID before one root continuation; omitted siblings, stale routes, broken parent links and a fourth agent tier fail closed. An Agent node maps one task to one exact frozen saved participant: direct agents reuse that claim-bound child loader and render their own stored variables into their instructions, while saved pipelines compile as isolated native `SubgraphNode` values over the same Checkpointer. Saved-pipeline nodes emit the standard Pipeline wrapper and route child LLM events through the existing call-bound hierarchy projector. Direct saved-agent descendants stream through the same typed event bus, with a synthetic pipeline Application call/result providing the exact `{name,call_id,sibling_ordinal}` parent tier expected by the UI without storing browser events in graph state. Sensitive descendants keep their cards as the only public HITL events; one internal graph interrupt binds the complete interrupt-ID set to the pending Agent node, and exact approve/block replay handles parallel leaves with colliding provider call IDs without dispatching an incomplete set. Child state is isolated to explicit input/result/resume channels. A dynamic child-pipeline HITL pauses the parent Agent node; its public identity binds both private checkpoints, the card retains the exact wrapper hierarchy, approval resumes the exact child once, replay fails, and browser output exposes neither child thread nor checkpoint. Sensitive child LLM denial forwards the replay envelope into that child and returns the structured block under the original call ID without dispatch or replanning. Direct Toolkit (including dynamic inline OpenAPI) and remote MCP nodes resolve one exact selected read and execute it once through native `ToolContext`, without a model turn; an RMCP authorization challenge becomes a native graph interrupt, exact-resource token rebuild or clean Skip/`END`. Router is deterministic bounded state routing; Decision reuses the same claim-bound model factory as a no-tool nested `LlmAgent`, then both select one compiler-validated target through atomic `goto`. Node kind/alias/policy/frozen selection is checked before credential redemption or MCP connection. The checkpointed JSON `messages` channel preserves observable behavior, but Rust omits Python's custom runnable, coercion model call, mutable wrappers, hidden tools, flattened history, warning-and-skip and raw exception-as-assistant-message paths. Frozen `model_project_id` preserves shared/project-bound model authority. Sensitive read-only Toolkit/MCP nodes use a separate checkpoint-bound graph confirmation: approval returns the normal tool result; Reject and Block With Comment execute zero provider calls, preserve the SDK-formatted terminal explanation for chat users, record `sensitive_tool_blocked` under the visit-specific call ID and route to `END` without nulling typed outputs. Printer uses compiler-owned native `interrupt_after`. Fixed and declared-parameter catalogue-backed HTTP MCP is resolved by Main after claim and consumes bounded sensitive headers in Rust. Partial-sibling replay, approved-effect receipts, incremental pipeline tool-progress chunks, independent partial authorization decisions, arbitrary static breakpoints, and stdio remain fail-closed. Pipeline LLM sensitive calls checkpoint and replay the exact pending ADK turn; denial supplies the shared structured result under the original call ID without executing the real tool or model replanning. Production PostgreSQL/Redis/bootstrap registration, Main's private immutable child-version route, recursive pipeline Agent nodes beyond the admitted materialized child, per-CALL child variable arguments (the SDK's `application_variables`, whose tool-argument schema is derived from the child's variable list), nested static pauses/auth/effects, SharePoint and remaining OpenAPI delegated/content capabilities, Code nodes and context editing, a dedicated summary model and pipeline-graph context management remain gates |
| SDK `runtime/tools/tool_binding.py::{select_tools_for_binding,build_tool_binding_plan}`, `runtime/tools/llm.py` and `tests/runtime/test_tool_binding.py` | Retain toolkit-qualified identity across selection, expose unique provider-safe names, and route colliding tool calls to the exact original implementation | `src/toolkits/tool_binding.rs`, `src/agents/{ordinary,application_tools,sensitive_tools,pipeline}.rs`, `src/agents/graph/llm.rs`, `src/toolkits/delegated_auth.rs` | Generated collision matrix plus direct Application/ad-hoc and pipeline-LLM component stories calling two same-named MCP tools in one model turn; exact source/result/call-ID and sensitive/delegated guard binding assertions | Implemented capability-disabled. Unique names are unchanged; collisions receive deterministic sanitized toolkit-qualified aliases with a stable hash only when required by a reserved name, secondary collision or length bound. Runtime-owned control tool names remain reserved. Provider aliases never replace the frozen scoped identity used for policy, authorization or execution. Direct graph nodes remain exact scoped calls without provider aliasing |
| SDK `runtime/tools/llm.py::{_collect_parallel_application_specs,_build_parallel_dispatch_specs,_run_parallel_application_calls}`, `runtime/tools/application.py` hierarchy metadata and EliteaUI execution hierarchy/grouping helpers | Run repeated LLM-selected saved participants concurrently and retain stable recursive invocation breadcrumbs for containers and leaves | `src/agents/{ordinary,session,events,application_tools,direct_hitl}.rs` | Native `LlmAgent` parallel tool dispatch for complete Application-only root and nested direct-agent tool sets; Elitea owns the eight-call bound, bounded typed child-event bridge, native branch isolation and frozen `{name,call_id,sibling_ordinal}` presentation join | Same-participant barrier/provider-order proof, two-wrapper root projection, real child-event ordering, root -> child -> leaf hierarchy fixture, sequential three-tier resume and a four-leaf parallel three-tier collision/resume proof | Capability-disabled recursive direct slice implemented. Mixed toolsets stay sequential and a mixed owner with a sensitive descendant application is rejected before execution. Provider call ID is durable invocation identity and ordinal is display-only. Descendant ADK events are routed through exact container/call identity and projected before wrapper completion. They are branch-isolated from later root model history but persisted by the same root Runner, so no independent child-session or interrupt table is needed. Parallel nested confirmations aggregate as distinct durable UI-path events; a complete decision set replays the exact hierarchy through the admitted three agent tiers and continues once, while incomplete sets, broken parent links and over-nesting fail closed. Invocation-scoped coordinator maps keep identical child/leaf call IDs collision-safe under distinct concurrent parents. Internal pause results are removed while completed sibling results are retained. A pipeline Agent parent now uses that same hierarchy and replay owner through an internal graph checkpoint; partial-sibling replay and saved pipelines as direct tools remain explicit gates. The future explicit pipeline parallel node remains capability-closed and separate |
| Python worker invocation-local execution memory and SDK Runner conversation/session behavior; Main execution claim/fence lineage | Persist ordered ADK conversation events and app/user/session state for direct and graph agents without conflating graph frontier state | Main `internal/application/execution/claims.go` and `internal/transport/runtimegrpc/control/server.go`; Rust `src/protocol/control.rs`, `src/execution/agent_lease.rs`, `src/agents/{ordinary,pipeline,session,direct_hitl}.rs`, `src/state/{writer_lease,postgres_session,postgres_checkpointer}.rs`, state tests, agentstate migrations/tests `0001_agent_graph_checkpoints.sql` and `0002_agent_sessions.sql` | Control receipt/lease expiry/renewal/shutdown corpus; complete-event canonical round trip, bounds/authority unit corpus, cross-execution grant rejection, injected restore/seed-once history, request-independent definition-lineage fixture, exact direct-call decision resolution, read-only and blocked-result replay/restart transcripts, full admitted pipeline pause/resume/completion, migration structure and credential-gated isolated PostgreSQL stories covering exact replay/conflict, timestamp ties, state tiers, stale writer fencing, graph descendants and delete/recreate; local Centry PostgreSQL 18 proof passed | Claim-fenced `adk-session.2.0.0.v1` `SessionService`, `adk-graph.2.0.0.v1` `Checkpointer` and common Runner injection target the separate `agentstate` database under an isolated `elitea_runtime` schema. Legacy LangGraph tables in `public` are untouched. `AUTHORIZED_NOW` mints the one-use session grant; the pipeline backend consumes it once and derives both writers from the same immutable claim/fence. Main supplies its database-authored fresh-claim start, the supervised Rust lease supplies the live guard, and both state adapters check that guard before writer locking and commit while retaining a durable newer-writer fence. They do not query or duplicate Main tables or need a separate interrupt table. This is intentionally a bounded non-atomic two-database protocol, not a distributed transaction. The pseudonymous session is tenant/project/thread scoped and is also the private graph checkpoint thread; the public chat thread never becomes checkpoint authority or leaks through HITL output. Direct and pipeline replays use the standard session lineage and record bounded internal Runner events because ADK has no no-input resume call; adapters exclude those markers from provider/graph semantic input. Exact direct partial suffixes recover safely before or after a persisted result. Production bootstrap registration, pipeline continuation-suffix crash proof, restricted pool/role wiring, existing cleanup-owner extension, Main transcript cutover and deployed pooler/load proof remain gates |
| Python worker `protocol/node_event.py` and Main `runtimegrpc/nodeevent/codec.go` | Bounded 13-field current JSON/`NodeEventV1` codec, arbitrary JSON fragments and Go-compatible escaping | `src/protocol/node_event.rs` | `tests/node_event_contract.rs`: existing 36-type corpus, Python wire/browser/drift vectors, limits and wire mutations | Intentional deviation: lossless compact fragment spellings and typed resource-limit failures follow the Go durable boundary; SDK/ADK event bridge is separate |
| Python worker `protocol/codec.py::{build_node_event_output_frame,build_output_frame,_runtime_error_message}` | Claim-fence-bound deterministic progress and terminal frames, safe errors, payload digests and settlement proposal | `src/protocol/output.rs` | Python-produced complete progress/success/cancellation `ExecutionOutputFrameV1` goldens plus identity/digest/fence/limit mutations | Implemented frame construction; PrepareSettlement execution is not |
| Python worker `transport/output_spool.py::EncryptedOutputSpool` and `serve.py::{_execution_spool_binding,_prepare_execution_spool}` | Execution-bound HKDF/AES-256-GCM disk format, immutable sequence publication, replay, exact replacement and ACK cleanup | `src/spool.rs` | Python-generated binding/directory/fixed-nonce golden plus `tests/output_spool_contract.rs` filesystem, capacity, corruption, ownership and cleanup corpus | Implemented synchronous primitive for macOS/Linux; Rust adds directory-relative operations and an advisory exclusive owner lock. The ordered output session now consumes this primitive |
| Python worker `transport/output_grpc.py::OutputGrpcSession` and Main `runtimegrpc/output/server.go::Server.Publish` | Pre-network recovery decision, one bootstrap grant, absolute credit, spool-before-send, exact bound ACK, ACK-before-delete, ordered replay, recovery CAS and typed winner/retry rejection | `src/transport/{output_session,output_grpc}.rs` | Pure ACK/credit/fence/sequence mutations plus component tests with the real encrypted spool, ACK loss, reopen, recovery replacement, reconciliation and exact replay | Implemented for one caller-verified tonic channel/session attempt. Bounded reconnect, TLS construction, control settlement and delivery coordination remain separate slices |
| Python worker `transport/control_grpc.py::ExecutionControlClient` and generated `RuntimeControlService` | One deadline-bound attempt for claim, begin, authorize, renew, observe and settlement with exact metadata and directional whole-message limits | `src/transport/control_grpc.rs` | Injected RPC component corpus covering all six methods, one-attempt failures, metadata/deadline mutations and exact 64/80 KiB boundaries | Implemented over one caller-verified tonic channel. TLS construction and semantic response authorization remain separate composition/protocol slices |
| Python worker `handlers/agent_events.py`; SDK graph state/mapping/result extraction, `langraph_agent.py::{PrinterNode,ConditionalEdge,DecisionEdge}`, `runtime/tools/{application,hitl,llm,function,router}.py` and sensitive-tool middleware | Project ordered runtime events, reduce checkpoint state, execute selected saved participants, node-scoped LLM or direct Toolkit/MCP work, select Router/Decision branches, pause exact calls, publish Printer checkpoints and select the public result | `src/agents/{native_runtime,pipeline,session,events,application_tools}.rs`, `src/agents/graph/{agent,application,compiler,resume,hitl,state_modifier,llm,node_events,direct_tool,printer,router,decision}.rs` | Native routing and root events; direct sensitive pause/redaction; stored-pipeline admission/restart; root and child graph checkpoint and direct-call identity; UI state, Agent, LLM, Toolkit, MCP, Printer, Router and Decision mappings; typed projection; exact participant/tool scope; reducer and public-result corpus | Partial capability-disabled runtime. The compiler accepts bounded Agent/HITL/state-modifier/LLM/configured-Toolkit/direct-MCP/Printer/Router/Decision documents and rejects unsupported nodes or user-authored static interrupts before authority construction. Agent nodes resolve one frozen participant alias; variable-free direct saved agents execute through the typed child-agent adapter, and saved pipelines execute through isolated checkpointed `SubgraphNode`. Direct saved-agent descendants stream beneath a synthetic Application call with ordered invocation ancestry. Their sensitive leaf cards remain the sole public events while one internal graph interrupt binds the exact complete decision set to the pending Agent node; approve and same-call block replay the exact hierarchy, and incomplete sets dispatch nothing. A dynamic inner pipeline interrupt is re-bound to the parent event with both exact private checkpoints, projected without leaking either identity and resumed once through the parent Agent node. Each LLM node owns a fresh scoped `LlmAgent`; each direct node calls one exact read-only `Tool`. Router evaluates state without model authority; Decision receives one no-tool model and both can only `goto` a declared route or fallback. Sensitive reads pause before dispatch and resume the same pending node; denial records the same-call structured tool result, emits the formatted user explanation and terminates cleanly at `END`. The common Runner composes durable `SessionService` and graph-only `Checkpointer`; fixtures prove direct/saved-pipeline Agent mapping and completion, root/nested pause and exact resume, transforms, LLM/Toolkit/MCP/Printer projection, Router/Decision selection, blocked no-call termination and replay rejection. `START`/`END` remain control sentinels. Production config/TLS/signal bootstrap, continuation-suffix recovery, recursively materialized pipeline child participants beyond the admitted level, nested static pauses, Code nodes, incremental pipeline tool-progress chunks, MCP OAuth authorization, approved-effect receipts, skills and compaction analytics remain gates |
| SDK `runtime/clients/client.py::{_inject_summarization,_inject_context_editing}` and summarization middleware; Main conversation summary projection | Context management around direct and graph agents | `src/agents/context_management.rs`, `src/agents/session.rs`, `src/state/{postgres_session,postgres_checkpointer}.rs` | Admission per branch (disabled/enabled/malformed/out-of-range) plus behavioral compaction over budget, preserved tail, pinned system turns and tool-loop skip; future restart/nested/HITL/analytics corpus | Active for direct and ad-hoc agents. `context_management.rs` admits Main's frozen contract (`enabled`, `enable_summarization`, `enable_context_editing`, `max_context_tokens`, `preserve_recent_messages`, `preserve_system_messages`, `summary_instructions`, `summary_llm_settings`) before PAT redemption and composes an ADK Runner `CompactionConfig` pairing a strategy that reproduces the SDK's `SummarizationMiddleware.before_model` ordering with ADK's `LlmEventSummarizer`. The Runner applies it through `MutableSession::replace_events`, so compaction is per invocation and the PostgreSQL `SessionService` keeps the complete durable transcript; `EventsCompactionConfig` is deliberately NOT used, because its persisted `EventCompaction` markers would make compaction irreversible. Default remains off end to end: empty settings, an absent conversation, an explicit `enabled=false` and `enable_summarization=false` all admit `Disabled` and never reach the Runner seam. Tool-output context editing, a dedicated summary model and pipeline-graph context management stay fail-closed, as does conversation analytics projection. Indexing/RAG remains last |
| SDK `configurations/azure_search.py` and `tools/azure_ai/search/**`; Main tool freezer/materializer; Python worker shared SDK adapter | Two configured-index Azure Search reads, selected-tool semantics, read grouping and application/ad-hoc parity | `src/toolkits/families/azure_search/{config,client,tools}.rs` | Eleven focused configuration/wire/projection/bound/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family; `limit=-1` is bounded to 100, the broken Azure OpenAI check is omitted, and live materialization/HITL composition remain gates |
| SDK `configurations/gitlab.py`, `tools/gitlab_org/{__init__,api_wrapper}.py`, shared file-edit/diff helpers and `python-gitlab==4.5.0`; Main toolkit schema/freezer/materializer; Python worker shared SDK adapter | Separate GitLab Org catalog with 17 repository/branch/issue/MR/file/commit operations, configured or dynamic project selection, invocation-local active branch, empty/subset selection, operation groups and application/ad-hoc parity | `src/toolkits/families/gitlab_org/{config,client,edit,diff,tools}.rs` | Thirteen focused configuration/route/result/pagination/file-edit/diff/effect-bound/model-metadata/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family: all 8 reads, 8 writes and 1 delete are retained with bounded project/file/diff authority. Dynamic repository mode, live configuration-check composition, exact-interrupt HITL and cancellation-safe effect reconciliation remain activation gates |
| SDK `configurations/gitlab.py`, `tools/gitlab/{__init__,api_wrapper,utils}.py`, `tools/elitea_base.py::{BaseCodeToolApiWrapper.{search_file,edit_file},extend_with_file_operations}`, `tools/utils/{text_operations,file_metadata}.py` and `python-gitlab==4.5.0` at the pinned SDK `b5113a1`; Main toolkit schema/freezer/materializer | Single-project GitLab catalog: branches (create/delete/list/set active), files (list/read/batch read/grep/create/update/append/delete), issues, merge requests (create, comment, diff, inline comment) and commits, with the protected base branch and the invocation-local active branch | `src/toolkits/families/gitlab/{config,client,tools}.rs`, `src/toolkits/families/vcs_text.rs`; reuses `gitlab_org::{client::reqwest_transport,edit,diff}` | `src/toolkits/gitlab_tests.rs` (catalog, strict config, selection/index omission, SDK conformance gate, exact routes/bodies/messages, pagination, read guard/active branch, MR diff + inline position, error mapping) and `vcs_text` unit tests | Partial capability-disabled family: 21 of 27 SDK tools (all non-index). SDK first-page reads (`get_issues`, `get_issue` notes, `get_commits`) stay first-page; `get_all` reads are bounded to 10 pages/1000 items. Deliberate deviations: provider failures are typed errors instead of `Unable to … due to error:` strings; `create_file` creates only on 404 (the SDK creates on any read error); update/append commits carry GitLab's `last_commit_id` fence from the read they edit; non-UTF-8 files return a text-only notice instead of the SDK's LLM document/image parsers. Index tools, live check, exact-interrupt HITL and effect reconciliation remain gates |
| SDK `configurations/bitbucket.py`, `tools/bitbucket/{__init__,api_wrapper,cloud_api_wrapper,bitbucket_constants}.py`, shared `elitea_base`/`text_operations`/`file_metadata` helpers and `atlassian-python-api==4.0.7` at the pinned SDK `b5113a1`; Main toolkit schema/freezer/materializer | One Bitbucket Cloud or Server repository: branches (create/delete/list/set active), files (list/read/batch read/grep/create/update), pull requests (create, read, commits, changes, comment, decline) with the protected main/master rule and the invocation-local active branch | `src/toolkits/families/bitbucket/{config,client,tools}.rs`, `src/toolkits/families/vcs_text.rs`; reuses `gitlab_org::{edit,client::wildcard_matches}` | `src/toolkits/bitbucket_tests.rs` (catalog, hosting detection incl. spoofed hosts, selection/index omission, SDK conformance gate, exact Server and Cloud routes/bodies/multipart/form encoding, paging, `next`-link and redirect origin checks, messages, error mapping) | Partial capability-disabled family: 16 of 22 SDK tools (all non-index). Deliberate deviations: provider failures are typed errors; `create_file` creates only on 404; Server PR commits/changes return the provider objects (the SDK returned a generator repr), Server decline sends the PR version and inline comments return guidance (both raised `TypeError` in the SDK), Server file listing returns repo-relative paths (the SDK raised on string items), Cloud PR reads return the provider JSON (the SDK's `__dict__` projection also exposed the client credentials), Cloud writes carry a commit message, an unset `cloud` follows a bitbucket.org host, and HTTP 429 is a retryable typed error instead of the SDK's in-call 5-attempt backoff. Index tools, live check, exact-interrupt HITL and effect reconciliation remain gates |
| SDK `tools/localgit/{__init__,local_git,tool}.py` at the pinned SDK `b5113a1` | 13 working-tree file, branch, commit and diff tools over a caller-chosen `base_path`/`repo_path` checkout, optionally cloned from `repo_url` | None in the cloud worker; the desktop host's `libs/rust/local-tools` is the confined equivalent (ADR-0029) | `docs/source-mapping/configuration-toolsets.md#localgit-is-not-a-worker-family` | Blocked by design: the root is user-authored and unconfined (`normpath` join, no containment), `repo_url` is unmanaged git egress, and the worker has no per-claim workspace. The pinned SDK itself never stores `self.repo`, so every call returns an `AttributeError` traceback. A frozen `localgit` reference stays skipped as an unsupported family |
| SDK `configurations/aha.py`, `tools/aha/{__init__,api_wrapper}.py`, shared artifact fetch helper and current/pinned Aha unit/e2e fixtures; Main toolkit/configuration catalogs, freezer/materializer; Python worker shared SDK adapter | Complete 33-tool Aha REST/GraphQL catalog, JSON/CSV/Markdown results, parent-scoped record effects, links/comments/custom-field metadata, empty/subset selection, operation groups, application/ad-hoc parity and artifact-backed attachments | `src/toolkits/families/aha/{config,client,format,artifact,tools}.rs`, `src/toolkits/materialize.rs` | Fifteen focused catalog/schema/model-description, exact-route/pagination/GraphQL/format/effect/attachment-authority/bound/production-profile tests plus the SDK conformance gate on the production profile; future credentialed application/ad-hoc/live-provider proof | Materialized partial family (32 of 33): all 25 reads, 5 writes, 1 delete and the effectful combined execute surface are served; `attach_file` stays implemented but unserved (`supported_tools.aha`) until an artifact-read grant plane feeds its sealed resolver. Live connection check, exact-interrupt HITL, cancellation-safe effect reconciliation and shared disk/concurrency budgets remain gates |
| SDK `configurations/{jira,utils}.py`, `tools/jira/{__init__,api_wrapper}.py` and `atlassian-python-api==4.0.7` `Jira`/`AtlassianRestAPI` at the worker-pinned revision `b5113a12`; Main toolkit schema/freezer/materializer; Python worker shared SDK adapter | Twelve Jira REST tools: JQL search with v3 token / v2 `startAt` pagination, issue create/update/labels/status transitions, comments, projects, field and remote-link reads, issue links and the generic REST passthrough, with default labels, additional fields, custom headers and the `/myself` credential probe | `src/toolkits/families/jira/{config,client,tools}.rs`, `src/toolkits/materialize.rs` | `jira_tests.rs`: fourteen configuration/selection/SDK-gate/argument/route/pagination/probe/refusal/generic-path/cookie/bound tests on Cloud v3 and Server v2 fixtures; future credentialed application/ad-hoc/live-provider proof | Materialized partial family (12 of 23, `supported_tools.jira`). Not served: `get_field_with_image_descriptions` and `get_comments_with_image_descriptions` (vision-model call), `get_attachments_content` (SDK content parser plus LLM image path), `add_file_to_issue_description` and `update_comment_with_file` (verified artifact bytes), and the six index tools. Plain-HTTP origins, `verify_ssl: false` and custom headers that replace the credential skip the toolkit as unsupported. Exact-interrupt HITL and cancellation-safe effect reconciliation remain gates |
| SDK `configurations/{confluence,utils}.py`, `tools/confluence/{__init__,api_wrapper}.py` and `atlassian-python-api==4.0.7` `Confluence` at the worker-pinned revision `b5113a12`; `markdownify`; Main toolkit schema/freezer/materializer; Python worker shared SDK adapter | Sixteen Confluence tools: page create (v1, or Cloud v2 `pages`)/batch create/delete/update by id or title/batch update/label replacement, page tree, label reads, page read in seven body formats rendered as Markdown, CQL search by text/title, site search, title lookup and the generic REST passthrough, with default labels and custom headers | `src/toolkits/families/confluence/{config,client,tools}.rs`, `src/toolkits/materialize.rs`; `html-to-markdown-rs` for `markdownify` | `confluence_tests.rs`: eight configuration/selection/SDK-gate/argument/read-format/search-CQL/label/create-v1-v2/update-version-label/delete/tree/generic tests on Cloud and Server fixtures; future credentialed application/ad-hoc/live-provider proof | Materialized partial family (16 of 25, `supported_tools.confluence`). Not served: `get_page_with_image_descriptions` (vision-model call), `get_page_attachments` (SDK content parser plus LLM), `add_file_to_page` (verified artifact bytes) and the six index tools. Updates keep the page's storage body instead of the SDK's rendered-view resend; the SDK's tenacity retries are not reproduced. Exact-interrupt HITL and cancellation-safe effect reconciliation remain gates |
| SDK `configurations/sql.py`, `tools/sql/{__init__,api_wrapper,models}.py`, SQL connection-string fixtures; Main toolkit/configuration catalogs, freezer/materializer; Python worker shared SDK adapter | Complete PostgreSQL/MySQL two-tool catalog, committed arbitrary statement execution, default-schema table/column discovery, empty/subset selection, operation groups and application/ad-hoc parity | `src/toolkits/families/sql/{config,lexer,project,client,tools}.rs` | Eleven focused catalog/schema/model-description, statement-scanner, MySQL session-mode, backend-value, query, bound, redaction and effect-outcome tests plus future credentialed PostgreSQL/MySQL protocol/TLS proof | Capability-disabled complete family: the read and unrestricted effect are retained with typed fixed authority and bounded JSON-safe results. Exact-interrupt HITL, effect reconciliation, environment-free TLS ownership and driver preallocation controls remain activation gates |
| SDK `configurations/postman.py`, `tools/postman/{__init__,api_wrapper,postman_analysis}.py`; Main toolkit/configuration catalogs, freezer/materializer; Python worker shared SDK adapter | Complete 31-tool Postman collection, folder, request, script, search, deterministic-analysis and stored-request execution catalog; empty/subset selection, operation groups and application/ad-hoc parity | `src/toolkits/families/postman/{config,collection,analysis,client,tools}.rs` | Sixteen focused catalog/schema/model-description, configuration/redaction, analysis, exact management/effect route, path ambiguity, dynamic-auth/body and effect-outcome tests plus future credentialed application/ad-hoc/Postman/downstream proof | Capability-disabled complete family: all 8 reads, 19 writes, 3 deletes and the effect-capable execute surface are retained. Fixed management authority is separated from production-closed dynamic egress; exact-interrupt HITL, cross-worker collection fencing, cancellation-safe effect reconciliation, sealed downstream authority and live proof remain activation gates |
| SDK `tools/yagmail/{__init__,yagmail_wrapper}.py`; pinned `yagmail==0.15.293`; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete one-tool implicit-TLS SMTP family, inline secret materialization, empty/exact selection, literal MIME body, recipient projection and `write` grouping | `src/toolkits/families/yagmail/{config,client,tools}.rs` | Eleven focused configuration/schema/model-description, SMTP transcript/auth/MIME/bound, tenant-isolation, recipient-result and effect-phase tests plus future credentialed application/ad-hoc/live-SMTP proof | Capability-disabled complete family: the send effect is retained with verified TLS, bounded one-attempt SMTP and unknown-outcome handling after DATA. Rust deliberately repairs null-host and required-nullable `cc` source mismatches; exact-interrupt HITL, durable Message-ID intent/receipt reconciliation, SMTP egress and live proof remain activation gates |
| SDK `tools/pptx/{__init__,pptx_wrapper}.py` (`PPTXToolkit`, `PPTXWrapper.{fill_template,translate_presentation,translate_batch,_generate_content_batch}`) at the pinned SDK `b5113a1`; python-pptx; Main toolkit schema/freezer/materializer; ADR-0032 P3 | The `pptx` toolkit: the SDK's `fill_template` and `translate_presentation` (names, arguments, descriptions, prompts, batching of 20, result shapes `{status, message, url, stats}` and error text) plus the deterministic halves `list_placeholders` / `replace_placeholders` and 17 authoring tools (`create_presentation`, `add_slide`, `delete_slide`, `move_slide`, `set_title`, `set_text`, `add_bullets`, `add_table`, `set_table_cell`, `add_chart`, `add_image`, `set_notes`, `apply_theme`, `render_preview`, `design_check`, `to_markdown`, `export_pdf`); artifacts in and out as bytes | `libs/rust/agent-runtime/src/toolkits/families/{office_host,pptx/{config,schema,model,ops,tools}}.rs` (feature `toolkit-office`, enabled here), `libs/rust/office-core` (`Deck`, `Deck::facts`), `src/transport/office_host.rs` (`ClaimOfficeHost`: platform object API under the claim's credential, the run's model via `generate_text`), `src/agents/ordinary.rs::office_host`, `src/execution/production.rs::{drive,office_core_stuck}` | `libs/rust/agent-runtime/src/toolkits/pptx_tests.rs` (23: SDK schema gate with 19 reasoned exemptions, selected-tools rule, materialization with and without the Office capability, every tool end to end on real packages, fill/translate against a scripted model including batching, index continuation and the SDK fallbacks, prompt text byte for byte, hostile bytes, save-loss guard); `src/transport/office_host_tests.rs` (5: binary round trip, size from HEAD, classification, egress, model call, a real toolkit through the host); `execution/production.rs` tests (4: stuck engine drains and exits, deadline, healthy, the real probe); `libs/rust/office-core/tests/{facts,vendor_patches}.rs` | Direct agents and pipeline nodes (a pipeline binds no chat model to a tool, so `fill_template` and `translate_presentation` answer there with a sentence naming `replace_placeholders`; direct `toolkit.call_tool.v1` calls leave the toolkit out as unsupported). Deliberate differences from the SDK: a partly translated deck answers `status: partial` with the count (the SDK reported success); the 19 Rust-only tools are listed in the editor from `runtimecomposition/rust_only_toolkit_tools.json`; `url` is the key every tool accepts as `file_name` (the bucket is a separate `bucket` field; a leading `/{bucket}/` is accepted on input); `pdf_file_name` is accepted and the template's own first three placeholder slides are sent as images (no PDF rasteriser), WITH THE FIRST BATCH ONLY (the SDK sends its PDF pages with every batch; later batches here carry a one-line note that they were given, to avoid repeating the image tokens); paragraph structure of a filled frame is kept (the SDK flattens to one paragraph); run formatting is kept in the first run (the SDK's translate dropped it); table cell text is written as one plain paragraph; identical paragraphs are translated once; translations are matched by `index`; SmartArt text is neither read nor written (`smartart_updates` is always 0). The pptx edit paths that the slide library saves lossily (first notes on a slide, theme replacement) are refused with nothing written when they would drop tables, charts or pictures. Desktop wiring is P6 |
| None: the SDK has no `docx` toolkit (its content parsers read `.docx`; nothing authors or edits one). The tool names, descriptions and schemas are this family's own, following the `pptx` family; `sdk_conformance::has_type("docx")` is asserted false so the day the SDK grows one the family is reconciled with it; Main catalogue entry from `rust_only_toolkit_tools.json` `new_types`; ADR-0032 P4 | The `docx` toolkit: `create_document`, `read_document` (Markdown or plain text), `add_heading`, `add_paragraph`, `set_paragraph_text`, `add_table`, `set_table_cell`, `add_image`, `add_chart`, `list_placeholders`, `replace_placeholders` (one pass, leftmost-longest, over the original text; a replacement is never searched again), `export_pdf`, `render_page`; artifacts in and out as bytes; every write is planned against the file, applied once, saved once and checked on the saved bytes at the paragraph, table or cell it was aimed at | `libs/rust/agent-runtime/src/toolkits/families/{office_common,office_host,docx/{config,schema,ops,tools}}.rs` (feature `toolkit-office`, enabled here), `libs/rust/office-core` (`Document`, `Document::replace_all`, `facts_of_docx_package`), `src/transport/office_host.rs`, `src/agents/ordinary.rs::snapshot_carries_office_family` | `libs/rust/agent-runtime/src/toolkits/docx_tests.rs` (25: contract, selected-tools rule, SDK-absence gate, catalogue entry, create without overwrite, authoring that chains reported numbers, line breaks as break runs, refusals before any write, picture-bearing paragraph, proportions, chart, placeholders, one-pass replace, target-location verification, loss refusal, names that round-trip, reads, outputs beside the source, one sentence per host failure, one open and one save per edit, materialization); `libs/rust/office-core/tests/p4.rs`; `src/agents/pipeline_artifact_tests.rs` (docx node) | Direct agents and pipeline nodes (no model tool, so a pipeline loses nothing). Gaps stated in the tool descriptions: content is appended at the end (no insert-at, no delete); `set_paragraph_text` keeps the first run's formatting and replaces the rest of the paragraph (a paragraph holding a picture or chart is refused); headers, footers and text boxes are replaced in but not listed or read; `replace_placeholders` takes at most 100 keys, keys without line breaks or tabs and not partly overlapping (refused before any edit); tracked changes, SmartArt and fields are neither read nor edited; page renders and PDFs are approximate (bundled fonts, charts may be placeholders). The `pandas` toolkit's `.xlsx` output is untouched. Desktop wiring is P6 |
| None: the SDK has no `xlsx` or `excel` toolkit (its `pandas` toolkit writes `.xlsx` through `df.to_excel` and is not superseded here; P5 decides its replacement). The names are this family's own; `sdk_conformance::has_type` is asserted false for `xlsx` and `excel`; Main catalogue entry from `rust_only_toolkit_tools.json` `new_types`; ADR-0032 P4 | The `xlsx` toolkit: `create_workbook`, `list_sheets`, `add_sheet`, `rename_sheet`, `delete_sheet`, `read_range` (Markdown with addresses, or JSON), `sheet_to_markdown`, `write_cells` (batch, all or none, typed values and formulas), `clear_range`, `add_chart` (from cell ranges); artifacts in and out as bytes; every write is checked on the saved bytes at the cells aimed at | `libs/rust/agent-runtime/src/toolkits/families/{office_common,office_host,xlsx/{config,schema,ops,tools}}.rs` (feature `toolkit-office`, enabled here), `libs/rust/office-core` (`Workbook`, `Workbook::{write_cells,clear_range,add_chart}`, `facts_of_xlsx_package`), `src/transport/office_host.rs`, `src/agents/ordinary.rs::snapshot_carries_office_family` | `libs/rust/agent-runtime/src/toolkits/xlsx_tests.rs` (22: contract, selected-tools rule, SDK-absence gate, catalogue document, create without overwrite, typed writes checked in the file, refusals before any write, distinct-cell cap with a whole-batch refusal, clear, sheets, formula-named sheets, chart, reads with addresses, the cell-cap cut, inline strings, names that round-trip, one sentence per host failure, one open and one save per edit, materialization); `libs/rust/office-core/tests/p4.rs`; `src/agents/pipeline_artifact_tests.rs` (xlsx node) | Direct agents and pipeline nodes. Gaps stated in the tool descriptions: there is no render and no PDF (a spreadsheet is previewed as a Markdown table); formulas are stored, not calculated (a read shows the last calculated value); error values and dates cannot be written (a date is its serial number); a workbook that stores text as inline strings (`t="inlineStr"`, as some producers write) is READ correctly from the package but an edit is refused, because the spreadsheet library reads those cells back as empty and a save would erase them; renaming or deleting a sheet that a formula or chart names is refused (references are not rewritten); styles, merged cells, validation and conditional formats are neither read nor edited. Desktop wiring is P6 |
| SDK `tools/keycloak/{__init__,api_wrapper}.py`; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete one-tool Keycloak Admin REST family, inline service-account materialization, empty/exact selection, arbitrary bounded HTTP method/path/body execution and `execute` grouping | `src/toolkits/families/keycloak/{config,client,tools}.rs` | Eleven focused configuration/schema/model-description, OAuth/Admin-wire/token-shape, strict-body/path, response, status, redaction, selection and effect-outcome tests plus future credentialed application/ad-hoc/live-Keycloak proof | Capability-disabled complete family: reads, writes, deletes and action endpoints remain reachable through the one generic operation over a fixed HTTPS realm. Rust repairs the SDK's session-level Basic/Bearer collision and pseudo-JSON parsing; exact-interrupt HITL, durable effect reconciliation, approved egress and live proof remain activation gates |
| SDK `tools/cloud/azure/{__init__,api_wrapper}.py`; `azure-identity==1.16.0`, `azure-mgmt-resource==23.0.1`; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete two-tool Azure Resource Manager family, inline service-principal materialization, empty/subset selection, arbitrary bounded HTTP methods plus the resource-group health read, and `execute`/`read` grouping | `src/toolkits/families/azure/{config,client,tools}.rs` | Fourteen focused configuration/schema/model-description, OAuth/ARM-wire, subscription-authority, strict-option/inline-multipart, response, status, redaction, selection and effect-outcome tests plus future credentialed application/ad-hoc/live-Azure proof | Capability-disabled complete family: both public tools and all HTTP method tokens are retained inside one fixed public-cloud subscription. Rust repairs the SDK's missing request helpers, unsafe arbitrary origin/kwargs, raw errors and unbounded transport; exact-interrupt HITL, durable effect reconciliation, approved DNS/IP egress and live service-principal role proof remain activation gates |
| SDK `tools/elastic/{__init__,api_wrapper}.py`; Elasticsearch and OpenSearch Search API contracts; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete one-tool Elasticsearch read family with explicit REST-compatible OpenSearch support, inline origin/optional encoded API-key materialization, empty/exact selection, Query DSL search and `read` grouping | `src/toolkits/families/elastic/{config,client,tools}.rs` | Ten focused configuration/schema/model-description, exact Search API wire, index/query bounds, native/vendor JSON media type, status, output, redaction, selection and tenant-isolation tests plus future credentialed application/ad-hoc/live-cluster proof | Capability-disabled complete read family: the sole public search operation is retained over one fixed verified-TLS Elasticsearch or compatible OpenSearch cluster origin. Rust repairs the absent provider dependency, string-versus-tuple API-key mismatch, disabled TLS verification, class-global client and unbounded query/response behavior; Basic authentication, Amazon SigV4, approved DNS/IP egress, application/ad-hoc materialization and live privilege/load proof remain activation gates |
| SDK `tools/cloud/gcp/{__init__,api_wrapper}.py`; Google service-account OAuth contract; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete one-tool GCP REST family, inline sealed service-account materialization, empty/exact selection, explicit OAuth scopes, arbitrary bounded HTTP methods and `execute` grouping | `src/toolkits/families/gcp/{config,client,tools}.rs` | Twelve focused configuration/schema/model-description, JWT/OAuth-wire, Google-origin/option/body, tenant-isolation, response, status, redaction, selection and effect-outcome tests plus future credentialed application/ad-hoc/live-GCP proof | Capability-disabled complete family: the generic operation retains reads, writes, deletes and actions on approved Google API origins. Rust repairs eager network construction, the SecretStr credential mismatch, arbitrary kwargs/origins, redirects and raw errors; exact-interrupt HITL, durable effect reconciliation, approved DNS/IP egress and live service-account role proof remain activation gates |
| SDK `configurations/bigquery.py`, `tools/google/bigquery/{__init__,api_wrapper,schemas,tool}.py` and `google-cloud-bigquery` REST semantics; Main toolkit/configuration catalogs, freezer/materializer | Eight of eleven BigQuery tools: configured-table row reads, Euclidean nearest-neighbour searches (single, filtered, multi-vector, batch), vector-index DDL, job statistics and Delta Lake external-table creation; explicit-only selection and `json.dumps` result text | `libs/rust/agent-runtime/src/toolkits/families/bigquery/{config,client,format,tools}.rs`; signing reuses `families/gcp/client.rs::{service_account_token_request,access_token_from_response}` | `toolkits/bigquery_tests.rs`: nested sealed configuration, SDK selection/omission, schema gate, `_create_filters` and quoting corpus, exact SQL/parameters per tool, jobs.query/getQueryResults wire with polling and paging, Python-client row decoding and `json.dumps` text, job statistics, `exists_ok` table creation, typed and effect-aware failures | Partial, capability-disabled. `similarity_search`, `similarity_search_with_score` (need a text embedding model the SDK never wires) and `execute` (reflection over Python client methods) are not served; `supported_tools.bigquery` lists the eight. The SDK binds every tool without `mode`, so each SDK call fails with `Unknown tool name`, and its SQL calls the nonexistent `VECTOR_DISTANCE`: Rust serves the wrapper methods' intended behavior with `EUCLIDEAN_DISTANCE` (the index's distance type), escapes filter values and refuses `;` in conditions. Results above 1000 rows are refused. Live service-account, HITL and effect-reconciliation proof remain activation gates |
| SDK `runtime/toolkits/mcp_config.py::McpConfigToolkit` and `runtime/toolkits/tools.py` `mcp_config` branch; Main `runtimecomposition/agent_prebuilt_mcp.go` claim-time resolution | Saved pre-built MCP definitions (`server_name` plus declared parameters) materialized as remote MCP tools; selected/excluded tools | `libs/rust/agent-runtime/src/toolkits/{snapshot,mcp}.rs` (`is_mcp_type`, `parse_mcp_authority`); worker `src/toolkits/direct_runtime.rs` MCP branch; capability `supported_tool_types` | `toolkits/mcp_tests.rs::claim_materialized_prebuilt_http_uses_fixed_headers_and_exclusions`; Go `TestRustCapabilitySnapshotMatchesTheRustSource` derives `mcp_config` from `snapshot.rs` | Partial: HTTP pre-built definitions only, resolved by Main to a fixed HTTPS endpoint and headers before the claim. The SDK's stdio servers and its global `mcp.json` fallback stay external (no process spawning in the worker). The type has no static SDK tools, so there is no schema gate; the capability now declares it so Main's direct tool-run and discovery verdicts stop refusing a type the Rust worker already serves |
| SDK `tools/cloud/k8s/{__init__,api_wrapper}.py`; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete two-tool Kubernetes REST family, inline origin/token materialization, empty/subset selection, arbitrary bounded HTTP methods plus the `/version` health read, and `execute`/`read` grouping | `src/toolkits/families/kubernetes/{config,client,tools}.rs` | Twelve focused configuration/schema/model-description, exact-origin/path, body/header wire, response, status, redaction, selection, health and effect-outcome tests plus future credentialed application/ad-hoc/live-cluster proof | Capability-disabled complete family: both public tools and all HTTP method tokens are retained on one exact verified-TLS cluster authority. Rust removes ambient kubeconfig, disabled TLS verification, class-global client state, broken object parsing and raw errors; exact-interrupt HITL, durable effect reconciliation, approved DNS/IP egress, CA policy and live RBAC proof remain activation gates |
| SDK `tools/zephyr/{__init__,api_wrapper,Zephyr}.py`; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete four-tool legacy ZAPI family, inline Basic-auth materialization, empty/subset selection, exact step read plus all single/batch append effects and `read`/`write` grouping | `src/toolkits/families/zephyr/{config,client,tools}.rs` | Nine focused configuration/schema/model-description, exact Basic-auth GET/POST wire, read projection, batch validation/order, shared-string bound, status, partial-effect, post-success and tenant-isolation tests plus future credentialed application/ad-hoc/live-ZAPI proof | Capability-disabled complete family: all 1 read and 3 writes are retained over one fixed verified-TLS ZAPI prefix. Rust separates the legacy inline contract from Zephyr Scale configuration, removes class-global authority/logging/retries, bounds sequential batches and returns compact receipts; exact-interrupt HITL, durable partial-effect reconciliation, approved egress and live provider proof remain activation gates |
| SDK `configurations/service_now.py` and `tools/servicenow/**`; `pysnc==1.1.10`; Main tool freezer/materializer; Python worker shared SDK adapter | Three incident tools, empty/subset selection, raw-value JSON-string results, `read`/`write` grouping and application/ad-hoc parity | `src/toolkits/families/service_now/{config,client,tools}.rs` | Eleven focused configuration/wire/projection/effect-bound/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family; per-toolkit authority replaces SDK class globals, query/response/fanout are bounded, create/update are present, and live materialization, durable HITL plus cancellation-safe effect reconciliation remain gates |
| SDK `configurations/salesforce.py` and `tools/salesforce/**`; Main tool freezer/materializer; Python worker shared SDK adapter | Six CRM tools, empty/subset selection, lazy OAuth, dedicated Case/Lead reads and effects, generic GET/POST/PATCH/DELETE, operation groups and application/ad-hoc parity | `src/toolkits/families/salesforce/{config,client,tools}.rs` | Ten focused configuration/auth/wire/result/effect-bound/model-metadata/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family; all six operations are present, fixed-origin/version routing and response fanout are bounded, weak source descriptions are clarified, and live materialization, durable HITL plus cancellation-safe effect reconciliation remain gates |
| SDK `configurations/slack.py` and `tools/slack/**`; `slack_sdk==3.35.0`; Main tool freezer/materializer; Python worker shared SDK adapter | Seven messaging, membership and workspace reads/effects, empty/subset selection, configured-channel fallback, operation groups and application/ad-hoc parity | `src/toolkits/families/slack/{config,client,tools}.rs` | Ten focused configuration/wire/result/fanout/error/model-metadata/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family; all seven source operations are present, descriptions are selection-oriented, history and collections are first-page bounded, redundant `auth.test` and unbounded member fanout are removed, and live materialization, durable HITL plus cancellation-safe effect reconciliation remain gates |
| SDK `configurations/rally.py` and `tools/rally/**`; `pyral==1.6.0`; Main tool freezer/materializer; Python worker shared SDK adapter | Eight WSAPI type/entity/context reads and create/update effects, empty/subset selection, API-key or Basic authentication, operation groups and application/ad-hoc parity | `src/toolkits/families/rally/{config,client,tools}.rs` | Eight focused configuration/auth/wire/result/effect-bound/model-metadata/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family; all eight source operations are present, the eager class-global SDK client becomes lazy invocation-scoped authority, one-page reads and descriptions are bounded, and live materialization, durable HITL plus cancellation-safe effect reconciliation remain gates |
| SDK `tools/zephyr_squad/{__init__,api_wrapper,zephyr_squad_cloud_client}.py`; Main current toolkit snapshot/freezer/materializer; Python worker shared SDK adapter | Fifteen Jira-backed Zephyr Squad step, BDD, cycle, folder and execution operations, inline credential materialization, empty/subset selection and `read`/`write`/`delete` grouping | `src/toolkits/families/zephyr_squad/{config,client,tools}.rs` | Eight focused inline-config/JWT-golden/exact-route/body/error/argument/model-metadata/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family: all 5 reads, 8 writes and 2 deletes are present; fixed-origin JWT requests and provider results are bounded, descriptions are selection-oriented, and live materialization, exact-interrupt HITL plus cancellation-safe effect reconciliation remain gates |
| SDK `configurations/zephyr_enterprise.py` and `tools/zephyr_enterprise/{__init__,api_wrapper,zephyr_enterprise}.py`; Main toolkit snapshot/freezer/materializer; Python worker shared SDK adapter | Five flex-REST operations: test-case read, ZQL search, ZQL test-case listing, test-case creation and versioned step appends; nested bearer configuration and empty/subset selection | `src/toolkits/families/zephyr_enterprise/{config,client,tools}.rs` over shared `src/toolkits/families/zephyr_rest/{config,client,tools}.rs` | Eight focused configuration/catalogue/route/body/output/partial-effect/redaction tests including the SDK schema gate, plus seven shared transport tests | Partial family: all five business operations are served; the six indexing tools are not (no indexing in this runtime) and the capability snapshot lists the served tools. HTTP failures use the stable redacted taxonomy instead of the SDK's `Unable to ...` text; live provider proof, exact-interrupt HITL and effect reconciliation remain gates |
| SDK `configurations/zephyr_essential.py` and `tools/zephyr_essential/{__init__,api_wrapper,client}.py`; Main toolkit snapshot/freezer/materializer; Python worker shared SDK adapter | Zephyr Scale Cloud v2 test case, cycle, execution, project, folder, link and issue-link operations, nested bearer configuration with the SDK's default base, empty/subset selection | `src/toolkits/families/zephyr_essential/{config,client,tools}.rs` over shared `src/toolkits/families/zephyr_rest/` | Nine focused configuration/catalogue/every-route/projection/guidance/folder-lookup/argument tests including the SDK schema gate | Partial family: 41 of 45 business operations; not served are the six indexing tools, the three automation uploads whose SDK `files` string can never be sent, and the binary BDD ZIP download. Folder lookup pages past the SDK's first page; HTTP failures use the stable redacted taxonomy; live provider proof, exact-interrupt HITL and effect reconciliation remain gates |
| SDK `configurations/zephyr.py` and `tools/zephyr_scale/{__init__,api_wrapper}.py`; `zephyr-python-api==0.1.0` Cloud wrapper; Main toolkit snapshot/freezer/materializer; Python worker shared SDK adapter | Twenty Zephyr Scale Cloud test-case, step, folder, link, version, script and search operations with `next`-link pagination, folder-tree lookups and client-side search; token-only configuration against the fixed Cloud API | `src/toolkits/families/zephyr_scale/{config,client,folders,render,tools}.rs` over shared `src/toolkits/families/zephyr_rest/` | Fourteen focused configuration/rendering/catalogue/pagination/output/effect/folder/search tests including the SDK schema gate | Partial family: all twenty business operations; the six indexing tools are not served. Output text reproduces the SDK's `str()`/`json.dumps` forms (sorted members); four SDK defects are fixed (two always-failing reads, steps never added on create, unparsed `additional_fields`); live provider proof, exact-interrupt HITL and effect reconciliation remain gates |
| SDK `configurations/carrier.py` and `tools/carrier/{__init__,api_wrapper,carrier_sdk,tools,tickets_tool,backend_reports_tool,backend_tests_tool,ui_reports_tool,run_ui_test_tool,update_ui_test_schedule_tool,create_ui_test_tool,cancel_ui_test_tool}.py` at the worker-pinned revision; Main tool freezer/materializer; Python worker shared SDK adapter | Fifteen of eighteen Carrier ticket, backend-report/test and UI-report/test operations with their conversational confirmation steps, empty/subset selection and application/ad-hoc parity | `src/toolkits/families/carrier/{config,client,tools}.rs` (in `libs/rust/agent-runtime`) | Seventeen focused configuration/selection, exact-route/header/form-body, projection, confirmation-step, date-filter, effect-outcome, redaction and SDK-conformance tests (`toolkits/carrier_tests.rs`) plus future credentialed application/ad-hoc proof | Partial capability-disabled family: `get_report_by_id`, `create_excel_report` and `create_ui_excel_report` stay SDK-only because they download, unzip, merge and re-upload JMeter/Gatling archives or build `.xlsx` workbooks, which needs archive/spreadsheet writers neither the worker nor the desktop lock carries; elitea-main's per-tool capability marks them unavailable. Rust honours `settings.selected_tools` (the SDK reads it from the wrong level and always serves everything), compares `run_test_by_id`'s integer `test_id` as text (the SDK's int-vs-str comparison never matches), returns JSON values instead of `json.dumps` strings, and keeps an ambiguous effect (5xx/408/429/transport after dispatch) an unknown-outcome error instead of the SDK's failure markdown. Durable HITL, effect reconciliation, approved egress and live proof remain gates |
| SDK `configurations/figma.py`, `tools/figma/{__init__,api_wrapper,figma_client}.py` and pinned `FigmaPy==2018.1.0` at the worker-pinned revision; Main tool freezer/materializer; Python worker shared SDK adapter | Ten of seventeen Figma operations: eight REST reads/comment with the `extra_params` output reduction (`limit`, `regexp`, `fields_retain`/`fields_remove`, `depth_start`/`depth_end`) and both design-token extractors with all four batch formats; empty/subset selection and application/ad-hoc parity | `src/toolkits/families/figma/{config,client,output,tokens,tools}.rs` (in `libs/rust/agent-runtime`) | Ten focused configuration/selection, exact-route/query/header, FigmaPy-shape, Python-`json.dumps` output/reduction/regexp, token extraction/dedup/rounding, batch-format, effect-outcome, redaction and SDK-conformance tests (`toolkits/figma_tests.rs`) plus future credentialed application/ad-hoc proof | Partial capability-disabled family: `analyze_file` (LLM frame analysis over rendered images plus the 2,900-line TOON serializer; a toolkit here holds no model authority) and the six indexing tools stay SDK-only and are marked unavailable. Rust keeps FigmaPy's result attribute names (`last_modified`, `thumbnail_url`, ...) but not `simplified_dict`'s depth-3 `str()` of deeper values (Python reprs), sends `geometry`/`version`/`scale`/`format` as named query parameters (FigmaPy appended the bare value, so Figma never saw them), returns token results as JSON values, bounds batches to 100 entries and responses to 16 MiB, and adds `fancy-regex` so caller regexps keep Python's lookaround. Durable HITL for `post_file_comment`, approved egress and live proof remain gates |
| SDK `runtime/tools/data_analysis.py` over `tools/pandas/{api_wrapper,dataframe/**}` at the worker-pinned revision | `pandas_analyze_data`: LLM-generated pandas code run over a conversation attachment, charts saved as artifacts | None (blocked) | None | Blocked, not ported. It is an internal tool the worker already recognizes and skips (`agents/internal_tools.rs`), and it needs a model (code generation with up to five retries), an isolated Python runtime with pandas/matplotlib (the Pyodide code-runner behind `host::CodeSandbox`, reached today only by pipeline code nodes) and attachment-bucket artifact access, all at once. See `docs/source-mapping/configuration-toolsets.md` ("Toolkit types that cannot be ported as configured families yet") |
| SDK `tools/imagegen/{__init__,api_wrapper}.py` (pinned patch `afff2349`, #864); elitea-llm-gateway `/llm/v1/images/{generations,edits}` | `generate_image` and `edit_image` through the gateway, saved to the toolkit's artifact bucket | None (blocked) | None | Blocked, not ported. The artifact half exists (`ArtifactToolAuthority`), but nothing in the Rust runtime can call the images API under the claim: `llm-wire` has no images route, the worker's mTLS model facade binds one claim-scoped credential to one chat model, and no family is lent a model authority. See `docs/source-mapping/configuration-toolsets.md` ("Toolkit types that cannot be ported as configured families yet") |
| SDK `tools/pptx/{__init__,pptx_wrapper}.py` (`python-pptx`, `PyMuPDF`, `Pillow`) at the worker-pinned revision | `fill_template` and `translate_presentation` over a bucket's PPTX files | None (blocked) | None | Blocked, not ported. Both tools are LLM-driven: `fill_template` asks the toolkit's `llm` for structured placeholder content in batches (optionally with PDF page images rendered by PyMuPDF), and `translate_presentation` asks it to translate every text run and SmartArt node. A configured Rust family holds no model authority, and the worker and desktop locks carry no zip/OOXML writer. See `docs/source-mapping/configuration-toolsets.md` ("Toolkit types that cannot be ported as configured families yet") |
| SDK `configurations/report_portal.py` and `tools/report_portal/{__init__,api_wrapper,report_portal_client}.py`; Main configuration/toolkit catalog, freezer/materializer; Python worker shared SDK adapter | Nine project, launch, item, log, user, dashboard and raw/readable report reads, empty/subset selection, live connection-check contract and application/ad-hoc parity | `src/toolkits/families/report_portal/{config,client,tools}.rs` | Thirteen focused configuration/wire/export/text/result/bound/model-metadata/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete read family: all nine operations are present; raw HTML is bounded UTF-8, small raw PDF has a bounded base64 fallback, readable analysis uses deterministic HTML text, and the provider page index defaults to zero. Authorized materialization, egress policy, provider check composition, durable large-export artifact streaming and live proof remain gates |
| SDK `configurations/ado.py` and `tools/ado/{__init__,work_item/{__init__,ado_wrapper}}.py` at worker-pinned `b5113a1`; `azure-devops` v7.1 work item tracking routes; Main toolkit catalogue/freezer/materializer | `ado_boards`: WIQL search, work item read/create/update/delete, links, relation types, comments, wiki-page artifact links and work item type field reports, PAT Basic authentication and empty/subset selection | `src/toolkits/families/{ado,ado_boards}/` (shared `ado::{config,client,work_items,format,toolset}`) | `toolkits/ado_boards_tests.rs`: fifteen configuration/wire/result-text/effect/selection/policy/SDK-conformance tests | Partial: 11 of 13 non-index tools; `get_image_by_url` and `attach_file_to_work_item` need a vision LLM and artifact storage a toolkit does not hold here, image description is omitted, and the six index tools await Rust indexing |
| SDK `configurations/ado.py` and `tools/ado/test_plan/{__init__,test_plan_wrapper}.py` at worker-pinned `b5113a1`; `azure-devops` v7.0 test plan routes and the v7.1 work item routes the wrapper borrows; Main toolkit catalogue/freezer/materializer | `ado_plans`: test plan/suite create/read/delete, test case add/create/read, ElementTree step XML and Test Case field discovery | `src/toolkits/families/ado_plans/{config,client,steps,tools}.rs` over shared `families/ado` | `toolkits/ado_plans_tests.rs`: ten step-XML/create-params/wire/result/effect/argument/SDK-conformance tests | Partial: all 12 non-index tools; the six index tools await Rust indexing |
| SDK `configurations/ado.py` and `tools/ado/{__init__,wiki/{__init__,ado_wrapper}}.py` at worker-pinned `b5113a1`; `azure-devops` v7.0 wiki/core routes; Main toolkit catalogue/freezer/materializer | `ado_wiki`: wiki and page reads, page delete, create/update with eTag and version retry, page move, default wiki identifier | `src/toolkits/families/ado_wiki/{config,client,tools}.rs` over shared `families/ado` | `toolkits/ado_wiki_tests.rs`: eight format/route/eTag/retry/create/move/argument/SDK-conformance tests | Partial: all 8 non-index tools; LLM image description in page content is omitted (content returned unchanged) and the six index tools await Rust indexing |
| SDK `configurations/ado.py`, `tools/ado/{utils,repos/{__init__,repos_wrapper}}.py` and the shared edit/read-guard helpers at worker-pinned `b5113a1`; `azure-devops` v7.0 git routes; Main toolkit catalogue/freezer/materializer | `ado_repos`: branches, file read/list/create/update/delete, pull requests with threads/commits/diffs/work items, PR comments, commits, active-branch state | `src/toolkits/families/ado_repos/{config,client,diff,tools}.rs` over shared `families/ado` and `gitlab_org::edit` | `toolkits/ado_repos_tests.rs`: fourteen difflib-golden/date/slice/guard/push/branch/PR/comment/commit/effect/argument/SDK-conformance tests | Partial: all 15 non-index tools; the six index tools await Rust indexing |
| SDK `configurations/testio.py` and `tools/testio/{__init__,api_wrapper}.py` (pinned SDK `b5113a1`); Main toolkit schema/capability snapshots, freezer/materializer; Python worker shared SDK adapter | Thirteen Test IO customer-API v2 reads (products, features, user stories, exploratory tests, test cases, launch results, bugs) with `client_fields` projection, empty/subset selection and the SDK's result members | `libs/rust/agent-runtime/src/toolkits/families/testio/{config,client,tools}.rs` | `testio_tests.rs`: configuration/redaction, exact Token-auth wire paths and queries, SDK result members, error categories, argument refusal before network, selection/policy and the SDK conformance gate | Partial (13 of 15): `create_exploratory_test` and `confirm_bug_fix` are not served because their SDK payloads map to no current Test IO operation (see `docs/source-mapping/configuration-toolsets.md`). Auth is `Authorization: Token` as the connection check and provider require, not the runtime's `Bearer`. `list_products` applies the documented id filter itself (the SDK's dict query param encodes to `filter=product_ids`, which filters nothing); `list_exploratory_tests`/`get_exploratory_test` need a `product_id` (the SDK builds `/products/None/` or cannot pass it), so Rust adds an optional `product_id` to `get_exploratory_test` and refuses a call without one |
| SDK `configurations/testrail.py` and `tools/testrail/{__init__,api_wrapper}.py` (pinned SDK `b5113a1`); `testrail_api==1.13.4` session/category wire contract; Main toolkit schema/capability snapshots, freezer/materializer; Python worker shared SDK adapter | Sixteen TestRail tools: case/suite/section/run/result reads with suite-mode handling (`get_project` then per-suite `get_cases`/`get_sections`), `*_bulk` offset paging for results, allowlist projection with ISO-8601 timestamps and `json`/`csv`/`markdown` output; add/update/delete case, add cases, add/delete section | `libs/rust/agent-runtime/src/toolkits/families/{python_repr.rs,testrail/{config,client,format,schema,tools}.rs}` | `testrail_tests.rs` plus `format`/`python_repr` unit tests: Basic-auth `index.php?/api/v2/` wire paths and `&`-joined params, multi-suite fan-out with a skipped failing suite, Python-literal JSON text, pandas-shaped CSV and tabulate-pipe markdown, bulk paging, effect bodies and unknown-outcome mapping, argument refusal before network, selection/policy and the SDK conformance gate | Partial (16 of 23): `add_file_to_case` is not served because it uploads raw bytes from artifact storage and the runtime's artifact authority (`PlatformWriter::read_artifact`) returns decoded text, not bytes; the six inherited index tools wait for Rust indexing. Provider failures become category errors (the admission wrapper replaces family text), where the SDK returned `TestRail API error ...` text for reads. Markdown follows tabulate's pipe layout (numeric right-aligned, `g` floats) but not every tabulate type rule; `get_cases`/`get_sections` keep the SDK's first-page-only reads, results stop at 40 pages (10,000 rows) |
| SDK `configurations/xray.py` and `tools/xray/{__init__,api_wrapper}.py` as type `xray_cloud` (pinned SDK `b5113a1`); `python_graphql_client==0.4.3` request contract; Main toolkit schema/capability snapshots, freezer/materializer; Python worker shared SDK adapter | Six Xray Cloud tools: `get_tests` (paged `getTests` by the toolkit `limit`, empty preconditions dropped), `create_test`/`create_tests`/`execute_graphql` (raw GraphQL, Python-literal response text), `add_attachment_to_test_step` (`updateTestStep` with base64 data), `get_test_step_attachments` (key-to-id lookup, `json.dumps(indent=2)` output) | `libs/rust/agent-runtime/src/toolkits/families/xray_cloud/{config,client,tools}.rs` | `xray_cloud_tests.rs`: default origin and `limit` (the SDK's runtime default 20), one client-credential authentication per invocation with the token as Bearer, page variables, Python-literal and indent-2 ASCII JSON output, mutation bodies, refusal texts, JQL-injection guard, unauthorized and unknown-outcome mapping, selection and the SDK conformance gate | Partial (6 of 12): the six inherited index tools wait for Rust indexing. `add_attachment_to_test_step` serves `filedata`; a `filepath` call is answered with the explanation that artifact storage is not readable as raw bytes in this runtime (`PlatformWriter::read_artifact` returns text). GraphQL `errors` from the attachment tools are returned as the SDK's `GraphQL errors: ...` text; HTTP failures become category errors. `get_tests` stops at 100 pages or an empty page instead of looping until the count matches |
| SDK `configurations/qtest.py` and `tools/qtest/{__init__,api_wrapper,tool}.py` (pinned SDK `b5113a1`); the generated qTest `swagger_client` request paths/serialization; Main toolkit schema/capability snapshots, freezer/materializer; Python worker shared SDK adapter | Seventeen qTest Manager v3 tools: test-case DQL search and lookup with the SDK's parsed rows, generic entity DQL search/lookup, requirement/test-run/defect relationship reads over `linked-artifacts`, modules, field definitions (with the `properties`/`properties-info` fallback on 403), test-case versions; create/update/delete test cases with the full property-validation report, Jira/qTest requirement links and manual test-run status logs | `libs/rust/agent-runtime/src/toolkits/families/qtest/{config,client,search,ops,effects,fields,html,schema,tools}.rs` (+ shared `families/python_repr.rs`) | `qtest_tests.rs` plus `fields`/`html`/`search` unit tests: project/token configuration, Bearer project-scoped wire paths and search query parameters, page following, Python-literal row text, entity/relationship JSON, field fallback, property mapping and validation report, create/update/link/status bodies, version validation text, selection and the SDK conformance gate | Partial (17 of 25): `add_file_to_test_case` and `upload_attachment_to_test_run` are not served because they upload raw bytes from artifact storage and the runtime's artifact authority returns decoded text; the six inherited index tools wait for Rust indexing. `extract_images`/`prompt` are accepted but embedded images are always removed (no in-family LLM transcription). Messages the SDK raises from its own checks (unknown field, missing test run, invalid status/version) are returned as the tool's text because the admission wrapper replaces error text; provider failures become category errors. The DQL search follows `next` links page by page (the SDK always re-requests page 2) up to 50 pages; `html.unescape` covers numeric and the common named entities |
| `projects/centry/pylon_indexer/plugins/indexer_worker/**` | Current application/ad-hoc invocation, callback, checkpoint, child dispatch, and indexing behavior | `src/agents/`, `src/compat/`, then `src/indexing/` | Differential fixtures plus cross-process tests | Not started |
| `adk-rust = 2.2.0` published crates | Native agent, graph, toolset, session, checkpoint, MCP, and HITL primitives | Capability owners under `src/agents`, `src/toolkits`, `src/state` and `src/transport` | Native direct-agent/graph/session/checkpoint/model/tool/MCP/HITL component and contract corpus | Adopted selectively. Direct HTTP MCP graph nodes use RMCP challenge classification plus ADK dynamic interrupts and the existing durable session/checkpoint owner. Materializer-known root and pipeline-LLM calls use native original-call confirmation. Fixed and declared-parameter catalogue-backed HTTP MCP use Main claim-time resolution and bounded sensitive headers. External Elitea-as-MCP read-only toolkit calls use the separate durable direct-tool capability. Nested same-family authorization cards now aggregate under one paused terminal; independent partial resume, mixed-guardrail aggregation, platform OAuth/DCR, runtime-discovered configured families, MCP catalog/sync, stdio clients, effectful external toolkit calls and external agent/pipeline controls remain explicit gates |
| This reconstruction | Fail-closed diagnostic with no production registration | `src/capabilities.rs`, `src/lib.rs` | Deterministic JSON and rejection tests | Implemented; transport availability does not enable agent execution |

The tracked mapping will be expanded to source symbols and proving test files as
each slice is implemented. Intentional behavioral improvements and unresolved
contract drift must be recorded explicitly; absence from the table is never
evidence of parity.

Delegated-authorization parity note: the SDK's `McpAuthorizationRequired` is a
compatibility-named control signal used by remote MCP and by general delegated
Toolkits such as SharePoint. Rust now represents that signal in
`src/toolkits/delegated_auth.rs`, preserves it through configured-tool policy,
and handles direct `toolkit`/`mcp` graph pause plus Skip using the common
checkpoint path. Remote MCP additionally performs an exact-server token
rebuild. Bounded inline OpenAPI adds the same exact-base-URL rebuild, and the
delegated SharePoint subset rebuilds eight exact Graph reads against its frozen
site URL. Complete nested model-owned sets use the existing
public authorization ID/action contract; any broader “MCP OAuth gated” wording
above refers to independent partial resume, platform discovery/DCR,
post-token model-loop challenges, remaining SharePoint content/app-only tools
and the remaining OpenAPI capabilities rather than the implemented root,
pipeline-LLM, nested and direct-node paths.

## ADR-0030 I2: Microsoft families, 2026-10-12

The SDK behaviour was read at `elitea-sdk` `1b335d5` (0.9.68). What the Rust
connectors keep and where they deliberately differ:

### sharepoint (`libs/rust/connectors/src/sharepoint/`)

- **Scope kept:** document-library files only, filtered by `include_extensions`
  / `skip_extensions` (bare extension or glob, case-insensitive, skip first),
  scoped by `path` and `form_name`. Lists and list items are not indexed, as
  in the SDK.
- **Auth:** app-only client credential (`client_id`/`client_secret`) redeemed
  for a Microsoft Graph token at the tenant's `oauth2/v2.0/token`; the tenant
  comes from `oauth_discovery_endpoint`, else `tenant_id`, else the site host
  (`contoso.sharepoint.com` → `contoso.onmicrosoft.com`). A pre-issued
  `access_token` is used as is. The SDK's REST (`Office365-REST-Python-Client`)
  path and its delegated-token Graph path are not used: an index run has no
  user. The token is renewed a minute before expiry and once more on a 401.
  **Egress:** `graph.microsoft.com`, `login.microsoftonline.com` and the site
  host must be on `index_egress_allowlist`.
- **Listing:** Graph `drives/{id}/root/delta`, one paged walk per library,
  no per-folder requests, no page cap, as Graph documents the feed for
  SharePoint: delta is served on the drive root only (a `path` scope is
  applied after the walk to the reconstructed paths, and a folder no
  library holds is reported); an item may appear more than once across the
  pages and its last occurrence wins (a later `deleted` facet removes it);
  items carry `parentReference.id` but no `parentReference.path`, so folder
  paths are rebuilt from the feed's own folder items. A library is matched
  (`form_name`, or the first segment of a server-relative `path`) by display
  name or by the percent-decoded last segment of its `webUrl`
  (`Documents` ↔ `Shared Documents`), case-insensitively. The SDK walks
  folders breadth-first with `limit_files` (default 1000); here
  `limit_files` caps the listing — the first files by library and path —
  only when given (`0` lists nothing).
- **Version:** `cTag` (content tag), falling back to `eTag`, then
  `lastModifiedDateTime`. The SDK compares `lastModifiedDateTime`.
- **Key:** `{library}/{driveItem id}`, the SDK's item-id keying (stable
  across renames and moves within the library; a key that changed with the
  path would re-index a moved file). The media type comes from the file
  name; the human path is in the metadata (`Path`) and drives the `path`
  and extension filters. Metadata keeps the SDK names (`id`, `Name`, `Path`,
  `Created`, `updated_on`, `Link`). The generic `whitelist`/`blacklist`
  arguments are not part of the SharePoint index form (the SDK has none
  there); given anyway, they match keys, not paths — use
  `include_extensions`/`skip_extensions`.
- **Fetch:** `items/{id}/content`, following Graph's redirect to the site
  host without the bearer; PDF/Office bytes go to the extractor sidecar.
- **Not ported:** OneNote (`include_onenote` is refused with the reason: it
  needs a delegated user token and image descriptions, ADR-0030 I4).

### ado_wiki (`libs/rust/connectors/src/ado/wiki_source.rs`)

- **Scope kept:** one wiki (`wiki_identifier`, else the toolkit's
  `default_wiki_identifier`, else the run fails with that reason);
  `path_contains` (case-insensitive, `-` equal to a space).
- **Listing:** `pages?path=/&recursionLevel=full` (every page and sub-page in
  one answer, with `gitItemPath`) plus the backing repository's tree at the
  wiki's branch (`wikiMaster` by default). The SDK pages `pagesbatch` by 100
  and downloads every page to hash it.
- **Version:** the page file's blob id (the server's content hash); the SDK
  stores `sha256(markdown)`. A fetch is the blob, byte-identical to
  `includeContent=true`.
- **Key:** `{page path}.md`; metadata carries the SDK's `id` (page id),
  `path`, `title`.
- **Attachments:** opt-in (`include_attachments`), filtered by extension as
  in the SDK, versioned by blob id. Deliberate difference: keyed by path
  (`.attachments/{file}`) rather than `{page id}::{path}`, so an attachment
  linked from two pages is one document and one linked from none is still
  indexed; `path_contains` does not filter attachments. The SDK's
  `::partial::` parent re-marking has no equivalent: a failed attachment is a
  `fetch_failed` skip retried next run.
- **Not ported:** `process_images` / `image_description_prompt` (I4).

### ado_boards (`libs/rust/connectors/src/ado/boards_source.rs`)

- **Scope kept:** `wiql` (required), `fields`, `sanitize` (HTML to text,
  identities to `displayName`). Flat queries run in id windows of 2 000:
  the user's `WHERE` is wrapped as `WHERE (…) AND [System.Id] > last` (a
  top-level `OR` keeps its meaning), its `ORDER BY` replaced by
  `ORDER BY [System.Id]` and its `ASOF` kept, so more than 20 000 work items
  list; a link query (`FROM WorkItemLinks`) is read in one answer, as in
  the SDK, and refused as truncated when the answer holds the server's
  ceiling of relations (the server does not say it cut the answer).
- **Version:** `System.Rev` for the work item document; the SDK compares
  `System.ChangedDate`. Each dependent has its own version (below), so a
  field edit re-indexes the work item only and a new or edited comment the
  discussion only.
- **Document:** `work_items/{id}.json` = `{id, url, fields}` (the SDK's
  `json.dumps(fields)` plus the item's URL), JSON-chunked.
- **Attachments:** every `AttachedFile` relation, no size or extension filter
  (as the SDK), but under the connector's document cap; downloaded from the
  relation URL, which must be on the organization's host; versioned by the
  attachment's own identity (`{GUID}:{resourceSize}:{resourceModifiedDate}`
  — the bytes under one GUID never change) and linked to the work item by
  `parent_key`. `include_attachments: false` switches them off (not an SDK
  argument).
- **Comments (new, ADR-0030 "Dependent documents"):** the discussion is one
  dependent document `work_items/{id}/comments.md` (every page followed),
  on by default, `include_comments: false` switches it off; versioned by a
  SHA-256 over the comments' `id:version:modifiedDate` lines, read at
  listing time (one comments read per work item with comments; a fetch that
  finds the discussion moved on reports `Changed`). The SDK does not index
  comments.
- **Not ported:** `process_images` (I4); the toolkit `limit` setting (the SDK
  does not apply it to indexing either).

### ado_plans (`libs/rust/connectors/src/ado/plans_source.rs`)

- **Scope kept:** `plan_id` (required), `suite_ids`.
- **Listing:** suites and test cases with `x-ms-continuationtoken` followed;
  the SDK reads the first page of each. `witFields` asks for the fields the
  document needs, so no per-case work item read is made (the SDK reads each
  case's work item and discards it).
- **Version:** `System.Rev`, as the SDK.
- **Document:** `plans/{plan}/suites/{suite}/{case}.md`: title, description
  (HTML to text) and the steps rendered as a numbered list. The SDK indexes
  the raw `Microsoft.VSTS.TCM.Steps` XML alone with the `xml` chunker; the
  rendered form keeps the title and description searchable. A case in
  several suites is a document in each, as in the SDK.
- **Not ported (as the SDK):** test case attachments and shared steps.

## Known contract drift gates

The last source audit used SDK revision `5277b38`. The gateway's SDK projection
was still pinned to older revision `b5113...`; that pin must not move until each
newer runtime behavior has a Rust conformance proof. Qualified colliding-tool
binding is closed by the mapping above. Still-open audited drift includes
provider-safe empty tool results, bounded nested/pipeline output continuation,
and explicit terminal outcome/provider-error projection (including MCP
failures). These are compatibility slices, not permission to add the SDK as a
Rust runtime dependency.

- Current Centry application and ad-hoc handlers consume `truncated_content`,
  but `AgentExecutionInputV1` does not currently carry it. Rust must not invent
  a private field; the language-neutral contract owner must resolve the drift.
- `AgentExecutionTerminalStateV1` declares `PARKED_CHILDREN`, while the currently
  admitted Python/Go execution path does not yet support that state end to end.
  Rust must not advertise parked-child compatibility until cross-language
  delivery and projection tests pass.

## Code compiled cohort follow-up, 2026-10-06

The SDK remote sandbox supplies ordinary Code execution behavior without compiled-cache authority.
Worker startup now retains the selected dependency cohort for exact compiled profile selection.
Fresh unmatched Rust jobs keep ordinary execution; recorded compiled work retains its original recovery fence.
Main's exact profile admission remains unchanged.
The [source mapping](docs/source-mapping/code-compiled-cohort-selection-20261006.md) separates the failed four-language fixture from passing Cargo browser runs.
The unchanged four-language fixture and Cargo regression pass after the Worker-only correction deploys.
Replacement CI and complete-cohort restart acceptance remain required.

## Point 5 consolidation, 2026-10-06

PR 1014 is merged. Preserved graph source remains intact while missing Main and Web consumers are connected.
The [consolidation mapping](docs/source-mapping/point5-consolidation-20261006.md) records focused checks and real-backend editor acceptance.
It also records the failed Worker restart probe and its delivery latency boundary.
The later v7 Worker-loss probe proves the exact Docker recovery boundary, including browser reload and container cleanup.
Deployed Main continuation and complete-cohort recovery remain open.
The [typed failure mapping](docs/source-mapping/code-terminal-failure-20261006.md) records Code category propagation and read-only failed-journal projection.
Its focused Rust and Main checks pass. Deployed negative UI acceptance remains open.
Parallel, Map, resilience, data shaping, and HTTP actions retain their separate acceptance gates.
Workspaces remain in the post-worker backlog, outside active Point 5 scope.

## Main merge continuation, 2026-10-07

The [integration mapping](docs/source-mapping/main-integration-20261007.md) records source, snapshot, deployment-render, generation, and listener verification.
The merge preserves existing runtime work and mandatory NATS replacement.
These source checks do not prove current images, integrated CI, or candidate Code recovery.
All worker gates retain their requirement-specific acceptance boundaries.

## Code failed-journal recovery, 2026-10-07

The [owner-loss mapping](docs/source-mapping/code-failed-stop-owner-recovery-20261007.md) records current NATS recovery in chat 843.
The current application's safe terminal failure behavior remains the reference.
Main grants checkpoint authority to a fresh claim; the c53 Worker restores the original recorded failure from a fresh private spool.
The original failure and completed Code journals remain unchanged, with no Code re-execution or new dispatch.
One typed output and one FAILED settlement commit. Live and reloaded UI retain the same support reference.
All 16 isolation checks pass for each original runtime; independent HTTP 404 and absence checks prove their removal.
The old Worker and spool remain preserved and unopened.
This closes the finite failed-journal Worker-loss case, not the remaining preparation, service-loss, Kubernetes, graph, or release gates.
The source record preserves capture-matcher fixes, the restricted descriptor-based initializer repair, and earlier refused attempts.

## Code Web image and acceptance capture, 2026-10-07

The [reload mapping](docs/source-mapping/code-chat-reload-ui-20261007.md) traces persisted Main authors and selected participants into Web rendering and fresh-chat navigation.
Its immutable 42a0 source passes 76 focused Web tests, image build, and the strict local Alpine scan with zero HIGH or CRITICAL findings.
The later exact Web deployment and normal author/selection browser acceptance pass.
The frozen browser receipt is `9406b43e2e008c791f2487476d2699097287773473936b8fcde6b5830e43de5f`.

The deployed native source remains c53. No product source changes for the later preparation-test controller fixes.
Chats 839 and 844 complete normally after their capture controllers refuse before Stop or Worker loss.
These normal results do not close preparation cancellation or recovery.
Their source mappings retain the logical-to-dispatch correction and the source-defined admission transitions.

Chat845 later cancels downstream preparation after the original preparation window is missed.
Its first Code job already completes. No Worker fault occurs.
All three original runtimes are physically removed, but preparation-Stop and recovery receive no credit.

## Code live canvas terminal correction, 2026-10-08

Chat848 proves ordinary early Stop before user Code and durable cancelled History.
Its live canvas remains active after cancellation.
The [terminal callback mapping](docs/source-mapping/code-live-canvas-terminal-20261008.md) records the correction through the existing event bridge.
The canvas requires matching response and generation, then clears active nodes and sets Stopped or Error.
The selection passes75 unique source tests. Its Node26 production image and strict Alpine scan later pass.
Node26 CI and deployed browser proof remain open.
This source correction supplies no pending preparation or Worker-loss proof.

The separate [chat850 mapping](docs/source-mapping/code-pending-stop-owner-recovery-20261008.md) closes pending preparation Stop across Worker loss.
The fresh-spool replacement settles the same run under claim2/epoch2, without user Code or downstream execution.
Original runtime removal, canonical cancellation, independent acknowledgement drain, and normal reloaded History pass.
The live canvas correction and remaining service-loss and Kubernetes proofs stay separate.

The [chat851 mapping](docs/source-mapping/code-live-owner-recovery-20261008.md) closes live Worker and Supervisor loss for the original JavaScript job.
The same runtime returns count2 after both independently verified live cuts.
Main grants checkpoint recovery at claim2/epoch2. The original result projects once before the next empty-source node fails safely.
Both original runtimes are removed. NATS drains, and normal reloaded chat retains the same error code and support reference.

The [chat852 mapping](docs/source-mapping/code-compiled-publication-main-recovery-20261008.md) closes Main loss during original successful compilation publication.
The same compiler receipt becomes Ready. One result and settlement, physical removal, NATS drain, and exact live/reloaded answer pass.
Preparation outcomes, debug replacement, NATS restart, Kubernetes, and deployed canvas acceptance remain open.

## ADR-0030 I2 Atlassian families, 2026-10-12

`confluence` and `jira` are indexed by the Rust worker: `ContentSource`
connectors in `libs/rust/connectors/src/{confluence,jira}` over what
`src/atlassian.rs` shares (the SDK's credential forms, the Cloud decision,
custom headers, ADF rendering, base64 image stripping), dispatched by
`src/indexing/source.rs`, with the five index tools attached in
`elitea-agent-runtime` (`INDEX_FAMILIES`) and the catalog frozen
by elitea-main (`indexcatalog.Families`). Proving tests: the connector suites
(pagination, dependents, selection, restricted pages, 401/403, 429 with
`Retry-After`, the version pin) and `src/indexing/atlassian_tests.rs`
(Verification (2) against the `elitea-vector` fake: unchanged source writes
nothing, one changed page/issue/comment rewrites only its chunks, a vanished
document's chunks are removed).

| SDK behaviour (`tools/confluence`, `tools/jira`, pinned `b5113a1`) | Rust | Deliberate difference |
| --- | --- | --- |
| Document key is the page/issue `id`; `updated_on` is the page version's `when` / the issue's `fields.updated` | Confluence: `page/{id}.html`, versioned by `version.number`; Jira: `issue/{id}.md`, versioned by `fields.updated` | Keys carry an extension so the shared chunker router picks markdown (after the worker's HTML→markdown step) without a per-family rule; nothing stored by the SDK is read, so the key shape is free (ADR-0030 decision 6) |
| Attachments are dependents with the parent's `updated_on`; Confluence re-indexes a page when its attachment *id set* changes (`_dependents_diverged`); Jira comments are appended to the issue's `updated` | Attachments and comments are documents of their own under `parent_key`, each with its own version (Confluence attachment/comment `version.number`; Jira attachment `id:size`, comment `updated`) | A re-uploaded attachment or an edited comment rewrites only its own chunks; a parent edit never re-embeds its dependents |
| Confluence comments are appended to the page text (`include_comments`) | Each comment is `page/{id}/comment/{comment id}.html` | Comment edits are incremental; search hits name the comment |
| `limit`/`max_pages` (Confluence), `max_total_issues` (Jira, default 1000) cap the listing | Accepted and ignored; the listing follows every page; `SourceLimits::max_documents` bounds the run | ADR-0030 decision 3: list everything, no page caps |
| `include_restricted_content=false` drops pages via `is_public_page` (also requires `status == current`) | Drops pages with any read restriction; an archived page is kept when `include_archived_content` asks for it; with `include_restricted_content` the restriction's users/groups/emails become the document's `Acl::Restricted` | The LangChain status check defeats `include_archived_content`; ACLs are recorded per ADR-0028 D3 |
| `keep_markdown_format`/`keep_newlines` (markdownify vs BeautifulSoup text) | Page bodies are handed over as `text/html`; the worker converts to markdown (`elitea-doc-extract`) | One HTML path; the flags are accepted and ignored |
| `ocr_languages`, `bins_with_llm`, `process_images` | Accepted and ignored; images stay as markup, PDFs/Office go to the extractor sidecar | Model-calling steps wait for I4 |
| Jira `fields_to_index` are rendered only when also requested via `fields_to_extract` | Both lists are requested from the API | SDK defect: a field named only in `fields_to_index` was silently missing |
| Jira attachment download tries `/rest/api/3/attachment/content/{id}` then falls back; Server uses `/secure/attachment/{id}/{name}`; Confluence downloads `_links.download` | One shared downloader (`atlassian::AttachmentDownloader`) for both. Jira: the attachment's `content` URL (on the configured origin only), else the Cloud or Server path. Confluence: `_links.download` under the API root. Cloud answers with a 302/303 to `api.media.atlassian.com`: followed (at most 5 hops) to an `https` target without the credential or custom headers, and only to that host or a host the egress allowlist names (the `*` opt-out does not widen it); a same-origin redirect must stay on a download path, a redirect to a login or SSO page or back to the origin after leaving it is refused; a 200 HTML page for a non-HTML attachment is refused | A deployment must allowlist `api.media.atlassian.com` for Jira Cloud and for Confluence Cloud attachments; otherwise the attachment is skipped `fetch_failed` and the rest of the run proceeds |
| Confluence CQL results are all loaded as pages | Only content of type `page` is listed (a CQL can match comments, attachments, blog posts) | The SDK indexes pages; the other types are indexed as the page's dependents when asked |
| `is_public_page` reads only the page's own expanded restrictions; Jira ignores `fields.security` | Fail-closed: a Confluence page whose JSON lacks `restrictions.read` has them read explicitly (`restriction/byOperation/read`), a level that cannot be read restricts to nobody and the page is excluded even with `include_restricted_content`. Jira requests `security` explicitly; an issue whose response lacks the key is restricted when its project has an issue security scheme (`project/{key}/issuesecuritylevelscheme`, else `project/{key}/securitylevel`, asked once per project per run) or that cannot be established; only a project without a scheme reads absence as unrestricted | An unreadable ACL must never index a document as project-visible |
| `source` metadata is the web URL | `source` is the document key (the pipeline's rule for every family); the web URL is `url` | Shared pipeline convention |
| Page `metadata.labels` only with `include_labels` | Same; attachment labels are always recorded | — |

Follow-ups: point the agent-runtime `confluence`/`jira` families at
`elitea_connectors::atlassian` (they keep their own copies of the credential
and base-URL logic today, as the git families did before #1249); Confluence
child-page trees (`ancestors`) as `parent_key` when a CQL selects a subtree.

## ADR-0030 I3: Figma, 2026-10-12

`figma` is indexed by the Rust worker: a `ContentSource` connector in
`libs/rust/connectors/src/figma` (`config.rs` the token and `index_data`
arguments, `text.rs` the node-text extraction, `source.rs` the listing),
dispatched by `src/indexing/source.rs`, with the index tools attached in
`elitea-agent-runtime` (`INDEX_FAMILIES`) and the catalog frozen by
elitea-main (`indexcatalog.Families`). Proving tests: the connector suites
(documented REST shapes, batching and bisection, the page-too-large fallback,
401/403/404, 429 with `Retry-After`, the token-origin check, selection,
subframe extraction, projects/teams, comments, caps) and
`src/indexing/figma_tests.rs` (Verification (2) against the `elitea-vector`
fake: an unchanged file, even one saved again, writes nothing; one frame whose
text changed rewrites only its chunks; a resolved comment rewrites only the
comment; a deleted frame's chunks are removed; a refused token is an error
summary, never an empty index).

| SDK behaviour (`elitea_sdk/tools/figma`, working tree of 2026-10-12) | Rust | Deliberate difference |
| --- | --- | --- |
| A document is one leaf frame: a page's top-level node with a size (vectors without text skipped), or, when larger than `subframe_extract_threshold` (15000 px), its children, recursively; at most 50 frames a page | Same frames and the same extraction; **no 50-frame cap** (the cap bounds model calls, and none are made yet); the run is bounded by `SourceLimits::max_documents` | A cap that silently drops frames is a deletion hazard once an incremental run reads absence as removal. Ordering by position is not kept (documents are keyed, not ordered) |
| The frame's text is the TOON header (`FRAME: name [x,y WxH] type/state #id`), the model's Purpose/Goal/Visual lines, then `Headings`, `Buttons` (with an inferred destination) and `Inputs` (typed form-field heuristics); body, label, error and placeholder text is **not indexed** without a model | The header without `type/state`, then **every text node's text** by the SDK's role rules (`extract_text_by_role`: font size/weight, red or grey fill, node and path names; button and input modes come from the whole words of nested container names only, not the indexed frame's own name): Headings, Buttons, Labels, Body, Errors, Placeholders, duplicates ignoring case removed | "Text content of nodes" is in scope; the SDK keeps it only in the model's description. Inputs/CTA-destination/type/state heuristics are not ported (their labels appear under Labels/Buttons). The SDK's `000`, `xxx`, `placeholder` and `{{` text filters are dropped (they removed real text such as `1000`); `lorem ipsum` is still skipped |
| Image frames get a vision description (text-only nodes none); `type` is `image` or `text` | No model call: `needs_model` work waits for I4. `type` keeps its meaning (`text` for TEXT nodes and frames extraction could not split, `image` otherwise); `image_url` is not recorded. A frame with no text is **not listed** (its document would be a header only) | I4 adds the descriptions and lists the image-only frames then |
| Version: none per frame; the SDK re-embeds everything every run | Figma versions the whole file, so a frame's version is the SHA-256 of what is indexed (file name, page id and name, frame name and size, `type`, the text). A new file `version` with no text change writes nothing. **Stored metadata never drifts from the version:** the file's own `version` and `lastModified` (they move on every save, and would rewrite every frame) are not stored on any point; what is stored is the file key and name, the page, the frame's id, name, size, `type` and the URL with its `node-id`, all of which the version covers (a comment's version covers its text and resolved state, file name, pinned node and `parent_key`) | Incremental per ADR-0030 decision 6. Every run reads every page (Figma gives no cheaper change signal), so a run's cost is the reads, not the embeddings |
| Pages are fetched ten ids to a request; a batch that errors is bisected; a page that still fails falls back to the shallow page (`depth=1`) and its frames; a frame that fails is skipped (logged) | Same batching and bisection (400/408/413/5xx or no answer). A page that fails alone is read as its frames; also a body over the response cap; **a frame that fails, and any page or frame the listing discovered that Figma answers `null` for (`"nodes": {"id": null}`, missing or unrenderable), fail the listing**; a node the user named (URL `node-id`, `node_ids_include`) that Figma has no node for in a file is skipped with a warning, as the SDK skips a missing id | A skipped frame would read as deleted on the next run and lose its chunks |
| `node_ids_include` / `node_ids_exclude` / URL `node-id` select pages (a URL's ids win; exclude applies only without an include list); `-` or `:` spelling | Same; entries naming one file are merged (node ids unioned; a bare key, or a file a `project_ids`/`team_ids` entry reaches, widens it to the whole file); URLs must be on `figma.com`/`www.figma.com`, and a branch URL (`/design/:fileKey/branch/:branchKey/...`) indexes the branch key; a named id that is a frame rather than a page is read as a container, as in the SDK; URL spellings of instance sublayers (`I5-10;20-30`) are converted per `;` segment; layers with `visible: false` are not indexed | — |
| Text extraction stops at depth 10 | Not kept: text is read to a depth of 256 (one safety bound for every walk) and at most as many nodes as the listing page cap, so deeply nested text is indexed | The SDK's cut dropped real text of nested designs |
| `node_types_include` / `node_types_exclude` are accepted but the TOON loader never reads them | Honoured on a page's top-level nodes (include list wins over exclude) | The SDK's argument was dead |
| `number_of_threads`, `image_max_dimension`, `frame_spatial_order`, `scale_to_limit_threshold` | Accepted and ignored | Image rendering/scaling and thread fan-out wait for I4 or have no counterpart |
| Not in the SDK: `project_ids`, `team_ids`, `include_comments` | **Additions.** `project_ids`/`team_ids` expand to the files of the project (`GET /v1/projects/:id/files`) or of the team's projects (`GET /v1/teams/:id/projects`); `include_comments` lists `GET /v1/files/:key/comments` as `file/{key}/comment/{id}.txt` documents (author, time, resolved state, message) with `parent_key` the frame a comment is pinned to when that frame is listed; when the selection is restricted (URL/include node ids, excluded pages) only comments pinned inside the selected pages or frames (a reply follows its thread) are listed; versioned by their text, resolved state, pinned node and `parent_key` | The Figma API gives these cheaply; the toolkit form does not offer them yet (arguments only) |
| Auth: `X-Figma-Token` (or an OAuth bearer on the wrapper); `FigmaPy` base `https://api.figma.com/v1/` | `figma_configuration.token` as `X-Figma-Token` (sensitive header), to `api.figma.com` only: requests are built from that fixed origin and no URL in an answer is followed; the egress allowlist must name `api.figma.com` | The toolkit has no OAuth setting; none is read |
| A 4xx/5xx is a `ToolException` with the status | 401/403 (Figma answers a bad token with 403) is an error naming the credential; 404 names the file; 429 backs off within the shared bound honouring `Retry-After`, and a wait longer than the bound (Figma's can be hours) fails the run as rate limited | Fail-closed: no failure is an empty listing |

Follow-ups: I4 (vision descriptions for image frames, then list them); a form
field for `project_ids`/`team_ids`/`include_comments`; point the agent-runtime
`figma` family at `elitea_connectors::figma` (it keeps its own client, as the
git families did before #1249); `api.figma.com` in the deployment egress
allowlist (`indexEgressAllowlist`, `deploy/runtime/worker-runtime.*.json`) for
deployments that index Figma.

## ADR-0030 I3: test-management families, 2026-10-12

`testrail`, `qtest`, `xray_cloud`, `zephyr_scale`, `zephyr_essential` and
`zephyr_enterprise` are indexed by the Rust worker: `ContentSource`
connectors in `libs/rust/connectors/src/{testrail,qtest,xray}.rs` and
`libs/rust/connectors/src/zephyr/{scale,essential,enterprise}.rs` over
`src/testmgmt.rs` (HTTPS base-URL rule, header-safe credentials, bounded
arguments) and the listing scaffolding of `src/atlassian.rs`, dispatched by
`src/indexing/source.rs`, with the five index tools attached in
`elitea-agent-runtime` (`INDEX_FAMILIES`) and the catalog frozen by
elitea-main (`indexcatalog.Families`). The SDK's `zephyr` and `zephyr_squad`
toolkits have no indexer and are not served; "the three zephyr types" of
ADR-0030 are Scale, Essential and Enterprise. Proving tests: each connector's
suite (vendor-shaped fixtures: pagination, dependents, 401/403, 429 with
`Retry-After`, the version pin, credentials only to the configured origin) and
`src/indexing/testmgmt_tests.rs` (Verification (2) against the `elitea-vector`
fake: unchanged source writes nothing and embeds nothing, one changed
document rewrites only its chunks, a removed document's chunks go).

| SDK behaviour (pinned `b5113a1`) | Rust | Deliberate difference |
| --- | --- | --- |
| A test case is a `Document` keyed by `id`/`key`; chunker `json` (`markdown` for qTest) | Keys carry an extension: `case/{id}.json` (TestRail), `testcase/{id}.md` (qTest), `test/{issueId}.json` (Xray), `testcase/{key}.json` (Zephyr) | The shared chunker router picks by extension; nothing stored by the SDK is read, so the key shape is free (ADR-0030 decision 6) |
| TestRail attachments carry the case's `updated_on`; `_dependents_diverged` compares the attachment id set | Attachments are documents of their own, `case/{id}/attachment/{att}/{name}`, with `parent_key` and their own version (`id:size`) | A re-uploaded attachment rewrites only its chunks |
| TestRail: a `StatusCodeError` on one suite or at project level is swallowed (`continue`, `logger.warning`) | A 401/403/other failure on any call fails the listing, naming the credential | An index that silently lacks a suite is wrong; fail closed |
| Xray: attachments are dependents of the test; the bearer is sent to whatever `downloadLink` says | Same dependents, versioned by attachment id; a link is followed only on the configured origin or on Xray's documented Cloud hosts (global, `us.`, `eu.`, `au.xray.cloud.getxray.app`; the global host serves links for a regional tenant), never elsewhere (the credential goes to Xray hosts only), redirects through `atlassian::AttachmentDownloader` | SDK would send the token to any host in a response |
| Xray: custom `graphql` runs whatever the user wrote, a mutation included | A `mutation` or `subscription` keyword refuses the run | An index run must not change Xray |
| Xray: GraphQL errors yield `None`, a "JQL may be invalid" message | A 200 with `errors` fails with the first message, never an empty listing; a well-formed `graphql` answer with an empty list of tests is an empty listing, one with no list at all is an error | — |
| Xray `updated_on` is the first 16 hex digits of the SHA-256 of the content | The version is the full SHA-256 of the document bytes; the document also carries the Jira key and summary | The SDK kept key and summary in metadata only, so a rename never moved the version |
| Zephyr Essential: `list_test_cases()` is called with `max_results=None`, so the API's default page (10) is all that is indexed | Every page (`maxResults`/`startAt` until `isLast`) | SDK defect: an index of a project with more than ten cases was silently truncated |
| Zephyr Essential hashes the steps or script alone | The version hashes the case object with its steps or script | A renamed case or a changed label moves it |
| Zephyr Scale: `updated_on` is the last version's `createdOn`, which an in-place edit never moves | The version is the SHA-256 of the case with its steps or script (Scale stamps nothing on a case), read at listing time | An edit is seen; a listing reads each case's steps (8 at a time) |
| Zephyr Scale: the `jql` regex parser silently ignores what it does not understand; `folder`/`folderPath` that match nothing list the whole project; `customFields` and `steps` filters | Unknown keys, malformed clauses, `customFields` and `steps` are refused; a folder filter that matches no folder is an error; `limit` cuts after sorting by name; `orderBy`/`orderDirection` are accepted and ignored | Never index more than was asked for; the two refused filters are a follow-up |
| Zephyr Scale `text ~ "x"` matches `str(tc.values())` | Matches the case's VALUES only (nested values included), case-insensitively, never a field name | `text ~ "status"` must not match every case because each has a `status` key |
| Paged listings stop on a short or empty page | Xray JQL, Zephyr Scale/Essential `values`, Zephyr Enterprise (`resultSize`) and qTest DQL (`total`) treat an empty page, or a missing next link, before the reported total as an error; with no total the page-size rule stays | A silently short index is wrong; fail closed |
| TestRail attachment download: the case's `updated_on` for the attachment, redirects followed by the HTTP library | Through the shared `AttachmentDownloader` (credential to the configured origin only, shared redirect rules) with the shared 401/403 credential error; a case document is the listed case (`get_cases`), nothing is read again at fetch | One download path for every connector; no per-case `get_case` round trip |
| Zephyr Scale folders (`TEST_CASE`, `TEST_PLAN`, `TEST_CYCLE`) are yielded with empty content | `folder/{id}.json`, the folder with its path, so folder names are searchable | — |
| Zephyr Scale: only the Cloud API, `base_url` ignored | Only the Cloud API, from the regional origin the `base_url` names when its host is the EU Zephyr Scale API host (`eu.api.zephyrscale.smartbear.com/v2`); any other `base_url` (absent, a Jira URL, anything else) uses `api.zephyrscale.smartbear.com/v2` (US) and logs one warning naming the ignored host only (no path or query) | The SDK's required "Zephyr base URL" field may hold a Jira or other URL in existing configs, so an unknown host is ignored rather than refused; the token goes to the chosen SmartBear origin only. SmartBear's overview page documents the US origin; the EU origin (the EU residency region) is corroborated by third-party integrations, not by a page we could fetch |
| Zephyr Enterprise: `get_testcases_by_zql` takes the first answer as everything | `offset`/`pagesize`, the offset being the result rows received (not the distinct cases kept), until `resultSize` rows are in; a server that ignores them and answers with everything is read whole; one that owes rows and serves only repeats is an error | Never a silently short listing. The paging parameters are an assumption about the Zephyr Enterprise REST API, covered by both behaviours |
| Zephyr Enterprise indexes the steps of the last version only (via the tree id, then `testcase/versions`) | The document is the case object with the steps of its own version (`testcase.id`, which Zephyr documents as the test case version id, the id `testcase/{testcaseVersionId}/teststep` takes); its version is `lastModifiedOn`; a malformed steps body is an error, only `null`, no `steps` or a 404 is "no steps" | One id keys, fetches and reads steps, so the stamp, the case and the steps describe one version. The SDK has no other stamp; a steps-only edit that does not move `lastModifiedOn` is not seen. `GET testcase/{id}` taking that id is the SDK's usage, not a page of Zephyr's documentation we could fetch |
| qTest `full`/`module` page with `size` 100 until a page is short; `dql` pages until no `next` link; DQL "may return incomplete results" | The same | The incompleteness cannot be detected here |
| qTest `extract_images` transcribes images with an LLM | Accepted and ignored; embedded base64 images are removed | Model-calling steps wait for I4 |
| qTest keys documents by `pid` (`TC-12`) | `testcase/{numeric id}.md`, versioned by the hash of the case's modified stamp and its module's name | `pid` is renumbered by moves; the numeric id is stable; `pid` is in the metadata and the text; a module rename re-indexes its cases (the stamp does not move). Modules are bounded by the directory cap, not the document cap |
| Page-size knobs (`no_of_items_per_page`, Xray `limit`) cap nothing but a page | Page sizes only; the source's document cap bounds the run, counted against the documents kept after the TestRail/Zephyr Scale filters (rows read are bounded by the page cap) | ADR-0030 decision 3: list everything |
| Fixed SaaS hosts | The chart's default `indexEgressAllowlist` and `deploy/runtime/worker-runtime.*.json` name the Zephyr Scale Cloud API hosts, Zephyr Essential's default SaaS host (`prod-api.zephyr4jiracloud.com`) and the Xray Cloud hosts; TestRail, qTest and Zephyr Enterprise hosts (and a custom Essential `base_url`) are tenant-specific and stay operator-configured | The egress allowlist is fail-closed |

Restricted items: none of the six APIs exposes a per-test-case ACL to the
credential that lists it (TestRail roles, qTest project permissions, Xray and
Zephyr Jira permissions all gate the call, not the item), so every listed
document is `Acl::Project` and a project the credential cannot read fails the
run (401/403 named), never indexing nothing.

Follow-ups: point the agent-runtime TestRail/qTest/Xray/Zephyr families at
the connectors' config parsing (they keep their own copies, as the git
families did before #1249); Zephyr Scale `customFields`/`steps` filters;
Xray preconditions and test sets as documents; TestRail runs/results and
Zephyr cycles/executions are not documents (the SDK did not index them).

## ADR-0030 I4 (PR-A): the model-calling chunkers, 2026-10-13

The proposal and statistical chunkers are served by the Rust worker. The
chunking crate stays model-free: `elitea_doc_chunk::plan` returns a
`ChunkPlan` (the windows to send to a chat model, or the splits to embed) and
`src/indexing/model_steps.rs` fulfils it; the pipeline's `prepare` task
returns `Preparation::Model` and the runner fulfils the plan between
documents (so Stop, the deadline and the lease are checked before every
model call). The chat model is `index.llm_model` (a JSON string) with the
`temperature` and `max_tokens` of `index.llm_configuration`, which Main
already ships and the worker now reads; it is bound per call through
`ChatModelBinder` (the model facade, OpenAI-compatible over `/llm/v1`, the
claim's resource project as the model owner, as the step-back binder does).
The statistical chunker embeds with the index's own embedding model through
`EmbeddingsClient`. Proving tests: `libs/rust/doc-chunk/tests/model_steps.rs`
(structural goldens with a fixture model: windows, chunk types, order, ids,
metadata, boundaries) and `src/indexing/model_step_tests.rs` (a scripted
`Llm` and the scripted embeddings gateway against the `elitea-vector` fake:
the four chunk types stored and `extended_search` returning the document
chunk, `no_model`, a refusal failing the run with the gateway's code, a
retryable failure with its breaker and the stored version untouched, the
call cap, an unchanged document making zero model calls, a changed one
replacing its points).

| SDK behaviour (`tools/chunkers/sematic/{proposal,statistical}_chunker.py`, pinned `b5113a1`) | Rust | Deliberate difference |
| --- | --- | --- |
| Proposal: 1024-token cl100k windows (`max_doc_tokens`), one structured-output call per window, four chunks per `ChunkDetails` (`title`, `summary`, `propositions` = lines joined, `document` = the whole window with `chunk_title` / `chunk_summary` in its metadata), the same `chunk_id` for the four, counted across windows | Same windows, prompt (verbatim `CHUNK_SUMMARY_PROMPT`), order, numbering and metadata | The model is asked for the JSON object in the prompt and the reply is read tolerantly (fenced or introduced); the gateway's strict `json_schema` response format is not used because it is labelled for continuation summaries and not every provider behind the gateway takes it. A title, summary or proposal list that is blank is not emitted (a blank text cannot be embedded); its `document` chunk is |
| A failed model call is logged and swallowed: the window silently produces nothing | Never dropped silently. A retryable failure (429, 5xx, a timeout, the transport, an unreadable reply after three attempts, an empty `chunks` list) skips THE DOCUMENT as `model_failed` and leaves its stored version untouched, so the next run retries it; five MODEL-STEP documents in a row whose step failed stop the run (any model-step success, proposal or statistical, resets the streak; documents without a model step and `no_model` / `model_cap_exceeded` skips neither count nor reset it). A refusal no attempt cures (the credential, the project, the budget, an unknown model, any other 4xx) fails the run with the gateway's stable code in the message. No model bound: `no_model`. A document whose calls alone exceed the whole per-run cap: `model_cap_exceeded`; a reply cut by `max_tokens` fails the run naming it; the SDK's `<think>` text and braces are skipped when reading the JSON | The SDK lost text without a trace |
| The four chunks of a passage share a PGVector row set (random ids) | `elitea-vector` derives a point id from (project, source, namespace, generation, document key, chunk id); the four would overwrite each other, the `document` chunk written last winning. The id now covers the chunk TYPE for every chunk (no list of sibling types), and every part is length-prefixed (`services/elitea-vector/src/layout.rs::point_id`), so no part can forge another identity. Ids are never recomputed to find existing points: a changed document is replaced by a delete on its (key, namespace) filter and a fresh upsert, an unchanged one is not rewritten, so points written under the earlier id rule coexist harmlessly with new ones (each re-index of a changed document converges them). `chunk_id` / `chunk_type` with a control character are refused at the service boundary | A service change the SDK's storage did not need. A worker that writes proposal chunks against an `elitea-vector` older than this change loses three of four chunks per passage: deploy the service first. Deploying the service first is also safe for every other writer: nothing else upserts into a toolkit-index namespace, and a changed document's old points are deleted by filter, not by id |
| Statistical: 300-token splits (`max_doc_size`), embeddings in batches of 64, a running mean of the previous `window_size` (5) splits, a binary search (`dynamic_threshold`) for a threshold whose median chunk is 100-300 tokens (tolerance 10, step 0.01), else `score_threshold` 0.5, chunks joined with a space | Same arithmetic (`libs/rust/doc-chunk/src/statistical.rs`), same keys and defaults, `chunk_token_count` metadata | The whole document is planned once. The SDK planned each batch of 64 splits alone and carried the last chunk of a batch into the next batch's start: that chunk was emitted twice and the next batch repeated its text. Here `batch_size` only bounds an embeddings request (identical chunks for documents of 64 splits or fewer) and no text is emitted twice. `min_split_tokens` / `max_split_tokens` also bound the grouping (the SDK grouped with its defaults whatever the config said); a `threshold_adjustment` that is not positive takes the default (the SDK's search would not end) |
| Both chunkers take the toolkit's `embedding` and `llm` from the toolkit instance | `index.embedding_binding` and `index.llm_model` / `index.llm_configuration` of the claim's bundle; no model bound is `no_model`, never a startup error. The chat entries are fetched LAZILY and together (`ChatEntrySource`, `ChatLoader`): when a run could use the chat model (`ChunkingConfig::needs_chat_model`) the claim hands the run a loader, and the run calls it once, on the first image that needs a description or the first proposal document, so a run that never needs the model (text only, statistical only, images off) fetches nothing. Both entries load together: if either cannot be fetched or parsed the run has NO chat model (images `no_model`; a proposal document fails the run with a sentence), never default settings for a configuration that failed, and the fetch failure is recorded as a toolkit input failure in both cases. The arguments are parsed once per claim and handed to the run | The claim, not the process, names the models |
| No budget for the chunkers' calls beyond the provider's | A per-run cap on model calls (one per proposal window, one per statistical embeddings request; the final chunk embedding is not counted): `index_model_call_cap` in the worker's runtime config (default 2000; Helm `worker.runtime.indexModelCallCap`; compose `deploy/runtime/worker-runtime.rust.json`). Reaching it fails the run with a sentence; the documents written stay and a re-run continues, because unchanged versions never call a model. The gateway's budget applies as well | A guard against a surprise bill on a large source |
| A re-index re-runs the chunker on every document | Unchanged document versions are skipped before any model call (the pipeline's existing diff). A document's version carries no prompt or model hash: changing the prompt, the chunker's settings or the model needs `clean_index` to re-chunk what the source did not change | Same as every chunking setting today |
| The model steps of one run are serial across documents | Same: fetch and extract run `document_concurrency` ahead, the model calls one document at a time. In-document parallelism is a follow-up | Simplicity first; the cap and the breaker are exact because of it |

## ADR-0030 I4 (PR-B): image-file vision descriptions, 2026-10-13

A standalone image file is described by the chat model and the description is
indexed (SDK `document_loaders/EliteAImageLoader.py`, `utils.py`
`perform_llm_prediction_for_image_bytes`, `image_cache.py`,
`tools/utils/content_parser.py` `image_processing_prompt`, pinned `1b335d5`).
Planning is pure and model-free (`elitea_doc_chunk::plan_image`,
`elitea_doc_extract::image_for_vision`); the worker fulfils it
(`indexing/model_steps.rs::ModelSteps::describe`) through the same binder,
cap, liveness checks and call helper as the proposal chunker, with the image
failure policy below; `indexing/image_fit.rs` fits and rasterises pictures.
Proving tests: `src/indexing/image_fit.rs` (downscale, BMP, pixel bomb), `src/indexing/vision_tests.rs` (formats, request shape, cache,
bounds, closed skip reasons, cap accounting with retries, failure policy, the
log-safety child process), the prompt digests in `doc-chunk/src/vision.rs`.

| SDK behaviour | Rust | Deliberate difference |
| --- | --- | --- |
| `use_llm` and `use_default_prompt` default ON for `.png .jpg .jpeg .gif .webp .bmp .svg` (`DEFAULT_ALLOWED_IMAGE_LLM`); the SDK reads the extension's own block only | ON by default (owner decision 2026-10-12, SDK parity). `use_llm: false` in the extension's block, or at the top level, switches it off; such an image is skipped `unsupported_type`, whatever its bytes are (checked before the bytes are classified; the SDK indexed a placeholder document with `processing_method: none`). Top-level `use_llm` applies to IMAGE FILES ONLY: it never makes a text document `needs_model`, and `use_llm: true` on a non-image extension (embedded images, PR-C) is ignored with one warning (`index_use_llm_ignored`) | A top-level `use_llm` also counts. Because the step is on by default, `ChunkingConfig::needs_chat_model` is true for almost every run: the two chat entries are fetched for it, but an entry that cannot be fetched or read only leaves images `no_model` unless the run selects the proposal chunker (which still fails clearly) |
| Prompt: `image_processing_prompt` when `use_default_prompt` (default true), else the extension's `prompt`, else `DEFAULT_MULTIMODAL_PROMPT`; a `prompt` given without `use_default_prompt: false` is ignored by the SDK's override logic | Both prompts ported byte for byte (digest golden). The prompt, `use_default_prompt`, `max_tokens` and `token_overlap` are read from the extension's OWN block (SDK parity), defaulting to the SDK's 512 / 10: top-level text settings never leak in. A `prompt` with `use_default_prompt` unset is honoured | The SDK would silently drop a custom prompt that did not also say `use_default_prompt: false` |
| One `HumanMessage`: text part, then an `image_url` data-URL part; the bytes are re-encoded after a small-image upscale (min side 256 px) | One chat call, user message = text part then an `InlineData` part, no system message; the media type is read from the magic bytes (png, jpeg, gif, webp), not the name or the declared type (`image/jpg`, `image/pjpeg`, `image/x-png` are accepted aliases); a key with no image extension is governed by the sniffed type's block. Bytes that are none of the declared formats: skipped `unreadable` | No upscaling of small images. **Downscale**: the SDK sends the original bytes whatever their size; here a picture over the gateway's 256 KiB inline bound is decoded under strict bounds (header pixel count <= 40 MP else `too_large`, 256 MiB decoder allocation limit, 2 decodes at a time, 30 s), resized proportionally and re-encoded (PNG when it has transparency, else JPEG quality 85) until it fits; it is `too_large` only when it cannot fit or exceeds those bounds, `unreadable` when the decoder refuses it. The cache key stays on the ORIGINAL bytes. Pure-Rust `image` crate (png, jpeg, gif, webp, bmp; no default features) in the worker only |
| Image over the loader's read limit is refused; the provider rejects the rest | A picture over the facade's per-image bound (256 KiB, `MAX_INLINE_IMAGE_BYTES`) is downscaled (previous row); the run-wide `max_document_bytes` applies first | See the previous row |
| SVG (svglib) and BMP (Pillow) are rasterised to PNG before the call | **BMP is rasterised to PNG** with the same bounded decoder (always re-encoded: no model takes BMP) and described. **SVG stays `unsupported_type`**, with or without `use_llm`: a rasteriser needs fonts the distroless image does not carry, and SVG can name external resources that must never be fetched | SVG only if a font-free renderer that fetches nothing is accepted |
| Reply text is the document; blank reply becomes `[No readable text detected by LLM]`; chunked with the markdown chunker at `max_tokens` 512 / `token_overlap` 10 (loader kwargs, overridable per extension); chunk metadata `processing_method: llm` (a single chunk keeps the base document's metadata without `chunk_id`) | Same text, placeholder and chunker settings; every chunk, single or not, carries `chunk_id`, `chunk_type` and `processing_method: llm`. `original_dimensions` / `scaled` are not recorded | Uniform chunk metadata with the other chunkers |
| `ImageDescriptionCache`: in-memory LRU of 500 per toolkit instance, key md5(bytes) + md5(prompt), blank descriptions not cached | A per-run LRU of 500 in the run's `ModelSteps` (`Runner` field), key sha256(bytes) + sha256(prompt) + model name; blank descriptions and descriptions over 32 KiB are not cached. A hit makes no call and is not charged against the run's model-call cap | Per run, not per toolkit instance: the worker has no instance that outlives a run; across runs the pipeline's version skip already avoids re-describing an unchanged file. The key adds the model so a model change inside a run (it cannot happen today) never serves another model's text |
| A failed vision call raises and the document is lost or the whole load fails | The policy is the proposal chunker's for what is shared (three attempts, every attempt charged against `index_model_call_cap`, liveness before each attempt and retry wait, one shared call helper), but an image NEVER stops or fails a run for a reason of its own, and never feeds or resets the proposal failure streak. Stops the run: only the credential (`unauthorized`), the project (`forbidden`) and the budget (`budget_exhausted` family, `resource_exhausted`). Skips the image: a retryable failure after its attempts `model_failed`; the gateway's generic rejection (a text-only model, a provider 400 for one picture) `vision_refused`; `request_too_large` / `event_too_large` `too_large`; the model cannot be bound `no_model` (warned once, the rest of the run's images skip without asking); no chat model `no_model`; the call cap exhausted `model_cap_exceeded` (the next run resumes). Images have their own budget INSIDE the cap: at most half of `index_model_call_cap` (rounded down, at least one when the cap is at least one), every attempt counted; a spent image budget skips the remaining images `model_cap_exceeded`, so a proposal/statistical document always has at least half of the cap, and proposal calls keep the full cap minus what was actually spent. Three `vision_refused` in a row mark the run's model as text-only (warned once, naming it): the rest of the images are skipped `vision_refused` without a call; a success resets the streak. The cheap checks run before the expensive ones (chat model, sha256 of the original bytes and the cache, the image budget, and only then the decode and resize), so no picture is decoded for a run with no model, a cache hit or a spent budget. A fit that outlives its 30 s wait is skipped `extract_timeout`; the decode keeps its permit until it really ends, so at most two decodes run at once even after timeouts A reply cut by `max_tokens` is KEPT (SDK parity) with a warning. Image bytes and descriptions are never logged (only the facade's error code) | An unknown model reaches the worker as the gateway's generic rejection, so it reads as `vision_refused` per image (one call each) rather than a stop |
| Images embedded in PDF / DOCX / PPTX (`extract_images`, off by default) | Ported by PR-C, below | — |
| Connector `process_images` (ADO wiki/boards, Confluence `bins_with_llm`) | **Not ported**: those settings are accepted and ignored. OneNote (`include_onenote`) stays refused: it needs a delegated user token as well as descriptions | — |

Follow-ups: SVG only if a font-free renderer that fetches nothing is
accepted.

## ADR-0030 I4 (PR-C): pictures embedded in PDF, DOCX and PPTX, 2026-10-13

The pictures inside a PDF, DOCX or PPTX are described by the chat model and
the descriptions are written into the document's text before it is chunked
(SDK `EliteAPDFLoader.read_pdf_page`, `EliteAPowerPointLoader.read_pptx_slide`,
`EliteADocxMammothLoader.__handle_image` / `__postprocess_original_md`,
`utils.perform_llm_prediction_for_image_bytes`, pinned `1b335d5`). HTML, XML
and spreadsheets are NOT in this step: the SDK's loaders for them describe no
picture (the Excel loader only counts them).

The extraction sidecar carries the pictures (`elitea.extract.v1`:
`ExtractHeader.images` asks, `ExtractedText.images` / `images_skipped`
answer; `services/elitea-doc-extractor`, `libs/rust/doc-extract`). A request
that does not ask reads no picture; one that asks gets the same text as one
that does not (measured: the placeholders stay on, because turning xberg's
placeholders off drops DOCX's own `[Image: ...]` lines). The pictures are
the package's own bytes (the extractor decodes nothing it returns; a PDF's
come from the parser as PNG), recognised by their magic bytes, bounded in
count and bytes, each with a byte offset into the text where its description
belongs. Planning is pure and model-free (`elitea_doc_chunk::plan_embedded`,
`EmbeddedVisionPlan::inline`); the worker's `prepare` task returns
`Prepare::Embedded` and the run's loop describes the pictures, inlines
the descriptions and chunks (`pipeline.rs::finish_embedded`), each picture
through `ModelSteps::describe_one`, the one path an image FILE takes.
Proving tests: `services/elitea-doc-extractor/tests/socket.rs` (generated PDF,
DOCX and PPTX with one or two pictures: positions, pass-through bytes, caps,
oversized, too many, unsupported, text invariance),
`libs/rust/doc-extract` (`anchors`), `libs/rust/doc-chunk/src/embedded.rs`
(the SDK's blocks byte for byte), `src/indexing/embedded_picture_tests.rs`
(a whole run), `src/transport/extractor_client.rs` (`pictures_of`).

| SDK behaviour | Rust | Deliberate difference |
| --- | --- | --- |
| `use_llm` is OFF for documents (`DEFAULT_ALLOWED_WITH_LLM`); `extract_images` is False for PDF and PPTX and True for DOCX (whose handler transcribes with an llm, else writes `Transcript is not available`) | Per-extension opt-in: a PDF, DOCX or PPTX is described only when the extension's own `use_llm` (else the top-level one) is true; the default is OFF. `use_llm` false asks the sidecar for no picture at all, makes no call and changes nothing in the text. The prompt, `use_default_prompt` follow the image-file step (extension's own block; a `prompt` without `use_default_prompt` is honoured) | The SDK's DOCX transcribes whenever an llm is bound; here DOCX is opt-in like the other two so the three types behave alike and a deployment that never asks pays nothing |
| PDF: after each page's text, `\n**Image Transcript:**\n<d>\n--------------------\n` per picture | The same block at the end of the picture's page (the page located in the sidecar's text) | — |
| PPTX: the same block at the picture shape's place among the slide's shapes | The same block at the END of the slide's text | The sidecar's plain text has no shape order to put a block by |
| DOCX: `\n**Image Transcript:**\nImage: <file name>, <d>\n` where mammoth printed the picture; the second copy of the same bytes in the document writes `Image: <name> [already transcribed, see above]` | The same blocks. The block follows the picture's own `[Image: ...]` line (DOCX prints it for every picture); the file name is the package's (`image1.png`, else `image_<n>`); a repeat within a document is the same back-reference, and it is written only when the first copy WAS described | The SDK writes the back-reference even when the first copy failed (it then points at nothing); here a failed first copy leaves the second to try again |
| A picture that fails to describe is skipped (PDF, PPTX) or written as `Transcript is not available` (DOCX) | The text simply has no description at that place, for all three; the summary's message names the count by reason (`Embedded pictures not described: 2 no_model, 1 too_large.`). It is not a skipped document and does not make the run `PARTLY_INDEXED` | No placeholder text is indexed for an image nobody described |
| A blank description is written as an empty block (PDF, PPTX) | The SDK's placeholder sentence `[No readable text detected by LLM]` (as for an image file), so a DOCX back-reference always points at a transcript that was written | The placeholder keeps the block, and the parity with the image-file path |
| Every picture is sent, in the original bytes | At most 20 pictures per document, 5 MiB each, 12 MiB together (the worker's ask; the sidecar's caps `ELITEA_EXTRACTOR_MAX_IMAGES` 24 / `_MAX_IMAGE_BYTES` / `_MAX_IMAGE_TOTAL_BYTES` lower it). Over a bound or not PNG/JPEG/GIF/WebP/BMP by its bytes (SVG, TIFF, EMF, masks): counted `too_many_pictures` / `too_large` / `unsupported_type`, not described. A picture over the gateway's 256 KiB inline bound is downscaled exactly as an image file is | The first 20 by document order win |
| `ImageDescriptionCache` (md5 + prompt) | The run's cache of the image-file step (sha256 + prompt + model): a picture repeated in a document or across documents costs one call; a hit is not charged | — |
| No budget beyond the provider's | The image budget of the image-file step: pictures of all kinds together use at most half of `index_model_call_cap`; three `vision_refused` in a row mark the model text-only for the rest of the run; only the credential, the project and the budget stop the run | — |
| Descriptions are computed whenever the document is loaded | The document's version is unchanged by the setting: switching `use_llm` on (or changing the prompt or model) re-describes only documents that change or a `clean_index` run | Same as every chunking setting |
| Pictures are described one after another | A document's distinct pictures are described four at a time, results applied in document order; the cap and the image budget are charged atomically; identical bytes in one document are one call | — |
| (no SDK equivalent) | A picture skipped for a reason another run cures (`model_failed`, `model_cap_exceeded`, `extract_timeout`) still indexes the document, stored under `<source version>#pictures-pending-<n>` (n = the consecutive run, 1 to 3); the next run finds it changed and prepares it again; once every picture is described or skipped for a final reason (`no_model`, `vision_refused`, `reply_unusable`, `too_large`, `unsupported_type`, sidecar limits), or the document has carried the mark for 3 runs in a row, the plain source version is stored. Only retryable gateway errors, a spent call cap and timeouts are transient; a reply the model declines (safety stop) or one over the size bound is final. Connectors are told the source version, never the mark | The SDK re-describes on every load; here an unchanged document costs nothing |
| (no SDK equivalent) | With no chat model, or a model that refuses images, a document's pictures are not requested from the sidecar at all (counted in the message as `Embedded pictures not requested for documents: N no_model`) | — |
| Key without a file extension | Planned by the media type the source delivered it as (`application/pdf` is `.pdf`) | — |

Memory in the sidecar: a returned picture is held about three times (the
parser's copy, the response's copy, the encoded message). The defaults cost
at most 144 MiB per slot on top of the document (a DOCX/PPTX: the media the
parser may hold, four times the total bound; a PDF: 8 pages x 2 pictures at
the per-picture bound, its own bytes again and its text three times), held
under a quarter of the default container limit by a config test that fails
if a default grows past it. The parser reads a picture into memory before
the caps can drop it, so the extractor bounds the work first: a DOCX/PPTX
whose media outweigh four times the caps (counted from the zip's central
directory) gives no picture; xberg decodes the pictures of every page of a
PDF with no way to stop early, so a PDF is read twice, text first, and its
pictures are read only for a PDF of at most 8 pages (a longer one, or an
image-only one, gives its text and no picture, counted `too_many`). A
document with no text and pictures (a scan of at most 8 pages) is indexed
from the pictures' descriptions.
