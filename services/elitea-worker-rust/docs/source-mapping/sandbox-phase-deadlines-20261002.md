# Sandbox phase deadlines

Date: 2026-10-02. Baseline: merged PR 883, `20f0dfd04`.

## Problem and behavior

The supervisor measures readiness and execution from the sandbox reservation time.
Time before runtime binding can exhaust readiness before the runtime becomes ready.
Time before dispatch can reduce the remaining execution observation allowance.

This feature starts readiness at the first durable runtime binding.
It starts execution observation at the durable dispatch transition.
The existing 60-second readiness and 3,660-second execution observation limits remain unchanged.
The runner still enforces its separate configured execution timeout and resource limits.

## Source mapping

| Behavioral reference | New source | Result |
| --- | --- | --- |
| SDK `elitea_sdk/runtime/tools/sandbox.py` interprets remote execution results and failures. | `src/sandbox/docker_supervisor.rs::run_owned` | Docker and Kubernetes share the same durable phase clocks. |
| SDK `infra/data/sandbox/main.ts::install_imports` resolves packages before code execution. | `src/sandbox/ledger.rs::bind_runtime`, `mark_dispatched` | Time before binding or dispatch does not consume the following phase allowance. |
| PR 883 `JobLedger::age_seconds` uses `created_at` for both phases. | `readiness_age_seconds`, `execution_age_seconds` | PostgreSQL time preserves each deadline across reconnects and supervisor replacement. |
| PR 883 fences binding and dispatch by owner, epoch, phase, and live lease. | Existing fenced updates in `ledger.rs` | The first timestamp remains immutable during retries, renewal, and takeover. |
| Main owns versioned agentstate migrations. | `migrations/agentstate/0010_sandbox_phase_deadlines.sql` | Nullable timestamps extend the existing receipt table. |
| The supervisor validates migrated runtime columns during startup. | `src/sandbox/process.rs::connect_receipts` | Missing deadline columns fail startup before admission. |

The current SDK reference revision is `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`.
The mapping records an intentional durability improvement over the current platform.
It does not claim that the current platform supports supervisor replacement.

## Migration and rolling compatibility

Apply agentstate migration 0010 before starting the new supervisor.
Main applies the migration and backfill in one transaction.
Existing runtime bindings retain `created_at` as their readiness timestamp.
Existing dispatched or terminal rows retain `created_at` as their dispatch timestamp.
Unbound reservations keep both timestamps NULL.

Old supervisors can still insert and update rows because the new columns remain nullable.
Reads use `created_at` when an old writer leaves a phase timestamp NULL.
A new supervisor preserves this fallback when it takes over an existing old runtime binding.
The first new runtime binding uses database time.
Dispatch records its timestamp before the external runtime signal.
A repeated or stale dispatch cannot replace the timestamp.

Lease renewal and replacement do not extend either deadline.
Cancellation before dispatch leaves `dispatched_at` NULL.
Deadline failure confirms termination of the original runtime before recording terminal failure.
The change does not recreate a sandbox or authorize repeated code execution.

## Implementation history

PR 883 adds durable job receipts, runtime identity binding, cancellation, and recovery.
The initial deadline implementation uses one reservation clock.
This feature adds two phase timestamps and changes only their deadline consumers.
Existing PostgreSQL fixtures apply the new migration.
The abandoned-job fixture now expires the dispatch timestamp.
The migration manifest advances the agentstate head to 10.
CI explicitly runs the new PostgreSQL deadline tests.

The complete pending Point 5 work remains in a separate preserved Git snapshot.
This PR excludes on-demand dependency delivery, native package execution, and Code editor controls.
These capabilities retain their separate implementation and acceptance gates.

## Verification

The six focused phase deadline tests pass against the existing isolated PostgreSQL service.
The test run reports zero failures and zero ignored tests.
Five existing PostgreSQL receipt, fencing, and dispatch-journal tests also pass without skips.
The existing PostgreSQL/Docker recovery integration test also passes with zero ignored tests.
It uses the cached ARM64 receipt runner `sha256:c5248d3c1d855d274979059549c2789bb6cbe4e10cfc514134a649616b19ae7d`.
A two-hour-old reservation completes after a fresh dispatch.
Committed dispatch without a runtime signal recovers through the original container.
Cancellation and expired dispatch terminate the original runtime and retain durable receipts.
All nine migration package tests pass, including the embedded agentstate head check.
Rust formatting, strict Clippy across all targets and features, and workflow YAML parsing pass.
The binary-file policy check passes for all 10,572 tracked files.
Focused PostgreSQL tests cover phase separation, timestamp preservation, fencing, cancellation, and rolling migration.
Runtime doubles exercise supervisor behavior; they do not prove Docker, Kubernetes, or browser execution.

## Deployed Docker and Kubernetes acceptance

Main applies agentstate migrations 9 and 10 to the rehearsal receipt database.
The migration uses the existing Main migration role and verifies PostgreSQL TLS.
A private database backup precedes the migration. Existing receipt rows remain unchanged except for the migration backfill.
Both supervisor deployments use the same migrated database.

The Docker rollout replaces only the supervisor image.
Runtime profiles, mounts, trust material, network aliases, and resource limits remain unchanged.
The Kubernetes rollout replaces the supervisor and material-preparation images together.
The Pod specification otherwise remains unchanged, including its service account, trust material, limits, and security context.
The replacement Kubernetes supervisor becomes Ready with zero restarts.

The shared `CodeJobRuntime` contract selects Docker or Kubernetes.
Both adapters use `docker_supervisor.rs::run_owned`; the filename does not indicate Docker-only ownership.
No separate Kubernetes deadline implementation is required.

| Backend | Rehearsal image identity |
| --- | --- |
| Docker local image | `sha256:a8204031512e8e7c5afb395610518521f749e34a55900acbef6a8c2191b37d68` |
| Kubernetes imported manifest | `docker.io/library/elitea-sandbox-supervisor@sha256:e552d2a52516c409e0838e2844f017e936dacb8bcc781b1c765f053e2fb12606` |

The initial Kubernetes image import records the tag but not the digest reference.
Kubernetes therefore attempts a registry pull and cannot start the material-preparation container.
Registering the imported digest reference in the local containerd cache corrects the rollout.
Both deployment images remain pinned to that digest.

Fresh Playwright browser tabs submit pipeline 141, version 148, through the actual UI.
The input is `{"rows":20000,"seed":42}`. The pipeline executes fixed Python, JavaScript, TypeScript, and Rust Code nodes.
It uses prepared runtime packages and makes no model call. No browser routes or API responses are mocked.

| Backend | Chat surface | Result | New receipt evidence |
| --- | --- | --- | --- |
| Docker | Persistent chat 771 | PASS; reload preserves the result | Four completed receipts contain both phase timestamps. |
| Docker | Editor Test chat 779 | PASS | Four completed receipts contain both phase timestamps. |
| Kubernetes | Persistent chat 771 | PASS; reload preserves the result | Four completed receipts contain both phase timestamps. |
| Kubernetes | Editor Test chat 780 | PASS; one response for one submission | Four completed receipts contain both phase timestamps. |

Each result has 18,947 accepted records, 1,053 rejected records, and total cents `866440800`.
Each result retains all four language metrics and SHA-256 `6a3c4d063a5ba0c21949cf7d67d4bda843cb50a0bc76169c19e2c5f7e272326d`.
The Kubernetes editor execution is `b16e5fd963fc2e649a4c53dbdca1dfb2`.
All checked execution containers and Pods are removed after durable completion.
The UI releases the composer and shows no execution alert.

An earlier editor run uses the old Kubernetes supervisor after migration.
Its four completed receipts retain NULL phase timestamps and the result remains correct.
This proves old-writer compatibility; it does not prove the new timestamps.
An additional Docker editor run also completes with the new timestamps.

The Docker application logs a pre-existing `index_types` catalogue HTTP 404 in both chat surfaces.
The Kubernetes editor has zero console warnings or errors during this check.
Neither catalogue repair nor general UI rendering changes belong to this feature.

### Acceptance limits

The rehearsal image uses the exact feature source and a private build configuration with LTO disabled and optimization level zero.
It retains the locked dependency closure, audit metadata, diagnostic sections, non-root runtime, and existing TLS boundaries.
Two optimized local build attempts are cancelled after they cause Docker VM resource pressure.
These rehearsal runs prove behavior, not production performance or an optimized supervisor release artifact.
The tracked production build configuration remains unchanged.

At code head `abadda788`, PR 1014 reports 57 successful checks and two configured skips.
The skipped checks are the credential-dependent live toolkit/image lane and embedded-document screenshot capture.
Production worker release, PostgreSQL, image scans, Helm, and browser CI checks pass at that head.
The final documentation commit requires its own CI read-back.

The successful UI runs do not force delayed reservation or supervisor replacement.
The separate PostgreSQL/Docker integration proves delayed reservation and recovery through the original runtime.
Full Kubernetes application acceptance remains deferred; this rehearsal retains external PostgreSQL, provider, identity, and telemetry dependencies.
This feature does not close Point 5 or implement automatic dependency delivery.

Run the focused phase suite from `services/elitea-worker-rust`:

```bash
cargo test --locked --lib --all-features sandbox::docker_supervisor::deadline_tests -- --ignored
```

Set `ELITEA_TEST_DATABASE_URL` to an isolated PostgreSQL service with database creation permission.
The test fixture creates separate databases and removes them after each lifecycle.
