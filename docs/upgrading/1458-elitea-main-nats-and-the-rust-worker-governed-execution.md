---
title: "elitea-main, NATS and the Rust worker: governed execution context, grants and identities (off by default)"
issue: 1458
date: 2026-10-27
order: 30
---

**Affects:** every deployment re-runs the NATS bootstrap Job (Helm does it
on upgrade). Nobody else has to act while
`ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED` stays `false`. Four
additive shared migrations (0184–0187) create empty tables.

**What changed.**

* The NATS bootstrap adds a governed subject (`elitea.rt.v1.<route>.g.*`) to
  each command stream and three durables,
  `elitea-{configuration,agent,index}-worker-governed-v1`. The chart's
  permission table grants the worker identity the new durables. A flag-off
  Main accepts a stream with or without the governed subject, so the order of
  the upgrade does not matter.
* With the flag on, every governed admission stores an immutable execution
  security context (0187), and the run's signed commands carry its digest.
  Governed commands go only to governed durables. Main refuses to boot
  without the governed subject and durables, and it refuses claims from a
  worker that does not verify the context (`GOVERNED_WORKER_REQUIRED`).
  The chart sets the Rust worker's `ELITEA_WORKER_GOVERNED_CONTEXT` from the
  same flag. **Outside the chart, run at least one Rust worker with
  `ELITEA_WORKER_GOVERNED_CONTEXT=true` before turning the flag on**, or
  governed runs wait until their deadline.
* With the flag on, credential use grants are mounted under
  `/api/v2/projects/{project_id}/credential_grants` (0184). The five
  `models.credentials.governed.*` permissions are granted to no role. External
  agent identities are mounted under `/api/v2/admin/external_agent_identities`
  (0186, `models.agent_identity.external.manage` for `super_admin` and
  `admin`).
* With the flag on, an OIDC provider whose stored settings say
  `"entra": true` keeps encrypted delegated tokens for on-behalf-of exchange
  (0185) and admits only its `entra_tenants`.

**Still not enforced.** No credential path calls the grant decision or the
broker until #1118. The operator guide
(`/docs/deployment/credential-governance`) covers each setting.
