# ADK-Rust adoption ledger

This ledger records what the Rust worker adopts from reviewed published
ADK-Rust sources, what Elitea wraps, and what remains deferred. The filename is
retained so existing documentation links remain stable. It keeps framework
evaluation separate from capability claims: a promising crate or passing
upstream test does not register a production worker capability.

## Decision rules

- Prefer a native ADK primitive when it preserves Elitea's admitted behavior,
  authority boundary, durability and resource limits.
- Wrap a primitive when Elitea must add tenant scope, workload identity,
  credential redemption, effect fencing, backpressure or `NodeEventV1` output.
- Reject a direct integration when it would create a second lifecycle authority
  beside Main, bypass egress policy, expose raw credentials, or claim durability
  the primitive does not provide.
- Enable optional Cargo features one reviewed slice at a time. Do not enable the
  `minimal`, `standard`, `enterprise`, `full` or `action-full` presets.

## Dependency baseline

Every direct and transitive ADK dependency now resolves to `2.2.0`. The direct
pin upgrade is a bounded model/Runner compatibility change, not an MCP
prerequisite. The architect's preceding `2.1.0` baseline already contained the
OpenAI/OpenAI-compatible and Anthropic provider corrections. The worker's
custom transports bypass the stock clients, so those corrections were reviewed
as behavior rather than assumed from the dependency bump. Elitea now preserves
open tool turns for both provider dialects, defaults GPT-5.6 Chat Completions
tool calls to reasoning `none` when Main supplies no effort, and rejects
explicit sampling for the current restricted Anthropic models before network
I/O. Provider-neutral `none` disables native Anthropic thinking by omitting
the provider's thinking and effort fields; it does not reproduce the current
Python SDK's truthy-string fallback to a 4096-token legacy budget. The
platform's frozen profile still intentionally exposes only
`none`/`low`/`medium`/`high`; newer `minimal`/`xhigh`/`max` values need a
coordinated Main/UI contract before worker admission. Provider catalogs,
pricing, custom provider credentials/headers and server-side refusal fallback
remain Main/Bifrost-owned or explicitly gated.

The reviewed `2.1.0` to `2.2.0` source delta adds Vertex Gemini RAG support,
exposes the current Runner session state through `ReadonlyContext`, and
instruments the drained agent stream. The `adk-anthropic` implementation is
source-identical across those two releases apart from package metadata.
Elitea's provider transport, claim authority and durability adapters remain
unchanged. The full offline and PostgreSQL-backed corpus must pass before this
checkpoint is pushed.

## Capability decisions

| Area | ADK-Rust evidence | Decision | Elitea owner / next proof |
| --- | --- | --- | --- |
| Agent and runner | `LlmAgent`, `GraphAgent`, `Runner`, `RunConfig`, `Event`, `AgentTool` | Adopt behind the sealed one-shot invocation state | Main claim/authorize fence, supervised lifetime, progress/terminal output and Redis retirement remain Elitea-owned. Application `agent_type=agent` and ad-hoc build one direct `LlmAgent`; configured native toolsets and variable-free saved direct children join that loop through an ADK-compatible application tool. Stored `agent_type=pipeline` builds a graph agent through the same exclusive Runner/SessionService entrypoint and adds a Checkpointer rather than wrapping direct agents in fake graphs. One strict capability-disabled native assembler now selects the exact branch from the frozen request before authority consumption and erases only the completion type, preserving the same lifecycle/projector/settlement owner. Runtime/admin policy projects `require_tool_confirmation` on every owning root or nested `LlmAgent`; pipeline HITL remains graph-owned. Capability-disabled continuation restores exact persisted calls without provider replanning; graph continuation covers configured HITL, sensitive direct tools, Printer and direct MCP authorization through the same Checkpointer. Materializer-known root and pipeline-LLM MCP challenges also resume the original provider call through native confirmation; authorization remains distinct from sensitive approval. The nested application adapter retains native child-agent execution but replaces stock `AgentTool`'s buffered child stream with bounded typed event forwarding. Native branch scoping separates descendant model history while the same root Runner persists the full recursive transcript and nested confirmations. Recursive direct rejoin through the admitted three agent tiers is proven, and a pipeline Agent node now checkpoints and replays the complete sensitive descendant set; production bootstrap/capability registration, approved-effect ownership, independent partial authorization decisions, remaining active graph nodes and context editing, a dedicated summary model and pipeline-graph context management remain closed |
| Event stream | `EventStream`, `Event`, `Content`, `Part` | Adopt as internal semantic input, not as the browser/output contract | The projector maps bounded root-model text/thinking/tool events, exact direct confirmation pauses, configured root or child-pipeline graph HITL, sensitive direct Toolkit/MCP-node interrupts and compiler-owned Printer checkpoints to current `NodeEventV1` shapes and drains through EOS. Direct saved-child agents forward typed descendant ADK events through a bounded channel before their wrapper result; recursive projectors extend the exact ordered `{name,call_id,sibling_ordinal}` hierarchy while retaining call ID as identity. Sensitive inputs are retained privately for identity but masked publicly; graph direct-tool cards bind the checkpoint/pending node/definition/visit-specific call and canonical argument digest. A nested dynamic graph interrupt privately binds the outer checkpoint plus namespaced child thread/checkpoint, but projects the same single public card without leaking those identities. Direct-child confirmations project as real hierarchical cards and persist as root-session events; a complete parallel decision set replays each exact child call, projects native confirmation-tagged results and continues the root model once. A pipeline Agent wrapper preserves the same ancestry and exposes only descendant confirmation cards; its internal graph event binds their exact ID set to the pending node/checkpoint. Parallel cards retain distinct invocation identities, completed siblings drain, and the internal pause result is removed before persistence. Printer exposes only the bounded checkpoint output as an ordinary nonterminal `agent_response`/`full_message`, not a HITL card. Elitea's capability-disabled lifecycle retains the command-bound cursor, encrypted spool, live/restored session, one pending frame and attempt budget while ACKing each batch before polling ADK again. A decision pause is durably ACKed before `PAUSED_HITL`; Printer instead settles the command normally and awaits a later ordinary continuation. Materializer-known root and pipeline-LLM delegated-authorization cards now use this durable event owner; independent partial authorization decisions, nested static Printer projection, skills, arbitrary static-breakpoint projection and production resume remain gates |
| Graph/tool custom progress | Functional `TaskContext::emit(StreamEvent::custom(...))`, graph `NodeOutput.events`, and `ToolContext::emit_progress` | Wrap; never treat raw JSON or broadcast delivery as authoritative output | Useful for allowlisted pipeline and later indexing semantic events. Functional `emit` uses a Tokio broadcast sender and silently has no effect without listeners, so durable indexing/progress must still pass through the Elitea spool/send/ACK coordinator with schema, size and redaction policy. Tool progress already enters the ADK event stream but remains gated until its call identity and current browser shape are mapped |
| Models | Provider-neutral `Llm`; OpenAI/OpenAI-compatible plus native Anthropic types | Adopt semantic/native wire types behind `ModelFacade`; wrap transport and identity policy | Construct a provider only after strict authorized profile admission consumes the claim-scoped PAT. `ModelFacade` selects the bounded OpenAI-compatible or native-Anthropic adapter over the existing Main pool and carries the frozen `model_project_id` independently from the claim actor/resource project. This intentionally fixes the SDK's execution-project substitution for shared/project-bound models. ADK supplies request/response/cache/thinking types; Elitea owns mTLS/origin, zeroizing token, model-project header, bounded/redacted SSE and exact completion. The custom adapters explicitly mirror ADK 2.1+'s open tool-turn semantics, GPT-5.6 tool/reasoning default and current Anthropic sampling restrictions because using ADK types alone does not inherit stock-client behavior. Provider-neutral `none` is an actual disable operation in Rust: native Anthropic omits thinking/effort rather than inheriting the Python SDK's truthy-string fallback. Main/Bifrost own the model catalog, pricing, credentials and selectable provider policy. Native citations, context editing, expanded reasoning vocabulary, Anthropic custom beta/refusal-fallback profiles, a dedicated summary model and pipeline-graph context management, bounded pre-stream retry and live platform proof remain gates |
| Tools/toolsets | `Tool`, `Toolset`, `BasicToolset`, typed tool macro | Adopt and wrap | Elitea owns saved configuration, selected-tool identity, grants, sensitive policy, absolute deadline, bounded concurrency and result projection. The partial GitHub family parses the claim-materialized SDK shape and shares one invocation-scoped pooled client across twenty native ADK reads. These reads cover identity, branches, files, repository navigation, issues, pull requests, commits, code search, workflow status and Project V2. A mixed explicit SDK selection retains implemented reads and omits other operations with one bounded warning. Empty selection still rejects the unported catalog. Rust preserves SDK read behavior while it adds strict resource and diagnostic bounds. A standalone UI run proves mixed-tool materialization, a real `list_branches_in_repo` call, terminal delivery and reload persistence. Google Places adds both current reads with one invocation-scoped pool and Places API (New). Sonar adds its complete project-bound read with stable errors and fixed scope. Azure Search adds both current reads with fixed index authority and one explicit 100-result request. Tool grouping remains separate from sensitivity. Deployment policy can require the shared HITL flow for any tool. Main retains public check, authorization, revision and audit ownership. Other provider component proofs, the remaining GitHub tools, App installation auth, shared HITL composition and indexing remain gates |
| Sessions | `SessionService` and Runner session integration | Adopt the interface with an Elitea PostgreSQL adapter | `src/state/postgres_session.rs` and agentstate migration `0002_agent_sessions.sql` implement a bounded `adk-session.2.0.0.v1` lineage in the separate `agentstate` database under `elitea_runtime`, leaving legacy LangGraph public tables untouched. It preserves complete ADK events, deterministic ordinals, app/user/session state tiers and exact event replay. Direct and graph agents both use this conversation/session contract; a graph agent additionally uses the separate `Checkpointer` contract for workflow frontier/state/interrupts. `AUTHORIZED_NOW` mints a non-cloneable session grant. The pipeline backend consumes it once and derives both writers from the same accepted claim/fence and supervised lease; no separate interrupt table is needed. Neither state adapter queries Main tables. This is a bounded, non-atomic two-database protocol rather than a distributed transaction. The common Runner accepts injected PostgreSQL state, restoring an existing session or seeding deterministic frozen history once. Saved applications use exact application/version identity for a secret-free definition lineage; ad-hoc chat keeps one lineage inside the tenant/project/thread-scoped session. A second claimed direct or graph request persists an explicit internal event because ADK Runner has no resume-without-user-event API; the direct model and graph input adapters remove the marker from semantic input. The pipeline checkpoint uses the private pseudonymous ADK session ID, never the public browser thread. Direct partial replay and full pipeline pause/resume/completion are proven. Local PostgreSQL 18 component proof passes. Deployment routing, pipeline continuation-suffix recovery, restricted pool/role wiring, cleanup-owner coverage, Main transcript cutover and deployed pooler/load proof remain gates |
| Context management | `EventsCompactionConfig`, `IntraCompactionConfig`, `BaseEventsSummarizer`, `LlmEventSummarizer`, feature-gated context-compaction strategy | Adopted for direct/ad-hoc agents through the same explicit owner | `src/agents/context_management.rs` admits Main's frozen contract before PAT redemption and composes the Runner's feature-gated `CompactionConfig` with `LlmEventSummarizer`. `EventsCompactionConfig` stays unused on purpose: it persists `EventCompaction` markers into the session service, which would create a second, irreversible transcript. The adopted strategy is applied through `MutableSession::replace_events`, so the custom PostgreSQL `SessionService` remains the single durable transcript and a resume reloads and recompacts the full history. Tool call/result pairs are preserved by skipping compaction while the transcript ends inside a tool loop. Context editing, a dedicated summary model, pipeline-graph context management and conversation analytics projection remain closed |
| Observability | `tracing` instrumentation below ADK plus Elitea lifecycle spans | Wrap at orchestration/tool boundaries | The binary installs a crate-only level-filtered subscriber. Authenticated prepare/lifecycle, provider-neutral assembly, configured/MCP tool calls, nested `AgentTool` calls and pipeline LLM/direct-tool nodes carry safe correlation, phase, outcome, duration and stable code fields. Node spans record only node identity, node family, selected-tool counts, structured mode and stage; prompts, history, arguments, results, URLs, credentials and provider bodies are excluded. OTLP/metrics export and deployed retention/cardinality policy remain the next local-platform gate |
| Checkpoints | `Checkpointer`; bundled SQLite implementation | Adopt exact trait with Elitea PostgreSQL backend | Implemented fresh native canonical-JSON-text lineage with current-claim fencing and shared pooling; invocation composition and cutover remain |
| Graph | `StateGraph`, `StateSchema`, native reducers, custom `Node`, nested `LlmAgent`, `AgentTool`, `SubgraphNode`, native `ToolContext`, `interrupt_with_data`, `interrupt_after`, atomic `NodeOutput::with_goto`, deferred nodes and bounded compiled concurrency | Adopt compiler substrate | The complete-document compiler/assembler admits strict stored `agent`, `hitl`, `state_modifier`, `llm`, configured `toolkit`, direct remote `mcp`, `printer`, `router` and `decision` nodes. A disconnected parallel prototype informs the proposed Agent-only V1, but the production compiler does not bind it. YAML, state, routes and behavior-bearing digests are validated before model/tool/state construction. An Agent node binds exactly one frozen saved participant: variable-free direct agents use claim-bound `AgentTool`; a saved pipeline compiles as an isolated native `SubgraphNode` over the same Checkpointer and exchanges only explicit task/messages/resume/result channels. A direct saved agent streams typed semantic events under a synthetic Application call/result, preserving exact UI hierarchy without copying events into graph state. If sensitive descendants pause, their cards remain public while one internal `interrupt_with_data` graph event binds the complete interrupt-ID set to the pending Agent node; exact approve/block replay is collision-safe across parallel leaves. Dynamic child-pipeline HITL pauses the outer graph, binds both private checkpoints into one public interrupt identity and resumes that child once through the pending Agent node. This preserves native checkpoint semantics without the Python generic-invoke wrapper or another interrupt table. Configured HITL and sensitive direct-tool confirmation use distinct schemas over the same ADK checkpoint mechanism. Runtime-owned channels use native reducers. LLM nodes bind one invocation-owned agent with exact node-selected toolsets, native structured retry and native sensitive-tool confirmation joined to the outer graph by a bounded private exact-call replay envelope. Decision binds the same claim-owned model factory without tools and may select only a declared route; Router is deterministic. Toolkit and auth-free remote MCP nodes bind one exact read-only ADK tool. A sensitive direct-node read pauses before dispatch; approval returns normal output, while denial records a same-call structured block, preserves the SDK user explanation and routes to `END` without corrupting typed state. Printer uses a compiler-owned native static pause. Rust omits the SDK custom runnable/coercion/model-wrapper and broad exception machinery. Approved-effect receipts, incremental pipeline tool-progress chunks and platform OAuth/DCR and runtime-discovered auth refresh remain fail-closed. Direct agents and graphs share Runner/SessionService; only graphs add Checkpointer. Production bootstrap, continuation-suffix recovery, deeper recursively materialized pipeline Agent nodes, child variables, nested static pauses, Code nodes, arbitrary static breakpoints and durable explicit pipeline fan-in remain gates |
| Fixed subagents and dynamic saved-participant calls | `SequentialAgent`, `LoopAgent`, `ParallelAgent`; `LlmAgent` `ToolExecutionStrategy` | Adopt where semantics match | Fixed ADK agents remain candidates for declared static orchestration. Current ad-hoc fan-out is model-selected tool dispatch: a complete Application-only tool set at the root or a nested direct child opts into native `LlmAgent` parallel execution with an eight-call run ceiling, while mixed toolsets stay sequential. A mixed owner with a sensitive descendant application is rejected until later non-application effects can be stopped safely. A bounded typed child-event adapter plus native branches lets the projector build stable recursive accordions from frozen display name, provider call ID and sibling ordinal even when container and leaf calls overlap. Parallel nested HITL aggregation, durable root-session persistence and complete decision rejoin through the admitted three agent tiers are proven; a pipeline Agent parent uses the same event hierarchy and binds the full leaf set through its graph checkpoint. Independent partial sibling replay and runtime-discovered OAuth refresh remain gates. This is distinct from the capability-closed future explicit pipeline `parallel` node |
| MCP and delegated Toolkit auth | HTTP MCP client/toolset plus Elitea's compatibility-named `mcp_auth` contract | Wrap | Claim-materialized direct plus fixed and declared-parameter catalogue-backed HTTP MCP reads enter direct agents, LLM nodes and direct graph nodes through bounded selected toolsets. Direct graph nodes preserve RMCP 401/403 challenge metadata as a guarded selected tool, emit the current `mcp_authorization_required` card through native `interrupt_with_data`, and resume the exact latest server/checkpoint after Main supplies a claim-fetched token or decline. Root and pipeline LLM owners mark that original guarded tool with native `require_tool_confirmation`: Authorize rematerializes the frozen server and replays its original provider call without replanning, while Skip returns `mcp_auth_decision` under the same call ID without dispatch. The authorization replay deliberately leaves a separate sensitive-tool confirmation pending. The SDK also raises `McpAuthorizationRequired` from general delegated-auth Toolkits, so Rust preserves one family-neutral typed signal through configured-tool policy wrappers. Nested same-family cards are emitted immediately as progress and one ordered exact-ID/call/hierarchy aggregate owns the `PausedMcpAuth` terminal. Bounded inline OpenAPI and the explicitly selected eight-read delegated SharePoint Graph core consume this exact-resource token flow; a direct SharePoint node additionally maps a real Graph 401 into the same interrupt. Main now resolves enabled prebuilt definitions after claim and admits only their declared project parameters before vault redemption. Elitea still owns browser OAuth/DCR/callbacks, authorization-card persistence, saved admission, name isolation, independent partial sibling decisions, mixed-guardrail aggregation and the external stdio runner. OpenAPI remote-spec loading, rich OAuth discovery/DCR metadata, model-loop post-token 401 handling, non-JSON/artifact bodies and remaining SharePoint content/app-only operations remain gated. Exposing Elitea agents/toolkits/pipelines as an MCP server is a separate platform capability and is not implemented by this client slice. A server-declared graph effect remains rejected before execution |
| Sensitive tools/HITL | `require_tool_confirmation`, call-ID decisions/fingerprints, dynamic graph interrupts/checkpoints | Compose | Direct and pipeline-node LLM tool calls plus read-only graph Toolkit/MCP-node calls are implemented capability-disabled. Direct calls bind a fresh public `interrupt_id` to invocation, function call, tool and canonical raw arguments while publishing masked arguments; standard fenced session events retain the transcript. A pipeline LLM node uses the same native confirmation primitive, then stores a bounded private replay envelope in the existing graph-interrupt/session binding and validates it against the graph checkpoint. Resume validates the exact input, node configuration, predecessor state, pending ADK content and provider call ID; approval produces the ordinary tool result, while Reject/Block With Comment replaces only that exact tool with the common no-effect adapter and feeds `sensitive_tool_blocked` to the native model loop under the original call ID. It neither injects a user message nor asks the model to plan the tool call again. A direct Toolkit/MCP node has no LLM loop, so it pauses through `interrupt_with_data` before provider dispatch and binds the public identity to the latest session interrupt plus private thread/checkpoint, pending node, definition, `pipeline:<node>:<step>` call ID and canonical argument digest. Its approval consumes the resume value and yields the ordinary projection; denial executes no provider call, records the same structured result, preserves the formatted chat explanation, leaves typed outputs untouched and goes directly to `END`. A configured HITL inside a saved-pipeline Agent participant additionally binds and validates the exact parent and child checkpoints before forwarding its one-use decision. Nested direct-agent confirmations use the same native mechanism and root session: a bounded complete parallel decision set re-emits the original Application calls through each admitted tier, installs one-use child replay plans by owning invocation plus call ID and continues only after every exact child result exists. A pipeline Agent node converts that exact descendant ID set into one internal graph interrupt while keeping descendant cards as the only browser events; incomplete sets, stale routes and colliding provider call IDs in distinct branches fail closed before dispatch. Main remains the atomic browser-decision owner; Rust rejoins direct, nested and graph decisions to durable raw state. No path adds an interrupt table. Approved-effect receipts, incremental pipeline tool-progress chunks, platform OAuth/DCR, runtime-discovered auth refresh, nested static pauses and continuation-suffix recovery remain closed. Identical name/arguments can be a new call; neither arguments nor name alone authorize it |
| Action HTTP | Graph action creates a fresh unrestricted `reqwest::Client`, interpolates URL/auth and includes dependency text in errors | Do not enable directly | A future Elitea HTTP capability must enforce approved origins, DNS/IP/redirect policy, workload credential references, response/body bounds, deadlines and redacted errors; the ADK config/action shape may be reused behind it |
| Action database | ADK 2.0.0 validates config but its SQL/Mongo/Redis executors are explicit placeholders | Do not enable the placeholders | The current product SQL toolkit is implemented separately with backend-specific SQLx ownership, fixed claimed authority, bounded results and effect semantics; other database actions still require dedicated contracts |
| Action code / code tools | Optional action/code/sandbox feature families exist | Separate plan | Current code-node parity requires a reviewed isolation boundary, filesystem/network policy, resource metering and artifact contract; it is not enabled inside the main worker process by this replatform slice |
| Managed runtime | Optional `managed-runtime` exposes another lifecycle abstraction | Defer | Its façade is a useful shape, but the audited implementation keeps its checkpoint manager, sequence counter, parking lot and active channels in process. Injecting the PostgreSQL `SessionService` would persist conversation events, not the parked execution frame. Main plus the worker delivery coordinator therefore remain the only execution authority; revisit only as an internal adapter after proving restart-safe external checkpoint/parking injection cannot duplicate claims, settlement or recovery |
| Semantic memory | Optional memory service and database/Redis/SQLite backends | Defer and wrap | Define tenant/project/agent namespace, consent, retention, deletion, embedding/provider and poisoning policy before enabling global agent or graph memory |
| Realtime/voice | Optional realtime runner and transport/provider features | Defer and wrap | Map current voice/ASR/TTS session behavior, cancellation, quotas and `NodeEventV1`/media boundaries first; do not enable the enterprise preset |
| Redis | ADK offers optional execution-state backends | Do not substitute | The restricted redis-rs transport owns Elitea command intake, PEL reclaim/heartbeat and exact post-settlement retirement. The production connector reloads the ACL password and TLS files, opens and pings both restricted connections, and installs them through the serialized generation owner; retryable loss replaces only that generation without replaying an ambiguous command. The executable process owns SIGINT/SIGTERM and one global drain deadline around the stop-aware delivery runtime plus native processor. A real Redis 7 reconnect/reclaim system test remains. A later memory/session Redis backend must use separate keys and failure semantics |

MCP ownership boundary: Main owns persisted MCP definitions, project policy,
credential and token authority, OAuth/DCR/callback orchestration, public
authorization contracts and audits. The Rust worker owns admitted MCP
transport sessions, discovery, selected-tool materialization, invocation,
durable confirmation/replay and runtime event projection. UI is the final
configuration, decision and progress layer. Current SDK, Indexer and Core
implementations remain source evidence for observable business behavior; they
must not become extra authorities or Rust runtime dependencies. ADK MCP
primitives should replace worker-local transport/discovery mechanics when they
preserve these boundaries, resource limits and exact replay semantics.

Provider tool-name isolation: the SDK's toolkit-qualified binding behavior is
now implemented once at the Rust model boundary. `src/toolkits/tool_binding.rs`
freezes admitted toolsets, preserves unique provider names, creates bounded
deterministic toolkit-qualified aliases for collisions, and delegates calls to
the exact original tool. Root and nested direct agents and pipeline LLM nodes
share the plan. Sensitive and delegated-authorization catalogs bind the alias
back to the original toolkit identity and provider call ID. Direct Toolkit/MCP
graph nodes remain exact scoped calls. Generated alias tests and complete
direct/pipeline LLM component stories cover two same-named MCP tools in one
model response. Any older broad row above that lists name isolation as a gate
is superseded by this checkpoint.

Delegated-authorization status: broad “MCP OAuth remains gated” wording in the
Graph, Event and Sensitive/HITL rows now means Main activation, platform
OAuth/DCR and concrete unported configured families. It does not include the
direct-node path or materializer-known remote-MCP calls in root/pipeline LLM,
direct saved-agent or pipeline Agent-node hierarchies; those Authorize/Skip
paths are implemented as described in the dedicated row. Rust intentionally
replaces the Python proxy plus model-called auth-control round trip with native
confirmation on the original guarded call.

Pipeline LLM progress checkpoint: ADK's nested `LlmAgent` remains the model/tool
loop owner, while `src/agents/graph/node_events.rs` multiplexes its native
model/tool lifecycle events into the outer Runner stream through one bounded
invocation-local channel. The bridge strips provider request payloads, rebinds
root invocation identity and stamps the owning graph node; no browser event is
stored in graph state. The event projector closes a denied call under the
original tool-call ID and suppresses the duplicate graph terminal model turn.
Saved-pipeline Agent nodes now emit the ordinary Pipeline wrapper start/end and
route inner LLM/model/tool events through the same call-bound descendant
projector used by direct saved agents. The public stream therefore keeps the
existing `parent_agent_path`, `parent_agent_call_id`, `sibling_ordinal` and
`langgraph_node` contract. Configured and sensitive-LLM child interrupts now
retain that wrapper hierarchy while their original root/child checkpoint
binding remains the public decision identity. The explicit subgraph bridge also
forwards the LLM replay channel, so denial returns `sensitive_tool_blocked`
under the original call ID without a fresh planning turn. Raw incremental
tool-progress chunks remain closed.

The toolset foundation also contains the complete capability-disabled
`ServiceNow` incident family: one bounded read plus create and update effects,
all backed by one invocation-scoped origin/credential pool and exact SDK result
projection. The effects are implemented rather than omitted. Production
composition remains closed until authorized materialization, provider proof and
the shared durable sensitive-tool pause path can bind the exact invocation.
Write activation additionally requires an owned cancellation-safe effect
identity/reconciliation boundary: dropping an ADK tool future must never turn
an accepted but response-lost remote mutation into an ordinary cancellation.

The complete capability-disabled `Salesforce` family adds six native ADK
tools in SDK order: two creates, one SOQL read, two updates and the generic
GET/POST/PATCH/DELETE escape hatch. One invocation-scoped client owns the
claim-materialized nested credential, lazy OAuth token, fixed HTTPS/version
root and bounded response projection. All effects are present, but production
composition remains closed until authorized materialization, provider proof,
the shared exact-`interrupt_id` sensitive-tool flow and a cancellation-safe
effect identity/reconciliation owner are composed. Operation grouping remains
model/catalog metadata and never grants execution authority.

The complete capability-disabled `Slack` family adds all seven current SDK
tools in source order: three messaging/membership effects and four bounded
conversation/workspace reads. One invocation-scoped zeroizing token owner uses
the fixed Slack Web API origin with no redirects or automatic retry. Rust
preserves configured-channel fallback and success projections while removing
the redundant history `auth.test`, bounding every first page and running
member-info fanout with an eight-request structured concurrency limit.
Selection-oriented tool and parameter descriptions are part of the tested
contract. Production composition remains closed until authorized application
and ad-hoc materialization, live Slack scope/rate-limit proof, the shared exact
`interrupt_id` sensitive-tool flow and a cancellation-safe effect
identity/reconciliation owner are composed; reads may be sensitive regardless
of their catalog group.

The complete capability-disabled `Rally` family adds all eight current SDK
tools in source order: six WSAPI reads plus create and update. One lazy,
invocation-scoped client owns the claim-materialized API key or Basic
credential and exact HTTPS origin; it replaces the SDK's eager class-global
`pyral` client. Reads use bounded first pages and structured query encoding,
Basic effects obtain a same-session security token, and neither effect is
retried after dispatch ambiguity. Tested descriptions expose type aliases,
query syntax, context fallback, field/identifier requirements, result shape
and effect risk to the model. Production composition remains closed until
authorized application/ad-hoc materialization, live Rally proof, the shared
exact-`interrupt_id` sensitive-tool flow and cancellation-safe effect
identity/reconciliation are composed. Read/write grouping remains metadata,
not approval authority.

The complete capability-disabled `GitLab Org` family is a separate toolkit
from the standard GitLab catalog and adds all seventeen current SDK operations
in source order: eight reads, eight writes and one delete. One claim-scoped,
zeroizing private-token client owns the accepted HTTPS GitLab origin and either
a strict configured project allowlist or the source-compatible dynamic-project
mode. Active-branch state is invocation-local and the complete toolset is
serialized so a concurrent model loop cannot race branch selection with a
repository effect. Rust preserves the current OLD/NEW file-edit and merge-
request diff-position functionality while bounding project pages, files,
commits, diffs, edits and results. The explicit limits include a 1 MiB decoded
read source, 200,000-character plus 512 KiB serialized read result, and 256 KiB
writable result/edit. It also fixes the SDK's eager construction I/O, unbounded
pagination, cross-project append, error-as-missing create probe and raw provider
failures. Production composition remains closed until Main binds dynamic mode
to explicit organization-wide repository authority, the
delegated GitLab connection check and live provider proof are composed, and the
shared exact-`interrupt_id` HITL plus cancellation-safe effect receipt/
reconciliation owner protects all remote effects. Operation grouping remains
metadata, so any GitLab read may also be configured as sensitive.

The complete capability-disabled `ReportPortal` family adds all nine current
SDK tools in source order. Every operation is a read: launch exports and
details, paged launch/item/log collections, individual test items, users and
dashboards. One invocation-scoped zeroizing Bearer client owns the
claim-materialized project and HTTPS origin, replaces the SDK's class-global
client, percent-encodes authority components, disables redirects and retries,
and bounds every request, response and tool result. Raw HTML is explicit UTF-8,
small raw PDF has an explicit base64 conformance fallback, and the readable
report uses a deterministic bounded HTML-to-text projection rather than the
SDK's broken PDF-as-HTML branch. The inline PDF bound is not the production
download architecture: large exports require durable artifact/object-store
streaming and a reference result before activation. Generic PDF analysis should
extract text page by page and render only scanned or visually relevant pages for
a multimodal model, rather than rasterizing every page by default. Tested
descriptions expose the operation, identifier, page/format
default, bounds and result shape without leaking endpoint or project details.
Production composition remains closed until authorized application/ad-hoc
materialization, approved egress, configuration-catalog projection, durable
large-export artifact streaming and a live provider proof are composed.
Although no effect owner is required, any report,
log or user read may independently require the shared exact-`interrupt_id`
sensitive-tool policy.

The complete capability-disabled `Aha` family adds all thirty-three current
SDK tools in source order: 25 REST/GraphQL/search/metadata reads, six writes,
one delete and the legacy combined create/update/delete execute surface. The
current and worker-pinned SDK implementations are functionally identical;
current adds only operation-group metadata. One invocation-scoped zeroizing
Bearer client owns the claim-materialized HTTPS account origin, disables
redirects and retries, validates expected pagination collections and bounds
every page, body, JSON value, formatter result and diagnostic. Native
deterministic JSON/CSV/Markdown replaces optional pandas/tabulate behavior.
Record parent scopes, GraphQL Markdown reads, comments, all seven link codes,
release duplication, custom-field metadata and every mutation are retained.
The combined `manage_record` is authorized according to its exact requested
action rather than its generic `execute` label. `attach_file` accepts only an
authorized Elitea `/{bucket}/{filename}` artifact reference: content is
downloaded to a private bounded temp spool, immutable version/length/SHA-256
are verified before dispatch, then the verified file is streamed once as
multipart `attachment[data]`. This preserves files below Aha's 300 MB provider
limit without a same-sized memory buffer. Production composition remains
closed until authorized application/ad-hoc materialization, live Aha egress and
connection proof, the sealed artifact resolver plus shared disk/concurrency
budget, exact-`interrupt_id` sensitive-tool continuation and cancellation-safe
effect receipt/reconciliation are composed. Any read may be sensitive; group
metadata grants no authority.

The complete capability-disabled `SQL` family adds both current SDK actions:
one unrestricted committed statement executor and one default-schema table/
column discovery read. PostgreSQL and MySQL have separate SQLx connection and
row projection paths so normal decimal, temporal, UUID, JSON, binary and array
results are not narrowed to SQLx `Any` primitives. Rust repairs the registered
integer-port mismatch, replaces credential-bearing DSNs and unbounded
reflection fanout, enforces a single bounded statement and finite results, and
never echoes SQL or database authority. MySQL keeps the provider's existing
session SQL mode and rejects `NO_BACKSLASH_ESCAPES` before user-statement
dispatch instead of silently replacing tenant policy. `execute_sql` is always an effect;
post-dispatch failure is a nonretryable unknown outcome. Production composition
remains closed until application/ad-hoc materialization, exact-`interrupt_id`
approval, cancellation-safe effect reconciliation, owned TLS/trust, real
PostgreSQL/MySQL proof and a response preallocation boundary are composed.

The complete capability-disabled `Postman` family adds all thirty-one current
SDK tools in source order: eight reads, nineteen writes, three deletes and one
stored-request execute surface. One claim-scoped zeroizing client owns only the
configured Postman HTTPS management origin and disables redirects and hidden
retries. Collection traversal, request/folder resolution, JSON bodies, provider
responses and projections are bounded. The SDK's deterministic analyzer is
retained for collection, folder and request security, performance,
documentation, testing, issue, score, recommendation and improvement results;
read projections redact stored credentials and payload values without changing
raw-field search matching. All management mutations are implemented but remain
closed behind exact-`interrupt_id` sensitivity, durable per-collection fencing
and cancellation-safe effect receipt/reconciliation. `execute_request` has a
separate structurally closed dynamic-egress authority because reading a saved
Postman URL cannot authorize downstream HTTP. Activation additionally requires
claim-bound origin and DNS authorization, SSRF and redirect controls, dangerous
header filtering, secret-safe output, and a distinct downstream effect receipt.
The disabled executor preserves request-level auth fallback and commented-JSON
behavior without inheriting broader collection/folder credentials or corrupting
quoted `//` URL text.
No public connection check is advertised: the configuration contract marks it
unsupported and the source toolkit's nominal checker addresses fields that do
not exist on its generated nested model.

The complete capability-disabled `Yagmail` family adds its one current SDK
`send_gmail_message` write effect from inline claim-materialized settings. A
non-debuggable zeroizing client owns the frozen DNS host, username and password;
implicit TLS port 465 verifies native roots and hostname, SMTP authentication
supports the source mechanisms, and the bounded MIME builder always treats the
message as literal text rather than a local path. The client makes one send
attempt, returns the source empty object on complete acceptance, reports partial
recipient refusal without diagnostic text, and converts every ambiguous failure
after DATA begins to a nonretryable unknown outcome. Rust intentionally makes
`cc` optional-null, matching the Python method/runtime default rather than its
accidentally required generated schema. Production composition remains closed
until exact-`interrupt_id` approval, a durable pre-send Message-ID intent and
effect receipt/reconciliation owner, claim-only secret redemption, fixed-host
SMTP egress/DNS enforcement and live verified-TLS proof are composed. The
`write` group alone grants no authority and sensitivity remains independent.

The complete capability-disabled `Keycloak` family adds its one current SDK
generic Admin REST `execute` operation from inline claim-materialized settings.
All bounded HTTP method tokens remain available, so reads, writes, deletes and
action endpoints are not omitted. One non-debuggable invocation client owns the
frozen HTTPS origin/context path, realm, client ID and zeroizing client secret;
each call makes one client-credentials token request and one Bearer Admin API
request with no redirects, hidden retries or token refresh. Rust deliberately
separates form credentials from Admin Bearer auth, replacing the SDK session's
Basic/Bearer collision, and accepts only a strict bounded JSON-object body rather
than single-quote rewriting. Tool metadata explains path/query/body semantics,
confidential reads, effect scope, response limits and unknown-outcome retry risk.
Production composition remains closed until exact-`interrupt_id` approval,
durable effect intent/receipt reconciliation, claim-only secret redemption,
fixed-origin DNS/IP egress enforcement and live Keycloak role/provider proof are
composed. The `execute` group is metadata and grants no authority.

The complete capability-disabled `Azure Resource Manager` family adds both current SDK cloud
operations in source order: the generic ARM `execute` surface and its read-only
resource-group health tool. One invocation-scoped client owns the frozen
public-cloud subscription plus zeroizing Microsoft Entra client secret, admits
the exact subscription-scoped `management.azure.com` authority, obtains one
client-credentials token, and performs one bounded Bearer request with no
redirect or automatic retry. Rust deliberately repairs the SDK's missing JSON
and domain helpers, replaces unscoped request kwargs with explicit bounded
headers/query/JSON/form/inline-multipart options, and never interprets a file
value as a local path. All bounded HTTP method tokens remain available, so
write, delete and action endpoints are not omitted. Tested descriptions explain
the absolute URL/API-version contract, option shapes, confidential reads,
effect risk, 512 KiB result ceiling, one-attempt behavior and reconciliation.
Production composition remains closed until exact-`interrupt_id` approval,
durable effect intent/receipt reconciliation, claim-only secret redemption,
public-cloud DNS/IP egress enforcement and live Azure role proof are composed.
The `execute`/`read` groups remain metadata and grant no authority.

The complete capability-disabled `Elasticsearch` family adds its one current
SDK `search_elastic_index` read from inline claim-materialized settings. One
invocation-owned client binds an exact verified-TLS cluster origin and optional
zeroizing encoded API key, performs one bounded `POST /{index}/_search`, and
never inherits proxy, redirect, retry or disabled-certificate behavior. Rust
deliberately repairs the missing Python dependency, string-versus-tuple API-key
mismatch and class-global client. The Query DSL object, index expressions,
result window, provider body and 512 KiB model result are bounded while the
native search response remains intact. Tested descriptions explain
Elasticsearch and REST-compatible OpenSearch targeting, wildcard and comma
forms, Query DSL shape and post-7.10.2 divergence, first-response-only behavior,
confidential-data risk and expensive-query cues. The shared search wire works
with anonymous or compatible API-key OpenSearch clusters; Basic-authenticated
and Amazon SigV4 domains remain explicit unsupported activation gaps.
Production composition remains closed until claim-only materialization, exact
cluster DNS/IP egress and live authentication, index-privilege and query-load
proof are composed. The `read` group remains metadata and sensitivity stays
independent.

The complete capability-disabled `GCP` family adds the current SDK's one
generic `execute_request` operation from inline claim-materialized
service-account JSON. One invocation-owned client validates and zeroizes the
PKCS#8 key, signs a one-hour RS256 JWT for one to thirty-two explicit official
Google OAuth scopes, exchanges it at the fixed token endpoint, and sends one
bounded Bearer request only to `googleapis.com` or a subdomain. It inherits no
ambient credential, proxy or redirect authority and performs no automatic
retry. Every bounded HTTP method token remains available, so reads, writes,
deletes and action endpoints are not omitted. Tested descriptions explain the
scope/origin contract, option shapes, confidential-read risk, 512 KiB result
ceiling, 202 semantics, one-attempt behavior and reconciliation. Production
composition remains closed until exact-`interrupt_id` approval, durable effect
intent/receipt reconciliation, claim-only key redemption, Google OAuth/API
DNS/IP egress enforcement and live service-account role proof are composed.
The `execute` group remains metadata and grants no authority.

The complete capability-disabled `Kubernetes` family adds both current SDK
cloud operations in source order: generic `execute_kubernetes` and the
read-only `/version` health tool. One invocation-owned client requires the
claim-materialized exact HTTPS cluster origin and zeroizing Bearer token,
verifies native-root TLS, rejects redirects and ambient proxy/kubeconfig
authority, and performs one bounded request without automatic retry. Rust
deliberately repairs disabled certificate verification, class-global client
state, object inputs passed through `json.loads`, status-blind health and raw
provider errors. Every bounded HTTP method token remains available, so writes,
deletes, patches and action subresources are not omitted. Tested descriptions
explain the exact sub-URL/query contract, body/header shapes, patch media type,
confidential reads, 512 KiB result ceiling, 202 semantics, one-attempt behavior
and reconciliation. Production composition remains closed until exact-
`interrupt_id` approval, durable effect intent/receipt reconciliation,
claim-only token redemption, cluster DNS/IP plus certificate authority policy,
and live RBAC proof are composed. The `execute`/`read` groups remain metadata
and grant no authority.

The complete capability-disabled legacy `Zephyr` family adds all four current
SDK tools in source order: one test-step read and three single/batch append
effects. This toolkit uses inline Basic-auth settings and is deliberately
separate from the configured, indexing-backed `zephyr_scale` family and the
fixed-cloud Zephyr Squad family. One invocation-owned client binds the frozen
HTTPS ZAPI prefix, verifies TLS, disables redirects, proxies and retries, and
performs one bounded GET or POST per step. Batch inputs are completely
prevalidated and limited to 50 steps per case, 20 cases, 100 total steps and a
64-KiB encoded JSON string before sequential dispatch. Tested descriptions
explain exact input shapes, selection cues, duplicate/partial-effect risk and
reconciliation; compact batch receipts avoid echoing the model's payload.
Production composition remains closed until authorized application/ad-hoc
materialization, approved endpoint egress, a harmless live legacy-ZAPI read,
the shared exact-`interrupt_id` sensitive-tool flow and durable single/partial
effect receipts are composed. The read/write groups remain metadata and grant
no authority.

The complete capability-disabled `Zephyr Squad` family adds all fifteen
current SDK tools in source order: five reads, eight writes and two deletes
covering test steps, BDD content, cycles, folders and executions. Its inline
claim-materialized account/access/secret settings remain distinct from the
separately configured Zephyr variants. One invocation-scoped client signs the
exact fixed-origin route with a five-minute HS256 JWT, preserves query order
for QSH, disables redirects and retries, and bounds JSON objects, response
bodies and model output. All effects are implemented, but production
composition remains closed until authorized application/ad-hoc
materialization, a harmless live credential read, the shared exact
`interrupt_id` sensitive-tool flow and cancellation-safe effect
identity/reconciliation are composed. Read/write/delete grouping is catalog
metadata and never grants effect authority.

Broad table rows above that still call MCP OAuth/on-demand authorization a gate
refer to platform OAuth/DCR, post-token model-loop challenges, remaining
SharePoint content/app-only operations, the documented remaining OpenAPI
capabilities and production activation. Root, pipeline-LLM,
direct nested-agent and pipeline Agent-node materializer-known authorization
paths are implemented by the specific MCP/delegated-Toolkit row.

## Future explicit pipeline parallel-node decision

This customer-requested YAML node is a graph construct. It is not the current
model-selected saved-participant dispatch.

The code in `src/agents/graph/{yaml,parallel}.rs` is a disconnected prototype.
The proposed V1 redesign is documented in
`docs/parallel-pipeline-node-design.md`.

V1 runs owned Agent nodes only. It supports `wait: all`, fail-after-drain,
declared result order, and at most eight active branches.

The production compiler does not bind it yet. Compiler ownership, nested
interrupts, crash recovery, UI progress, and live chat remain gates.

ADK 2.2.0's deferred tracker and timeout origins are not part of `Checkpoint`.
Elitea therefore does not compile this feature as native deferred fan-in and
does not use `NodeContext::run_node_with`, whose child ledger is saved only
after the parent returns. Each branch is a small ADK `CompiledGraph` with a
claim-fenced descendant checkpoint thread. ADK writes the child's terminal
empty-frontier checkpoint before its invocation returns. After a crash, the
same parent activation reopens completed children at terminal state and runs
only unfinished children. The child thread digest binds the opaque execution,
generation and graph definition, root thread, parent step, complete parallel
configuration, branch ID, target, ordinal and bounded canonical business-input
digest. Resume controls are excluded from that digest. A later loop visit or
changed business input therefore cannot consume an earlier result.

The upstream action `WaitAll` implementation is not used as the reducer. It
accepts whatever `branch:*` keys happen to exist, does not prove the expected
branch set, and collects hash-map iteration order rather than YAML declaration
order. Elitea keeps the useful wait-all concept while enforcing the stronger
expected-set and deterministic-order contract at its adapter boundary.

Action `WaitAny` and `WaitN` only inspect results already present after a graph
frontier. They do not wake early or cancel siblings. YAML `wait: one` and
`wait: many` therefore fail validation until a separate scheduler persists its
completed set/deadline, provides a timer wakeup, and defines durable sibling
cancellation and late-result handling.

The redesign treats an inner interrupt as a paused branch. It aggregates paused
branches through one standard outer graph interrupt.

Root and child checkpoints remain the durable source of truth. No separate
interrupt ledger is planned.

Resume requires one exact complete branch decision set. Partial or stale sets
fail before dispatch.

External side effects remain at-least-once between an effect and the following
child checkpoint. Tool and effect fencing remains mandatory.

## Future data-driven map-node decision

Keep list-driven map fan-out separate from the fixed pipeline `parallel` node.

Model-driven Application fan-out is a third contract. The current ad-hoc root
`LlmAgent` already creates repeated calls from one chat request.

A pipeline planner LLM can create the source list before map execution. Do not
put implicit model planning inside either scheduler.

ADK `LoopAgent` repeats fixed subagents sequentially. ADK `ParallelAgent` runs
a fixed subagent list without bounded per-item admission.

The inspected ADK graph API has no LangGraph `Send` equivalent. Elitea must own
an equivalent bounded scheduler and one item checkpoint per invocation.

The owned worker keeps its existing input mapping and structured output
projection. The collector sorts by input ordinal before applying a reducer.

Declare approved reducers on state channels. Do not accept executable custom
reducers from stored YAML.

See `docs/map-reduce-pipeline-node-design.md` for the proposed contract.

## Performance and operations

The scaling foundation is bounded asynchronous admission, shared pools,
per-resource concurrency ceilings, backpressure and one owner for each durable
delivery. The internal `AgentDeliveryProcessor` is now the sole raw Redis
application/ad-hoc entrypoint and `AgentInvocationCoordinator` is its sole
prepared-request continuation. The processor keeps the Redis PEL processing
future open across verification, claim, output preflight, preparation,
supervised invocation and exact retirement. It constructs authorization plus the common native
lifecycle and synchronously transfers work to the invocation supervisor only
with a non-cloneable reservation from its exact bounded admission pool. The
supervisor keeps accepted work alive when a result waiter disappears, closes
admission during drain and returns an unaccepted authority-bearing future
intact for explicit close. The process panic hook
also replaces arbitrary panic payloads with one static diagnostic. The
capability-disabled native application/ad-hoc lifecycle now owns one authorized
run through ADK EOS, per-event durable ACK backpressure, final lease and
deadline reduction, terminal ACK, settlement and Redis retirement. Its
ownership-heavy async phases are boxed at deliberate transition points so the
generated poll stack remains bounded without increasing thread stack sizes.
The authorization job retains its large one-use inputs behind the existing box
until the async frame itself is heap-allocated; the default-stack delivery E2E
test guards this construction boundary.
Cancellation-safe assembly and post-EOS result selection are raced against the
supervised lease. Exact ADK-Rust 2.2.0 creates its `EventStream` without I/O on
the first poll; the wrapper enforces that as a synchronous fail-closed start
boundary, while session lookup and agent work remain inside the supervised
stream. Normal shutdown must stop and drain the Redis delivery runtime before
stopping and closing the invocation coordinator; a hard drain timeout belongs
to the one process-wide exit budget, because dropping only the waiter cannot
cancel already supervised native work.
Production capability registration still waits for input-free
running/ambiguous recovery, real provider, session, tool and policy proof plus
cross-process integration. Config, trust, clients, state pool, native runtime,
CLI `serve`, SIGINT/SIGTERM ownership and a process-wide drain deadline are
composed. `serve` separately requires a bounded mounted snapshot containing the
current runtime/admin `toolkit_security` dictionary, so the shared Python
deployment-v1 document is unchanged and absence never defaults to an empty
policy. Container orchestration still has to produce that snapshot, and atomic
generation refresh remains open. `smallvec`,
`crossbeam` and lock-free queues are not architecture requirements. Add them
only after representative profiles identify an allocation or contention hot
path and a benchmark proves an end-to-end gain without weakening cancellation
or durability.

Tracing is added at the orchestration owner as functionality lands. Stable
span fields include execution/generation/claim attempt, capability, graph/node,
tool and function-call identity. Prompts, credentials, tool payloads,
checkpoint state and provider response bodies are never log fields. Low-level
adapters return typed errors with stable code and retryability; the lifecycle
owner records the error and its trusted source once.

Delegated-authorization checkpoint update: parallel and recursive direct
saved-agent calls now use the same native confirmation/session lineage as
sensitive calls. The browser-visible hierarchy stays call-bound, an exact
Authorize/Skip set is validated against persisted interrupts and exact-server
authority, and Skip produces a structured same-call result without dispatch.
Older table cells that broadly say parallel/nested authorization remains closed
refer now to independent partial sibling resume, mixed-guardrail terminal
aggregation, platform OAuth/DCR, post-token model-loop challenges and remaining
SharePoint content/app-only or configured-family gaps. Main's
scalar and bounded complete-set routes produce the shared `mcp_auth`
decision-list shape and consume siblings atomically. The same complete-set replay now crosses a
pipeline Agent node through its graph checkpoint, including mixed
sensitive/auth leaves; partial authority fails before materialization.
