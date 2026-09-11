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
standard OTLP traces across command verification, Redis delivery, claim,
preparation, native ADK execution, model requests, session/checkpoint writes,
output, settlement and retirement. A valid W3C `traceparent` continues the
upstream trace. `ELITEA_RUST_LOG` and `ELITEA_RUST_TRACE` remain crate-scoped,
so dependency HTTP, SQL and provider payloads cannot be enabled accidentally.
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
| Python worker `config.py::{RuntimeDeployConfig,RuntimeLimits,load_deploy_config,read_regular_file,validate_private_directory}`, `security.py::{RuntimeTrustMaterial,ExactEd25519PublicKeyResolver,_load_redis_password,load_ed25519_keyring}` and `serve.py::{serve_from_config,_serve_deployment_inner}` | Admit one credential-free deployment document, own exact trust and per-generation Redis credentials, construct private-plane clients plus `agentstate`, then compose one application/ad-hoc delivery runtime | `src/{lib,main,config,security,bootstrap}.rs`, `src/transport/redis_connector.rs`, `src/execution/production.rs` | Strict config/origin/URL/related-limit and file/symlink/permission unit corpus; exact/duplicate/malformed keyring, Redis profile/error taxonomy, explicit/missing/malformed policy snapshot, CLI and production-build error corpus; live mTLS/Redis/PostgreSQL/process proof remains | Partial capability-disabled executable composition: TLS 1.3 identity, exact-CA roots, command keys and zeroizing secrets feed concrete control/output/content/runtime-context/model channels, a bounded shared PostgreSQL pool and reconnectable Redis generation which reloads credentials and requires two successful pings. `serve` preserves the shared Python-compatible deployment document and separately requires one bounded regular-file snapshot containing the current runtime `toolkit_security` dictionary; missing or misspelled policy fields fail closed while an explicit empty dictionary is valid. The multi-thread runtime installs SIGINT/SIGTERM ownership before dependency composition, emits data-free lifecycle/error fields, stops intake, and applies one process-wide shutdown timeout across Redis and native-agent drain. Capability registration, container/orchestrator projection of the runtime dictionary, atomic policy refresh and live cross-process proof remain gates |
| `libs/proto/elitea/runtime/v1/*.proto` | Language-neutral worker command, input, output, control, and settlement contracts | `build.rs`, `src/protocol/`, `src/transport/` | Generated client compile plus Python-produced command, input, NodeEvent and output-frame binary fixtures; ACK/replay state corpus | Partial: strict agent protocol and one ordered output-session attempt are implemented; control transport, reconnect ownership and settlement are not |
| `services/elitea-worker-python/src/elitea_worker/protocol/codec.py` | Verify exact signed bytes before closed-wire command decode and validate the selected capability | `src/protocol/{command,wire}.rs` | `tests/agent_command_contract.rs` HMAC/Ed25519 vectors and authenticated mutation corpus | Partial: application/ad-hoc command admission is implemented; other capabilities and offline execution envelope are not |
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
| SDK `configurations/aha.py`, `tools/aha/{__init__,api_wrapper}.py`, shared artifact fetch helper and current/pinned Aha unit/e2e fixtures; Main toolkit/configuration catalogs, freezer/materializer; Python worker shared SDK adapter | Complete 33-tool Aha REST/GraphQL catalog, JSON/CSV/Markdown results, parent-scoped record effects, links/comments/custom-field metadata, empty/subset selection, operation groups, application/ad-hoc parity and artifact-backed attachments | `src/toolkits/families/aha/{config,client,format,artifact,tools}.rs` | Fourteen focused catalog/schema/model-description, exact-route/pagination/GraphQL/format/effect/attachment-authority/bound tests plus future credentialed application/ad-hoc/live-provider proof | Capability-disabled complete family: all 25 reads, 6 writes, 1 delete and the effectful combined execute surface are retained. Authorized materialization, live connection check, exact-interrupt HITL, cancellation-safe effect reconciliation, sealed artifact resolver and shared disk/concurrency budgets remain activation gates |
| SDK `configurations/sql.py`, `tools/sql/{__init__,api_wrapper,models}.py`, SQL connection-string fixtures; Main toolkit/configuration catalogs, freezer/materializer; Python worker shared SDK adapter | Complete PostgreSQL/MySQL two-tool catalog, committed arbitrary statement execution, default-schema table/column discovery, empty/subset selection, operation groups and application/ad-hoc parity | `src/toolkits/families/sql/{config,lexer,project,client,tools}.rs` | Eleven focused catalog/schema/model-description, statement-scanner, MySQL session-mode, backend-value, query, bound, redaction and effect-outcome tests plus future credentialed PostgreSQL/MySQL protocol/TLS proof | Capability-disabled complete family: the read and unrestricted effect are retained with typed fixed authority and bounded JSON-safe results. Exact-interrupt HITL, effect reconciliation, environment-free TLS ownership and driver preallocation controls remain activation gates |
| SDK `configurations/postman.py`, `tools/postman/{__init__,api_wrapper,postman_analysis}.py`; Main toolkit/configuration catalogs, freezer/materializer; Python worker shared SDK adapter | Complete 31-tool Postman collection, folder, request, script, search, deterministic-analysis and stored-request execution catalog; empty/subset selection, operation groups and application/ad-hoc parity | `src/toolkits/families/postman/{config,collection,analysis,client,tools}.rs` | Sixteen focused catalog/schema/model-description, configuration/redaction, analysis, exact management/effect route, path ambiguity, dynamic-auth/body and effect-outcome tests plus future credentialed application/ad-hoc/Postman/downstream proof | Capability-disabled complete family: all 8 reads, 19 writes, 3 deletes and the effect-capable execute surface are retained. Fixed management authority is separated from production-closed dynamic egress; exact-interrupt HITL, cross-worker collection fencing, cancellation-safe effect reconciliation, sealed downstream authority and live proof remain activation gates |
| SDK `tools/yagmail/{__init__,yagmail_wrapper}.py`; pinned `yagmail==0.15.293`; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete one-tool implicit-TLS SMTP family, inline secret materialization, empty/exact selection, literal MIME body, recipient projection and `write` grouping | `src/toolkits/families/yagmail/{config,client,tools}.rs` | Eleven focused configuration/schema/model-description, SMTP transcript/auth/MIME/bound, tenant-isolation, recipient-result and effect-phase tests plus future credentialed application/ad-hoc/live-SMTP proof | Capability-disabled complete family: the send effect is retained with verified TLS, bounded one-attempt SMTP and unknown-outcome handling after DATA. Rust deliberately repairs null-host and required-nullable `cc` source mismatches; exact-interrupt HITL, durable Message-ID intent/receipt reconciliation, SMTP egress and live proof remain activation gates |
| SDK `tools/keycloak/{__init__,api_wrapper}.py`; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete one-tool Keycloak Admin REST family, inline service-account materialization, empty/exact selection, arbitrary bounded HTTP method/path/body execution and `execute` grouping | `src/toolkits/families/keycloak/{config,client,tools}.rs` | Eleven focused configuration/schema/model-description, OAuth/Admin-wire/token-shape, strict-body/path, response, status, redaction, selection and effect-outcome tests plus future credentialed application/ad-hoc/live-Keycloak proof | Capability-disabled complete family: reads, writes, deletes and action endpoints remain reachable through the one generic operation over a fixed HTTPS realm. Rust repairs the SDK's session-level Basic/Bearer collision and pseudo-JSON parsing; exact-interrupt HITL, durable effect reconciliation, approved egress and live proof remain activation gates |
| SDK `tools/cloud/azure/{__init__,api_wrapper}.py`; `azure-identity==1.16.0`, `azure-mgmt-resource==23.0.1`; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete two-tool Azure Resource Manager family, inline service-principal materialization, empty/subset selection, arbitrary bounded HTTP methods plus the resource-group health read, and `execute`/`read` grouping | `src/toolkits/families/azure/{config,client,tools}.rs` | Fourteen focused configuration/schema/model-description, OAuth/ARM-wire, subscription-authority, strict-option/inline-multipart, response, status, redaction, selection and effect-outcome tests plus future credentialed application/ad-hoc/live-Azure proof | Capability-disabled complete family: both public tools and all HTTP method tokens are retained inside one fixed public-cloud subscription. Rust repairs the SDK's missing request helpers, unsafe arbitrary origin/kwargs, raw errors and unbounded transport; exact-interrupt HITL, durable effect reconciliation, approved DNS/IP egress and live service-principal role proof remain activation gates |
| SDK `tools/elastic/{__init__,api_wrapper}.py`; Elasticsearch and OpenSearch Search API contracts; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete one-tool Elasticsearch read family with explicit REST-compatible OpenSearch support, inline origin/optional encoded API-key materialization, empty/exact selection, Query DSL search and `read` grouping | `src/toolkits/families/elastic/{config,client,tools}.rs` | Ten focused configuration/schema/model-description, exact Search API wire, index/query bounds, native/vendor JSON media type, status, output, redaction, selection and tenant-isolation tests plus future credentialed application/ad-hoc/live-cluster proof | Capability-disabled complete read family: the sole public search operation is retained over one fixed verified-TLS Elasticsearch or compatible OpenSearch cluster origin. Rust repairs the absent provider dependency, string-versus-tuple API-key mismatch, disabled TLS verification, class-global client and unbounded query/response behavior; Basic authentication, Amazon SigV4, approved DNS/IP egress, application/ad-hoc materialization and live privilege/load proof remain activation gates |
| SDK `tools/cloud/gcp/{__init__,api_wrapper}.py`; Google service-account OAuth contract; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete one-tool GCP REST family, inline sealed service-account materialization, empty/exact selection, explicit OAuth scopes, arbitrary bounded HTTP methods and `execute` grouping | `src/toolkits/families/gcp/{config,client,tools}.rs` | Twelve focused configuration/schema/model-description, JWT/OAuth-wire, Google-origin/option/body, tenant-isolation, response, status, redaction, selection and effect-outcome tests plus future credentialed application/ad-hoc/live-GCP proof | Capability-disabled complete family: the generic operation retains reads, writes, deletes and actions on approved Google API origins. Rust repairs eager network construction, the SecretStr credential mismatch, arbitrary kwargs/origins, redirects and raw errors; exact-interrupt HITL, durable effect reconciliation, approved DNS/IP egress and live service-account role proof remain activation gates |
| SDK `tools/cloud/k8s/{__init__,api_wrapper}.py`; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete two-tool Kubernetes REST family, inline origin/token materialization, empty/subset selection, arbitrary bounded HTTP methods plus the `/version` health read, and `execute`/`read` grouping | `src/toolkits/families/kubernetes/{config,client,tools}.rs` | Twelve focused configuration/schema/model-description, exact-origin/path, body/header wire, response, status, redaction, selection, health and effect-outcome tests plus future credentialed application/ad-hoc/live-cluster proof | Capability-disabled complete family: both public tools and all HTTP method tokens are retained on one exact verified-TLS cluster authority. Rust removes ambient kubeconfig, disabled TLS verification, class-global client state, broken object parsing and raw errors; exact-interrupt HITL, durable effect reconciliation, approved DNS/IP egress, CA policy and live RBAC proof remain activation gates |
| SDK `tools/zephyr/{__init__,api_wrapper,Zephyr}.py`; Main inline toolkit schema/freezer/materializer; Python worker shared SDK adapter | Complete four-tool legacy ZAPI family, inline Basic-auth materialization, empty/subset selection, exact step read plus all single/batch append effects and `read`/`write` grouping | `src/toolkits/families/zephyr/{config,client,tools}.rs` | Nine focused configuration/schema/model-description, exact Basic-auth GET/POST wire, read projection, batch validation/order, shared-string bound, status, partial-effect, post-success and tenant-isolation tests plus future credentialed application/ad-hoc/live-ZAPI proof | Capability-disabled complete family: all 1 read and 3 writes are retained over one fixed verified-TLS ZAPI prefix. Rust separates the legacy inline contract from Zephyr Scale configuration, removes class-global authority/logging/retries, bounds sequential batches and returns compact receipts; exact-interrupt HITL, durable partial-effect reconciliation, approved egress and live provider proof remain activation gates |
| SDK `configurations/service_now.py` and `tools/servicenow/**`; `pysnc==1.1.10`; Main tool freezer/materializer; Python worker shared SDK adapter | Three incident tools, empty/subset selection, raw-value JSON-string results, `read`/`write` grouping and application/ad-hoc parity | `src/toolkits/families/service_now/{config,client,tools}.rs` | Eleven focused configuration/wire/projection/effect-bound/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family; per-toolkit authority replaces SDK class globals, query/response/fanout are bounded, create/update are present, and live materialization, durable HITL plus cancellation-safe effect reconciliation remain gates |
| SDK `configurations/salesforce.py` and `tools/salesforce/**`; Main tool freezer/materializer; Python worker shared SDK adapter | Six CRM tools, empty/subset selection, lazy OAuth, dedicated Case/Lead reads and effects, generic GET/POST/PATCH/DELETE, operation groups and application/ad-hoc parity | `src/toolkits/families/salesforce/{config,client,tools}.rs` | Ten focused configuration/auth/wire/result/effect-bound/model-metadata/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family; all six operations are present, fixed-origin/version routing and response fanout are bounded, weak source descriptions are clarified, and live materialization, durable HITL plus cancellation-safe effect reconciliation remain gates |
| SDK `configurations/slack.py` and `tools/slack/**`; `slack_sdk==3.35.0`; Main tool freezer/materializer; Python worker shared SDK adapter | Seven messaging, membership and workspace reads/effects, empty/subset selection, configured-channel fallback, operation groups and application/ad-hoc parity | `src/toolkits/families/slack/{config,client,tools}.rs` | Ten focused configuration/wire/result/fanout/error/model-metadata/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family; all seven source operations are present, descriptions are selection-oriented, history and collections are first-page bounded, redundant `auth.test` and unbounded member fanout are removed, and live materialization, durable HITL plus cancellation-safe effect reconciliation remain gates |
| SDK `configurations/rally.py` and `tools/rally/**`; `pyral==1.6.0`; Main tool freezer/materializer; Python worker shared SDK adapter | Eight WSAPI type/entity/context reads and create/update effects, empty/subset selection, API-key or Basic authentication, operation groups and application/ad-hoc parity | `src/toolkits/families/rally/{config,client,tools}.rs` | Eight focused configuration/auth/wire/result/effect-bound/model-metadata/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family; all eight source operations are present, the eager class-global SDK client becomes lazy invocation-scoped authority, one-page reads and descriptions are bounded, and live materialization, durable HITL plus cancellation-safe effect reconciliation remain gates |
| SDK `tools/zephyr_squad/{__init__,api_wrapper,zephyr_squad_cloud_client}.py`; Main current toolkit snapshot/freezer/materializer; Python worker shared SDK adapter | Fifteen Jira-backed Zephyr Squad step, BDD, cycle, folder and execution operations, inline credential materialization, empty/subset selection and `read`/`write`/`delete` grouping | `src/toolkits/families/zephyr_squad/{config,client,tools}.rs` | Eight focused inline-config/JWT-golden/exact-route/body/error/argument/model-metadata/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete family: all 5 reads, 8 writes and 2 deletes are present; fixed-origin JWT requests and provider results are bounded, descriptions are selection-oriented, and live materialization, exact-interrupt HITL plus cancellation-safe effect reconciliation remain gates |
| SDK `configurations/report_portal.py` and `tools/report_portal/{__init__,api_wrapper,report_portal_client}.py`; Main configuration/toolkit catalog, freezer/materializer; Python worker shared SDK adapter | Nine project, launch, item, log, user, dashboard and raw/readable report reads, empty/subset selection, live connection-check contract and application/ad-hoc parity | `src/toolkits/families/report_portal/{config,client,tools}.rs` | Thirteen focused configuration/wire/export/text/result/bound/model-metadata/policy tests plus future credentialed application/ad-hoc component proof | Capability-disabled complete read family: all nine operations are present; raw HTML is bounded UTF-8, small raw PDF has a bounded base64 fallback, readable analysis uses deterministic HTML text, and the provider page index defaults to zero. Authorized materialization, egress policy, provider check composition, durable large-export artifact streaming and live proof remain gates |
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
