---
title: "One edge route: elitea-main serves `/` and relays `/app` and `/docs` to elitea-web — BREAKING"
issue: 1390
date: 2026-10-22
order: 21
---

**Affects:** Kubernetes installs that publish the platform through
`main.ingress.enabled`, and any cluster that applied
`deploy/gateway-api/httproute.yaml` or a copy of it (#1359 P3).

**What changed.**

* **elitea-main is the browser edge in a cluster too.** The chart sets
  `ELITEA_WEB_UPSTREAM` to the elitea-web Service (`http://elitea-web:80`)
  whenever `web.enabled` is true. elitea-main then redirects `/` to `/app/`,
  relays `/app…` and `/docs…` to elitea-web, and keeps the paths it serves
  itself under `/app` — the MCP server at `/app/<project id>/mcp[/…]` and
  the two icon file servers — exactly as it does in compose.
* **The edge is ONE `/` rule to elitea-main**, on the chart's HTTPRoute, its
  Ingress, and `deploy/gateway-api/httproute.yaml`. The second HTTPRoute
  (`elitea-web`, `/app`, `/docs` and the `/` redirect) and the per-path rule
  list are gone. MCP clients now work on every Gateway: the carve-out once
  needed a RegularExpression path match, which the manifest never carried.
  The chart's HTTPRoute now renders `PathPrefix` for `main.ingress.pathType:
  Prefix` (it rendered `Prefix`, which Gateway API does not accept).
* **The plain-Ingress identity guard is gone**, with
  `main.ingress.identityHeadersStrippedByController`. elitea-main reads no
  `X-Auth-*` header and deletes every client identity header itself, so a
  plain Ingress with Form sign-in renders. The HTTPRoute keeps its
  `stripIdentityHeaders` filter as defence in depth.
* **Probes.** With the edge on, elitea-main refuses `/readyz` and `/startupz`
  to a non-loopback caller, so the startup and readiness probes run
  `/elitea-main -healthcheck <path>` inside the container. `-healthcheck`
  takes an optional path (`/readyz`, the default, `/startupz` or `/healthz`).
  The chart refuses any other `main.probes.{startup,readiness}.path` while
  the edge is on. Liveness stays an httpGet on `/healthz`. An older
  elitea-main ignores the path argument and reports `/healthz`, so a probe
  passes before the replica is ready: with `web.enabled` (the default),
  `main.image.tag` must be this release or later. Leave it unset to follow
  the chart's appVersion. `main.probes.execCommand` names the binary and
  `main.probes.*.timeoutSeconds` the probe timeouts.
* **NetworkPolicy.** elitea-web gets `elitea-web-netpol`: its port admits
  elitea-main, `networkPolicies.web.extraIngressFrom` and
  `networkPolicies.probeFrom` only.

**What to do.**

* A cluster that applied `deploy/gateway-api/httproute.yaml`: re-apply it, and
  delete the old `elitea-web` HTTPRoute (`kubectl delete httproute elitea-web`).
  Keep `elitea-main`'s HTTPRoute pointing at port 8080.
* An Ingress or route of your own that sent `/app` or `/docs` to elitea-web:
  send them to elitea-main instead, or simply route `/` to elitea-main.
* Remove `main.ingress.identityHeadersStrippedByController` from your values
  (it is ignored).
* Anything else that reached elitea-web directly (a debug pod, a scraper):
  list it in `networkPolicies.web.extraIngressFrom`.
