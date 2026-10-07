# Rust worker verification gaps

Updated: 2026-10-07.

## Progression decision

Gate 2, delegated OAuth and MCP authorization, is accepted for progression.
Gate 3 is also accepted for progression; [the audit](source-mapping/point3-audit-20260913.md) records its deployed proof and limits.
Implementation now continues with gate 4. Historical rows below retain broader verification requirements.
This decision does not declare full parity or permit production capability registration.
Known implementation gaps remain implementation gaps, not passing tests.

The latest slice preserves skills during version creation and synchronizes the continuation branch with Main revision `f28189b5`.
The [remaining-gates register](remaining-gates.md) defines the next implementation order.
Internal MCP lets chat build Elitea entities through Main-owned operations.
External MCP shares opted-in agents, pipelines, and toolkits with other systems.
Both paths already have implementations and retained evidence. Their remaining gaps do not invalidate that evidence.

Standalone toolkit discovery now implements `toolkit.available_tools.v1` and has deployed gate 3 acceptance.
The focused [discovery](source-mapping/toolkit-discovery-authorization.md) and [request recovery](source-mapping/toolkit-request-recovery.md) ledgers own that evidence.

## Evidence retained

- [Delegated OAuth and DCR](source-mapping/delegated-oauth-dcr.md) records component, PostgreSQL, and deployed emulator proofs.
- Those proofs cover authorization-code exchange, public and confidential DCR, refresh, reload, Main restart, and OpenAPI cross-tab logout.
- The 2026-09-08 real SharePoint run adds authorization pause, exact credential binding, resume, and a protected read.
  Execution `82e49b3e50970c7456e09f541389a883` has no matching credential token and ends in `PAUSED_MCP_AUTH`.
  Execution `d1f07d98c1da0e9fbb9984b1dac45ac0` resumes with that credential and ends in `COMPLETED`.
  `get_files_list` returns 100 records without a tool error. The final answer persists and settlement succeeds.
  This is a regenerated turn followed by authorization resume, not a SharePoint refresh proof.
- [External MCP](source-mapping/external-elitea-mcp.md) separates toolkit execution from saved-agent and pipeline pause evidence.
- [Active-run expiry](source-mapping/delegated-auth-expiry.md) records component evidence and its remaining deployed boundary.

## Open register

| ID | Classification and owner | Remaining proof or work | Completion requirement |
| --- | --- | --- | --- |
| TG-01 | Verification: Rust, Main, UI | Sensitive-tool approval, Block, and Block With Comment through deployed chat, agent, and pipeline paths. | Preserve the original call identity. Denial executes no effect. Approval executes only the authorized operation. |
| TG-02 | Verification and possible implementation: Rust, Main, UI | Parallel branches with pipeline HITL, sensitive-tool guards, and delegated authorization together. | Resolve each branch independently by invocation and interrupt identity. Keep other branches paused. Reuse completed siblings. Join the parent only after required children finish. |
| TG-03 | Verification: Rust, Main | Partial decisions, reverse decision order, duplicate decisions, and identical tool names in different nested branches. | Resume only the owning child and checkpoint. Reject stale or foreign decisions. Do not replay completed effects or replan completed siblings. |
| TG-04 | Verification: UI, Main, Rust | Reload, regeneration, and competing tabs while mixed guards remain pending. | Preserve pending cards and ownership. Consume a decision once. Remove resolved controls from collaborating tabs. |
| TG-05 | Verification: UI, Main | Real-provider refresh, expiry, revoke, logout, and next-turn reuse for regular delegated toolkits. | Repeat the emulator outcomes against a real provider. Preserve unrelated credentials. Do not infer refresh from one successful protected read. |
| TG-06 | Verification: Rust, Main, UI | Token expiry or rejection during an active model/tool loop and remote MCP call. | Pause safely, reauthorize the correct toolkit, and resume without duplicate protected effects. Direct OpenAPI component evidence is not full system proof. |
| TG-07 | Implementation: Main, UI | Standalone editor Login for regular delegated toolkits. | Supply authorization metadata through the shared Main configuration path. Reuse the existing consent flow. Runtime guard authorization does not close this entry point. |
| TG-08 | Verification: UI | Cross-tab logout from the MCP editor and immediate collaborator guard-decision updates. | Repeat the deployed OpenAPI logout proof from the MCP editor. Separately prove prompt removal after another tab resolves a guard. |
| TG-09 | Observed warning: Rust | `agent_session_terminal_completion_unavailable` during the real SharePoint resume. | Determine whether the event needs streamed text. Prove durable history remains complete across a subsequent turn and worker replacement. |
| TG-10 | Verification: Main, Rust | External MCP saved-agent and pipeline completion, failure, mixed guards, resume, and replay. | Correlate the external result with durable terminal state. A settled authorization-pause job is not completed agent work. |
| TG-11 | Implementation: UI | Unrelated expired grants still trigger refresh failures during token collection. | Isolate grant failure to its credential and avoid unnecessary refresh work. Preserve valid grant reuse. |
| TG-12 | Verification: runtime and deployment | Process replacement, claim reclaim, lost acknowledgements, NATS JetStream restart/leader change, load, and Kubernetes. `execution/nats_live_tests.rs` covers the secured single-server transport path. | Prove another worker can continue durable work without the original process or local spool. Keep activation closed until these proofs pass. |
| TG-13 | Verification and UI parity: UI, Main | Participant editing, guard placement, history rendering, and regeneration under collaborative use. | Retain toolkit and owning-agent labels. Prove correct action routing separately from visual parity. Do not attribute a runtime defect to appearance alone. |
| TG-14 | Accepted for gate 3 progression: Rust, Main, UI | Standalone `toolkit.available_tools.v1` and saved-instance discovery have deployed evidence. Wider provider coverage remains separate. | Retain actor authority and fenced results; use the focused discovery and recovery ledgers for covered cases. |
| TG-15 | Verification: Main, Rust, UI | Chat-driven entity building with the three newly exposed typed configuration operations. | Select a real model and verify the endpoint project's saved default. Confirm denied permissions cause no mutation. Component and MCP protocol fixtures are not deployed proof. |
| TG-16 | Verification: Main, Rust, UI | Save As Version with attached skills, from the agent and pipeline editors and through internal MCP. | Select a non-default source version. Verify exact skill revisions after save and reload, then launch the new version and confirm runtime consumption. Main transaction and MCP protocol fixtures plus UI component tests pass; deployed browser and runtime proof remain open. |
| MODULE-RUST-01 | Implementation and verification: Rust, Main, UI | Gate 7a built-in runtime modules, distinct from internal MCP. | Complete the [module ledger](source-mapping/builtin-runtime-modules.md), with source mappings, real invocation, UI proof, authority, and replacement behavior before indexing. Exclude Swarm. |

The user moves `CODE-WORKSPACE-01` out of this active register on 2026-10-05.
Its complete scope is preserved as [WF-01 in the post-worker backlog](wanted_feature.md#wf-01--code-workspaces).
Complete and release the full worker before starting this future feature.

## Mixed-guard test fixture

Use one parent with three independently identifiable branches.
One branch pauses at a pipeline HITL node.
One branch requests sensitive-tool approval through an effect-counting emulator.
One branch uses a delegated toolkit and pauses at its authorization tool.
Repeat the fixture inside a saved pipeline Agent node.

Test each decision order, including partial decisions and all decisions together.
Test Authorize, Skip, approve, reject, and Block With Comment where each contract permits them.
Include two calls to the same toolkit and equal tool names in different toolkits.
Include reload and worker replacement after one sibling completes.
Use property tests for decision permutations and stale identity substitutions.
Use component tests before protocol and deployed browser proofs.

Retain invocation IDs, interrupt IDs, checkpoint identity checks, effect counters, terminal states, and test revisions.
Do not retain tokens, private URLs, provider file data, or credential values.
Do not weaken approval policy or enable effectful operations to make a test pass.

## Remaining implementation order

1. Complete internal and external MCP capability gaps from their source mappings.
2. Reconcile context management, summarization, model, continuation, and tool-name behavior with the current platform.
3. Complete remaining graph capabilities, including deeper composition, child variables, static pauses, and sandboxed Code nodes.
4. Implement the separately designed fixed parallel and data-driven map nodes.
5. Complete durable effect receipts, idempotency, authorization, and crash recovery before toolkit writes.
6. Complete artifact-backed capabilities.
7. Complete gate 7a built-in runtime modules; exclude Swarm.
8. Complete indexing last.

The [toolkit Test contract](source-mapping/toolkit-test.md) has gate 3 progression evidence; broader variants and production recovery retain their own requirements.
Detailed diagnostics remain tracked by [OBS-RUST-01](source-mapping/agent-runtime.md#obs-rust-01-detailed-runtime-diagnostics).

## Maintenance rules

The [Main merge continuation](source-mapping/main-integration-20261007.md) records final snapshot verification and source checks.
All six listener failures in the retained Main packet pass their complete owning selections after the sandbox boundary is removed.
Database and live-service skips remain explicit. Native live conformance remains unexecuted.
The [current deployed acceptance](source-mapping/code-nats-deployed-acceptance-20261007.md) records current native images and normal browser sign-in.
Four-language execution, typed refusal, failed preparation, active Stop, and debug off/on now have separate current evidence.
Later c53 cold, warm, and editor cache cases pass with one exact compiled descriptor.
Supported application-list denial and outer artifact RBAC refusal pass their finite cases.
The [failed-journal owner-loss case](source-mapping/code-failed-stop-owner-recovery-20261007.md) passes current NATS technical recovery, live/reloaded UI, isolation, and runtime removal.
Remaining preparation, authority, service-loss, and Kubernetes acceptance stay open.
Require complete Code acceptance before graph consolidation progresses.

The [2026-10-06 consolidation](source-mapping/point5-consolidation-20261006.md) records the latest Point 5 evidence.
Real-backend editor preservation checks pass. Main's new static consumers still require database and deployed acceptance.
The [Supervisor ownership mapping](source-mapping/code-supervisor-task-ownership-20261006.md) preserves the failed v6 probe and the successful v7 retest.
The deployed v7 Worker-only restart retains its execution and original runtime, then completes all four languages.
Eight unique reads commit. Runtime cleanup, one final browser result, and browser reload all pass.
This exact Worker-loss boundary closes. Supervisor/Main replacement and Kubernetes restart recovery retain separate acceptance requirements.
Generic typed failure display passes in ephemeral chat 826 and persistent chat 827, including persistent reload.
Both failures save their journals before publication, with zero sandbox dispatches and no later-node execution.
The fresh four-language positive run passes in chat 825, including exact answer persistence after reload.
These proofs use deployed Worker `d6568bae8`; they do not prove the later NATS replacement.
Keep NATS complete-cohort acceptance and preparation-message display open until their proofs pass.
The [typed failure source correction](source-mapping/code-terminal-failure-20261006.md) passes 50 Rust tests and 109 Main cases.
Strict Clippy and vet pass. Main preparation-message deployment remains a separate proof.
The later chat 843 case closes actual failed-journal replay on its accepted current NATS image boundary.
State rename reference rewriting remains a separate editor gap.
Workspaces remain outside this register in the post-worker backlog.

The [Web reload correction](source-mapping/code-chat-reload-ui-20261007.md) now passes source tests, image build, and its strict local Alpine scan.
Deployment and ordinary browser verification remain required.
Later preparation fixtures 839 and 844 complete normally after controller refusals before Stop or Worker loss.
They do not close preparation cancellation or owner recovery.
The current idle store retains 59 jobs, 59 dispatches, and 125 checkpoints after the second normal run.
Controller source corrections must cover the exact Main admission and preparation startup states before another fault release.

Update this register with each relevant implementation slice.
Keep detailed source mappings authoritative for business behavior and implementation ownership.
Distinguish unit, component, database, protocol, browser, and system evidence.
Close a row only with evidence for its stated completion requirement.
An accepted progression decision leaves unresolved rows open.
