---
title: "GKE Ingress support and SSE keepalives on /llm and MCP streams"
issue: 1390
date: 2026-10-22
order: 23
---

**Affects:** Kubernetes installs on GKE that publish elitea-main through a
standard Ingress; every client of `/llm` streaming and of MCP `tools/call`
streams (no action needed).

**What changed.**

* `main.ingress.gke` renders the GKE companions of the Ingress: the
  `cloud.google.com/neg` and `cloud.google.com/backend-config` annotations on
  the elitea-main Service (the `http` port only), a BackendConfig
  (`timeoutSec: 3600`, connection draining, HTTP `/healthz` health check on
  8080, optional `securityPolicy`) and a FrontendConfig (`redirectToHttps`,
  optional `sslPolicy`), bound with `networking.gke.io/v1beta1.FrontendConfig`.
  It turns on with `gke.enabled`, `className: gce` / `gce-internal`, or the
  same value in the `kubernetes.io/ingress.class` annotation.
* `main.ingress.certManager` (`issuer` or `clusterIssuer`, plus `issuerKind`
  and `issuerGroup` for an external issuer) annotates the Ingress for
  cert-manager. `main.service.annotations` is new.
* elitea-main writes `: keepalive` SSE comments into an idle `/llm` event
  stream (between events only) and into an MCP `tools/call` stream while it
  waits for the result, every 25 seconds of silence. Conforming SSE parsers
  ignore comments.

**What to do.**

* On GKE, set `main.ingress.gke.enabled: true` and admit `35.191.0.0/16` and
  `130.211.0.0/22` in `networkPolicies.main.ingressFrom`. If you already
  manage a BackendConfig or FrontendConfig, set its `create: false` and
  `name`.
