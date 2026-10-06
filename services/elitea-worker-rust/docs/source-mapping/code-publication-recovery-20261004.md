# Code publication recovery verification

Date: 2026-10-05. Point 5 remains open.

The [functional audit](code-functional-parity-20260930.md) records the current-platform Code behavior.
The [shipping record](compiled-diagnostics-shipping-20261004.md) records the verified Supervisor diagnostic source.
The new platform adds durable compilation, publication, and execution phases.
The current platform does not provide an equivalent publication restart contract.

## Ownership and source mapping

| Behavior | Owning source |
| --- | --- |
| Build and retain the compiled descriptor | [`RustAdapter::execute`](../../../../services/elitea-code-runner/src/rust_execute.rs) |
| Hold the compiler until publication releases it | [`await_release`](../../../../services/elitea-code-runner/src/compiled_snapshot.rs) |
| Record the completed child receipt | [`main`](../../../../services/elitea-code-runner/src/main.rs) |
| Stage content, release the compiler, and reconcile its receipt | [`DockerSupervisor::publish_snapshot`](../../src/sandbox/docker_compiled.rs) |
| Publish the Ready snapshot | [Compiled content publication](../../../elitea-main/internal/infra/storage/compiled_snapshot_http.go) |
| Execute the verified snapshot in a separate runtime | [`RemoteCodeRuntime`](../../src/agents/graph/code_compiled.rs) |
| Persist graph state | [`PostgresCheckpointer`](../../src/state/postgres_checkpointer.rs) |

The compiler can exit after Release, before the snapshot becomes Ready.
A Docker process exit code of zero does not establish successful child compilation.
The runner can write a failed child receipt and return normally.
Recovery verification must bind the original runtime, descriptor, publication reservation, and successful child receipt.
The verification must preserve authorization, lease, generation, and cleanup checks.

## Deployed natural completion

Root creates version 161 of isolated pipeline 145 through the UI.
Persistent chat 808 selects that exact saved version before one explicit Send.
The public source adds one fresh comment to the accepted Rust calculation.
It uses pinned Cargo dependencies, structs, a trait, and an async function.

The execution admits generation one and completes without a restart.
The compiled snapshot becomes Ready.
Compile and Execute use distinct jobs, and both confirm cleanup.
The result contains three accepted rows, a total of 30, and two ordered groups.
The latest graph checkpoint contains the same typed `probe_result` value.
Product storage contains one final answer and no active streaming flag.

The live browser keeps its pending display after settlement.
An explicit reload displays the exact persisted answer.
This observation does not identify the failing stream boundary.
The receipt lacks a browser cursor and SSE event capture.
Live streaming acceptance remains open.

The publication fault guard refuses with `container.running`.
It performs no stop operation and establishes no restart recovery result.
This refusal preserves the test's original evidence.
A corrected guard requires a fresh version, chat, source marker, and execution.
It must not retry this terminal execution.

## Exact evidence

| Record | SHA-256 |
| --- | --- |
| Supervisor rollout receipt | `9554b45511d5b905de20028b986414e7d2a61c89084af69c34dfd1ddd6452414` |
| Refused publication fault observation | `9990541a52713de9aa9e5e6d81cbe846715fa326a271084fe4cb831c5a09ea4e` |
| Independent terminal and typed-state readback | `c2f7fb733ebb95d4b4158a020028a2cf691434029e9c591a2036b9f6f9252613` |

Root verifies the current Main, Worker, Supervisor, and database identities before the run.
The Supervisor replacement preserves the Main and Worker identities and the seven isolated profiles.
No general feature enablement follows from this test.

## Remaining acceptance

Verify the corresponding Kubernetes compilation and publication path.
Assemble the saved workspace, platform-call, debug, and graph-extension contracts before their release checks.

## Main publication recovery passes, 2026-10-05

Root uses fresh version 162 of isolated pipeline 145 in persistent chat 809.
The saved source contains a fresh marker. The chat has no previous messages.
The watcher captures one original admission after one explicit UI Send.
The compiler exits successfully after Release and retains its exact completion receipt.
The guard verifies that receipt, the original runtime, source, image, descriptor, and live publication reservation.
Thirty-seven offline guard checks pass before this live test.

Root stops the exact Main container while the snapshot remains Publishing.
The guard records Publishing immediately before and after the stop.
The wrapper restores the same Main container and verifies health.
The original execution reaches Succeeded with the same command digest and generation.
The snapshot becomes Ready. The compile and execution jobs confirm cleanup.
No second Send or replacement execution follows the stop.

Independent readback verifies one final answer and the matching typed graph checkpoint.
The result contains three accepted rows, a total of 30, and two ordered groups.
The live browser shows that result without a reload and releases Stop.
This verifies the recorded Docker cohort. It does not establish the cause of the earlier chat 808 display failure.
It proves Main recovery around publication. It does not prove that an in-flight upload was interrupted.
The earlier refused fault remains separate evidence.

| Record | SHA-256 |
| --- | --- |
| Original admission scope | `6d1b51449b98ed2dd0b820bdc66d60de0c2c445336c6d1cf4852ae68ad0304f6` |
| Main stop and restore observation | `e15cf4b32417797c4a5cf752319be85d80dfe797b18eef4cd886657024e0df5f` |
| Independent final result and checkpoint readback | `48ff12203d59f525ba7b00511a5125cd273face7b13e710410ebed9831104ea7` |
| Live browser result | `b9291fdef915ab530d29b6cef2442339fcd774bb6a21612cbb2848b9068ef63c` |

Preparation-job cleanup remains a separate check. Its completed row does not report confirmed cleanup.
No general feature enablement follows from this isolated acceptance.
