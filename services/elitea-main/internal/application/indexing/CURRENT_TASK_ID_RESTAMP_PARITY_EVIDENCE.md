# Current index `task_id` restamp parity

> **Superseded (#1359).** The mechanism below is gone. Its consumer went with
> the per-project PgVector store, and the writer
> (`persistCurrentIndexMetaTaskRestampIntent` and
> `NodeEventFrame.CurrentIndexMetaTaskRestampSource`) was removed after it: a
> NodeEvent no longer records a restamp intent. Index metadata now lives in
> the index registry (ADR-0030), and the Rust worker sends no
> `agent_index_data_status` callback. The `index_meta_task_restamp_*` columns of
> `elitea_runtime.index_ingest_jobs` (migration 0049) are kept, and the index v2
> cutover preflight still counts `PENDING` rows. This document is kept as the
> record of what the Python path did.

## Current baseline evidence

The current behavior spans two repositories:

- `elitea-sdk/elitea_sdk/tools/base_indexer_toolkit.py:index_meta_init`
  resets `task_id` to `None` on reindex and emits `created_at`/`updated_on`
  through the `index_data_status` callback.
- `legacy/plugins/elitea_core/methods/stream.py:
  process_index_data_status_event` invokes the restamp only for
  `agent_index_data_status` with state `in_progress`.
- `legacy/plugins/elitea_core/utils/application_tools.py:
  ensure_index_data_has_task_id` loads the index metadata and sets `task_id`
  only when it is missing and the metadata `created_on` equals the callback
  `created_at`.

This is business behavior, not an OpenAPI/toolkit credential rule. It exists so
Stop and a fresh UI session can reconnect the visible in-progress index to the
current execution after the synchronous SDK reset.

## Go/Python parity mapping

| Current behavior | Replatform implementation | Fence |
| --- | --- | --- |
| Select only in-progress index status | `NodeEventFrame.CurrentIndexMetaTaskRestampSource` | Authenticated, exact-generation NodeEvent |
| Resolve target/toolkit/index/task | `persistCurrentIndexMetaTaskRestampIntent` plus `CurrentIndexMetaTaskRestampRepository` | Immutable admitted execution and frozen input bundle; browser identity fields ignored |
| Match current SDK run | `CurrentTaskRestampIndexMeta.CreatedOn` | Exact `created_on`, execution generation, logical index generation and `index_meta_id` |
| Set missing `task_id` | `CurrentIndexMetaWriter.MaterializeTaskID` | Sets only the admitted execution ID |
| Duplicate callback/restart | Immutable per-execution intent and same-task no-op | Idempotent |
| Later reindex | PostgreSQL supersession check plus PgVector generation check | Never overwrites a newer initialized generation |
| Stop/cancel race | PgVector cancelled-state check | Restamp resolves `SUPERSEDED`; cancelled `task_id` remains cleared |

The authenticated NodeEvent transaction performs no Configurations, secret or
PgVector I/O. It appends the bounded replay event and records one small restamp
intent atomically. A separately bounded reconciler claims at most twice its
worker count, redeems only the frozen project configuration, and retries failed
external effects with capped backoff.

`MaterializeTaskID` changes only the root `cmetadata.task_id`; it does not append,
replace, or otherwise rewrite the serialized `history`. The observed Main plus
SDK double-write that can leave an `in_progress` zero-count history entry next
to the completed entry is therefore a separate initialization/SDK ownership
gap. Restamp unit and real-PgVector tests assert that history cardinality and
bytes remain unchanged.

The restamp adds no public authorization surface. It inherits the already
authorized `models.applications.tool.patch` index admission and the exact active
workload claim; it cannot be requested by a viewer, arbitrary project member,
or browser payload. Tenant, resource project and projection project are
rechecked against the admitted execution in the same event transaction.

## Verification boundaries

- Unit/component tests cover event selection, forged browser identity, exact
  admission binding, idempotency, stale `created_on`, stale/newer generation,
  cancelled terminal race, bounded concurrency, retry and supersession.
- The PostgreSQL service-integration test crosses real admission, worker claim,
  authenticated event append, same-transaction intent, tenant rejection,
  durable claim/release/restart/reclaim and resolution.
- The PgVector service-integration test crosses real JSONB/vector tables,
  simulated SDK reset, exact restamp, process-restart retry, stale
  `created_on`, and newer-generation rejection.

## Composition and remaining UI gate

Production composition mounts the processor and reconciler if and only if
index-ingest dispatch is enabled. The processor shares the single
`CurrentIndexMetaWriter` instance already owned by terminal projection and
manual Stop cleanup, and claims durable intents only through
`Dependencies.TerminalEffectsPool`. Its concurrency and claim batch derive from
that dedicated pool, so slow Configurations or PgVector reconciliation cannot
consume output-ingestion transaction capacity. Enabled composition fails closed
when the terminal-effects pool, Configurations runtime, writer, or failure
reporters are absent.

The remaining live-browser gate is one deployment checkpoint against a real
PostgreSQL, Configurations, project PgVector and Python worker path: after the
SDK reset and authenticated `in_progress` event, close the original browser,
open a fresh session with no `sessionStorage` task fallback, prove index metadata
alone reconstructs the authoritative execution, and prove Stop targets that
execution. The backend composition and cross-store service tests do not claim
that final deployed UI evidence.
