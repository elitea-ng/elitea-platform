---
title: "The audit trail is hash-chained (#1499)"
issue: 1499
date: 2026-10-29
order: 47
---

* **Shared migration 0206** leaves `centry.audit_events` itself unchanged
  apart from two triggers: no column, no index, no table scan, so it applies
  in moments on any size of table. The chain lives in a new side table,
  `elitea_runtime.audit_events_chain` (one link per audit row: `audit_id`,
  position, `prev_hash`, `hash`), with the head table
  `elitea_runtime.audit_events_chain_heads`. Every insert is chained in its
  own transaction, an `UPDATE` that changes a chained row's content is
  refused, and links are append-only. Rows written before the upgrade have
  no link and are never checked. Nothing in the platform updates audit rows;
  a site script that does will now fail with
  `a chained audit event is append-only`. A script that deletes audit rows
  directly leaves their links behind, and verification reports them missing.
* **All audit writers serialise on one chain head.** At admin-request rates
  this is not measurable; a bulk import into `centry.audit_events` (for
  example replaying a legacy trail) runs one row at a time through the head.
* **Retention deletes the chain as a prefix.** The scheduler's
  `admin_audit` sweep deletes chained rows oldest-position first and stops
  at the first row still inside the window, so an expired row written late
  (a session end) can keep rows behind it for up to one relay interval. It
  deletes the links with their rows and records what it deleted in the
  head's prune anchor so verification still passes.
* **New admin route** `GET /api/v2/elitea_core/audit_chain_verify/administration`
  (operation `verifyAuditChain`, permission `models.admin.audit_trail.view`)
  verifies the chain and names the first break.
* **The SIEM export** (`elitea.audit.v1`) now carries the optional `chain`
  object (`partition` `0`, `sequence`, `prev_hash`, `hash`) on `admin_audit`
  events from chained rows (read from the side table). Receivers with a strict schema copy should take
  the updated `libs/jsonschema/audit/v1/event.schema.json`.
