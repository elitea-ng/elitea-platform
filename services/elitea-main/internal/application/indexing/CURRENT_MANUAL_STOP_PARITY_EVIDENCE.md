# Current manual Stop cleanup parity evidence

> **Superseded in part (#1359).** The "Go target" PgVector half below is gone
> with the per-project PgVector store. The cleanup intent (migration `0048`)
> and its reconciler remain, but the effect is now
> `indexing.RegistryManualStopCleaner`: it verifies that the index registry row
> is cancelled for exactly the stopped run and deletes nothing, because the
> registry's vectors belong to the index namespace in elitea-vector and a
> reindex is incremental (ADR-0030 decision 3). This document is kept as the
> record of the PgVector-era contract.

This slice preserves the current application distinction between explicit
index Stop and system/dependency cancellation. It does not redefine the index
deletion API.

## Current baseline

| Behavior | Current evidence |
| --- | --- |
| Permission and route | `elitea_core/api/v2/index_cancel.py` requires `models.applications.task.delete` on `DELETE index_cancel/prompt_lib/...`. |
| Explicit Stop | `index_cancel.py` calls `cancel_toolkit_index_meta` with `delete_embeddings=True` and `require_in_progress=False`. |
| System cancellation | `elitea_core/methods/task_callbacks.py` calls the same helper with `delete_embeddings=False`; collection data survives. |
| Metadata | `elitea_core/utils/application_tools.py` sets `state=cancelled`, clears `task_id`, and replaces only the last history item. |
| Cleanup predicate | The same helper deletes only rows for the index where JSONB `type != 'index_meta'`. PostgreSQL rows with missing/null `type` do not match and survive. |
| Other state | The `index_meta` row, its history, the toolkit schema, collection row, and schedule configuration survive. |

## Go target

| Behavior | Implementation evidence |
| --- | --- |
| Manual-only origin | `runtime_index_cancel.sql` atomically records cleanup intent only from the authenticated explicit Stop transition. Direct system changes to `desired_state` create no intent. |
| Authority | `CurrentIndexMetaTerminalBindingsRepository` derives project, actor, toolkit, index, metadata ID, execution generation, logical index generation, and frozen toolkit configuration from admitted PostgreSQL state. No worker-provided schema, DSN, tenant, project, or index is accepted. |
| No external request transaction | The Stop transaction changes PostgreSQL state only. The reconciler waits for durable `CANCELLED` settlement and resolved cancelled metadata before PgVector I/O. |
| Durability | Migration `0048` adds a leased, retryable, idempotent cleanup intent with `PENDING`, `APPLIED`, and `SUPERSEDED` outcomes. No historical backfill is attempted because old rows do not preserve manual/system origin. |
| Generation fence | PostgreSQL supersession runs before dispatch. The PgVector writer independently verifies metadata ID, execution ID, execution generation, logical index generation, toolkit, cancelled state, cleared task ID, and the last history item. |
| Exact deletion | `CleanupManualStop` uses `cmetadata->>'collection' = $1 AND cmetadata->>'type' <> 'index_meta'`. It does not broaden the current contract to missing/null `type`. |
| Idempotency | A retry after committed deletion removes zero rows and succeeds. A newer logical generation returns `ErrCurrentIndexMetaSuperseded` and its rows survive. |
| Missing metadata | Matching current behavior, a missing `index_meta` row resolves as a safe no-op and does not authorize collection deletion. |
| Preserved data | The cleanup transaction does not update `index_meta`; metadata, counts, history, schema, collection row, schedules, other indexes, and missing/null-type rows remain unchanged. |

## Verification boundary

- Unit/component tests cover authoritative binding, invalid identity, exact
  cancellation SQL, readiness gates, leases, bounded concurrency, retry,
  poison-target isolation, supersession, and reconciler backoff.
- A real runtime PostgreSQL test covers migration `0048`, atomic explicit-Stop
  intent, pre-initialization Stop, system-cancellation exclusion, terminal
  readiness, competing claims, lease expiry/reclaim, and resolution.
- A real PgVector test covers exact deletion, preservation of missing/null
  `type`, unchanged `index_meta` bytes/history, idempotent retry, other-index
  isolation, and stale-generation rejection.

## Deliberately deferred improvement

The SDK collection cleaner uses a broader predicate that also deletes
missing/null `type` rows. Adopting that behavior would delete the current
production rows that the explicit Stop helper preserves, so it is an explicit
future contract change, not part of parity.
