---
title: "Run step records: evidence, chain, export and retention (#1499)"
issue: 1499
date: 2026-10-29
order: 48
---

* **Shared migration 0205** creates `elitea_runtime.run_step_heads` (one row
  per run: step count, last hash, anchor position) and
  `elitea_runtime.run_steps` (one row per model call, tool call, retrieval,
  approval, delegation, cap, error and output, hash-chained per run; a
  trigger refuses every UPDATE except content expiry). It seeds the platform
  retention rows `step_content` and `step_record` (keep forever) and grants
  the new permission `runs.evidence.read` (platform `super_admin` and
  `admin`, project `admin`). Nothing is backfilled: runs before the upgrade
  have no step records.
* **Step content goes to object storage**, in the platform-scoped bucket
  `run-evidence` under `<project>/runs/<run>/<step>/<inputs|outputs>`; no
  bucket needs creating. Without an object store, the records keep only the
  digests and sizes and their content reads as `not_stored`. A toolkit's new
  `evidence_fields` setting (a list of top-level field names) keeps only
  those fields of its JSON results; a malformed value is refused on save
  with **400 `invalid_evidence_fields`**.
* **Anchoring runs off the settlement path.** Settlement only marks the
  run; the new routine `main.run_step_anchor` (every minute, listed under
  Administration → Schedules, paused in maintenance) appends the anchor,
  so a run is anchored within about a minute of ending.
  `main.step_record_retention` anchors any settled run a mark missed.
* **The security audit relay now runs on every install**, not only with the
  governed credential profile, so the anchor events
  (`ai.elitea.security.v1.run.steps.anchored`, carrying `chain_head` and
  `chain_length`) and the lifecycle events of #1497 reach
  `elitea_identity.security_audit_events` and the SIEM export. Its backlog
  alert defaults to 10000 rows when no governance revision sets one. SIEM
  rules keyed on event types should add the new type.
* **SIEM export: a third stream `run_steps`** (source `run_step`, event type
  `run_step.<kind>`, chain partition `run:<run id>`) with its own cursor per
  sink, on by default. It carries digests and `run-evidence/<key>` refs,
  never content. Turn it off with `ELITEA_AUDIT_EXPORT_RUN_STEPS=false`;
  `ELITEA_AUDIT_EXPORT_RUN_STEPS_FROM=beginning` replays existing rows on the
  first run. The existing lag and backlog alerts cover it by its `stream`
  label. Receivers that validate against `elitea.audit.v1` should take the
  updated schema (new `run_step` details, `run:` partitions).
* **Retention:** `step_content` and `step_record` are now live classes on
  Administration → Retention (floors 1 and 7 days, overridable per project,
  enforced). Two new routines, hourly and listed under Administration →
  Schedules: `main.step_content_retention` (expires content past the
  maximum, rows first, then deletes the objects, and deletes content
  objects whose step record never landed an hour after they were written,
  tracked in `elitea_runtime.run_step_content_pending`) and
  `main.step_record_retention` (anchors settled runs whose anchor was
  missed, then deletes whole runs past the maximum once every sink that
  currently exports `run_steps` has passed them; the position of a removed
  sink or a disabled stream holds nothing). Both skip what an active legal hold covers and pause in
  maintenance. Deleting a conversation expires its runs' step content
  (policy `conversation_deleted`); the records stay under `step_record`.
* **New routes** (each needs `runs.evidence.read`):
  `GET /api/v2/elitea_core/run_steps/prompt_lib/{project_id}/{run_id}`,
  `.../{run_id}/verify`, `.../{run_id}/{step_id}/{inputs|outputs}` (every
  content read writes an `audit_events` row first, and answers 503 when it
  cannot) and
  `GET /api/v2/elitea_core/conversation_runs/prompt_lib/{project_id}/{conversation_uuid}`.
* **Worker report sizes:** the runtime control service now admits a
  `ReportRunStepsRequestV1` of up to 1 MiB and a
  `ReportEgressSummaryRequestV1` of up to 256 KiB (other control requests
  keep 64 KiB). Before, an egress summary over 64 KiB was refused.
