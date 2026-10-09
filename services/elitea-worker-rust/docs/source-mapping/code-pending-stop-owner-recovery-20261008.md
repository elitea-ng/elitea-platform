# Pending Code preparation Stop after Worker loss, 2026-10-08

## Application behavior and ownership

The current application's Stop behavior remains the reference.
Stop must suppress user Code and downstream nodes, including when the Worker loses its process.
The implementation keeps PostgreSQL authority, durable dispatch identity, and normal NATS delivery.

| Owner | Source or symbol | Responsibility |
| --- | --- | --- |
| Main | `db/queries/runtime_agent_execution.sql`, `ListPendingAgentExecutionIDs` | Publish and repair visibility only for the admitted nonterminal state. |
| Main | `infra/db/repos/claims.go`, `ClaimRecoverRunningNoACK` | Grant a fresh attempt and lease epoch after original claim expiry. |
| Rust Worker | `execution/agent_delivery_processor.rs`, `process_output_recovery` | Observe pending cancellation and stop the original sandbox scope before settlement. |
| Rust Worker | `agents/graph/code_remote.rs`, `CodeRuntimeFactory::stop` | Stop or collect the original terminal receipt and resolve its dispatch. |
| Sandbox Supervisor | `sandbox/ledger.rs`, `sandbox/docker_supervisor.rs` | Preserve the original job, cancellation state, durable receipt, and runtime identity. |
| Rust Worker | `execution/command_bus.rs`, `transport/nats_jetstream.rs` | Confirm acknowledgement after the fenced terminal settlement. |
| Web | Existing editor Test, Stop, History, and Restore Test | Start and stop one normal run. Restore its original terminal history. |

Worker paths are relative to `services/elitea-worker-rust/src/`.
Main paths are relative to `services/elitea-main/internal/`.
The private acceptance controller observes these application contracts. It supplies no shipping behavior.

## Accepted deployed case

Chat850 starts one saved pipeline version176 through normal editor Test.
The original preparation runs with no result or bundle at the pending Stop cut.
All16 isolation checks pass for that original runtime.
The Worker loses its process after the repeated pending cut. Its replacement uses a fresh empty private spool.
The replacement receives claim2/epoch2 and settles the same execution as CANCELLED.
The original preparation job becomes cancelled, and its dispatch resolves.
No user Code job, downstream job, platform effect, or replacement execution starts.
One projected28-byte canonical cancellation and one matching settlement commit.
The original runtime returns HTTP404 and is absent from the complete job-container list.
Stream messages, consumer pending, and pending acknowledgements all return to zero.
Live chat, reloaded History, and Restore Test retain the original cancelled run.

The idle store retains72 terminal jobs,72 dispatches, and151 checkpoints.
The full protected cohort,72 material files,eight profiles,historical rows,and platform rows remain unchanged.
The prior Workers and their spools remain preserved.
Worker and Supervisor images retain the accepted c53 source boundary.

The technical result digest is `f4faf5b53c47083f7ca04f9eaca40cdcc3b8faaeaa520f8dd9da19e3439b9df0`.
The independent root acceptance digest is `56984a80275c3d3b1be7f0f2f55baa5bef5c6b4169ae84c9f6aad07e9e80f77c`.
The browser receipt digest is `2a668cedfcd83a9b77a7820eb4618ba10f865856e50b84a27c85b941fd9c5a18`.
The current native adapter digest is `92985c470d677c34dabddab89d76b80fd62f7e2576801d62ea7ae9de02d5e71e`.

## Limits and retained history

This case closes pending preparation Stop across Worker loss.
It does not prove the distinct preparation-cancelled failure message, live Code owner loss, other service loss, or Kubernetes.
The [live canvas correction](code-live-canvas-terminal-20261008.md) still requires shipping-image and deployed browser acceptance.
Stored cleanup flags remain false. Independent removal checks supply the physical cleanup proof.
Product and execution-store observations are sequential, not atomic.
The [deployed acceptance record](code-nats-deployed-acceptance-20261007.md) preserves earlier missed cuts and private harness corrections.
