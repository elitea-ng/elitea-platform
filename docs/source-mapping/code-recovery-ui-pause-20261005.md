# Code recovery pause UI

The timeout produces a durable operator recovery suspension.
The UI must show this suspension without declaring a terminal result.

| Source owner | Existing behavior | UI behavior |
| --- | --- | --- |
| `services/elitea-worker-rust/src/agents/graph/node_recovery_runtime.rs` | Operator approval stops automatic node recovery. | Display a recovery pause only from a validated receipt. |
| `services/elitea-worker-rust/src/agents/events.rs` | Project `agent_node_recovery_required` with `node_recovery_required_v1`. | Bind the receipt to its exact response and execution generation. |
| `services/elitea-worker-rust/src/execution/native_agent_lifecycle.rs` | Close the suspended worker without terminal output settlement. | Keep the observer and Stop control available. |
| `services/elitea-main/internal/infra/db/repos/node_recovery_projection.go` | Suspend the recovery visit and persist its receipt in message metadata. | Restore the notice only for the active streaming response. |
| `services/elitea-main/internal/domain/noderecovery/contract.go` | Validate the strict public receipt and replay safety rules. | Reject unknown, malformed, child-owned, stale-generation, and stale-revision receipts. |
| `services/elitea-main/internal/api/v2/agentexecution/node_recovery.go` | Authorize operator recovery requests separately from normal chat continuation. | Keep operator recovery actions disabled. |

The answer explains the timeout and disabled recovery controls.
New messages remain blocked until the execution stops.
The graph clears the active node indicator and its paused timeline spinner.
The graph keeps its existing Stop action for this recovery suspension.
Other graph controls and authored recovery settings do not change.

The existing generation-bound final result observer remains unchanged.
A recovery event does not synthesize a final result or a terminal event.
The UI does not read credentials, checkpoints, debug source, or execution state through a new endpoint.

Focused tests cover the live event, persisted restore, malformed receipts, child ownership, and stale identity.
Editor tests cover blocked sends, a settled graph spinner, and the existing Stop control.
Typechecking and focused lint validate the UI paths.
Deployment and browser acceptance belong to the parent task.
