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
| SDK `elitea_sdk/runtime/tools/sandbox.py` interprets remote execution results and failures. | `src/sandbox/docker_supervisor.rs::run_owned` | Readiness and execution use separate durable clocks. |
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
Fresh Chrome history inspection passes for the existing four-language benchmark in persistent chat 771.
Reload preserves one complete result with its recorded digest.
This history check uses the previous rehearsal deployment.
It does not prove the new phase deadline behavior.
Deployed browser acceptance remains required before release.
This feature does not close Point 5.

Run the focused phase suite from `services/elitea-worker-rust`:

```bash
cargo test --locked --lib --all-features sandbox::docker_supervisor::deadline_tests -- --ignored
```

Set `ELITEA_TEST_DATABASE_URL` to an isolated PostgreSQL service with database creation permission.
The test fixture creates separate databases and removes them after each lifecycle.
