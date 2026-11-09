---
title: "Default-deny NetworkPolicies; an ingress decision is required"
issue: 1176
date: 2026-10-09
order: 8
---

**Affects:** every install of the `deploy/helm/elitea` chart. A CNI that
enforces NetworkPolicy (Calico, Cilium, etc.) is needed for the objects to take
effect.

**What changed.** The chart now renders ingress NetworkPolicies, on by default
(`networkPolicies.enabled: true`):

- `elitea-main`: port 8080 admits the platform edge, the DeepWiki and Inventory
  pods that call back directly, and the peers you list; the runtime listeners
  admit the worker pods only.
- the platform edge: port 443 from the worker, `elitea-main` and (when
  `deepwiki.callbackViaPlatformEdge` is set) DeepWiki; egress only to
  `elitea-main` and cluster DNS.
- the worker: no inbound connections. Worker egress is not restricted.
- the sandbox supervisor: its profile ports admit the worker and `elitea-main`.

**What to do.** The chart cannot see the pods that deliver outside traffic to
`elitea-main:8080`, so it refuses to render until you state who they are:

- list the Gateway API gateway or ingress controller as NetworkPolicyPeer
  entries in `networkPolicies.main.ingressFrom` (or `extraIngressFrom`). This
  includes a Gateway API HTTPRoute applied outside the chart, such as
  `deploy/gateway-api/httproute.yaml`; and `main.ingress.enabled=true` needs
  `ingressFrom` itself; or
- set `networkPolicies.main.noExternalIngress=true` when nothing outside the
  chart's own pods reaches `elitea-main` (for example access by
  `kubectl port-forward` only). It is refused together with
  `main.ingress.enabled=true`.

If your own policy already covers these workloads, set
`networkPolicies.enabled=false` together with
`networkPolicies.externallyManaged=true`. If your CNI applies policy to kubelet
probes, list the probe sources in `networkPolicies.probeFrom`. The Argo CD
sample has a new parameter for the gateway namespace
(`networkPolicies.main.ingressFrom[0]...`) next to the other values to fill in.

**Related, unchanged checks.** (Superseded by "One edge route" above: the
guard below is removed.) A plain `networking.k8s.io/v1` Ingress with Form
sign-in is still refused unless the controller removes the inbound `X-Auth-*`
and `X-Elitea-*` identity headers and
`main.ingress.identityHeadersStrippedByController=true` says so. (Superseded
for `elitea-main` itself by the entry above: it now strips those headers and
serves no forward-auth endpoint.)
