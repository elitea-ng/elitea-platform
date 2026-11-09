---
title: "elitea-main: enterprise credential governance foundation (off by default)"
issue: 1456
date: 2026-10-27
order: 29
---

**Affects:** nobody has to act. Four additive shared migrations (0180–0183)
run on upgrade and create empty tables. Operators who plan to adopt
ADR-0039 read on.

**What changed.** `ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED` (default
`false`) adds the governed credential profile. With it off, admissions,
routes and wire bytes are unchanged and no new dependency is built. With it
on:

* `ELITEA_CREDENTIAL_GOVERNANCE_POLICY_REVISION` must name a revision created
  with `elitea-main credential-policy put --id … --file … --operator …`.
  Boot refuses a missing or tampered revision, one that allows no identity
  mode, a missing security audit outbox, or a Vault/OpenBao store the
  revision requires but `ELITEA_SECRETSTORE_VAULT_*` does not configure.
* Every new admission is pinned to the governed profile and that revision.
  A pin never changes, and a resumed run keeps the pin it started under, also
  after the flag is turned off.
* The agent identity binding and mandate admin API is mounted under
  `/api/v2/admin/agent_identities` and `/api/v2/admin/mandates`, gated by the
  four new `models.agent_identity.*` permissions.
* The security audit relay runs. `elitea-main security-audit verify --tenant N`
  checks the hash chain. The scheduler's
  `SECURITY_AUDIT_OUTBOX_RETENTION_DAYS` is `0` (delete nothing) by default.

**What this release does not do yet.** It records the profile; it does not
enforce it. No credential path reads the pin until the credential broker
lands (#1118), so a governed run uses credentials exactly as a legacy run
does. Turn the flag on in this release only to stage revisions, bindings and
mandates. The operator guide is the embedded docs page
"Enterprise credential governance" (`/docs/deployment/credential-governance`). When enforcement lands, drain before turning the flag off: a
governed run still active then fails closed at its next credential use.
