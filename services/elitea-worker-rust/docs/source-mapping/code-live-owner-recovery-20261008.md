# Live Code owner recovery, 2026-10-08

## Application behavior and ownership

The current application's Code behavior remains the reference.
An infrastructure restart must retain the original Code invocation, selected state, and result.
PostgreSQL owns execution authority. NATS carries commands. The Sandbox Supervisor owns the original sandbox receipt.

The private acceptance controller tests these contracts. It supplies no shipping behavior.

| Owner | Source or symbol | Responsibility |
| --- | --- | --- |
| Web | Normal saved-pipeline Chat and Send | Start one persistent request and show its original terminal result after reload. |
| Main | `infra/db/repos/claims.go`, `ClaimRecoverRunningNoACK` | Issue a fresh claim attempt and lease epoch with original checkpoint recovery authority. |
| Rust Worker | `agents/graph/code_runtime.rs`, `agents/graph/code_state.rs` | Select the saved source and count input. Apply the typed result once. |
| Rust Worker | `agents/graph/code_attempt_remote.rs`, `sandbox/code_recovery.rs` | Bind the original visit, request, job, prepared bundle, and dispatch identity. |
| Sandbox Supervisor | `sandbox/ledger.rs`, `sandbox/docker_supervisor.rs` | Reclaim the original job after lease expiry. Collect the original runtime receipt. |
| Rust Worker | `agents/graph/node_recovery_runtime.rs` | Preserve Completed and Failed.Stop journals. Suppress the downstream sentinel. |
| Main | `infra/db/repos/output_inbox.go` | Project one claim-fenced failure and commit its matching settlement. |
| Rust Worker | `execution/agent_delivery_processor.rs`, `transport/command_bus.rs`, `transport/nats_jetstream.rs` | Deliver the terminal result and acknowledge normal delivery after settlement. |

Worker paths are relative to `services/elitea-worker-rust/src/`.
Main paths are relative to `services/elitea-main/internal/`.
The current SDK Code reference remains `elitea_sdk/runtime/tools/function.py`.
This recovery design preserves user behavior through durable ownership rather than copying the legacy process implementation.

## Accepted deployed case

Chat851 starts unchanged saved pipeline147/version170 through normal Chat and one Send.
The JavaScript node waits30 seconds and returns its selected count2.
The next Python node intentionally has an empty variable source. The final9999 sentinel must not run.

The first strict cut observes the original running JavaScript runtime without a result or terminal settlement.
All16 isolation checks pass. The repeated cut still binds the same live claim and original job.
The original Worker loses its process. Its replacement uses the same immutable image and one fresh empty private spool.

The second strict cut observes that same running JavaScript runtime before its result.
The Sandbox Supervisor loses its process and restarts with the same full container contract.
All eight authenticated listeners return after restart.

The original JavaScript job completes with typed result2 under the same runtime and request identities.
Its sandbox lease epoch advances from4 to5 after normal expiry and recovery.
Main grants claim2/epoch2 with `AGENT_MODEL_CHECKPOINT` authority.
The original completed result projects once. The empty next node records typed invalid-input Failed.Stop.
No replacement execution, user Code rerun, extra sandbox job, or sentinel starts.

One189-byte `PIPELINE_CODE_FAILED` output and one matching FAILED settlement commit.
Both original runtimes return HTTP404 and are absent from the complete job-container list.
Stream messages, consumer pending, and pending acknowledgements return to zero.
Live and reloaded chat retain the same safe message and support reference `ca094eeb-04b0-531c-91a1-6b8dc0b657ba`.

The idle store retains74 terminal jobs,74 dispatches, and157 checkpoints.
All protected containers,72 material files,eight profiles,historical rows,and platform rows remain unchanged.
The three prior Workers and their unopened spools remain preserved.
The sole replacement Worker is `761c497b45b0a507587ce77d265f4d7fe14683363c7828d0d762cdd97552dff1`.

Its full fingerprint is `ce73a172195e21c7a9bf57aaaa02a80d08a84faf1f94c8c472c5b2e88a3dc081`.
Worker and Supervisor images retain the accepted c53 source boundary. Web retains its42a0 source boundary.

## Evidence and implementation history

The technical result digest is `b333f8f0c2a38ff6c19f52231c5f7facb2b3df07c6f40c3e1339f01d966e62c7`.
The independent root acceptance digest is `320c26ce098c1ba52137a0beeb2f5640b0b4a66f1901977ebd5aa5d3b7969f01`.
The browser receipt digest is `c1cf39ee24ec1791695c6501f3e45941d62deeb39825fe936f749c65e3457059`.
The current native adapter digest is `0caa1eabae0a3ca7447dcef539af5f010639ab48c262d2758f81e36c764bc794`.
The Worker live-cut digest is `59ec1bfeb15a070a08760673607bcbd2cd4723190bdb289ea204f8e8ee60fcc7`.
The Supervisor live-cut digest is `5aaddb1900fca8c34e0be8ca5bfc4dae8870e7105b5dbd2df1b022f9293bdf2e`.

This case requires no Rust product correction.
The private controller validates the accepted successor chain and exact retained container contracts.
Its mount exclusion cache uses an immutable conservative set after full preservation checks.
Every fault cut still reads fresh job, runtime, profile, claim, phase, and all16 isolation checks.

The terminal guard follows ledger behavior: a completed receipt can retain a future lease timestamp.
It requires completed receipt ownership and epoch continuity. It does not require an artificial lease wait.
The frozen earlier controller and all accepted historical receipts remain unchanged.

## Limits

This case closes live Worker and Supervisor loss for the original JavaScript Code job.
Preparation failure outcomes and replacement debug reconciliation remain open.
Main compilation publication loss, queued/in-flight NATS loss, and current Kubernetes acceptance remain open.
The separate live canvas correction still requires its shipping image and deployed browser acceptance.
Stored cleanup flags remain false. Independent removal checks supply physical cleanup proof.
Product and execution-store observations are sequential, not atomic. This case supplies no performance benchmark.
