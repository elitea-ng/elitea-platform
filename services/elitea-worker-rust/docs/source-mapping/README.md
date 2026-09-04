# Source-to-Rust mapping

These ledgers map observable Elitea behavior to concrete Rust ownership. They
are compatibility evidence, not a byte-for-byte port plan.

The evidence has three equal, complementary implementation sources:

1. the current platform Python worker owns the executable interpretation of the
   language-neutral protobuf contracts, control/output gRPC lifecycle,
   `NodeEventV1` bridge, result/artifact binding, delivery fencing, spool replay
   and settlement;
2. `elitea-sdk` owns most agent, configuration, toolkit and indexing business
   behavior;
3. Centry `indexer_worker` owns current application/ad-hoc invocation,
   continuation, callback, orchestration, indexing and UI-compatibility behavior.

The `.proto` files are the schema authority, but schema inspection alone is not
enough: the Python worker's validators, lifecycle code and tests define how the
contract is safely executed today.

Source snapshot used for the initial reconstruction inventory:

| Source | Revision |
| --- | --- |
| `elitea-platform` language-neutral contracts and Python worker | `4a553cee4302c863a562232f1b6b9964d5237357` |
| `elitea-sdk` | `fd6c01be2afcc696cce68b430fa1d89ddba3ae21` |
| Centry `indexer_worker` | `67d65b0dbbb8dbe2d590e8ecfe8ec13cd96998fa` |

Every implementation slice must update its rows in the same commit. Status has
one of these meanings:

- `planned`: target ownership is proposed, but the Rust file does not yet
  exist;
- `foundation`: the file exists and proves only the behavior named in the row;
- `partial`: some observable behavior is implemented, with missing gates listed;
- `blocked`: the current language-neutral or cross-service contract cannot
  express or consume the behavior end to end;
- `ported`: all named behavior, policy, error, cancellation, and differential
  tests pass;
- `intentional deviation`: Rust deliberately improves or retires an
  implementation detail while preserving the documented product contract.

A row is never `ported` based only on compilation or final-answer similarity.
The proof must cover the relevant wire shape, state transition, event ordering,
authorization, failure behavior, and recovery boundary.

Detailed ledgers:

- `agent-runtime.md` maps language-neutral worker delivery and agent execution.
- `pipeline-nodes.md` maps every current Python pipeline node/edge branch and
  the capability-closed future explicit pipeline `parallel` node core.
- `../parallel-pipeline-node-design.md` defines the proposed Agent-only V1
  parallel node, chat-planner boundary, durability, interrupts, and test gates.
- `../map-reduce-pipeline-node-design.md` defines the separate data-driven map
  node, LangGraph `Send` semantics, reducers, durability, and test gates.
- `configuration-toolsets.md` maps saved configuration and toolkit families.
- `prebuilt-mcp.md` maps fixed catalogue-backed HTTP MCP execution.
- `internal-elitea-mcp.md` separates internal builder categories from external
  Elitea-as-MCP publishing. It maps the Main-owned applications and skills
  categories.
- `indexing.md` maps indexing behavior and its later Rust capability.

Maintained Rust runtime ownership registry:

- `src/agents/{native_runtime,assembly,ordinary,pipeline,session,events,sensitive_tools,direct_hitl,internal_tools}.rs`: strict
  frozen-kind routing plus direct
  saved-agent and ad-hoc `LlmAgent` admission, common injected
  `SessionService`/Runner composition, request-independent definition lineage,
  seed-once frozen history, browser event projection, runtime-policy-bound
  sensitive-tool pauses and worker-side exact resolution of Main-authorized
  decisions against raw persisted calls. The ordinary assembler accepts one
  exact continuation before PAT redemption and requires a restorable session.
  Root output-limit continuation keeps the original response identity, restores
  the exact ADK session, and receives its model snapshot from the selected
  dummy/model participant. The event projector removes bounded overlap and
  repairs a missing alphabetic word separator consistently in stream and
  terminal output.
  Direct/ad-hoc regeneration is a separate admitted start mode: the exact
  claim-fenced session is deleted and recreated, then seeded from Main's
  pre-response history before the original question runs once. It never appends
  to the replaced assistant branch. Saved-pipeline regeneration remains a
  separate SessionService plus Checkpointer rewind slice.
  Approved reads replay through ADK `RunConfig`/`ToolExecutor`; denied calls
  produce a structured result under the original call ID without dispatching
  the real tool. Exact restart suffixes are recovered and the internal replay
  marker is removed before provider dispatch. A fresh direct agent whose
  complete frozen root tool set consists only of saved Applications uses
  ADK-native bounded parallel tool dispatch; mixed tool batches stay sequential.
  The same rule is applied recursively when a saved direct child owns only
  saved Applications. A bounded typed event bus forwards descendant ADK events
  into the parent stream; the projector extends an ordered
  `{name,call_id,sibling_ordinal}` path at every exact container invocation.
  Provider call ID remains invocation identity and ordinal remains presentation
  metadata, so repeated names and concurrently active nested leaves stay
  distinct for current UI grouping. Nested confirmations remain ordinary ADK
  session events under isolated branches. A complete decision set reconstructs
  the exact persisted parent-call chain, replays each admitted direct-agent tier
  through the original call IDs and rejoins the root model. Identical child and
  leaf call IDs under concurrent parents stay scoped by their owning invocation;
  missing siblings, broken parent links and a fourth agent tier fail closed.
  Delegated authorization uses the same decision tree: the persisted native
  confirmation classifies the guardrail, exact server authority is consumed as
  one bounded set, and parallel Authorize/Skip leaves resume without replanning.
  Skip preserves the original call ID and dispatches no real tool. Main's exact
  single-card authorization continuation now produces that existing versioned
  decision-list shape and the standalone Python worker forwards it unchanged;
  Main also admits and atomically consumes a bounded exact complete decision
  set. Independent partial-sibling resume and mixed-guardrail terminal
  aggregation remain closed.
  The runtime-owned `ask_user` internal tool is admitted explicitly rather
  than through a configured-tool snapshot. Direct agents, recursively saved
  direct agents and selected pipeline LLM nodes bind it through native ADK
  confirmation. Its normalized 1-4 question payload is stored on the ordinary
  session or graph interrupt, projected as `clarifying_question`, and an
  `answer` continuation supplies the SDK-compatible text result under the
  original provider call ID. The answer is never injected as a user turn and
  the fail-closed tool implementation is never dispatched. Main
  `agentexecution/{start,adhoc,continue}.go`, the current routes and
  `agent_chat.sql` now project the bounded selection and canonicalize object or
  string answers without changing the protobuf. Parallel saved-child
  clarifications retain separate hierarchy/checkpoint identities, require one
  atomic complete answer set, and replay each answer into its exact child call.
  Other internal tools remain
  separately gated.
  Approved effects and deployment registration remain closed. `native_runtime.rs` selects exactly one direct
  or pipeline assembler before either can redeem authority and keeps both
  completion owners behind the same lifecycle. The pipeline profile separately admits only
  frozen `agent_type=pipeline` applications, compiles their complete YAML before
  provider/state construction and retains the authorized state inputs for the
  graph assembler;
- `src/agents/application_tools.rs`: exact saved child application/version to a
  native child `LlmAgent` behind an ADK-compatible tool, bounded recursive
  assembly, model-visible delegation schema, nested sensitive-policy and
  delegated-authorization binding,
  typed descendant-event forwarding and the runtime-name-to-frozen-presentation
  join used by browser projection. The adapter exists because stock ADK
  `AgentTool` buffers child events and cannot expose an exact nested hierarchy.
  Recursive direct-agent confirmation resume uses the root `SessionService`
  transcript and an invocation-scoped replay tree rather than another child
  session or interrupt table. The same tree retains nested sensitive,
  delegated-auth and internal-tool catalogs. It routes exact Authorize/Skip or
  `ask_user` Answer decisions at each leaf, including parallel calls to the
  same saved child and when the root saved participant is invoked by a pipeline
  Agent node and bound to that graph checkpoint. `ordinary_tests.rs` owns the
  distinct hierarchy, complete-set admission, frozen-scope and no-replanning
  regression. Saved-pipeline Agent-node
  descendant streaming and checkpoint hierarchy are instead owned by `graph/application.rs`,
  `graph/node_events.rs` and the common event projector;
- `src/agents/context_management.rs`: disabled-first seam for SDK context
  settings and future ADK-native compaction through the durable session
  lineage, coordinated with graph checkpoints where applicable;
- `src/agents/graph/{agent,application,compiler,resume,hitl,llm,direct_tool,printer,router,decision,state_modifier,parallel,yaml}.rs`:
  stored-pipeline event identity, complete-document active-node compilation,
  latest-event/checkpoint decision binding and bounded YAML contracts. The
  explicit pipeline parallel-node files are a disconnected prototype that the
  proposed Agent-only V1 must replace before compiler binding. Direct Toolkit
  and remote MCP reads use
  native `ToolContext`; sensitive reads pause before dispatch, approval returns
  the ordinary result, and denial records the same-call blocked result plus the
  SDK-formatted terminal chat message before `goto END` without nulling typed
  outputs. A family-neutral delegated-auth signal preserves the SDK's
  compatibility-named `mcp_auth` card for both node kinds; remote MCP can rebuild
  with an exact-server claim-fetched token, while Skip nulls declared data state
  and stops at `END`. The configured OpenAPI family applies the same flow to an
  exact frozen API base URL. The delegated SharePoint read core applies it to
  the exact frozen site URL and also turns a direct-node Graph 401 into the same
  typed interrupt.
  Printer uses a compiler-owned native `interrupt_after` checkpoint,
  publishes one bounded ordinary chat result, and resumes through the generated
  reset node on the next ordinary user message. Router evaluates bounded
  state-driven conditions and Decision runs a no-tool claim-bound model; both
  select only compiler-validated targets with an atomic ADK `goto`. Agent nodes
  map one task to an exact frozen saved participant; direct saved agents reuse
  the claim-bound child-agent assembly, stream their typed descendant events
  under an exact synthetic Application call and project their final response;
  saved pipelines compile as isolated native `SubgraphNode` values over the
  same claim-fenced Checkpointer. A nested dynamic HITL binds one public
  `interrupt_id` to a bounded exact descendant checkpoint chain, resumes every
  pending Agent-node checkpoint in order and keeps child thread/checkpoint IDs
  out of browser output. Sensitive confirmations inside a direct saved Agent
  remain the only public cards; an internal graph interrupt binds their exact
  ID set to the pending Agent node, while approve/reject/block reuses the same
  call-bound application replay coordinator. Delegated-auth cards use that same
  binding and coordinator; complete parallel Authorize/Skip or mixed
  sensitive/auth sets replay atomically, while incomplete authority dispatches
  nothing. Parallel nested leaves with the
  same provider call ID remain separated by invocation hierarchy. The current
  materializer supports one selected child
  pipeline level and fails closed if that child declares another Agent node.
  A pipeline LLM node uses native `require_tool_confirmation` and a bounded
  private replay envelope in the existing graph-interrupt/session binding to
  join the exact pending ADK tool turn to the outer checkpoint. Resume places
  the resolved envelope in one reserved graph-state channel. Approve returns
  the ordinary result; reject or
  Block With Comment supplies the shared `sensitive_tool_blocked` result under
  the original provider call ID without a new user turn or model replanning.
  `src/agents/pipeline_tests.rs` owns the no-dispatch, same-ID and provider-call
  count regression proof for that path. `src/agents/graph/node_events.rs`
  forwards native model/tool lifecycle events through one bounded
  invocation-local channel, strips provider request payloads, stamps the owning
  pipeline node for the UI and leaves graph state/checkpoints business-only.
  `src/agents/graph/routing_tests.rs` owns their current/legacy YAML, exact
  fallback, normalized-label and common-Runner proof. Remote effects, child
  variables, nested static Printer
  interrupts, incremental pipeline tool-progress chunks, approved-effect
  receipts, arbitrary static interrupts and production activation remain
  separate gates. Saved-pipeline child events and configured/sensitive HITL
  cards use the existing wrapper hierarchy; the sensitive continuation keeps
  the original call ID and executes no blocked provider tool;
- `src/toolkits/{snapshot,materialize,delegated_auth,mcp,invocation,policy}.rs`: frozen configured,
  MCP and application references, native family toolsets and generic bounded
  call policy/tracing. Configured materialization receives the same
  claim-scoped exact-resource delegated-token map as MCP and returns a merged
  authorization catalog to root, pipeline and recursively saved agents. Main
  keeps a custom or Provider Hub participant attached when this deployment has
  no actor-visible schema, but omits it from that execution snapshot with a
  structured data-free warning. Rust applies the same rule only to a frozen
  family that this binary does not implement. Invalid settings, permissions,
  credentials and supported-family dependencies still fail closed;
- `src/toolkits/families/openapi/{config,spec,client,tools}.rs`: bounded dynamic
  OpenAPI 3.x tool generation from an inline JSON/YAML document. This is not a
  static catalog: each admitted operation supplies its tool name, description,
  argument schema, method, path, parameter locations and body contract from the
  specification, while the toolkit instance supplies the exact base URL and
  authorization. The invocation client then serializes that operation's data
  according to the spec. Per-parameter `style`/`explode` and
  `allowReserved`-conditional RFC 3986 query serialization operate over a raw
  HTTP URI; API-key, invocation-scoped expiring client-credentials and
  delegated OAuth modes produce schema-complete guarded tools for native
  same-call pause/resume. Remote specifications, legacy auth
  objects, rich OAuth discovery/DCR, runtime 401 re-authorization, non-JSON
  bodies and artifact/binary routing remain closed;
- `src/toolkits/families/sharepoint/{config,client,tools}.rs`: delegated Azure
  token resolution and eight explicitly selected Microsoft Graph reads for
  lists, columns, metadata-only recursive file discovery and raw bounded
  OneNote XHTML. The client keeps all requests and provider pagination on the
  exact Graph v1.0 origin, preserves SharePoint site/library path resolution,
  and exposes schema-complete guarded tools for native same-call authorization.
  Empty selection, ACS/app-only auth, file parsing/download, OneNote attachment
  interpretation, writes, rich discovery/DCR/refresh metadata and production
  egress remain closed;
- `src/transport/model_facade.rs`: provider-neutral model ownership over
  `openai_compatible_facade.rs` and `anthropic_facade.rs`, including frozen
  `model_project_id` authority;
- `src/transport/platform_client.rs`: claim-bound application/version lookup
  today and future artifact grants; `runtime_context.rs` owns the concrete
  private transport;
- `src/state/postgres_session.rs` plus Main migration
  `migrations/agentstate/0002_agent_sessions.sql`: bounded claim-fenced ADK
  conversation/session persistence for direct and graph Runners. Direct
  sensitive confirmations remain standard ADK events: Runner awaits persistence
  before yielding them to browser projection, so no duplicate pending-interrupt
  table is introduced;
- `src/protocol/control.rs::ClaimBoundSessionAuthority`: one-use session-writer
  grant minted only at `AUTHORIZED_NOW`, independently of output and settlement
  authority;
- `src/execution/agent_lease.rs` and `src/state/writer_lease.rs`: the supervised
  Main claim lease becomes a read-only state-writer guard. Main's accepted
  control receipt supplies the database-authored claim start used to order
  writer takeover in the separate state database;
- `src/state/postgres_checkpointer.rs` plus Main migration
  `migrations/agentstate/0001_agent_graph_checkpoints.sql`: graph-only frontier,
  node-state and interrupt persistence. Both adapters target the same separate
  PostgreSQL `agentstate` database under an isolated `elitea_runtime` schema,
  leaving legacy LangGraph tables in `public` unchanged. They implement
  distinct ADK contracts rather than duplicating transcript state. State writes
  check the supervised lease before locking and before commit, then retain a
  durable newer-writer fence. This does not claim an atomic transaction across
  Main and `agentstate`; cleanup-owner coverage remains a deployment gate;
- current native storage is seven runtime tables: two graph-checkpoint tables
  and five normalized session tables. `elitea_runtime.schema_migrations` is an
  eighth physical bookkeeping table, not session state. The four legacy
  LangGraph tables remain untouched. Future app/user/session-state
  consolidation is a measured storage optimization; parallel or nested HITL
  must use distinct session/checkpoint identities rather than new tables per
  pause scope;
- `src/config.rs` and `src/security.rs`: strict file-only
  `elitea.runtime-deploy.v1` admission, protocol-fixed transport limits,
  canonical private-plane endpoints, permission-bounded regular-file access,
  TLS 1.3 workload identity validation, exact Ed25519 command-key resolution
  and zeroizing spool/Redis secret ownership. Redis password and TLS files are
  reloaded for each connection generation;
- `src/{lib,main,bootstrap}.rs`, `src/transport/redis_connector.rs` and
  `src/execution/production.rs`: one capability-disabled executable production ownership
  path from validated trust into private control/output/content/runtime-context/
  model channels, the shared `agentstate` pool, reconnectable Redis generation,
  output preflight, semantic delivery processor, direct/graph native assembler
  and stop-aware Redis runtime. CLI `serve` preserves the shared
  `elitea.runtime-deploy.v1` file and separately requires a bounded mounted
  snapshot containing the runtime/admin `toolkit_security` dictionary. There
  is no missing-policy/default-policy branch. The multi-thread process owner
  installs SIGINT/SIGTERM before composition, emits data-free lifecycle/error
  fields and applies one global drain deadline. Capability registration,
  container/orchestrator snapshot projection and atomic policy refresh remain
  closed. An isolated Main, Redis, PostgreSQL, model facade and Rust-worker
  application/ad-hoc process proof now passes;
- `src/diagnostics.rs`, model adapters, state adapters and
  `src/execution/{agent_delivery_processor,agent_preparation,agent_coordinator,agent_invocation,invocation_supervisor,native_agent_lifecycle,output_delivery,redis_delivery}.rs`:
  crate-scoped structured logs, standard OTLP export and authenticated
  lifecycle/assembly/tool correlation. Valid W3C `traceparent` values continue
  the upstream trace. Delivery, claim, preparation, ADK execution, model,
  session, checkpoint, output, settlement and retirement spans use allowlisted
  identity, phase, outcome and stable error-code fields only. The concrete
  agent delivery processor keeps one raw Redis PEL owner alive through claim,
  output preflight, preparation, supervised native execution and retirement
  for both application and ad-hoc commands.
  `agent_invocation.rs` keeps authorization inputs boxed until its async frame
  is allocated, preserving the default thread-stack bound as runtime variants
  grow.
  Normal bootstrap must drain its Redis processing futures before closing the
  coordinator. The redacted panic hook retains only a sanitized source filename
  and line. Tokio-backed OTLP batching avoids exporter-thread reactor panics.
  The standalone proof also projects UI-shaped model configuration through
  Main and Bifrost. A selected `gpt-4o-mini` call reached the allowlisted OpenAI
  endpoint and returned unauthorized because the available shell value is not
  a valid provider key. Rust published `execution.failed`, retired the delivery
  and stayed at zero restarts. A credentialed OpenAI-compatible follow-up used
  the current local Elitea proxy. Execution
  `868af70730dc53444aeb27b7343f0f51` selected
  `eu.anthropic.claude-sonnet-4-6` with `openai_compatible=true`, streamed the
  unique provider marker, persisted the reply, retired the delivery, and left
  the worker and gateway at zero restarts. Gateway
  `internal/llmproxy/models.go` preserves the existing `open_ai` custom-base
  contract by selecting the per-key compatible provider for non-OpenAI
  origins; official OpenAI bases keep the native provider. Gateway
  `internal/account/account.go` normalizes one trailing `/v1` because Bifrost
  adds `/v1` itself, so user configuration can use either `/llm` or `/llm/v1`.
  `deploy/scripts/chat-smoke.py` treats the durable SSE `event:` name as
  terminal when a failure payload has no `data.type`.
  Backend retention, sampling, metrics and Kubernetes activation remain
  deployment-owned.

Maintained current-UI integration reference:

- Current UI
  `src/[fsd]/features/chat/lib/hooks/chat-button/useApplicationSubmenu.hooks.js`,
  `src/[fsd]/features/chat/ui/chat-button/{PlusChatButton,PlusChatSubmenu}.jsx`,
  and
  `src/[fsd]/features/chat/participants/lib/hooks/useAddNewParticipants.hooks.js`
  define existing-chat participant behavior. Replatform ownership is
  `processes/chat/model/usePlusMenuEntities.ts`,
  `widgets/chat/ui/chat-button/{PlusChatButton,PlusChatSubmenu}.tsx`,
  `widgets/chat-box/ui/hooks/useAddEntityParticipant.ts`, and `ChatBox.tsx`.
  Toolkit and MCP rows use persistent checked toggles. Agent and pipeline rows
  select and activate one catalog participant.
- Replatform Main ownership is
  `internal/api/v2/{configurations,toolkits}/handler.go`. It serves the current
  form contracts, preserves restored configuration admission, and supports
  toolkit tables with or without `owner_id`.
- Private-project rehearsal proved one OpenAPI tool call through the complete
  UI, Main, Redis, Rust, provider and persistence path. The same UI proved a
  toolkit participant DELETE and POST without closing the submenu. A later
  fresh tool response regenerated through `agent.regenerate.v1`, invoked the
  provider tool again and survived reload. The live UI retains the admitted
  request's question identity when replay frames carry `question_id: null`;
  explicit event identity always wins.
- New-conversation participant staging, current UI default-model assignment,
  generic OpenAPI additional headers, durable question identity for a
  replay-only late attach, and completed tool-accordion restoration remain
  explicit UI/protocol parity gaps.

Workspace-relative Python paths are included because the Python sources live in
independent repositories. The Rust targets all live in this package and are
repository-relative to `services/elitea-worker-rust/`.

The repository-level `.github/workflows/ci-rust.yml` gate covers this registry.
It runs locked quality, release, and PostgreSQL-backed test commands.
