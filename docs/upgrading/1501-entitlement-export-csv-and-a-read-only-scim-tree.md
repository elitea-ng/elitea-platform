---
title: "Entitlement export: CSV and a read-only SCIM tree (#1501)"
issue: 1501
date: 2026-10-30
order: 49
---

* **Shared migration 0209** grants the new permission `entitlements.export`
  to the platform `super_admin` and `admin` roles (administration mode). No
  table and no data change. A custom administration role needs the grant
  added by hand to reach the export.
* **`GET /api/v2/admin/entitlements`** (`exportEntitlements`, behind
  `entitlements.export`) answers who holds which role on the platform, on
  each project and on each agent (agent registry business and technical
  owners), with the governance capabilities each role carries: one row per
  (user, scope, role, capability, granted_via). `format=csv` streams every
  row from one snapshot as RFC 4180 CSV with an `X-Entitlement-Rows`
  header; `format=json` (default) pages with `limit` and `offset`. Filters:
  `user_id`, `project_id`. Admin › Users has an **Export entitlements**
  button for the CSV.
* **`/api/v2/scim/v2/entitlements`** is a second, read-only SCIM 2.0
  service provider for identity governance tools: `/Users` (each with
  `groups`, `roles`, `entitlements` and the
  `urn:elitea:params:scim:schemas:extension:entitlements:2.0:User` grants),
  `/Groups` (one per platform role and per project role, ids
  `platform-role-<id>` and `project-role-<id>`), `/Users/{id}`,
  `/Groups/{id}` and the discovery documents. `startIndex`/`count` paging,
  `filter` on `userName`, `displayName`, `id` and `active` (Users) and
  `displayName`, `id` (Groups). It takes the same SCIM client credential as
  provisioning (Admin › Configuration › Authentication). Write verbs answer
  405.
* **The SCIM tree is opt-in per client; nobody can read it after the
  upgrade.** SCIM clients carry no scopes, so a new platform-config section,
  `entitlement_export` (Admin › Configuration › **Entitlement Export**,
  behind `entitlements.export`), lists the SCIM client ids allowed to read
  it in `scim_client_ids`. The default is empty: every client — every
  existing provisioning credential included — gets 403 with a SCIM error
  body on every path of the tree, discovery included, until an
  administrator lists it. Provisioning (`/api/v2/scim/v2/Users`, `/Groups`)
  is unchanged. To connect an identity governance tool, create a SCIM client
  for it (or reuse one) and add its id to the list; the change is audited
  like every other configuration save, and the list is read on every
  request, so removing an id cuts access at once. The CSV/JSON admin export
  is not affected by this list.
* **Every export is audited** in `centry.audit_events` (`entity_type
  entitlements`, `entity_name` csv, json or scim, the row count in
  `action`, the administrator or `scim:<client>` as actor), written before
  any row is sent. When the row cannot be written the export is refused
  with 503.
* **Capabilities are derived from permissions** in one table,
  `services/elitea-main/internal/domain/entitlements/capabilities.go`; a
  test fails when a gated or granted permission is in neither that table
  nor its not-governance-relevant list. `connector.approve` is mapped to
  `approve_connectors` ahead of the connector approval change and produces
  no row until it is granted.
