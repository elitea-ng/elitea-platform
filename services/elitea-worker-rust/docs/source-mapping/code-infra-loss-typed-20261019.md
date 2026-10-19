# Code: infrastructure failures typed, a never-ready runtime re-run (FU-230, FU-234), 2026-10-19

Stacked on FU-214 (`code-runtime-lost-retry-20261016.md`). It reuses its lost-runtime allowance and classification.

## Problems

- **FU-234.** A Worker is lost after the Code job exists but before its runtime is ready. The provision request
  died with its owner, so the inert runtime never got its ready marker. After the 60 s readiness deadline
  (`src/sandbox/docker_supervisor.rs` `await_prepared`) the replacement claim's job failed with
  `sandbox.preparation_incomplete`. The Worker read that as a definite runtime failure (`InvalidResult`) and settled
  the user-Code card `PIPELINE_CODE_FAILED`, although user code never started. Evidence: pm1084 execution
  `4438b98ad647867831e8e7e292847fa1`.
- **FU-230.** A Supervisor kill during compiled-snapshot publication (Rust `release` phase) settled the node FAILED
  with the user-Code message about 0.7 s later. The compile job itself had completed, and the Supervisor reconciled it
  to `completed` (lease epoch 2) after its restart. Evidence: crash suite LM-SK-rust r2
  `f02cdb5f94f719b4bec2d99bd639da90` (images `9bf82d60d`), deterministic scenario CR-SK-P11.

## Design

- **The dropped connection (FU-230).** On `9bf82d60d` the Worker did not retry a publication RPC whose connection
  dropped. Since #1297 (`src/sandbox/client.rs` `local_transport_failure`), a local transport failure is
  `Unavailable` and is retried with the same identity until the observation deadline. After its restart the
  Supervisor answers a retried `Release` for a `completed` compile job from the durable record
  (`docker_compiled.rs` `publish_snapshot`, the `record.phase == Completed` branch). The kill is therefore expected to
  recover (R). A deployed re-proof on current main is listed below.
- **Infrastructure failures are typed, never user-Code text.**
  - A failed or unconfirmed publication, an unconfirmed compilation and the compile observation deadline become
    `CodePreparationFailure::Unavailable` (`agents/graph/code_compiled.rs`), class `DependencyUnavailable`.
  - A compiler failure (`CompilationOutcome::Failed`) stays the code's own failure.
  - The infrastructure classes (`DependencyUnavailable`, `WorkerInterrupted`, `RuntimeLost`, the last one after the
    FU-214 allowance) now settle `pipeline.code_runtime_unavailable` (`code_runtime.rs` `CODE_RUNTIME_UNAVAILABLE`).
    That projects to the existing retryable `DEPENDENCY_UNAVAILABLE` runtime error with the sentence Main already
    accepts ("A required runtime dependency is unavailable."; Main `runtimeFailurePolicyFor`), so Main and Web need
    no change.
  - Preparation failures keep their own codes.
- **A never-ready runtime is re-run (FU-234).**
  - `sandbox.preparation_incomplete` is written only before the dispatch fence (`provision_inert` →
    `await_prepared`, phase `reserved`). The runner waits for the dispatch file, so user code never started.
  - The Worker classifies it `RuntimeLost` (`code_attempt_remote.rs` `terminal_failure_class`,
    `runtime_loss::NEVER_DISPATCHED_FAILURE_CODES`). The node then re-runs it as a new ordinal attempt with a fresh
    job and runtime, under the FU-214 allowance (2 re-runs inside 15 min).
  - This applies also to a platform-client node, because a runtime that never started can have made no call. It is
    still refused when Main's journal says an effect is possible.
  - Past the allowance it settles as the infrastructure failure.
- **Code tool.** The data-analysis Code tool reports `sandbox.preparation_incomplete` as `Interrupted`, the same as
  a lost runtime.

## Recovery-guarantee rows

| Component × phase | Fault | Before | After |
|---|---|---|---|
| Worker × P09/P10 (final provisioning) | Worker lost after the job exists, before the runtime is ready | F, user-Code text | **I** (re-run, new attempt); infrastructure-typed F past the allowance |
| Supervisor × P11 (publication) | Supervisor killed during `release` | F, user-Code text | R expected (#1297 retry plus durable `completed` record); infrastructure-typed F if publication cannot be confirmed |
| Worker × P10 | an infrastructure class after the node's policy (`DependencyUnavailable`, `WorkerInterrupted`, `RuntimeLost`) | `PIPELINE_CODE_FAILED` | `DEPENDENCY_UNAVAILABLE` (retryable) |

## Tests

- Unit: `CodePreparationFailure::Unavailable` class and phase. A terminal-code table row for `DependencyUnavailable`.
  The shared policy fixture is now `DEPENDENCY_UNAVAILABLE`, and the Worker checks the sentence and the retryable
  flag that Main accepts.
- Real PostgreSQL, production `RemoteCodeRuntime` wiring:
  - a never-ready runtime is re-run, also for a platform-client node, and completes once;
  - with a possible effect it reconciles and is not re-run;
  - past the allowance it settles `pipeline.code_runtime_unavailable`;
  - the FU-214 exhausted case now settles as infrastructure.

Counts, deployed re-proof (CR-SK-P11 on current main; the FU-234 Worker loss during final provisioning) and browser
evidence: pending, see the PR.

## Follow-ups

- The Supervisor could re-provision a never-ready inert runtime itself instead of failing after 60 s. That would
  avoid one Main visit, but it needs proof that no earlier provision can still finish (the current comment in
  `await_prepared`).
- A publication row left `publishing` until its 5 min expiry after a failed publication (FU-230 evidence) is not
  changed here.
