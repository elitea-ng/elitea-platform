# Python preparation receipt

Date: 2026-10-01.

The sandbox ledger stores validated preparation metadata before shared content publication.
The receipt survives lease replacement and remains readable after terminal completion.
The receipt does not prove publication or admit code execution.

## Source mapping

The current SDK revision is `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`.
The source revision matches the current SDK checkout.

| Current source and behavior | New source | Result |
| --- | --- | --- |
| SDK `infra/data/sandbox/main.ts::install_imports` prepares packages before source execution. | `src/sandbox/ledger.rs::JobLedger::record_preparation_bundle` | The supervisor records validated package metadata before shared publication. |
| SDK preparation and source execution share temporary process state. | `JobLedger::read_preparation_bundle` | A replacement supervisor recovers metadata from the existing agentstate receipt. |
| Existing `JobLedger::claim` replaces expired ownership with a new lease epoch. | `JobLedger::record_preparation_bundle` | Writes require the current owner, epoch, and unexpired lease. |
| Existing `sandbox_jobs` constraints forbid execution results in the dispatched phase. | Main `migrations/agentstate/0009_sandbox_preparation_bundle.sql` | A separate nullable metadata column preserves the existing execution result constraint. |
| Existing `PythonDependencyBundle::parse` validates metadata, file bounds, and the bundle root. | `JobLedger::read_preparation_bundle` | Reads validate the stored body against its recorded database digest. |
| Existing execution receipts use `JobRecord` and `JobLedger::read`. | Separate preparation receipt methods | The execution record and its existing SELECT remain unchanged. |

## Storage and authority contract

Main adds only `preparation_bundle_json TEXT` to `elitea_runtime.sandbox_jobs` in agentstate.
The nullable column has a 131,072-byte limit through `octet_length`.
The migration does not change the application schema or create a separate table.
The receipt contains canonical validated metadata, including the bundle digest.
The receipt contains no wheels, grants, credentials, package URLs, progress fields, or graph checkpoints.

The write API accepts `PythonDependencyBundle`, which only its validated parser constructs.
The write stores the exact canonical bytes from `record_json` as UTF-8 text.
The write locks the eligible row inside a short transaction.
The lease time check follows row locking because a lock wait can exceed the lease.
The row must match the exact tenant, project, job key, request digest, owner, and lease epoch.
The lease must remain live and the phase must be `dispatched`.
The cancellation flag must remain false.

An identical write succeeds without changing the recorded metadata.
A changed recorded body returns `Conflict` while the lease remains eligible.
An expired, replaced, cancelled, reserved, or terminal lease returns `Fenced`.
The first write rechecks authority before its UPDATE.
The write does not change the phase or the execution result.
The supervisor must publish shared content before it records successful terminal completion.

The read API requires the exact tenant, project, job key, and request digest.
A missing row returns `Missing` and a different request digest returns `Conflict`.
A null preparation column returns `None`.
The read uses the digest recorded in the trusted database body.
The bundle parser validates that digest against the complete stored metadata.
Malformed metadata or changed content with an unchanged digest returns `Invalid`.
Reads remain available after cancellation, lease replacement, and all terminal phases.

## Implementation history

The previous ledger stores only execution results and the original runtime identity.
Its existing CHECK prevents storing an execution result while preparation remains dispatched.
Migration 0009 adds the separate bounded metadata receipt.
The two preparation methods preserve the existing `JobRecord` storage contract.
Claims, cancellation, and terminal updates retain the preparation metadata.
All existing sandbox job test bootstraps also apply migration 0009.
The shared database test helper adds a process-local sequence to prevent concurrent database name collisions.
The hexadecimal database name remains below PostgreSQL's 63-byte identifier limit.

## Verification and remaining gates

Three database tests cover the preparation receipt contract.
They require an isolated database through `ELITEA_TEST_DATABASE_URL`.
They are ignored by default and require explicit database execution.
The live probe runs all three against an isolated PostgreSQL 18 container; all three pass.
Existing sandbox receipt and dispatch-journal tests also pass in the same isolated service.
The five selected database tests report no skipped tests.
The probe removes its test container and databases afterward.

The first parallel run exposes a collision in timestamp-only test database names.
The fixture now includes a process-local sequence and retains PostgreSQL's identifier length bound.
Assigned Rust formatting and Clippy checks pass.

The replacement test covers concurrent identical writes, exact canonical bytes, changed metadata, scope conflicts, lease expiry, epoch replacement, and completion.
The cancellation test covers the stop flag and failed, uncertain, and cancelled terminal rows.
The validation test covers corrupt metadata, digest mismatch, null metadata, and the storage byte limit.
The byte-limit test includes multibyte UTF-8 text.

Run the focused compilation check from `services/elitea-worker-rust`:

```bash
CARGO_INCREMENTAL=0 CARGO_PROFILE_DEV_DEBUG=line-tables-only CARGO_PROFILE_TEST_DEBUG=line-tables-only cargo test --lib --features sandbox-supervisor -j2 sandbox_preparation_receipts -- --nocapture
```

Run the database tests only against an isolated test service:

```bash
CARGO_INCREMENTAL=0 CARGO_PROFILE_DEV_DEBUG=line-tables-only CARGO_PROFILE_TEST_DEBUG=line-tables-only cargo test --lib --features sandbox-supervisor -j2 sandbox_preparation_receipts -- --ignored --nocapture
```

Connected preparation, shared publication, replacement recovery, Docker, Kubernetes, and browser acceptance remain separate gates.
This receipt change does not provide deployed proof for those gates.
