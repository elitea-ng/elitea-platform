---
title: "Agent registry, change requests, release gate, recertification and attestations (#1497)"
issue: 1497
date: 2026-10-29
order: 46
---

* **Shared migration 0203** creates the release gate tables
  (`elitea_lifecycle.release_gate_suites`, `release_gates` and their runs;
  a trigger freezes a gate's outcome and override once recorded).
  **Shared migration 0204** creates `agent_registry`, `change_requests`,
  `change_request_approvals` (a CHECK refuses an approver equal to the
  proposer) and `attestations`, and grants the new permissions
  `models.applications.governance` (platform `super_admin` and `admin`,
  project `admin`), `lifecycle.attestations.write` and
  `lifecycle.registry.mark_due` (project `admin`).
* **Backfill, once:** every existing agent and pipeline gets a registry row,
  `uncertified`, tier **`medium`**, except those in a personal project
  (tier `low`). Every published version gets a `legacy_unreviewed` change
  request linked to its release.
* **Behaviour change: publishing a medium agent now needs an approval.**
  A direct publish of an agent whose tier requires approval answers
  **409 `change_request_required`**. Create and submit a change request
  (the web UI's Publish button does this); someone other than the author
  approves it, and the approval publishes. To keep the old behaviour for an
  agent, a governance role holder sets its tier to `low`. Scripts and CI
  jobs that publish agents through the API must either target `low`-tier
  agents or drive the change request routes.
* **Rollback of a governed agent needs the governance role.** A rollback
  (`POST application_releases/.../rollback`) of an agent whose tier
  requires approval answers **409 `change_request_required`** unless the
  caller holds `models.applications.governance`.
* **Loosening a governed agent's gate needs the governance role.** A
  `PUT release_gate_suites/...` that unmarks a suite, unsets the regression
  flag, removes a criterion, changes its kind, lowers `min_score` or raises
  `max_drop` on an agent whose tier requires approval answers
  **403 `governance_role_required`** without `models.applications.governance`.
* **New 409 and 403 answers** on the change request routes:
  `self_approval_refused`, `approver_role_required`,
  `release_gate_pending`, `release_gate_failed`, `attestation_missing`,
  `change_request_open`, `change_request_stale` and others listed in the
  Agent lifecycle docs page. With `lapse_action = suspend`, runs of a lapsed
  agent answer **409 `agent_certification_lapsed`**.
* **New platform config section `agent_lifecycle`** (permission
  `configuration.governance`): approvals per tier, recertification cadence,
  mandatory attestation kinds, `lapse_action` (default `warn`). Saved
  values below the ADR-0040 floors are raised to them.
* **New routines:** `main.release_gate` (every 5 minutes; settles gates and
  starts scheduled re-evaluations) and `main.agent_recertification`
  (hourly). Both appear under Administration → Schedules.
* **New security audit event type** `ai.elitea.security.v1.agent.lifecycle`
  (actions `agent.change.*`, `agent.registry.tier_changed`,
  `agent.certification.*`, `agent.attestation.*`). SIEM rules keyed on event
  types should add it.
* **Evaluation runs started by a gate cost model calls** like any evaluation
  run, billed to the user who submitted the change request (or, for a
  scheduled re-evaluation, the user who last saved the schedule).
