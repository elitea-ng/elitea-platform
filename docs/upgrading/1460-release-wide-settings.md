---
title: "Release-wide settings (#1460, chart)"
issue: 1460
date: 2026-11-01
order: 63
---

New chart values that fill many components at once: `global.publicUrl`,
`global.trustedProxyCidrs`, `global.otelEndpoint`, `certManager.issuers`
and `externalSecrets.sharedProperties`. A value you set on a component
still wins. New script: `deploy/scripts/mint-secrets.sh`.

**An install from your own values file (not `values-gke.yaml`)** gets
these changes:

- **Traces.** The chart now sets `OTEL_EXPORTER_OTLP_ENDPOINT` on
  elitea-main, the scheduler, the LLM gateway, DeepWiki, Inventory,
  elitea-vector and the worker to the bundled collector
  (`http://elitea-otel-collector:4318`) while `otelCollector.enabled`,
  where none of `OTEL_EXPORTER_OTLP_ENDPOINT`,
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or `OTEL_SDK_DISABLED` is set. With
  the collector off and no `global.otelEndpoint`, all but the gateway get
  `OTEL_SDK_DISABLED=true` and stop trying `localhost`. Set one of the
  three variables on a component to keep its old behaviour.
- **CPU requests.** Every request below 50m is now 50m (PgBouncer, the
  dbInit, objectInit and KEDA-cleanup Jobs, elitea-web, the worker's
  runtime-session sidecar, two sandbox supervisor init containers). GKE
  Autopilot raised them to 50m already; elsewhere the release requests
  about 0.2 vCPU more. A request you set is kept.
- **NATS bootstrap.** The Job's pod restarts in place
  (`restartPolicy: OnFailure`) instead of a new pod per attempt.
  `backoffLimit` still counts the restarts.
- **dbInit.** `dbInit.agentStateDatabase` and `dbInit.appRole` now
  default to empty: the Job reads them from the worker's
  `agent-checkpoint-connection` DSN and the user of
  `postgresql.existingSecret`. A value you set is checked against the
  DSN, and the Job **fails on a mismatch** (before, it created a database
  the worker never opened). A keyword-form DSN (`host=… dbname=…`) is not
  read: then set both values. The admin role still needs `CREATEDB`.
- **Placeholders.** The render now fails while any value holds a
  `<...>` placeholder (a `<` letter … `>` string), and lists each path.
- **Issuers.** No change: `certManager.issuers.create` defaults to false,
  and an empty kind keeps `ClusterIssuer`.

**An install from `values-gke.yaml`** also gets these. Read each before
you upgrade:

1. **Shared secret properties.** `values-gke.yaml` now sets
   `externalSecrets.sharedProperties: true`, so these ExternalSecrets
   read new property names from `elitea-platform`:

   | Before | Now |
   |---|---|
   | `elitea-runtime-material/runtime-ca.crt`, `elitea-worker-material/runtime-ca.crt`, `elitea-runtime-ca/tls.crt` | `runtime-ca.crt` |
   | `elitea-runtime-ca/tls.key` | `runtime-ca.key` |
   | `elitea-runtime-material/command-signing-keyring.json`, `elitea-worker-material/command-signing-keyring.json` | `command-signing-keyring.json` |
   | `elitea-llm-gateway-secrets/identity-secret`, `elitea-main-llm-gateway-secrets/gateway-identity-secret` | `gateway-identity-secret` |

   Add the four new properties to the secret, with the **same values**,
   before you sync. Or set `externalSecrets.sharedProperties: false` in
   your values to keep the old names. A missing property holds back the
   whole Kubernetes Secret.
2. **Issuers made by the chart.** `values-gke.yaml` now sets
   `certManager.issuers.create: true`. The chart renders the
   `elitea-internal-ca` chain (`elitea-internal-ca-selfsigned`, the
   `elitea-internal-ca` Certificate and Issuer) and the `elitea-runtime-ca`
   Issuer, and lists the Secret `elitea-runtime-ca` as an ExternalSecret.
   If you made these by hand, either delete the hand-made Issuers and the
   `elitea-internal-ca` Certificate before the sync (Helm and Argo CD do
   not adopt them; cert-manager can re-issue the gateway, DeepWiki and
   Inventory certificates, so restart elitea-main and those three once
   their Certificates are Ready again), or set `certManager.issuers.create: false` and
   `certManager.issuers.internal.kind` / `.runtime.kind` to `Issuer`. The
   example values' `externalSecrets.secrets.elitea-runtime-ca` block is
   not needed any more; kept, it merges into the same ExternalSecret.
   DeepWiki's and Inventory's mTLS Certificates now name the namespaced
   `Issuer` too (before, `values-gke.yaml` left them on `ClusterIssuer`).
3. **Worker identity.** With `global.publicUrl` and an empty
   `worker.runtimeSession.expectedIdentity`, the chart derives
   `spiffe://<host>/ns/<namespace>/sa/agent-worker`. The old example used
   `…/sa/worker`. If your worker certificate carries `…/sa/worker`, keep
   `worker.runtimeSession.expectedIdentity` set to it, or the worker's
   runtime session is refused.
4. **Gateway self origins.** Derived as `<publicUrl>/llm` and
   `http://elitea-main:8080/llm` (before: `…/llm/v1`). The gateway
   compares path segments, so `/llm` also covers `/llm/v1`: the check is
   wider, not narrower.
5. **Values moved to `global`.** `values-gke.yaml` no longer sets the
   Ingress hosts, `DEPLOYMENT_URL`, `ELITEA_TRUSTED_PROXY_CIDRS`,
   `OIDC_REDIRECT_URI`, `public_origin`, `trusted_proxy_cidrs`,
   `GATEWAY_SELF_LLM_ORIGINS`, `worker.runtime.publicBaseUrl` or the
   per-block `issuerRef`s. Set `global.publicUrl` and
   `global.trustedProxyCidrs`; values you already set keep working.
   `global.trustedProxyCidrs` holds a `<pod CIDR>` placeholder that the
   render refuses until you replace it.
6. **Budget.** The worker's CPU request is 1400m (was 1500m), and
   elitea-web's is 50m (was 10m): steady state 3700m, peak 3950m.
