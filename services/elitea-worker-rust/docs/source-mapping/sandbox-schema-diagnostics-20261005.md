# Sandbox schema diagnostics

This change has one owner: the Supervisor persistence error boundary. It does not change database queries, migrations, grants, leases, or retry deadlines.

## Current source to new source

| Source | Current behavior | New behavior |
| --- | --- | --- |
| `src/sandbox/ledger.rs` | Each SQLx error becomes `LedgerError::Database`. Its display is `sandbox job persistence failed`. | PostgreSQL SQLSTATE `42703` and `42P01` become a typed `SchemaMigrationRequired`. The type has a fixed column or table diagnostic. All other errors keep their current class. |
| `src/sandbox/service.rs` | Every ledger database failure returns RPC `Unavailable`. | A schema failure returns `FailedPrecondition`. Its fixed message directs the operator to verify the configured database, apply Main AgentState release migrations, and retry the same activation. |
| `src/sandbox/service_compiled.rs` | Compiled publication events classify database errors as `ledger_unavailable`. | Schema failures use `schema_migration_required`, `ERROR`, and an allowlisted `sqlstate` field. Other events use `sqlstate="none"`. |
| `src/sandbox/docker_compiled_observability.rs` | Compiled stage events classify database errors as `ledger_unavailable`. | Schema failures use the same safe class and SQLSTATE. The original typed error returns to the caller. |

The classifier reads SQLx `DatabaseError::code()`. It does not inspect a message, table name, column name, statement, payload, or DSN. The schema variant discards the raw SQLx cause. Its display, debug value, RPC message, and added log fields contain only fixed values.

The existing `docker_supervisor.rs::recover_cancellations` logs an error through `Display`. It therefore gains the fixed schema reason and SQLSTATE. Its recovery loop continues to retry. This lets it recover after the operator applies migrations. This change does not add a startup gate or a log limiter.

The existing Worker `code_preparation.rs::retryable` does not retry `FailedPrecondition`. The existing client `submission_code` preserves a server status. It maps only a local Hyper transport error to `Unavailable`. Neither source needs a change for this classification.

The Worker preparation client currently keeps the RPC status code and discards its message. This patch makes the Supervisor RPC and cleanup diagnostic actionable. It does not claim that the Worker UI displays the migration message.

## Verification and history

The private baseline comes from the current owning source at commit `ad4da99ff2dcf63b9c92c76e1fadb837c550153a`, with existing work in progress preserved. `BASE_PROVENANCE.json` records each exact baseline hash. The patch contains four production source changes, three test source changes, and this mapping.

Six focused tests are supplied. They cover the exact SQLSTATE allowlist, private cause exclusion, preservation of other database classes, RPC mapping, and both bounded log boundaries. Rust formatting and private patch application are checked. The tests are not run. The task excludes builds, live calls, credentials, and shared source edits.

Root must compose the patch, run the focused tests, build the Supervisor, and verify the missing-schema response and migrated recovery. A source check does not prove those runtime results.
