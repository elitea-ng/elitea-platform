# Built-in runtime modules

Status: gate 7a, required before indexing. Added by user steering on 2026-09-14.
These modules are distinct from internal MCP tools that manage Elitea entities.
Swarm Mode is excluded. Keep the existing code until its removal or migration has an explicit compatibility decision.

## Scope and source evidence

The current application's Modules control supplies the functional inventory.
New UI `apps/elitea-web/src/features/agents/lib/internalTools.ts` owns authorable flags.
Main `internal/application/agentexecution/tools.go` freezes selections and `internal/db/queries/agent_chat.sql` controls admission.
Rust `src/agents/internal_tools.rs::InternalToolCatalog` currently serves `ask_user` and records unavailable module flags.
A successful chat with an unavailable-module notice does not prove the module executes.

| Module | Current reference and native boundary | Required outcome |
| --- | --- | --- |
| Attachments | Core attachment delivery and SDK file tools; gate 7 artifact authority | Use the actor's authorized attachments and persist valid artifact references across replacement. |
| Data Analysis | SDK `runtime/tools/data_analysis.py`; Rust tool boundary | Execute analysis through a bounded runtime with declared file and artifact access. |
| Image Creation | SDK `runtime/clients/client.py` image-generation operation; Main model and artifact grants | Use an authorized image model and return a durable image artifact to chat. |
| Ask User | Rust `src/agents/internal_tools.rs` already supplies the tool and durable clarification contract | Reuse it; verify UI selection, exact question/answer identity, and recovery instead of adding another tool. |
| Planner | SDK `runtime/clients/client.py` PlanningMiddleware injection | Preserve the plan and task status as execution state across compaction and replacement. |
| Python Sandbox | SDK `runtime/langchain/pyodide_sandbox.py`; gate 5 isolated execution boundary | Reuse bounded sandbox execution with cancellation, output limits, and artifact authority. |
| Smart Tools Selection | SDK `runtime/tools/lazy_tools.py`, `runtime/clients/client.py` lazy-tools mode | Discover and select tools from the admitted catalogue without exceeding the context budget or bypassing authorization. |

SDK paths are in `projects/elitea-sdk/elitea_sdk` and supply business references only.
Main and Rust paths are relative to their services in this repository.
Read current source again when implementing each module; these references do not claim full parity.
Reuse installed ADK primitives when they satisfy the required behavior and ownership.

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
