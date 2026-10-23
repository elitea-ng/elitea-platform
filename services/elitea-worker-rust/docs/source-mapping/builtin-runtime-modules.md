# Built-in runtime modules

Status: gate 7a, required before indexing. Added by user steering on 2026-09-14.
These modules are distinct from internal MCP tools that manage Elitea entities.
Swarm Mode is excluded. Keep the existing code until its removal or migration has an explicit compatibility decision.

## Scope and source evidence

The current application's Modules control supplies the functional inventory.
New UI `apps/elitea-web/src/features/agents/lib/internalTools.ts` owns authorable flags.
Main `internal/application/agentexecution/tools.go` freezes selections and `internal/db/queries/agent_chat.sql` controls admission.
Rust `src/agents/internal_tools.rs::InternalToolCatalog` serves `ask_user`, the two builders, and (since the planner / attachments / Smart Tools Selection port below) `planner`, `attachments` and `lazy_tools_mode`, and `subagents` (#1226: the runtime's read-only sub-agents, `libs/rust/agent-runtime/src/subagents/`, bound on the toggle and on every cloud-executor workspace turn by `src/agents/subagents.rs`); it records the remaining unavailable module flags.
A successful chat with an unavailable-module notice does not prove the module executes.

## Ask User editor control, 2026-09-30

The current UI declares `ask_user` in `src/[fsd]/shared/lib/constants/internalTools.constants.js`.
The new UI omitted this entry from `features/agents/lib/internalTools.ts`.
This prevented users from selecting the existing Rust clarification tool in the Agent editor.
The restored entry uses the existing module selection, persistence, and runtime-capability paths.
No worker tool, database field, or approval contract changes.

The rehearsal UI image `elitea-web:ask-user-20260930` includes this correction.
Agent 137/version 144 displays Ask User, accepts its selection, and retains the checked switch after browser reload.
All 38 focused module-list and Agent-mapping tests pass. Typechecking and the application build pass.
This proves editor selection and persistence. Live child clarification and worker-replacement testing remain separate requirements.

| Module | Current reference and native boundary | Required outcome |
| --- | --- | --- |
| Attachments | Core attachment delivery and SDK file tools; gate 7 artifact authority | Use the actor's authorized attachments and persist valid artifact references across replacement. **Served** (2026-10-13): see "Planner, Attachments and Smart Tools Selection" below. |
| Data Analysis | SDK `runtime/tools/data_analysis.py`; Rust tool boundary | Execute analysis through a bounded runtime with declared file and artifact access. **Served** as deterministic table tools plus the code sandbox (ADR-0033); the SDK's model-written pandas is not ported. |
| Image Creation | SDK `runtime/clients/client.py` image-generation operation; Main model and artifact grants | Use an authorized image model and return a durable image artifact to chat. |
| Ask User | Rust `src/agents/internal_tools.rs` already supplies the tool and durable clarification contract | Reuse it; verify UI selection, exact question/answer identity, and recovery instead of adding another tool. |
| Planner | SDK `runtime/clients/client.py` PlanningMiddleware injection | Preserve the plan and task status as execution state across compaction and replacement. **Served** (2026-10-13). |
| Python Sandbox | SDK `runtime/langchain/pyodide_sandbox.py`; gate 5 isolated execution boundary | Reuse bounded sandbox execution with cancellation, output limits, and artifact authority. |
| Smart Tools Selection | SDK `runtime/tools/lazy_tools.py`, `runtime/clients/client.py` lazy-tools mode | Discover and select tools from the admitted catalogue without exceeding the context budget or bypassing authorization. **Served** (2026-10-13). |

SDK paths are in `projects/elitea-sdk/elitea_sdk` and supply business references only.
Main and Rust paths are relative to their services in this repository.
Read current source again when implementing each module; these references do not claim full parity.
Reuse installed ADK primitives when they satisfy the required behavior and ownership.

## Planner, Attachments and Smart Tools Selection, 2026-10-13

Three of the seven modules now run on the native runtime instead of being recognized and skipped.
`PLATFORM_INTERNAL_TOOLS` shrinks to `data_analysis`, `image_generation`, `internal_mcp`, `pyodide` and `swarm`.

| Module | Rust owner | What the model gets | Where it differs from the SDK |
| --- | --- | --- | --- |
| Planner | `libs/rust/agent-runtime/src/planner.rs` | `update_plan`, `start_step`, `complete_step`, `get_plan_status`, `delete_plan` with the SDK's names, schemas, result texts and rendering, plus the `PLAN_ADDON` guidance after the agent's instructions | The plan is session state (a `state_delta` on the tool-result event), not a file: it is persisted with the thread, survives compaction and worker replacement, and is scoped to the session (no `conversation_id` argument). Bounded to 100 steps. |
| Smart Tools Selection | `libs/rust/agent-runtime/src/lazy_tools.rs` | `get_tool_schema` and `invoke_tool` instead of the toolkit tools, and the SDK's tool index in the system prompt; off below 20 tools | Tools that need a confirmation or delegated authorization stay bound directly and are not reachable through `invoke_tool`. The registry is the admitted catalogue. Results are returned as JSON values. |
| Attachments | `src/agents/attachment_tools.rs`, `ClaimAttachmentSource` in `src/agents/attachments.rs` | `list_attachments`, `read_attachment`, `search_attachment` on every turn over the conversation's stored files, each read on first use | Reuses the claim-bound artifact `list` route (prefix `{conversation}/` in `chat-attachments`) and the attachment-object route; no new route. Search without an id reads at most 8 unread files. |

All three bind on the ordinary agent path only (where the builder tools bind); a nested child agent or a pipeline node gets none.
The agent form's availability flags follow: Main's `GET /elitea_core/runtime_capabilities` reports `planner`, `lazy_tools_mode` and `attachments` as available on the Rust worker, so the UI no longer shows them as Python-only.
Proof: `planner_tests.rs`, `lazy_tools_tests.rs`, `attachment_tools_tests.rs`, and the graph-level `ordinary_tests::internal_tools_graph` suite that drives each toggle through the ordinary assembler, the ADK loop and the model gateway.
Browser acceptance (fresh headed UI run correlated with worker execution) is still owed for these three before gate 7a closes.

## Builder relationship

Agent entity creation, separate persisted instructions and settings updates, skills, and project-context drafts have gate 3 evidence.
Use [the point 3 audit](point3-audit-20260913.md) and its focused mappings.
The module controls must route to those same contracts and preserve project and actor identity.
Do not implement another builder engine.
Pipeline creation through chat remains deferred until schema and graph work provide its composition contract.
The highlighted Agent & Pipeline Builder label is not proof that pipeline creation already works.

## Completion requirements

Each selected module must execute the intended tool or mode through Rust, with exact results and no duplicate calls.
Test saved-agent and ordinary chat selection, reload, authorization failure, and relevant replacement behavior.
Prove browser controls through fresh headed UI tests and correlate them with worker execution.
Record current-source mappings and implementation history with each code change.
Artifact and sandbox modules must reuse gates 7 and 5 respectively.
Do not advance indexing while these included modules remain unavailable.
