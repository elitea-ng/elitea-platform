# ledger-sync incident notes (raw, from the team wiki)

## INC-3312 — 2026-08-19 — consumer lag after NATS node restart

- LedgerSyncConsumerLag fired ~12 min after nats-2 was restarted for a kernel
  patch. Pending messages climbed to 180k.
- Pods were healthy (/healthz OK) but not making progress: the durable
  consumer had thousands of messages stuck in ack-pending.
- First check: `nats consumer info LEDGER ledger-sync-durable` — look at
  "Outstanding Acks" and "Redelivered Messages".
- Fix that worked: `ledgerctl consumer reset --stream LEDGER --consumer ledger-sync-durable`
  (keeps the consumer's position, clears the stuck ack-pending set).
- Someone suggested deleting and recreating the consumer. DON'T: it loses the
  position, every entry since the stream start gets redelivered. That is safe
  (idempotent writes) but took 2 hours to catch up last time.
- Lag drained in ~20 min after the reset.

## INC-3390 — 2026-09-30 — DB write errors

- LedgerSyncDBWriteErrors fired. Logs: "could not extend file: No space left
  on device" from Postgres.
- Cause: WAL volume on pg-reporting full (replication slot of a dead replica).
- Nothing to do on ledger-sync itself: it retries with backoff and NATS keeps
  the messages until acked. Page the DBA team in #dba-oncall.
- Somebody scaled ledger-sync to 6 replicas "to catch up faster" — this made
  it worse: each pod opens 20 DB connections and the ledger_sync role is
  capped at 60, so pods 4-6 crash-looped (which also fired
  LedgerSyncPodRestarts). Keep replicas at 3 maximum.

## Misc

- Old runbook said "restart by deleting pods". Use a rolling restart instead:
  `kubectl rollout restart deployment/ledger-sync -n ledger`
- Pod restarts alert on its own is usually OOM: check
  `kubectl describe pod -n ledger -l app=ledger-sync` for OOMKilled and raise
  a ticket with the team; it is not a page.
- Grafana dashboard: "Ledger / ledger-sync overview".
