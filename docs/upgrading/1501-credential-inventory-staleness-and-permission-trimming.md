---
title: "Credential inventory, staleness and permission trimming (#1501)"
issue: 1501
date: 2026-10-30
order: 50
---

* **Shared migration 0208** creates `elitea_identity.credential_last_use`
  (last use per credential, any kind), `elitea_identity.credential_review_state`
  (first seen, flagged, notified and revoked stamps),
  `elitea_lifecycle.permission_trimming_candidates` (removal candidates and
  what owners were notified of) and
  `elitea_lifecycle.permission_trimming_observed` (when the weekly routine
  first saw each granted item). It grants `credentials.inventory.read` and
  `credentials.inventory.manage` to the administration `super_admin` and
  `admin` roles, and `agents.trimming.manage` to every project's `admin`
  role (saved role matrices included). Nothing is backfilled: last use
  starts empty, so every credential reads as never used until it is used,
  and its stale clock starts at the first staleness pass (first seen), not
  at its creation.
* **Last use is recorded** on personal access token authentication, toolkit
  secret resolution for a run, MCP OAuth token use, provider OAuth token use
  and credential broker redemption (reference and use grant). Writes are
  coalesced in memory and flushed every 30 seconds in one batched upsert,
  at most one write per credential per hour (the SQL guard makes that hold
  across replicas too); no request path writes to the database for it.
* **New Platform settings section "Credential review"**
  (`credential_review`, gated on `configuration.governance`):
  `stale_after_days` (default 90, 1-3650), `stale_action` (`flag`, the
  default, or `revoke`) and `trimming_window_days` (default 30, 1-365).
  Under `revoke` the daily pass **deletes stale personal access tokens and
  revokes stale credential use grants**; check the inventory before
  switching it on. Toolkit secrets, MCP and provider OAuth tokens and
  credential references are only ever flagged: they are issued elsewhere.
* **Two new routines** under Administration → Schedules, both paused in
  maintenance: `main.credential_staleness` (daily) flags stale credentials,
  revokes under the `revoke` policy, audits both
  (`ai.elitea.security.v1.credential.review`) and notifies the owner once
  per flag; `main.permission_trimming` (weekly) recomputes the trimming
  report and notifies agent owners (registry business and technical owner,
  else the agent's author) of new removal candidates only. An item granted
  for less than the trimming window is never a candidate ("too new to
  judge"); egress entries and tools of an edited selection are dated by the
  routine's first sighting, so they become candidates no sooner than one
  window after the first weekly pass.
* **New admin pages** Credential inventory and Permission trimming, and a
  Permission trimming tab in an agent's ⋮ › Governance dialog. Removals
  edit the draft version only (a published version is changed by
  publishing), go through the same paths as the editor and the settings
  page, and are audited as `permission.trimmed`.
