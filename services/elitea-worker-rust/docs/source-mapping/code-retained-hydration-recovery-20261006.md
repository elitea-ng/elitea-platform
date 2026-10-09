# Code recovery through retained hydration

## Observed failure

The restart acceptance retained the original JavaScript runtime and acquired Main claim attempt 2.
The replacement claim arrived 254 milliseconds after the original lease expired.
It remained active for about 30 seconds before the original runtime reached its existing 60-second deadline.
Only Python's two platform reads and JavaScript's first read committed.

The root operator measured native Supervisor concurrency as 1.
The public configuration SHA256 was `484ca9439faaba86acbc89d8013e84a3f2b09c4dc6eca434431b4f831684d385`.
The timing receipt records JavaScript dispatch at `2026-10-06T14:30:05.014572Z` and claim replacement at `14:30:36.017259Z`.
The job failed at `14:31:05.747906Z`.
Both claims carried checkpoint recovery attestation.

## Owning path

`agents/graph/code_attempt_remote.rs` hydrates dependency-backed execution before starting the concurrent whole-job submission and platform pump.
`agents/graph/code_preparation.rs` retries capacity refusals while hydration remains unresolved.
`sandbox/docker_supervisor.rs` retains an execution capacity permit while the dispatched runtime runs.
Previously, `sandbox/docker_hydration.rs` requested another permit before observing the existing job.
With concurrency 1, this ordering prevented restored execution from reaching its platform pump.

## Correction and authority

Hydration still validates the execution grant, delivery authority, prepared request, dependency bundle, and index before reading the ledger.
`JobLedger::read` selects the exact tenant, project, and job key, then verifies the immutable request digest and decodes the phase.
Dispatched and terminal jobs return the existing readiness response without taking another capacity permit.
This response only permits reconciliation of the same request. It performs no hydration, runtime dispatch, or publication.
Digest conflicts, storage failures, and invalid phases remain errors.
Missing and Reserved jobs still require capacity before reserve, claim, provisioning, or content transfer.
Deadlines, lease fencing, cancellation, and execution admission remain unchanged.

## Backend coverage

`sandbox/process.rs` constructs the same `DockerSupervisor` around either Docker or Kubernetes `CodeJobRuntime` implementations.
The correction therefore applies to both backend source paths.
This source coverage does not prove deployed Kubernetes recovery.
Root must rebuild the Containerfile `supervisor` target and verify the retained-runtime restart acceptance on the deployed cohort.

## Verification boundary

Four service-free tests cover occupied concurrency 1, terminal readiness, capacity ownership, and typed ledger failures.
Six indexed authority tests and two client transport tests passed with locked, offline, single-job Cargo commands and all features.
The transport fixture used only its test-owned loopback listener.
Its initial sandbox-denied listener attempt remains in the private receipt packet.
Strict locked, offline, single-job Clippy passed for all targets and all features with warnings denied.
Scoped Rust formatting passed.
The initial Clippy failure identified one elidable helper lifetime; its correction preserves the borrowed permit owner.

Three PostgreSQL regressions use the existing hydration deadline test harness.
They cover retained dispatch with an occupied slot, authority/index/digest refusals, and missing/Reserved admission.
All 15 tests in that harness compile and remain ignored locally because isolated PostgreSQL and fixture TLS were not provided.
The existing `ci-rust.yml` fixture-TLS step runs `sandbox::docker_supervisor::hydration::deadline_tests` with `--ignored`.
That command selects the three new regressions without a workflow change.
Local checks do not prove that CI, PostgreSQL fixtures, Docker recovery, or Kubernetes recovery passed.

## Deployed retest, 2026-10-06

The Supervisor-only rollout uses source `9ae93a9eee2121beea76c79d24572ec0095d1563`
and image `sha256:308e7cd075d0e78927e603d5d9906e1f0d7da86afd60bc2c0f743a8fde72569c`.
All eight profile listeners start. Main, Web, Worker, runtime limits, and concurrency remain unchanged.

Persistent chat 825 retests the same saved pipeline version 166.
Execution `f7200bb11a4e9ff2578fa69b44377298`, generation 1, confirms Worker loss while the original JavaScript runtime remains active.
The Worker restarts and acquires claim attempt 2, but the execution still fails.
Only three platform reads commit. The JavaScript node journal records `authorization_denied`;
the retained runtime reaches `sandbox.deadline_exceeded`.
These records do not establish that authorization was the initial cause.
The final generic `INTERNAL` error persists after browser reload.

Source inspection identifies another cancellation boundary: each fast nonterminal Submit response drops the concurrent platform pump before retry/backoff.
Retained execution can still report Busy while the original runtime owns the capacity permit.
A single claim-bound pump must cover the complete observation loop before this recovery gate can close.
This retest proves neither successful recovery nor deployed Kubernetes behavior.
