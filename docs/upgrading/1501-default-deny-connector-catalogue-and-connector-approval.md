---
title: "Default-deny connector catalogue and connector approval (#1501, off by default)"
issue: 1501
date: 2026-10-30
order: 51
---

ADR-0044 decision 1. Shared migration **0207_connector_approval** adds
`centry.connector_requests` (states draft, submitted, approved, rejected,
withdrawn; a CHECK refuses an approver who is the requester), widens
`elitea_runtime.command_outbox`'s refusal CHECK with `CONNECTOR_NOT_APPROVED`,
adds `connector` to the attestation subject types, and grants the new
`connector.approve` permission to `super_admin` and `admin` in the
administration mode (also seeded in `001_initial.sql`).

- **Setting.** Configuration › Connector Catalogue (`catalogue` section,
  gated on `configuration.governance`): `mode` = `open` (default, behaviour
  unchanged) or `approved_only`, and `require_architecture_attestation`
  (default off). A mode value that is not `open` reads as `approved_only`.
- **Governed profile.** A deployment running the governed credential profile
  is always `approved_only`, whatever the setting says; so is any governed run
  (a command carrying a security context).
- **approved_only.** A toolkit type, a pre-built MCP server or a custom MCP
  URL is usable only by the projects an approved connector request names (or
  every project, for a platform-wide request). Platform-internal types
  (application, artifact, memory, vectorstore, datasource, the MCP registry)
  are exempt. Enforced on the type catalogue, toolkit create/update/fork
  (403 `connector_not_approved`), MCP tool discovery (an unapproved server is
  answered as an unstored preview with `connector_approval_required`), the
  agent freeze, and worker command admission (the run fails with
  `CONNECTOR_NOT_APPROVED`). **Switching an existing deployment to
  approved_only refuses every existing toolkit until its connector is
  approved** — file and approve requests first.
- **Approval** writes the toolkit-type grant (and an `enabled` policy row if
  none exists; a `disabled` policy refuses the approval) and the worker
  egress allowlist entries (the project's `worker_egress` setting, or the
  platform allowlist for a platform-wide request) in one transaction, and is
  audited (`ai.elitea.security.v1.connector.approval`). With the attestation
  setting on, an `architecture` attestation of subject `connector` with the
  request id must exist in the requesting project.
- **Custom MCP pinning.** An approved custom MCP server's tools are pinned by
  name and digest (SHA-256 of the canonical JSON of name, description and
  input schema). A tool the server adds or changes later is hidden from
  discovery and refused at call time until a new request is approved. The
  freeze writes `pinned_tools` into the frozen toolkit settings, and the
  worker (agent-runtime) keeps only pinned tools: **upgrade the worker with
  Main**; an older worker ignores `pinned_tools` (Main still refuses direct
  calls of unpinned tools and runs frozen without current pins).
- **UI.** Admin › Connector requests (`connector.approve`); a "Request
  connector" offer where a toolkit save or an MCP Load Tools is refused.
