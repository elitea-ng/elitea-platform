# Sandbox deadline fixture schema

## Source mapping

Main owns `migrations/agentstate/0013_sandbox_whole_code_recovery.sql`.
The migration adds nullable whole-Code and broker binding columns to the original sandbox job row.
`src/sandbox/ledger_code_platform.rs` reads these columns when it selects an inert runtime launch.
`src/sandbox/docker_supervisor.rs` performs this read before it observes runtime readiness.
Pure jobs also perform this read and return no broker launch when their binding is null.

## Failure and correction

The existing deadline fixture installs migrations through phase-clock migration 10 but omits migration 13.
The expired-readiness test receives `SchemaMigrationRequired(MissingColumn)` before it checks readiness.
Two concurrent test branches then wait for readiness observations that cannot start.
Their existing ten-second bounds report observation timeouts.

Each test creates its own database.
Earlier CI tests cannot supply migrations to these isolated databases.

The correction adds the exact Main-owned migration to these fixture owners:

| Fixture | Schema ordering |
| --- | --- |
| `src/sandbox/docker_deadline_tests.rs` | Apply phase clocks, then whole-Code schema. |
| `src/sandbox/docker_hydration_deadline_tests.rs` | Add migration 13 after migration 10. |
| `src/sandbox/docker_preparation_tests.rs` | Add migration 13 after migration 10. |
| `src/state/postgres_checkpointer_tests.rs` | Add migration 13 to the two Docker/TLS supervisor setups after prerequisite migrations. |

The legacy deadline test still inserts old rows before migration 10.
It applies migration 13 only after the phase-clock migration.
Nullable additions preserve the original legacy backfill and rolling-writer checks.

## Verification boundary

The required PostgreSQL groups cover six phase-clock tests, seventeen preparation tests, and twelve hydration tests.
Existing readiness, dispatch, cancellation, cleanup, and immutable-runtime assertions remain unchanged.
The tests enable their existing infrastructure cases with `--ignored`.
No new ignore attribute, wait extension, stack override, or production behavior change is added.

The state recovery fixture requires PostgreSQL, Docker, and `ELITEA_CODE_RUNNER_TEST_IMAGE`.
The shared submission fixture also supports the existing mTLS test with `ELITEA_TEST_TLS_DIR`.
These two setups serve three existing real-Docker cases; this focused check does not run them.

## Current focused evidence

The offline current-lock build uses one Cargo job and the existing native dependency cache:

```sh
CARGO_INCREMENTAL=0 cargo test --locked --offline --all-features -j1 --lib \
  sandbox::docker_supervisor::deadline_tests --no-run
```

The direct build exit is zero, and source and lockfile hashes remain unchanged.
The macOS debug linker emits its existing large `__eh_frame` compact-unwind warning.
This result is not warning-free.

Root runs the fresh macOS binary against an owned PostgreSQL 18 container and newly generated test TLS material:

```sh
"$WORKER_TEST_BINARY" sandbox::docker_supervisor::deadline_tests:: --ignored --nocapture
"$WORKER_TEST_BINARY" sandbox::docker_supervisor::preparation::tests:: --ignored --nocapture
"$WORKER_TEST_BINARY" sandbox::docker_supervisor::hydration::deadline_tests:: --ignored --nocapture
```

All three processes exit zero: six, seventeen, and twelve tests pass, respectively.
The total is thirty-five passed, with zero failed or ignored.
Each group uses default test concurrency.
The hydration heartbeat test retains its real twenty-second boundary.

No production runtime policy, SQL refusal, wait bound, or stack setting changes.
The focused proof does not establish full Linux CI or deployed recovery acceptance.
Replacement Linux CI remains required for the complete test sequence.
