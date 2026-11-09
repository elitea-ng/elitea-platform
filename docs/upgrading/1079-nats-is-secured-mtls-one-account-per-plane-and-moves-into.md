---
title: "NATS is secured (mTLS, one account per plane) and moves into the platform namespace — BREAKING; budget counters reset"
issue: 1079
date: 2026-10-06
order: 4
---

**Affects:** every Kubernetes deployment of `deploy/helm/nats`,
`deploy/helm/nats-bootstrap` and `deploy/helm/elitea`, and every Argo CD
install from `deploy/argocd`. Compose is not affected unless you opt into
`deploy/docker-compose.nats-secure.yml`.

**What changed.**

- The NATS server requires TLS and a client certificate on 4222. Each client's
  certificate carries a URI SAN (`spiffe://elitea.internal/nats/<identity>`)
  that maps to a user (`verify_and_map`); there is no anonymous or password
  access. All URLs are `tls://…` with no credential.
- Users live in one NATS account per plane — `MAIN` (elitea-main: presence
  KV, presence subject), `GATEWAY` (gateway and scheduler: budget and
  rate-limit counters, write-behind deltas, cooldown KV) and `RUNTIME`
  (reserved for the command bus). Only GATEWAY's per-project soft-alert
  subject crosses (into MAIN).
- Canvas presence is published on `elitea.events.project.<id>.presence`
  (was `gateway.events.project.<id>.events`); the SSE route accepts each
  event type only from its own subject.
- The `nats-bootstrap` Job creates every stream, KV bucket and the
  scheduler's `budget-writeback` consumer, once per account with a
  per-account certificate that exists only during the install (a one-hour
  hook Certificate). The scheduler only binds, and keeps retrying until the
  consumer exists (`GET /readyz/budget-writeback`). Its
  `BUDGET_WRITEBACK_ACK_WAIT` / `BUDGET_WRITEBACK_MAX_DELIVER` moved to the
  bootstrap chart's `deltas.writeback.ackWait` / `maxDeliver`.
- In HA, routes use their own route CA and certificate.
- An ingress NetworkPolicy admits 4222 only from elitea-main,
  elitea-llm-gateway, elitea-scheduler and nats-bootstrap pods.
- The Argo CD sample installs NATS and its bootstrap into **`elitea`** (the
  platform's namespace) instead of `elitea-gateway`, because the NATS CA
  Issuer is namespaced and signs the platform's client certificates.

**What this resets.** Moving the NATS Application to another namespace
creates a new StatefulSet with a **new, empty JetStream volume**; nothing is
migrated (greenfield, by decision). After the switch:

- budget counters and rate-limit windows start at zero for the current
  period, so budget **enforcement** for the period restarts from zero (spend
  already written back to `gateway.llm_budget_accumulators` stays, and the
  budget screens keep showing it);
- write-behind deltas the scheduler had not drained yet are lost;
- the soft-alert cooldown resets, so an 80% alert can fire once more;
- canvas presence rosters reset (they rebuild within one heartbeat).

**What to do.**

1. Install cert-manager (a prerequisite now). Optionally install
   cert-manager approver-policy and disable cert-manager's built-in approver;
   the NATS chart then renders policies that pin what its issuers sign
   (`security.approverPolicy`).
2. Restrict `create` on cert-manager `Certificate`/`CertificateRequest` and
   read on `Secret` in the NATS namespace to the deployer and operators: those
   rights can mint any NATS identity (`deploy/helm/elitea/NATS.md`, "Who can
   mint a NATS identity"; the `deploy/helm/nats` chart this entry names is now
   bundled into `deploy/helm/elitea`).
3. Install NATS, `nats-bootstrap` and the platform into one namespace (or use
   a `ClusterIssuer`; the platform chart refuses a different `nats.namespace`
   with the namespaced Issuer). With Argo CD, sync the updated
   `deploy/argocd` Applications; the bootstrap runs as a PostSync hook.
4. Optional, to keep the undrained deltas: before switching, scale the LLM
   gateway to zero, wait until the old NATS's `budget-writeback` consumer
   shows no pending messages, then switch.
5. Delete what the old install left in `elitea-gateway`: the JetStream PVC
   (`elitea-nats-js-elitea-nats-0`, and `-1`/`-2` in HA) is not pruned by
   Argo CD, and the namespace itself if nothing else uses it.
6. Remove `BUDGET_WRITEBACK_ACK_WAIT` / `BUDGET_WRITEBACK_MAX_DELIVER` from
   any scheduler values override (they are ignored), and set
   `deltas.writeback.*` on the bootstrap if you had tuned them.
