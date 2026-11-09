---
title: "Governance follow-ups"
issue: 1506
date: 2026-10-28
order: 38
---

* **elitea-main: definition digest checked at admission (#1462, governance
  flag on only).** Admission now computes the canonical digest of the
  agent version's stored definition (the value a root run records as
  `source_definition_sha256`) and compares it with the active binding's
  `definition_digest`. On a mismatch the run binds **no** agent identity:
  the broker denies every governed use, a mandate-bound schedule or trigger
  is refused, and the security audit records `definition_digest_mismatch`.
  Bindings proposed with any other digest stop binding after the upgrade.
  **Before upgrading a deployment with the governance flag on, plan to
  re-pin every active binding to its computed digest**, or governed runs
  lose their identity. The re-pin route ships with this release, so do it
  as the first step after the rollout, with both administrators on hand:
  list `GET /api/v2/admin/agent_identities?state=active`; for each
  row whose `definition_digest` differs from `computed_definition_digest`,
  one administrator calls `POST /api/v2/admin/agent_identities/{id}:repin`
  and a second calls `:approve`. Plan a short window: governed runs of a
  binding are unbound between the two calls. Re-pin again after every
  agent edit, toolkit edit and credential reference `:attach`/`:detach`,
  because the definition embeds the toolkits' stored settings. An agent
  from another project's catalogue cannot be bound. Deployments with the
  flag off are unaffected.
* **elitea-main: `POST /api/v2/admin/credential_references` (#1482).**
  Stages a credential reference from scratch (target binding, class,
  backend, locator) as `staged`, with the caller as `staged_by`; a second
  administrator activates it as before. Same permission
  (`models.agent_identity.bindings.manage`) and flag gating as the other
  reference routes; no migration. For a static secret the locator is
  checked through the secret store's metadata call under the
  `credential-migration` workload scope, so on Vault/OpenBao the
  `<role prefix>-credential-migration` role needs metadata read on
  `elitea/projects/*/credentials/*`. The value is never read.

* **elitea-main: a governed run's worker gets a run-scoped token (#1461,
  governed runs only).** With credential governance on, a run pinned to the
  governed profile no longer receives its user's personal access token.
  It receives an `elmrt_` run token instead. The token reaches the model
  gateway (`/llm`) and the run project's artifacts, nothing else. On
  artifacts it works only on the worker's five routes (create bucket, list
  objects, upload, download, stat). It works only for the run's project
  and execution, while the run's claim holds its lease. A request naming
  another project, and every other route, answers 403 for it. Model usage
  and artifact access are attributed to the same project and user as
  before. No new key or setting: the token key is derived from the
  personal access token signing key. Legacy-pinned runs and deployments
  with the flag off are unchanged.

* **elitea-main: new metric
  `elitea_security_audit_head_lock_wait_seconds{partition,result}`
  (#1480).** It times each security audit append's wait for its team's
  chain head. One team's governed credential use is serial on commit time:
  about 400 redemptions/s on PostgreSQL 18 with stock settings (the
  "Limits" section of the credential governance page). Nothing to
  configure; add it to dashboards and alert on its p99 if one team drives
  heavy governed load.
* **`migrate-references`: Jira Cloud and Confluence Cloud basic auth
  (#1481).** A configuration with no `token`, a sealed `api_key` and a
  `username` is now staged as one composite secret holding
  `{"token", "username"}`: `--apply` writes it to the built-in vault as
  `governed_<configuration uuid hex>` (or to the `--copy-to vault` path)
  through the secret write ledger, and the broker releases both keys. That
  write needs `SECRETS_MASTER_KEY` and a policy revision
  (`--policy-revision` or the configured one). A basic-auth reference
  staged by an earlier release is token-only and its governed calls fail:
  retire it and run `--apply` again to stage the replacement. Token-only
  references (PATs) are unchanged.
* **elitea-main: a refused governed resume settles its paused turn
  (#1485).** When a grant, reference or binding is revoked while a governed
  run waits on an approval, the resume is still refused with `403
  governed_credential_refused`, and now the turn is settled too: the
  approval card goes, the answer ends with `is_error`, `error_code:
  governed_credential_refused` and a reason that names no credential, and
  the paused job is cancelled. Nothing to configure.
