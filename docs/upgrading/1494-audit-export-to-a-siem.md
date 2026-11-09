---
title: "Audit export to a SIEM (#1494, off by default)"
issue: 1494
date: 2026-10-28
order: 42
---

* **elitea-main: OTLP logs and signed webhook export of audit evidence
  (ADR-0042 decision 3).** Set `main.env.ELITEA_AUDIT_EXPORT_OTLP_ENDPOINT`
  and/or `ELITEA_AUDIT_EXPORT_WEBHOOK_URL` (with
  `ELITEA_AUDIT_EXPORT_WEBHOOK_KEYS_FILE`) to export security audit events
  and `centry.audit_events` as the `elitea.audit.v1` envelope
  (`libs/jsonschema/audit/v1`). Both empty, as in every earlier release:
  nothing changes, no relay starts, nothing dials out.
* **Shared migration 0196** adds `elitea_runtime.audit_export_cursors` (one
  position per stream and sink) and `audit_export_dead_letters`, and gives
  `centry.audit_events` a column `export_txid` (the inserting transaction,
  so the relay reads rows in commit order and never skips one that commits
  late) with a partial index. Existing rows read 0: adding the column does
  not rewrite the table, but building the index reads it once while holding
  a lock that pauses audit writes, so on a very large table expect a short
  pause during the migration. No data, no permission. The migration job
  applies it.
* **Delivery semantics.** Each sink keeps its own position: a healthy sink
  is not sent events again while another is down. A webhook batch refused
  with 400, 413 or 422 is split in halves down to single events; only a
  single event refused on five passes is dead-lettered, alone. An
  undecodable security outbox record is dead-lettered alone (sink `decode`)
  instead of blocking the stream. The relay holds no database transaction
  while it sends; one replica relays at a time through a session advisory
  lock. A long-running write transaction delays export (lag alert) rather
  than letting a row be skipped.
* **elitea-scheduler: audit retention waits for the export.** While audit
  export is on, the `centry.audit_events` sweep deletes nothing after the
  slowest sink's position, so a long SIEM outage can grow the table past
  its retention window until the receiver is back. If you switch the
  export off, run `DELETE FROM elitea_runtime.audit_export_cursors WHERE
  stream = 'admin_audit'` to release the held rows.
* **With export on, the security audit relay changes meaning.** Without an
  exporter it marked an outbox row exported when the row reached the durable
  store. With one, a row stays pending until the receiver accepted it, so an
  unreachable receiver grows the outbox backlog (the existing
  `security_audit.backlog` figure and the new lag alert) instead of being
  ignored. The outbox retention job deletes only exported rows, so an outage
  also delays that cleanup. Nothing is dropped.
* **Existing `audit_events` history is not exported by default.** The relay
  starts at the newest row when it first runs. Set
  `ELITEA_AUDIT_EXPORT_AUDIT_EVENTS_FROM=beginning` before the first start to
  replay the table.
* **New listener and chart objects.** With an exporter set, elitea-main serves
  Prometheus `/metrics` on `:9464` (`ELITEA_AUDIT_EXPORT_METRICS_ADDR`; empty
  disables it). The chart declares the port, renders a PodMonitor and a
  PrometheusRule (`EliteaAuditExportLag`, `EliteaAuditExportBacklog`,
  `EliteaAuditExportDeadLettered`) where `monitoring.coreos.com/v1` is served,
  and mounts the optional Secret `main.auditExport.secretName` for the
  webhook keys and OTLP headers. Where `networkPolicies` are on, name your
  Prometheus in `networkPolicies.main.metricsFrom` to let it reach the port.
* A webhook on a private or loopback address needs
  `ELITEA_AUDIT_EXPORT_EGRESS_ALLOWLIST` to name it; plain `http` is accepted
  only then. The OTLP endpoint is operator-set and is not subject to it.
