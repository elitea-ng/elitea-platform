# Durable inert hydration retention

Date: 2026-10-02. Status: implemented; PostgreSQL and deployed acceptance remain separate checks.

## Current-to-new source mapping

The Python delivery feature binds one original execution runtime before indexed hydration and final submission.
Previously, retries can renew its lease indefinitely while it remains Reserved.
The worker also starts a new observation budget after replacement.
The inert runner has no independent expiry while it awaits dispatch.

| Existing contract | Change owner | Result |
| --- | --- | --- |
| Immutable `runtime_bound_at`, with `created_at` for legacy NULL bindings | `src/sandbox/ledger.rs::hydration_age_seconds` | Reuse binding age for an inert allocation retention ceiling. Rebinding, retry, and takeover do not reset it. |
| Readiness 60 seconds; execution 3660 seconds from `dispatched_at` | `src/sandbox/docker_supervisor.rs` | Preserve both clocks. Add separately named `MAX_HYDRATION_AGE_SECONDS = 3690`, from preparation maximum 3600 plus recovery allowance 90. |
| Indexed inert transfer and execution metadata proof | `src/sandbox/docker_hydration.rs`, supervisor expiry guard | Check retention before transfer, on heartbeats, after final verification, and before first durable dispatch. |
| Stable backend/profile owner and expiring lease | Ledger bounded discovery; supervisor recovery loop | Discover at most 32 Reserved, bound, lease-expired execution rows for that owner. Claim and recheck before terminating the original runtime. |
| Execution admission and independent Stop capacity | Supervisor retention sweep | Use bounded Stop capacity for cleanup. Occupied execution slots cannot block idle expiry recovery. Preparation profiles skip this sweep. |
| Durable Stop and terminal-write fencing | Existing `fail_expired_job` and ledger `finish` | Cancellation supersedes expiry. Persist Failed only after confirmed original termination, with safe code `sandbox.hydration_deadline_exceeded`. |

Worker paths are relative to `services/elitea-worker-rust`.
No protobuf, migration, request fingerprint, package root, or runtime identity change is required.
The existing client accepts this bounded failure code.
Hydrate returns Ready after durable expiry; subsequent Submit reports Failed through the existing Code error path.
Dependent nodes do not run.

## Implementation history and acceptance boundary

1. Reuse the existing binding clock and legacy fallback.
2. Add guards around indexed delivery, readiness completion, active observation, and first dispatch.
3. Add bounded idle discovery to the listener-owned cancellation recovery loop.
4. Preserve original-instance fencing and claim-authoritative phase checks. A discovered candidate that becomes Dispatched is skipped.
5. Add PostgreSQL/runtime-double regressions in `src/sandbox/docker_hydration_deadline_tests.rs`.

The ceiling applies to bound Reserved execution allocations, including profiles without a dependency bundle.
It does not impose a new overall job timer or shorten dispatched execution.
Actual background termination includes polling, lease, and cleanup delay. No exact physical kill time is claimed.
Unknown termination leaves Reserved and permits a fenced retry; no successful cleanup receipt is invented.

Tests cover fresh binding after old reservation, immutable age across takeover, legacy NULL fallback, and expiry before indexed transfer.
They also cover blocked transfer heartbeats, final metadata verification, pre-dispatch proof, owner partitioning, capacity, and phase races.
Stop during termination, preparation-profile exclusion, lease fencing, and unknown-termination retry have separate cases.
The heartbeat test uses the real 20-second interval; database clocks select expiry without a long age wait.
All database tests remain explicitly ignored unless their infrastructure is supplied.

On 2026-10-02, the supplied PostgreSQL fixture runs all 12 regressions successfully.
No regression fails or remains ignored in that run.
The Python baseline also passes strict all-target, all-feature Clippy.
The integrated native fixtures select the typed Python bundle without changing these assertions.
The updated supervisor remains undeployed at this acceptance boundary.

Required checks:

```sh
cargo test --locked --manifest-path services/elitea-worker-rust/Cargo.toml \
  --lib --all-features -j 2 sandbox::docker_supervisor::hydration::deadline_tests \
  -- --ignored --test-threads=1
```

The command requires isolated PostgreSQL through `ELITEA_TEST_DATABASE_URL`.
Four cases also require `ELITEA_TEST_BUNDLE_TLS` with disposable `ca.pem` and `client-combined.pem`.
Component results do not prove Docker/Kubernetes idle expiry, deployed takeover, or browser failure behavior.
Those acceptance checks remain root-owned.
# Combined phase-clock regression, 2026-10-02

The combined PostgreSQL run finds one stale phase-clock assertion.
Its legacy fixture has a two-hour runtime binding without new writer clocks.
Allocation retention now expires before readiness reconciliation for that fixture.
The test expects `sandbox.hydration_deadline_exceeded` and still requires zero dispatches and original runtime termination.
Expiry uses the legacy creation clock and preserves both unset writer clocks.
No production deadline or migration changes for this correction.
