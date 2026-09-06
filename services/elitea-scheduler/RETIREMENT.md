# `elitea-scheduler` ownership disposition

This process is not the owner of product schedule occurrences.

The current `internal/scheduler` implementation polls `centry.schedule`, takes a
Redis tick lock, and publishes legacy Pylon/Arbiter RPC payloads. It has no
PostgreSQL occurrence ledger, lease epoch, or fence. Running it for the same job
as the `elitea-main` scheduling kernel would create two clocks and is forbidden.

For the focused indexing transition:

- the current `index_scheduling` row is disabled in the hybrid deployment;
- `elitea-main` registers `index.schedule.scan.v1` on its one-minute cadence;
- PostgreSQL `elitea_runtime.scheduled_job_cursors` and
  `elitea_runtime.scheduled_occurrences` own planning, takeover, and completion;
- pipeline and other current `centry.schedule` rows remain owned by the current
  Pylon scheduling plugin until each receives a typed Go disposition; and
- the legacy Redis/Pylon scheduler must not be enabled as a fallback for a job
  registered in `elitea-main`.

## Dispatch is only recorded when it is received

The tick publishes to the `elitea_rpc` Redis channel, whose only consumer is
legacy Pylon — no Go service in this repository subscribes to it. Redis
`PUBLISH` to zero subscribers returns 0 and no error, so until issue #305 the
scheduler stamped `centry.schedule.last_run` after every publish and a Go-only
deployment recorded every due schedule as run while executing none of them. The
healthier the schedule history looked, the more completely the product was
broken.

The scheduler now reads the subscriber count `PUBLISH` returns and **refuses to
stamp `last_run` when it is zero**, logging at ERROR with the channel name. The
row is left untouched rather than advanced, so the job is still due on the next
tick and a consumer that was restarting picks it up instead of losing its
window. The hybrid deployment, where Pylon subscribes, is unaffected — the count
is at least 1 and the stamp happens as before. This note is not the guard; the
guard is `internal/scheduler/dispatch_test.go`, which asserts the stored
`last_run`, not that a publish happened.

The price-catalog sync, budget write-back and audit-retention workers currently
sharing this binary are independent lifecycle responsibilities. They require an
explicit relocation or retained-service decision before this image can be
deleted. This note does not claim that work is complete.

## The audit-retention sweep is not a schedule

`internal/auditretention` (issue #619) deletes rows of `centry.audit_events`
that are older than a configured window. It is NOT a `centry.schedule` row and
it is not registered with the `elitea-main` scheduling kernel, so the two-clocks
prohibition above does not apply to it: there is no occurrence ledger to
disagree about, no lease epoch and no cursor. The cutoff is recomputed from the
clock on every pass, the DELETE is idempotent, and a pass that never runs costs
only a later pass.

It takes its own loop, started from `cmd/elitea-scheduler`, in the shape the
price-sync and budget write-back workers already use. When this image is
retired, the sweep moves with those two and needs the same explicit decision.
