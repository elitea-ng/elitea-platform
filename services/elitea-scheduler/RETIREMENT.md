# `elitea-scheduler` ownership disposition

This process is not the owner of product schedule occurrences, and it no longer
dispatches anything.

## The legacy schedule dispatcher was deleted

Until this change the daemon ran a once-a-minute tick that polled
`centry.schedule`, took a `scheduler_tick` lock in Redis (Valkey), and PUBLISHed
legacy Pylon/Arbiter pickle RPC payloads to the `elitea_rpc` channel. Nothing in
the Go stack subscribed to that channel (issue #305): in a Go-only deployment
every publish reached zero subscribers, and the guard added for #305 only
stopped the tick from recording those runs as done.

The tick, its Redis lock, the RPC client, `REDIS_URL`, `RPC_CHANNEL`,
`RPC_HMAC_KEY` and `SCHEDULER_INSTANCE_ID` were removed together. The daemon no
longer connects to Redis at all.

What owns scheduled work now:

- `elitea-main` registers `index.schedule.scan.v1` and the pipeline schedule
  tick on its own platform scheduler; PostgreSQL
  `elitea_runtime.scheduled_job_cursors` and
  `elitea_runtime.scheduled_occurrences` own planning, takeover, and completion.
- `centry.schedule` rows are still listed and edited by the admin Schedules
  surface. In a hybrid deployment, a legacy Pylon scheduling plugin running
  against the same database is the only thing that executes them; nothing in
  this repository does.

## What this binary still runs

The price-catalog sync, budget write-back, audit-retention, sync-tombstone
retention and native-auth retention workers are independent lifecycle
responsibilities that share this binary. They require an explicit relocation or
retained-service decision before this image can be deleted. This note does not
claim that work is complete.

The retention sweeps honour the platform maintenance switch through
`internal/maintenance`, which reads the same `centry.platform_config` row the
admin Configuration page writes.

## The audit-retention sweep is not a schedule

`internal/auditretention` (issue #619) deletes rows of `centry.audit_events`
that are older than a configured window. It is NOT a `centry.schedule` row and
it is not registered with the `elitea-main` scheduling kernel: there is no
occurrence ledger to disagree about, no lease epoch and no cursor. The cutoff is
recomputed from the clock on every pass, the DELETE is idempotent, and a pass
that never runs costs only a later pass.

It takes its own loop, started from `cmd/elitea-scheduler`, in the shape the
price-sync and budget write-back workers already use. When this image is
retired, the sweep moves with those two and needs the same explicit decision.
