# Rust branch migration reconciliation

## Scope

This operator procedure handles one pre-production branch collision.
The old shared ledger contains `111=mcp_prebuilt_parameter_schema` and `112=toolkit_execute_read`.
Current main owns different migrations at these versions.
The current branch moves the Rust migrations to 123 and 122, respectively.

The ordinary migrator correctly rejects the old ledger.
Do not edit its checksums or disable its validation.
Do not use this procedure as a general migration framework.

## Preconditions

- Use an isolated database copy with the `elitea_cutover_` prefix.
- Preserve the original database and a recoverable backup.
- Stop other clients of the copy.
- Use the repository manifest through shared version 123.
- Keep original branch receipt identities unchanged.
- Keep affected runtime tables below the procedure's 128 MiB rehearsal bound.

The script rejects an unknown ledger, relation type, row-security policy, or user trigger.
An unknown foreign-key dependency stops the transaction. The procedure never uses `TRUNCATE CASCADE`.
Normal database names are not allowed.

## Procedure

Run a rollback rehearsal first:

```bash
python3 scripts/database/rehearse_rust_main_sync.py \
  --container <postgres-container> \
  --database <elitea_cutover_copy>
```

The script takes the normal shared migration advisory lock.
It copies 14 affected runtime tables into transaction-local temporary tables.
It archives both original receipts under `elitea_migration_audit.rust_branch_receipts_20260908`.
The archive retains original checksums, timestamps, source revision, and replacement versions.

The script temporarily removes affected runtime rows inside the transaction.
It recreates the two old read-execution tables through the canonical migration.
It executes migrations 111 through 123 without changing their SQL bytes.
Each new receipt records the checksum of the artifact that actually runs.

The script restores every staged row in foreign-key order.
It compares both directions with `EXCEPT ALL` and validates the canonical ledger.
It rolls back unless the operator supplies `--apply-copy`.

PostgreSQL sequences can advance during rollback. This is normal transaction behavior.
The rollback tests compare complete logical table rows, not sequence counters or physical storage layout.
Use a disposable copy for these tests.

Commit the verified copy with the explicit `--apply-copy` option.
Then run the unchanged current `elitea-migrate -all-tenants` against that copy.
Run it again to check repeat application.

The script prints only fixed progress markers, table counts, and SQLSTATE codes.
It does not print SQL errors, credentials, execution inputs, or results.

## Tests

Run unit tests without database access:

```bash
python3 -m unittest discover -s scripts/database -p 'test_rehearse_rust_main_sync.py' -v
```

Run the component proof against a disposable copy with the original ledger:

```bash
ELITEA_RUST_CUTOVER_TEST_DATABASE=<elitea_cutover_copy> \
ELITEA_RUST_CUTOVER_TEST_CONTAINER=<postgres-container> \
python3 -m unittest discover -s scripts/database -p 'test_rehearse_rust_main_sync.py' -v
```

The component test commits the copy upgrade. It does not delete or create a database.
It covers rollback, injected precommit failure, unknown foreign-key dependency, successful restoration, and repeat refusal.
The unit tests cover target rejection, original SQL bytes, commit opt-in, and redacted errors.

## Evidence: 2026-09-08

All five tests pass with the explicit database fixture. No test skips.
All 14 runtime tables retain their original rows after restoration.
These rows include 1,050 executions, 16,282 claims, seven read jobs, and one read result.
Both old migration receipts remain in the audit archive.
The unchanged current migrator accepts the reconciled copy twice with `-all-tenants`.

This proof changes only the disposable copy.
It does not make a live snapshot consistent across product, agent-state, Redis, and output storage.
It does not authorize old wire bytes under the merged protocol.
Repeat pending-work inspection and take a quiescent backup before deployment cutover.

See [the integration record](../../services/elitea-worker-rust/docs/source-mapping/main-sync-20260908.md) for protocol and deployment gates.
