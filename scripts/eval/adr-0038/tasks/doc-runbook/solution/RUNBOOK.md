# ledger-sync runbook

## Overview

ledger-sync consumes posted journal entries from the NATS JetStream stream
`LEDGER` (durable consumer `ledger-sync-durable`) and writes them to the
`reporting` Postgres database on `pg-reporting.ledger.svc`. Writes are
idempotent per entry id, so redelivered messages are safe. It runs as Helm
release `ledger-sync` in namespace `ledger` with 3 replicas, owned by the
payments-platform team. Dashboard: "Ledger / ledger-sync overview".

## Health checks

- Liveness: `https://<pod>:8443/healthz` (TLS)
- Readiness: `https://<pod>:8443/readyz` (TLS); expect HTTP 200 and `{"status":"ready"}`
- Metrics: `http://<pod>:9102/metrics` (Prometheus)
- Internal API: port 8080 (cluster-only)

Quick check from a workstation:

```sh
kubectl port-forward -n ledger deploy/ledger-sync 8443:8443
curl -sk https://localhost:8443/readyz
```

A healthy `/healthz` does not prove progress: also watch consumer lag.

## Alerts

### LedgerSyncConsumerLag (page)

Pending messages on `ledger-sync-durable` above 50,000 for 10 minutes.

1. Check the consumer: `nats consumer info LEDGER ledger-sync-durable` and look
   at "Outstanding Acks" and "Redelivered Messages".
2. If messages are stuck in ack-pending (seen after a NATS node restart,
   INC-3312), reset the consumer, which keeps its position:
   `ledgerctl consumer reset --stream LEDGER --consumer ledger-sync-durable`
3. Expect lag to drain in about 20 minutes. Do not delete and recreate the
   consumer: it loses the position and replays the whole stream.

### LedgerSyncDBWriteErrors (page)

DB write error rate above 1/s for 5 minutes.

1. Check the pod logs for Postgres errors such as "No space left on device"
   (INC-3390: WAL volume full).
2. ledger-sync retries with backoff and NATS keeps unacked messages, so no
   action on the service is needed. Page the DBA team in `#dba-oncall`.
3. Do not scale up to catch up faster (see below).

### LedgerSyncPodRestarts (ticket)

More than 3 container restarts in 30 minutes.

1. Check for OOMKilled: `kubectl describe pod -n ledger -l app=ledger-sync`.
2. If replicas were raised above 3, scale back to 3.
3. Raise a ticket with payments-platform; this is not a page. If pods must be
   restarted, use `kubectl rollout restart deployment/ledger-sync -n ledger`.

### Replica limit

Run at most 3 replicas. Each pod opens 20 database connections and the
`ledger_sync` role is capped at 60 connections, so a fourth pod cannot connect
and crash-loops.

## Rollback

1. Find the last good revision: `helm history ledger-sync -n ledger`
2. Roll back: `helm rollback ledger-sync <REVISION> -n ledger`
3. Verify readiness with the port-forward and `curl -sk https://localhost:8443/readyz`
   shown above (expect HTTP 200).
4. Watch consumer lag on the dashboard for 10 minutes.

Rolling back one revision is always safe (migrations are forward-compatible
for one release). Rolling back more than one revision needs a DBA.

## Escalation

1. Slack: `#ledger-oncall`
2. PagerDuty service: `ledger-primary`
3. Secondary: Ines Moreau, payments-platform lead
4. Database problems: DBA team in `#dba-oncall`
