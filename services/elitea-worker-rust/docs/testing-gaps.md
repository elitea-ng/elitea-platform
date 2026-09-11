# Rust worker verification gaps

Updated: 2026-09-09.

## Progression decision

Gate 2, delegated OAuth and MCP authorization, is accepted for progression.
The user accepts the verification gaps below so implementation can continue with gate 3.
This decision does not declare full parity or permit production capability registration.
Known implementation gaps remain implementation gaps, not passing tests.

The latest slice preserves skills during version creation and synchronizes the continuation branch with Main revision `f28189b5`.
The [remaining-gates register](remaining-gates.md) defines the next implementation order.
Internal MCP lets chat build Elitea entities through Main-owned operations.
External MCP shares opted-in agents, pipelines, and toolkits with other systems.
Both paths already have implementations and retained evidence. Their remaining gaps do not invalidate that evidence.

Standalone toolkit discovery remains separate because Rust does not yet implement `toolkit.available_tools.v1`.
Runtime tool enumeration and binding already work for supported native families.
The existing attachment query cannot replace live per-instance discovery.

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
| TG-12 | Verification: runtime and deployment | Process replacement, claim reclaim, lost acknowledgements, Redis TLS/ACL, load, and Kubernetes. | Prove another worker can continue durable work without the original process or local spool. Keep activation closed until these proofs pass. |
| TG-13 | Verification and UI parity: UI, Main | Participant editing, guard placement, history rendering, and regeneration under collaborative use. | Retain toolkit and owning-agent labels. Prove correct action routing separately from visual parity. Do not attribute a runtime defect to appearance alone. |
| TG-14 | Implementation: Rust, Main | Standalone `toolkit.available_tools.v1` and the internal saved-instance discovery operation. | Implement the shared command with current actor authority and fenced results. Reuse native enumeration. Stored attachment rows cannot replace live discovery. |
| TG-15 | Verification: Main, Rust, UI | Chat-driven entity building with the three newly exposed typed configuration operations. | Select a real model and verify the endpoint project's saved default. Confirm denied permissions cause no mutation. Component and MCP protocol fixtures are not deployed proof. |
| TG-16 | Verification: Main, Rust, UI | Save As Version with attached skills, from the agent and pipeline editors and through internal MCP. | Select a non-default source version. Verify exact skill revisions after save and reload, then launch the new version and confirm runtime consumption. Main transaction and MCP protocol fixtures plus UI component tests pass; deployed browser and runtime proof remain open. |

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
6. Complete artifact-backed capabilities, then indexing last.

The [toolkit Test contract](source-mapping/toolkit-test.md) remains an implementation gate, not a browser-only test gap.
Detailed diagnostics remain tracked by [OBS-RUST-01](source-mapping/agent-runtime.md#obs-rust-01-detailed-runtime-diagnostics).

## Maintenance rules

Update this register with each relevant implementation slice.
Keep detailed source mappings authoritative for business behavior and implementation ownership.
Distinguish unit, component, database, protocol, browser, and system evidence.
Close a row only with evidence for its stated completion requirement.
An accepted progression decision leaves unresolved rows open.
