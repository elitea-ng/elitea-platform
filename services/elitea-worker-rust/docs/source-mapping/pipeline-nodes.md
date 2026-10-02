# Python pipeline graph to Rust mapping

This ledger maps the current `elitea-sdk` pipeline compiler and worker runtime
evidence to the Rust files that replace it. It is traceability, not a plan to
copy Python's class structure. Rust uses ADK-Rust 2.2.0 where it preserves the
admitted behavior and adds small Elitea boundaries for YAML compatibility,
authority, durability and policy.

Primary Python evidence is
`elitea_sdk/runtime/langchain/langraph_agent.py::create_graph`. Worker-side
input, lease, output and continuation contracts remain mapped in
`agent-runtime.md`. Python currently has no `parallel` node; that row is an
intentional product addition requested for the Rust runtime.

The current product-authoring boundary is the EliteaUI flow editor at
`49bac1ba775e5e6d598dea8ccbf328d1d2d20fb3`; the current SDK behavior was
rechecked at `8a8ee3c3cfb1935e88eda6f357b0e89d94a80bbc`. The active add menu is the
authority for the migration blocker, not every historical branch still
accepted by `create_graph`.

## Active UI contract

### Fresh-turn checkpoint boundary: 2026-09-07

SDK evidence is `runtime/langchain/langraph_agent.py::LangGraphAgentRunnable.invoke` at `4b7691c7ddc4bc2a8e4f1b5f7da620d845b55b0e`.
Its completed-run branch starts a new turn with new input.
Its interrupted-run branch distinguishes explicit continuation from a new request.

ADK-Rust 2.2.0 loads the latest graph checkpoint without an explicit resume identifier.
A terminal checkpoint has no pending nodes. Loading it for a new turn bypasses node execution.
This explains the repeated pipeline answer observed in conversation 530.

Rust maps this boundary to `src/agents/session.rs` and `src/agents/graph/turn_checkpointer.rs`.
Each checkpoint stores its authenticated execution identifier and generation in private metadata.
A fresh execution loads only its own frontier. Claim replacement retains that frontier for the same execution and generation.
Explicit continuation still resolves the exact stored interrupt before the graph can load the previous frontier.
Fresh turns do not delete historical checkpoints or clear conversation history.
Regeneration retains its separate, claim-fenced reset contract.
Legacy checkpoints remain readable for explicit continuation. Fresh executions do not load unmarked legacy frontiers.

The two-turn component test fails before this change and passes after it.
Tests also cover completed and pending frontiers, generation replacement, same-execution replay, and legacy continuation.
All 35 pipeline tests pass, including nested parallel authorization and regeneration.
The full suite passes 866 library tests and 83 integration or contract tests with PostgreSQL available.
No Rust test is ignored. Live authorization grants and repeated child requests after Skip remain separate gates.

Live conversation 532 executes both successive user tasks through the saved pipeline.
The second task returns `PIPELINE_SECOND_TURN`, not the previous answer.
See [delegated authorization](delegated-oauth-dcr.md#complete-child-results-and-logical-event-limits)
for the subsequent fixes to child-task preservation, fragmented node output, and logical-part limits.

| Active designer node | Business contract and Rust disposition |
| --- | --- |
| `agent` | Invoke the exact saved application or pipeline selected on the node. A direct child runs as a native `LlmAgent` behind the typed application-tool adapter; a saved pipeline compiles as an isolated checkpointed `SubgraphNode`. Do not rebuild either as an untyped Python-style callback. Implemented capability-disabled for one selected level; recursive pipeline nesting and inner direct-child event streaming remain gated. |
| `code` | Execute editor-authored code. Keep outside the worker process behind a separately supervised sandbox, egress and artifact capability; the current Python network-enabled sandbox is not copied. Pending external runtime |
| `custom` | An editor placeholder whose current SDK execution rejects the node. Preserve that rejection until a concrete typed extension contract exists; it is not a hidden arbitrary-class escape hatch. Implemented rejection |
| `decision` | Ask a claim-bound model to select one declared route, then apply that answer and its fallback atomically through validated `goto`. Implemented capability-disabled as a no-tool nested ADK `LlmAgent`; it accepts the active UI `nodes` contract and legacy `decisional_inputs`, without reproducing the SDK's `nodes`/`routes` mismatch. |
| `hitl` | Pause the graph with structured data and resume the exact private checkpoint by a fresh `interrupt_id`. It is distinct from sensitive-tool confirmation and has no tool-call ID. Implemented capability-disabled |
| `llm` | Build an invocation-owned native ADK `LlmAgent` for the node. The node receives only its UI-connected `tool_names`; an empty selection grants no tools. Alias, policy and frozen configured-selection checks happen before credential redemption; exact dynamic MCP/configured catalog resolution finishes after authorized materialization but before model dispatch. Application references are rejected because the current UI does not expose them on this node. Implemented capability-disabled, including checkpoint-bound sensitive-tool confirmation and delegated MCP/configured-Toolkit authorization. Approval returns the normal result to the native model loop. Reject and Block With Comment return `sensitive_tool_blocked` under the original provider call ID without executing the real tool or asking the model to plan the call again. Authorize rematerializes and replays that original guarded call; Skip supplies `mcp_auth_decision` under its original call ID. No SDK-style model-called auth-control tool is introduced. |
| `mcp` | Execute one exact selected remote MCP read with the same fixed/state/template input mapping and typed output projection as a Toolkit node. It is a direct native ADK `Tool` call with no model turn. Implemented capability-disabled for claim-materialized HTTP MCP: exact alias/selection and runtime/admin policy are admitted before connection; bounded discovery must return the selected read-only tool. Sensitive reads pause on the graph checkpoint, approval returns the ordinary MCP result, and reject/block preserves both the same-call structured result and the SDK-formatted user message before routing to `END`. A 401/403 challenge becomes the common direct delegated-auth interrupt; exact-server Authorize rebuilds with the claim-fetched token, while Skip nulls declared outputs and routes to `END`. Materializer-known root and pipeline-LLM calls use native original-call confirmation and exact replay. Same-family nested cards retain their exact hierarchy and aggregate under one terminal pause; fixed and declared-parameter catalogue-backed HTTP definitions use Main claim-time resolution; independent partial resume, stdio and remote effects remain fail-closed. |
| `printer` | Publish the declared state value as an ordinary chat result, durably pause after it, and resume from a compiler-owned reset node without duplicating browser output. Implemented capability-disabled with native ADK `interrupt_after`; it is not a HITL decision card. |
| `router` | Render a bounded condition from selected state or the whole state and choose only a declared target. Implemented capability-disabled as a deterministic custom ADK node with bounded MiniJinja, `json_loads`, normalized route labels, an exact default and atomic `goto`. |
| `state_modifier` | Render one bounded MiniJinja template with the four current SDK filters, update the first declared output using the existing state type, and clear declared variables by type. When its explicit route targets the `END` control-flow sink, that output is also a public-result candidate. Implemented capability-disabled |
| `toolkit` | Execute the exact selected read-only tool from the selected configured toolkit with fixed/state/template argument mapping and declared typed output projection. It is one native ADK `Tool` call, not an `LlmAgent` turn. Implemented capability-disabled, including checkpoint-bound confirmation for sensitive reads and a family-neutral delegated-auth interrupt compatible with the SDK's `McpAuthorizationRequired`/`mcp_auth` contract. Approval can rebuild only when the concrete family consumes the returned token; Skip nulls declared data outputs and stops at `END`. Bounded inline OpenAPI consumes this token against its exact API base URL; the explicitly selected SharePoint read core consumes it against the frozen site URL and maps direct-call Graph 401 to the same interrupt. Remaining OpenAPI and SharePoint content/auth capabilities plus remote effects remain fail-closed behind their authority and reconciliation contracts. |

SharePoint checkpoint update: Toolkit nodes may now select one of eight
artifact-free delegated Graph reads. A missing token uses the same pre-dispatch
interrupt as OpenAPI, and a Graph 401 raised during the direct call is converted
to that native interrupt before state projection. Skip still leaves typed
outputs null and routes to `END`. Empty/all selection, ACS/app-only access,
document or attachment parsing, indexing, writes and model-loop post-token 401
handling remain closed. A mixed saved selection exposes only its selected
supported reads. This update supersedes older broad inventory rows below.

The UI marks `function`, `condition`, `pipeline`, `loop`, `loop_from_tool` and
`tool` deprecated and removes them from the add menu. `END`, `ghost` and
`defaultType` are structural/invisible. They remain compatibility evidence for
saved historical YAML, but they do not displace active-node completion or the
Redis/runtime activation work. Indexer-backed nodes remain last.

## Compiler and node inventory

| Python source branch / symbol | Current observable behavior | Rust target | ADK-Rust 2.0.0 use | Status / deliberate deviation |
| --- | --- | --- | --- | --- |
| `create_graph` plus `utils.py::{create_state,_hitl_decisions_reducer,_parallel_tasks_reducer}`: YAML load, state construction, node loop and entry point | Load `state`, state defaults, `entry_point`, `nodes`, interrupts and graph edges; keep runtime-owned channels separate from user variables. The SDK applies `clean_string` to node IDs, entry points and targets, so an older UI document containing `Agent 1` executes as `Agent1`. | `src/agents/graph/{compiler,yaml,application,llm,direct_tool,printer,router,decision}.rs` | `StateGraph`, `StateSchema`, `Channel` and native ADK reducers | Complete-document compiler implemented for dynamic `hitl`, `state_modifier`, `llm`, configured `toolkit`, direct remote `mcp`, saved direct `agent`, `printer`, `router` and `decision` nodes. It bounds the YAML at 512 KiB, nodes at 128 and declared state keys at 256; validates state types/defaults, the entry point, references, unique IDs and every route before graph construction; and binds a definition digest that includes behaviorally significant state declaration order plus node family, mapping/tool/output settings. Before validation, a runtime-local compatibility pass applies the SDK normalization only to otherwise-invalid legacy graph identifiers and targets. It does not update stored YAML, it preserves already-valid Rust identifiers, and normalized collisions fail closed. `messages` uses ADK list/append reduction, HITL decisions use append-or-clear and parallel task records use merge-or-clear; internal channels cannot be redefined by YAML. Arbitrary user-authored static interrupts and every other node family fail closed before execution authority is built; the compiler itself owns Printer `interrupt_after` checkpoints. |
| Node types `toolkit`, `mcp` -> `FunctionTool` | Map fixed, state-variable or `{name}` template values to one exact configured or remote-MCP tool argument object, execute once, and project declared outputs; if the selected read is sensitive or requires delegated authorization, pause before dispatch and terminate cleanly when declined | `src/agents/graph/{direct_tool,compiler,resume}.rs`, `src/agents/{pipeline,session,events}.rs`, `src/toolkits/{snapshot,materialize,delegated_auth,mcp,invocation,policy}.rs` | Native ADK `Tool`/`ToolContext`, ADK HTTP MCP toolset, family-neutral typed authorization signal, `interrupt_with_data`, claim-materialized `Toolset`, graph state and `goto END` | Implemented capability-disabled for configured and remote MCP read-only actions. Node kind, alias, frozen selection, blocked and sensitive policy are admitted before credential redemption or MCP connection; exact catalog identity/read-only metadata are checked after bounded materialization. Sensitive arguments are masked publicly and bound to the invocation, checkpoint, pending node, definition, `pipeline:<node>:<step>` call ID and canonical argument digest. Sensitive approval applies the normal result; reject/block executes no provider call, records `sensitive_tool_blocked` under the same call ID, emits the SDK-compatible markdown explanation, leaves typed outputs untouched and routes to `END`. Delegated auth uses a distinct SDK-compatible `mcp_auth` interrupt for both node kinds: remote-MCP or inline OpenAPI Authorize rebuilds the exact resource, while Skip nulls declared outputs, emits the current authorization-stop message and routes to `END`. Materializer-known LLM calls use native original-call confirmation and exact session/checkpoint replay. Empty outputs and `structured_output` retain the documented projection. Same-family nested cards aggregate with exact identities; fixed and declared-parameter catalogue-backed HTTP definitions use Main claim-time resolution; effects, independent partial resume, remaining family capabilities, and stdio remain separate gates |
| Node type `function` -> `FunctionTool` | Execute a legacy function with declared state mapping | Future typed legacy-function compatibility node | ADK `Tool` plus graph state | Pending. The deprecated node does not displace active-node completion |
| Node type `tool` -> `ToolNode` | Direct selected-tool execution with structured-output option | `src/agents/graph/nodes/tool.rs` | ADK typed tool | Planned |
| Node type `loop` -> `LoopNode` | Repeated tool work and declared state projection | `src/agents/graph/nodes/loop.rs` | ADK loop and graph primitives where semantics match | Planned; ADK's sequential `LoopNodeConfig.parallel` is not true parallelism |
| Node type `loop_from_tool` -> `LoopToolNode` | One tool supplies items and another processes iterations | `src/agents/graph/nodes/loop_from_tool.rs` | ADK custom node plus tools | Planned |
| Node type `indexer` -> `IndexerNode` | Chunk/index tool composition | `src/indexing/graph_node.rs` | ADK custom node around the later indexing capability | Deferred to indexing after agent execution |
| UI `AgentNode.jsx`; SDK `langraph_agent.py` Agent branch plus `runtime/tools/application.py::Application` | Invoke one selected saved entity by mapping exactly one `task`, append its final response to `messages`, and copy that response to declared outputs. The selected `application` entity can resolve to either a direct agent or a saved pipeline | `src/agents/graph/{application,compiler}.rs`, `src/agents/{application_tools,pipeline}.rs`, `src/transport/platform_client.rs` | ADK-compatible application tool over a native `LlmAgent` for a direct saved agent; isolated native `SubgraphNode` for a saved pipeline so inner interrupts remain graph interrupts | Partial capability-disabled implementation. The compiler admits the active strict YAML, fixed/variable/f-string task mappings, bounded output projection, exact frozen alias and only `agent`/`pipeline` participant kinds. Production assembly resolves the exact claim-bound child version: variable-free direct agents use the typed child-agent adapter, while saved pipelines compile once as isolated checkpointed `SubgraphNode` values with the same runtime-bound model/tool factories and Checkpointer. The child result passes only through `elitea_response`; child state does not leak into the parent. Dynamic child HITL pauses the parent Agent node, adds the exact child checkpoint to the private interrupt binding, projects one ordinary public card without child checkpoint/thread IDs, validates both checkpoints on continuation and resumes that child once. Configured and sensitive-LLM cards retain the wrapper hierarchy; Block With Comment reaches the nested model as a same-call structured result with no real-tool dispatch or fresh planning turn. Agent-node and production-assembly fixtures prove mapping, direct and child-pipeline completion, exact runtime-context lookup, namespaced checkpoint ownership, nested approve/resume and replay rejection. The current materializer supports one selected pipeline-child level and rejects an Agent node inside that child until the existing ancestry/cycle/hop owner can be propagated recursively. Direct-child semantic events are forwarded in ad-hoc/direct-agent composition. Saved-pipeline Agent nodes now emit an ordinary Pipeline wrapper and route inner LLM lifecycle events through the same call-bound descendant projector; one private reserved state value only bridges ADK SubgraphNode metadata propagation. Child variable schemas, the missing private immutable Main child-version endpoint, nested MCP OAuth/effect receipts and nested static `interrupt_after` remain explicit gates. |
| Node types `subgraph`, `pipeline` in the direct-tool branch | Python explicitly rejects these paths and tells callers to use `agent` | `src/agents/graph/nodes/subgraph.rs` only after an admitted contract exists | ADK child `CompiledGraph` | Dead Python code is not copied. A future subgraph feature needs explicit state/checkpoint mapping |
| Node type `code` -> sandbox `FunctionTool` | Execute editor-authored code with network-enabled Python sandbox | Separate code-execution plan and external isolation boundary | ADK action/code shape may inform the adapter | Intentionally not implemented in-process; filesystem, network, CPU/memory and artifact policy are prerequisites |
| UI `LLMNode.jsx`, `useLLMInputMapping.js` and node type `llm` -> SDK `runtime/tools/llm.py::LLMNode` | Map fixed, state-variable or `{name}` template inputs into system/task/chat history; run the model with only the node's selected tool names; always append the final assistant message and optionally project native structured output into declared state keys | `src/agents/graph/{llm,node_events,resume,compiler}.rs`, `src/agents/{pipeline,session,events,internal_tools}.rs`, `src/toolkits/snapshot.rs` | Invocation-owned nested ADK `LlmAgent`, native `output_schema` retry and `require_tool_confirmation`, exact compiler-restricted `Toolset`, bounded typed event bridge, dynamic graph interrupt/checkpoint, ADK `Content`/`Part` conversion and normal state reducers | Implemented capability-disabled. Current dictionary-shaped `tool_names` is admitted strictly; empty aliases redeem no authority and missing, duplicate or blocked tools fail before model dispatch. Selected sensitive tools and the explicitly selected runtime-owned `ask_user` tool use native confirmation. The existing private graph-interrupt/session event persists a bounded exact pending ADK turn, provider call IDs, input/configuration digests and accumulated decisions; resume writes the resolved envelope to one reserved graph-state channel. Sensitive approval replays the same pending call and yields the ordinary result. Reject or Block With Comment replaces only that exact tool with the no-effect adapter and sends `sensitive_tool_blocked` under the original call ID. An `ask_user` pause instead projects normalized questions as `clarifying_question`; `answer` supplies the SDK-compatible string result under the same call ID. Neither path injects a user message or makes a second planning request. Native model/tool start/end events use a 64-slot invocation-local channel, shed provider request payloads, retain the pipeline node identity and never enter graph state; root terminal projection is deduplicated. Dynamic MCP catalogs necessarily resolve inside the authorized materialization phase. The checkpoint `messages` value remains a bounded JSON business channel, not a recreated LangChain class hierarchy. Text and text-block histories become separate ADK parts; unsupported multimodal/tool-result blocks fail rather than being stringified. Rust deliberately omits the Python custom runnable, second model coercion call, mutable wrapper state, hidden always-bound tools, history flattening and warning-and-skip behavior. Incremental tool-progress chunks and approved-effect crash reconciliation remain gates. |
| SDK `runtime/tools/router.py::RouterNode`, `runtime/utils/evaluate.py::EvaluateTemplate`, `langraph_agent.py::{ConditionalEdge,DecisionEdge,create_graph}`; UI `RouterNode.jsx` and `DecisionNode/*` | Select one configured route from bounded state-template evaluation or a no-tool model answer, then fall back to the declared default | `src/agents/graph/{router,decision,llm,compiler}.rs` | Custom `Node`, nested claim-bound `LlmAgent` and atomic `NodeOutput::with_goto` | Implemented capability-disabled. Router preserves absent-input versus explicit-empty whole-state behavior, bounded `json_loads`, SDK route-label normalization and exact fallback, while repairing the Python substring check that could confuse `END` with labels such as `FRIEND`. Decision reuses the invocation-owned model factory without tools, converts selected `messages` to history, appends canonical-JSON additional state, renders bounded description substitutions and accepts only an exact declared answer. Rust uses the active UI `nodes` list consistently and records the current SDK edge's `nodes`/`routes` mismatch as a repaired defect. Eight focused fixtures include a common Runner graph, route normalization/collision, fallback, current/legacy inputs, no-tool model scope and compiler authority admission. Embedded deprecated decision-edge YAML and arbitrary static interrupts remain separate gates. |
| Node type `state_modifier` -> `StateModifierNode` | Apply a declared template, project the first output to the existing state type and clear selected variables | `src/agents/graph/{state_modifier,compiler}.rs` | ADK custom `Node`, state channels and ordinary edges | Implemented capability-disabled. MiniJinja execution is fuel-bounded; source/template/regex/variable counts and serialized result are bounded. `from_json`, `base64_to_string`, `split_by_words` and `split_by_regex` match the active SDK filter surface. Fresh state descriptors/defaults are admitted and resume state wins without reseeding semantic input. `END` remains only a graph sink; when this value-producing node explicitly routes there, its first output becomes a separate public-result candidate projected as one normal browser model turn. Private graph state is never serialized wholesale |
| EliteaUI `PrinterNode.jsx`, `usePrinterInputMapping.js`; SDK `langraph_agent.py::{PrinterNode,create_graph,LangGraphAgentRunnable.invoke,extract_terminal_state_output}` | Project the fixed/state/f-string `printer` input, append the configured continuation message, publish that ordinary chat result, pause after the node, then clear the marker before following the declared transition | `src/agents/graph/{printer,compiler,agent,resume}.rs`, `src/agents/{events,runtime,session}.rs` | Custom ADK `Node`, native `interrupt_after`, `Checkpointer`, `SessionService` and common `Runner` | Implemented capability-disabled. The compiler creates `<id>_reset`, reserves that identifier, inserts `Printer -> reset -> transition`, and owns the static breakpoint. The adapter loads the exact private checkpoint and exposes only the bounded `printer_output`; it never serializes graph state. The browser receives the SDK-style text through ordinary `agent_response`/`full_message`, without a HITL card or premature `pipeline_finish`. A later ordinary user message (`should_continue=true`, `hitl_resume=false`) is appended through the native `messages` reducer and resumes once at reset. Output is bounded to 8 KiB, final guidance to 4 KiB, nested structured values use canonical JSON instead of Python repr, and the public node cannot emit the reserved `PRINTER_COMPLETED` marker. Seven focused/compiler/Runner fixtures cover mappings, defaults, bounds, static pause, exact checkpoint identity, reset, normal continuation, changed-definition/replay rejection and public projection. Nested Agent-node `for_subgraph` Printer behavior and arbitrary static interrupts remain pending. |
| Node type `hitl` -> `HITLNode` | Durable user choice, optional state edit and route | `src/agents/graph/{hitl,resume}.rs`, `src/agents/events.rs` | ADK `interrupt_with_data`, graph checkpoint/resume and `goto` | Initial compiler path implemented. When a pipeline is the selected chat participant, its public interrupt carries `interaction_type=pipeline_hitl_node` and history contract version 1 so persisted history can distinguish this node from sensitive-tool cards. A fresh graph agent resolves one Main-authorized decision only against the latest persisted interrupt event and the exact latest checkpoint/thread/pending node/definition digest. Stale, advanced, tampered or already-consumed decisions fail closed; `block_with_comment` follows the configured reject route without publishing the comment |
| Node type `custom` | UI editing placeholder rejected at execution with supported-type guidance | `src/agents/graph/compiler.rs` rejection | None | Preserve rejection; it is not an executable Rust node |
| `transition`, standalone `decision` and router condition selection | Unconditional or runtime successor selection, including `END` | `src/agents/graph/{compiler,router,decision}.rs` | ADK edges and atomic `goto` | State-modifier transitions, HITL action routes, standalone active Router/Decision nodes and `END` are implemented and validated before graph construction. Historical embedded `decision`/`condition` edge blocks remain a deprecated compatibility gate. |
| `interrupt_before`, `interrupt_after`, printer successor analysis | Pause/resume at the current node or successor without false crash classification | `src/agents/graph/{compiler,agent,printer,resume}.rs`, `src/agents/events.rs` | ADK static interrupts/checkpoints | Compiler-owned Printer `interrupt_after` is implemented with exact private checkpoint identity and ordinary-chat continuation. User-authored arbitrary static interrupts remain explicitly rejected until each public projection and resume contract is defined. |
| `type: parallel` (absent in Python) | Run prepared Agent branch tasks concurrently, wait for all, retain declared result order, aggregate nested pauses, and reuse durable child outcomes | `docs/parallel-pipeline-node-design.md`, then `src/agents/graph/{compiler,parallel}.rs` and `src/state/postgres_checkpointer/parallel_children.rs` | Custom ADK `Node`; one separately checkpointed child `CompiledGraph` per owned Agent branch | Proposed redesign. The node has no implicit model planner and cannot split one raw chat request. V1 owns two to sixteen Agent branches, limits concurrency to eight, uses standard checkpoints, and requires one exact complete decision set. It adds no interrupt table and does not use `PARKED_CHILDREN`. Other branch types and `wait: one`/`many` remain closed. |
| Future `type: map` | Read one state list, dispatch each item through one owned worker definition, and reduce typed outputs in input order | `docs/map-reduce-pipeline-node-design.md`, then a compiler-owned map node and the parallel child runtime | Custom ADK `Node`; one separately checkpointed child graph per item; approved ADK state reducers | Proposed LangGraph `Send` equivalent and separate from fixed topology. An explicit upstream LLM can derive the source list from chat. `LoopAgent` is sequential. `ParallelAgent` owns a fixed unbounded subagent list and fresh process-local shared state. Activation requires bounded dispatch, reducer descriptors, output selection, child replay, aggregate HITL, and live UI proof. |

## Legacy saved-pipeline proof

The current saved `Resolve Name` pipeline uses `Agent 1` as its stored node
identifier. Python applies `clean_string` before graph construction. Rust now
normalizes this legacy identifier to `Agent1` during compilation. Stored YAML
does not change. Normalized collisions fail validation.

The node identifier remains part of checkpoint and call identity. Public
participant and breadcrumb labels use the selected application name. The
projection regression test compiles the legacy document and checks both rules.

Local external-MCP execution `b13c0d177a1da5f35b6c689e88ed8661` invokes
`Full Name Resolver` through this node. Its `Name Resolver` and
`Surname Resolver` descendants start model requests 386 microseconds apart.
The durable session records separate branches and tool-call IDs. The graph
checkpoint binds two distinct authorization interrupt IDs. The outer job
settles and releases its claim after Main returns the authorization pause.

Live authorization resume and final pipeline completion remain separate
gates. See [the external MCP ledger](external-elitea-mcp.md).

## Saved-pipeline regeneration

The current Indexer deletes the requested pipeline checkpoint before regeneration.
The source is `indexer_worker/methods/indexer_agent.py`, in the `is_regenerate` branch.
Core prepares the earlier message history in `elitea_core/api/v2/regenerate.py`.
These sources define behavior, not the Rust implementation structure.

Rust accepts the existing regeneration flag through
`src/agents/assembly.rs` and `src/agents/runtime.rs`.
`src/agents/session.rs` resets the exact pipeline checkpoint and session.
The existing PostgreSQL services enforce claim, thread, and definition ownership.
The new session receives Main's frozen history. Other pipeline threads remain unchanged.

This reset is necessary because ADK Graph loads the latest checkpoint without an explicit resume identifier.
Regeneration must not reuse an old pause or a completed graph frontier.
The operation invalidates old interrupt identifiers for that pipeline thread.
It does not authorize or resume a protected tool.

Component tests cover paused and completed graphs, unrelated checkpoint preservation,
fresh interrupt identity, stale resume rejection, and successful resume of the new graph.
Another test rejects requests that combine regeneration and interrupt resume flags.

The live regression first failed during profile validation, before any model call.
Execution `5a6c3bd33a5e9135863791271af44825` provides this evidence.
Its accepted Main request contained the regeneration flag and a saved pipeline version.
This failure differs from a browser request rejected with HTTP 400.

## ADK action reuse boundary

ADK Graph 2.0.0 already wraps `adk-action` configurations as graph nodes. The
Elitea compiler should translate stored YAML into those actions where the
business contract is genuinely the same, rather than duplicating their node
executor. That does not make every action safe to expose:

| ADK action pattern | Elitea YAML use | Disposition |
| --- | --- | --- |
| `Set` / `Transform` | State modifier and simple state projection | Reuse the reducer/mapping pattern. The current state modifier remains a small custom node because its four SDK filters, typed projection and cleaning contract are not the stock transform language |
| `Switch` | Router and non-model decision edges | The route grammar and target validation informed the implementation, but the bounded Elitea Router remains a smaller custom `Node` because its MiniJinja/`json_loads`, input-selection and SDK label-normalization contract does not match an arbitrary `ActionNodeConfig`. Stored YAML never becomes an unreviewed action document. |
| `Loop` / `Merge` | Deprecated loop compatibility and future fan-in | Reuse only where iteration and merge semantics match. Durable true parallel children keep the existing Elitea checkpointed scheduler because action merge/wait does not own child execution or restart identity |
| `Wait` / trigger | No active UI node today | Do not invent a visible capability. It may be used internally after deadline, cancellation and checkpoint semantics are specified |
| File, code, HTTP, database, email and notification actions | Code node or provider effects | Do not enable directly. These require the existing claim-owned filesystem, sandbox, egress, credential, effect-receipt and resource boundaries; several upstream backends are placeholders or intentionally unrestricted |

The crate currently enables ADK graph primitives without its optional broad
`action` feature. A later node slice may enable only the needed adapter after
the translation and authority tests exist; stored pipeline YAML never becomes
an unreviewed `ActionNodeConfig` pass-through.

## Initial active-node compiler slices

`PipelineDefinition::from_yaml` now admits a complete frozen pipeline document
when every executable node is a bounded dynamic `hitl`, `state_modifier`, `llm`,
configured `toolkit`, direct remote `mcp`, `printer`, `router` or `decision`
node. An LLM- or Decision-bearing document can compile only when the
assembler supplies the invocation-owned node factory; a Toolkit-bearing
or MCP-bearing document requires one exact materialized direct-tool resolver. A pure/control
graph never redeems model or tool authority.
`AuthorizedNativeAssembly::admit_pipeline` selects only a frozen application
whose `version_details.agent_type` is `pipeline`, validates the complete YAML
before PAT, model, tool or state construction, and retains the claim-bound
runtime/session authorities for the graph assembler. It rejects direct agents
and every unsupported node rather than falling through to the ordinary
`LlmAgent` path. Frozen top-level tool references remain only an inventory:
each pipeline LLM, Toolkit or MCP node authority-reduces it to its own
compiler-selected aliases and concrete names; this is not the ad-hoc binding
rule.

It derives typed state defaults and channels, builds `START -> entry_point`,
registers every node, validates all action/transition targets and compiles an
invocation-owned ADK `GraphAgent` with an injected `Checkpointer`. A fresh graph
instance can resume the exact latest checkpoint through `PipelineHitlDecision`;
no caller-supplied checkpoint identifier is trusted. Pipeline continuation is
the graph decision contract with an empty tool-call identity; it never passes
through sensitive-tool `require_tool_confirmation`.

The focused corpus reconstructs the graph twice around two sequential pauses
through the real common `Runner` and injected `SessionService`, proves final
completion and one-use decisions, and rejects stale identities and unsupported
node/static-interrupt documents. It also runs active UI-shaped state/default
YAML through the real Runner, covers the four template filters, typed output,
cleaning, exact serialized bound and stable digest, and proves native message
append, terminal-data precedence, assistant-message selection and deterministic
zero-LLM state fallback. LLM fixtures cover current UI mapping, legacy messages
input, bounded text blocks, exact selected-tool order, structured and plain
state projection, missing-tool rejection and a real compiled graph checkpoint.
Direct Toolkit/MCP fixtures cover current UI fixed/variable/template mappings, exact
kind/alias/action admission, checkpoint-stable `pipeline:<node>:<step>` call identity, one-call
execution, legacy and repaired structured projection, type/bound failures,
read-only enforcement, sensitive same-call denial plus formatted chat output,
and real compiled graph checkpoints without a model turn. The MCP component
fixture proves one bounded discovery, one selected read call, normal Runner
completion, exact checkpoint projection and pre-dispatch rejection of a
server-declared effect. Printer fixtures cover the SDK default and mappings,
compiler-generated reset node, exact static checkpoint enrichment, ordinary
nonterminal browser projection, normal-message resume, one-use replay and
changed-definition rejection through the common Runner. Reducer fixtures cover HITL append/clear and parallel
task merge/clear semantics. Admission fixtures prove the stored
application selector, graph-decision identity and fail-closed tool/node
boundary. The assembler e2e consumes one state grant, pauses, projects a browser
interrupt, resumes the exact private checkpoint and emits the normal terminal
browser lifecycle.

`NativePipelineStateBackend` consumes the post-authorization session authority
once and derives both PostgreSQL writers from the same immutable claim, fence
and live lease. Conversation events and graph frontier state remain in their
existing separate `elitea_runtime` tables inside `agentstate`; no parallel
interrupt table is introduced. The public chat thread stays presentation data,
while the pseudonymous ADK session ID is the private checkpoint thread and the
interrupt-digest input. The Runner-required resume marker is persisted only in
the private session lineage; the graph input mapper does not turn it into
`input` or `messages`.

## Native LLM-node boundary

The graph `messages` channel is retained because it is observable pipeline
state, is part of the checkpoint, and participates in current UI/result
behavior. It is canonical JSON with the ADK append reducer; it is not a Rust
copy of LangChain message classes or runnable control flow. At execution time
the node converts admitted text history into ADK `Content`/`Part` values and
converts the terminal model text back into the declared state projection.

The native ADK agent owns the model/tool loop, tool-call correlation and
structured-schema retry. Elitea owns only the frozen YAML mapping, exact node
tool authority, state type validation, checkpoint join and result projection.
In particular, Rust does not port `llm.py`'s per-call custom graph/runnable, its
fallback second model call for output coercion, mutable client/wrapper state,
implicit hidden tools, string-flattened history or warning-and-skip treatment
of missing tools. Those patterns either duplicate ADK behavior or weaken
authority and restart semantics.

For a sensitive selected tool, native `require_tool_confirmation` produces the
pending function call. Elitea stores one private, bounded
`elitea.graph.llm-tool-replay.v1` envelope in the existing graph interrupt and
claim-fenced session event: exact ADK
history before the pending turn, the pending model content and call IDs,
input/configuration/predecessor digests and at most sixteen exact decisions.
The serialized envelope is capped at 512 KiB and never enters the public HITL
card. Resume validates it against the latest checkpoint, places the resolved
envelope in one reserved graph-state channel and re-emits that saved model turn
once. Approval lets the real tool return its normal result. Reject
and Block With Comment replace the selected call with the common no-effect
adapter, so the next provider request contains a structured
`sensitive_tool_blocked` tool result under the original call ID and no synthetic
user turn. Multiple sensitive calls from one model response accumulate bounded
decisions before continuation. Blocked policy remains an admission failure; it
is not user-authorizable.

Native model/tool lifecycle events cross a bounded invocation-local channel
from the nested `LlmAgent` into the outer Runner stream. Provider request
payloads are stripped, every event is rebound to the root invocation and
stamped with the owning node for `langgraph_node`/`original_name` projection,
and browser progress never enters graph state or checkpoints. The graph terminal
result remains the completion owner but is not rendered as a duplicate model
turn after native node events. A saved-pipeline Agent node now emits the
ordinary Pipeline wrapper and routes inner node progress under that wrapper's
existing call-bound hierarchy. Configured and sensitive-LLM child HITL cards
retain the same path while the durable decision identity still binds the root
and child checkpoints. The explicit LLM replay channel produces the structured
same-call block and closes the browser tool run without provider dispatch or a
fresh planning turn. Raw incremental tool-progress chunks remain a separate
gap. Approved effectful calls also remain closed for production until durable
intent/outcome receipts can distinguish a crash before dispatch from an unknown
result after dispatch. Delegated authorization is a separate `mcp_auth`
interrupt contract shared by MCP and configured Toolkit families. A
materializer-known remote-MCP challenge now marks the original selected LLM
tool for native confirmation. Its existing replay envelope carries the exact
call through Authorize rematerialization or supplies `mcp_auth_decision` on
Skip. Authorization approval does not pre-approve a separate sensitive action.
Parallel nested authorization inside direct saved agents now binds every card
to the persisted interrupt, provider call and application hierarchy, and the
worker resumes a complete Authorize/Skip set without replanning. Main retains
its exact scalar authorization route and now admits a bounded exact complete
decision set through the same field, restores persisted call identity, and
consumes all siblings atomically. Independent partial resume remains an explicit
gap. The Rust delivery owner now publishes
each sanitized authorization card immediately as ordinary progress, preserves
its exact interrupt/call/hierarchy metadata, and binds one ordered
`authorization_requests` aggregate as the sole `PausedMcpAuth` terminal
artifact after ADK reaches EOS. Mixed sensitive-plus-authorization terminal
aggregation remains fail-closed. The same exact replay now
crosses a pipeline Agent node through its existing graph checkpoint: parallel
Authorize/Skip and mixed sensitive/auth leaves retain their hierarchy, while a
partial set fails before materialization or dispatch. Configured families whose
challenge appears only during execution still require their own adapters.

## Native direct Toolkit/MCP-node boundary

The active UI Toolkit and MCP nodes are direct actions, so Rust compiles either
as one ADK graph `Node` around one exact claim-materialized `Tool`. The node kind
is part of the behavior digest and must match the frozen configured-tool or MCP
reference; it cannot switch authority families during resume. It does not
create an `LlmAgent`, expose the rest of the toolset, or ask a model to
reinterpret the result. Auth-free remote MCP uses the native bounded HTTP MCP
connector. Direct-node OAuth/on-demand authorization uses the distinct durable
`mcp_auth` continuation described above; root and pipeline LLM owners use native
original-call confirmation plus session/checkpoint replay. Same-family nested
authorization cards now aggregate at the delivery boundary; independent
partial sibling resume and mixed-guardrail aggregation remain closed. Fixed
catalogue-backed HTTP definitions use Main claim-time resolution. Parameterized variants
and stdio remain closed. The tool context preserves the parent invocation/session/user identity,
scopes, memory, artifacts and described secret access, while the stable
`pipeline:<node-id>:<graph-step>` function-call identity leaves a clear seam for the later
graph confirmation owner.

The active SDK `FunctionTool` converts broad exceptions into assistant messages
containing raw arguments, schemas and dependency text. Rust returns one stable,
data-free graph failure instead. It also validates declared output state types
rather than checkpointing an incompatible value. The UI exposes
`structured_output` for direct Toolkit/MCP nodes but Python never forwards it to
`FunctionTool`; Rust deliberately makes the flag meaningful by accepting only
an object with every declared non-message key.

A sensitive read uses one dynamic ADK graph interrupt before dispatch. The
public decision is rebound to the latest durable session interrupt and exact
private graph checkpoint, pending node, node-definition digest, visit-specific
tool call ID and canonical argument digest. Approval clears the resume channel,
executes once and returns the ordinary projection. Reject or Block With Comment
never invokes the provider: Rust preserves the SDK's formatted
`**Pipeline stopped**` explanation, stores a structured
`sensitive_tool_blocked` tool record with the same call ID, and uses
`NodeOutput::with_goto(END)`. Because no LLM exists in this node, it does not
pretend the graph can choose an alternative. Unlike Python, it leaves declared
typed outputs unchanged rather than writing incompatible nulls. Remote effects
remain unavailable until durable outcome reconciliation can preserve the same
graph checkpoint.

`START` and `END` remain only the graph's entry and sink sentinels. Public
result selection is a separate policy matching the current SDK's business
order: the last populated declared output of a value-producing terminal node
wins; otherwise the last non-empty assistant message wins; otherwise the last
non-empty user-declared state value in YAML declaration order is used. Runtime
channels such as `messages`, context metadata, HITL decisions, parallel task
records, printer/router markers and private resume state are never fallback
results. An all-control graph with no candidate maps to the fixed bounded result
`Pipeline completed.`. Structured state values use compact JSON, Claude-style
text blocks omit thinking blocks, and the selected result is capped at 512 KiB.
The declaration order is included in the definition digest because it affects
the zero-LLM fallback. If the process stops after the Runner appends the
continuation event but before the graph advances, exact suffix recovery still
remains an activation gate.

## Parallel YAML v1

```yaml
- id: gather
  type: parallel
  branches:
    - id: source_a
      node: fetch_a
    - id: source_b
      node: fetch_b
  max_concurrency: 2
  wait: all
  error_policy: fail_after_drain
  output: [gathered]
  transition: summarize
```

The future UI card can expose exactly these fields. Proposed V1 bounds are two
to sixteen branches and at most eight active branches.

V1 declares exactly one output channel. Branch identifiers are stable and
unique. Results are arrays in declared order with `branch_id`, `node`, and
typed `outputs`.

ADK action `WaitAll` is not invoked directly because it treats every current
`branch:*` state key as complete and iterates hash-map order. `WaitAny` and
`WaitN` likewise do not schedule early completion. They remain useful framework
evidence, while the Elitea adapter owns the immutable expected set, durable
child identity and deterministic projection.

Each branch is compiled as a small child graph and receives an opaque,
claim-fenced descendant checkpoint thread bound to its bounded canonical
business-input digest. Resume controls do not change that digest.

Its terminal checkpoint is saved before the branch
returns. A restart with the same input loads finished branches without running
their nodes again; changed input creates a new child lineage, and only
unfinished or failed same-input branches execute. A later loop visit differs by
the checkpointed parent step.

The redesign treats an inner interrupt as a paused branch. The standard outer
graph interrupt aggregates all paused branches in declared order.

Resume requires one exact complete decision set. Standard child and parent
checkpoints recreate a lost aggregate pause without a separate interrupt table.

This node does not implement dynamic map fan-out. The separate map design reads
one list and repeats one owned worker with item-local state.
