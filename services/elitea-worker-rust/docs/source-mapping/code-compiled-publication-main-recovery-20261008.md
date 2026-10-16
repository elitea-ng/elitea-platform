# Original compiled publication recovery, 2026-10-08

## Application behavior and source ownership

The current application defines Code behavior.
A service restart must preserve the original invocation and its result.
The Rust implementation gives compilation, publication, execution, and settlement separate durable owners.
The private acceptance controller observes these owners and controls the isolated fault. It supplies no shipping behavior.

| Owner | Source or symbol | Responsibility |
| --- | --- | --- |
| Current SDK | `elitea_sdk/runtime/tools/function.py` | Define Code source selection, selected input, and legitimate node behavior. |
| Web | Normal saved-pipeline Chat and Send | Start one request and show the original answer after reload. |
| Rust Worker | `agents/graph/code_preparation.rs`, `agents/graph/code_compiled.rs` | Bind the original visit and immutable compiled snapshot before execution. |
| Sandbox Supervisor | `sandbox/docker_compiled.rs`, `sandbox/ledger.rs` | Preserve the original compiler runtime, export epoch, descriptor, and terminal receipt. |
| Main | `domain/runtime/compiled_snapshot.go`, `domain/runtime/compiled_snapshot_receipt.go` | Validate the typed descriptor and receipt. |
| Main | `infra/db/repos/compiled_snapshots.go`, `infra/storage/compiled_snapshot_http.go` | Recover publication of the same immutable snapshot. |
| Main | `runtimecomposition/compiled_snapshots.go`, `transport/runtimegrpc/control/compiled_snapshot.go` | Expose the authenticated compilation lifecycle to the original owner. |
| Main | `infra/db/repos/output_inbox.go` | Project one original result and commit its matching settlement. |
| Rust Worker | `execution/agent_delivery_processor.rs`, `transport/nats_jetstream.rs` | Acknowledge normal command delivery after durable settlement. |

Worker paths are relative to `services/elitea-worker-rust/src/`.
Main paths are relative to `services/elitea-main/internal/`.
This mapping preserves user behavior through durable ownership. It does not copy the legacy process architecture.

## Accepted deployed case

Chat852 uses application145/version177 and one normal Send.
The version retains the accepted Cargo fixture and adds one source comment to produce a distinct compiled binding.
The preparation, selected input, dependency policy, and execution timeout remain unchanged.
The original execution is `76d4865967cadf00a21e30d34261e762`, generation1.

The strict cut observes a completed compiler child with exit0 and its exact typed descriptor.
All16 compiler isolation checks pass.
The durable snapshot remains Publishing, with a live lease, no job result, no cleanup request, and no settlement.
The controller repeats this frontier after receipt capture and immediately before Main stops.

Main stops with a known HTTP204 result.
The stopped-Main check still observes Publishing and the same original compiler receipt and descriptor.
Main restarts with HTTP204 and returns healthy under the same full container contract.
No SQL lock, runtime pause, cache deletion, second Send, or retry changes the publication window.

The original compiler job advances from lease epoch5 to6. Its export epoch remains4.
The same snapshot becomes Ready, with the original request, runtime, descriptor, and terminal receipt.
Exactly one compiled execution follows. The original Main claim remains attempt1/epoch1.
One451-byte result projects and one matching SUCCEEDED settlement commits.

Live chat and normal reload show the same140-byte answer, with accepted count3 and total30.
The answer digest is `00d261ea0865daaa7f144670021653e04c70fc8a7a82af5ac017a5a8b4ebcb19`.
Its original response is `6ce515db-b999-5bc3-93fa-8c7e1b746bec`.
Both original runtime IDs return HTTP404 and are absent from the complete container list.
Both job records have `runtime_cleanup: true`. The separate job `cleanup` fields remain false.
NATS stream messages, consumer pending, and pending acknowledgements return to zero.

The final idle store contains76 terminal jobs,76 resolved dispatches,161 checkpoints, and one compiled snapshot.
Original services, historical rows, platform rows, runtime material, profiles, and retained Workers remain unchanged.
Main retains source `efa7213e803d6f314ee8b6c482b78f45544140b4`.
Worker and Supervisor retain source `c53ab7d5496a571c1844e9906a83c261ff0da06e`. Web retains its42a0 source boundary.

## Evidence and implementation history

The technical result digest is `bb3b6c17aa3eba69df6092488f59694761b49e1718fd096d0e2f7094598430b9`.
The independent root acceptance digest is `bbd428515651d077ed20779901512e8d4623ca412a6907e000f3fbdea4f361b9`.
The browser receipt digest is `89d3fb956d25fee988067ed93a831d51aa115d57dbd4234cc564504773580ba9`.
The before-loss cut digest is `3db61b1b45b0e137c29d949d9d7143ddc850fb79068aab6ff36b654c2e5c27ec`.
The final pre-loss fence digest is `54708ef4fafa46c9e7b1f6b170d152413be4e8ae4a1d3ecf85c7ff8d0cead752`.

The reviewed controller digest is `f4922121b65b0f121ab5b05a5a8c89534bbabdecdac2442859b0349d7e7c25f3`.
Its dependency inventory verifies16 product source pins and51 private source or evidence pins.
The reviewed `prepare` and `execute` commands return direct exit0.
Independent terminal, removal, preservation, NATS metadata, and browser readback return direct exit0.

This case requires no product source correction.
The earlier preflight refuses before Send because the original NATS metadata read exceeds its three-second limit.
The successor allows five seconds for that exact metadata GET. Other read and control limits remain unchanged.
Six focused checks verify this narrow correction. The original refusal and diagnostic receipts remain preserved.

## Limits

This case closes Main loss during publication of the original successful compilation.
It does not prove Main claim replacement or arbitrary publication phases.
Preparation outcomes, debug replacement, queued/in-flight NATS loss, Kubernetes, and deployed canvas acceptance remain open.
The compiler has all16 capture checks. This case supplies no new execution-runtime capture or performance benchmark.
Product and execution-store observations are sequential, not atomic.
Private controller socket limits do not supply an independent total watchdog for incremental HTTP headers.
Complete Code and full worker release remain open. Workspaces remain deferred.
