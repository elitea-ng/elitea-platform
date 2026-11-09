---
title: "A direct client is keyed by its socket address; compose trusts no proxy"
issue: 1366
date: 2026-10-21
order: 16
---

**Affects:** every install with a Form authentication document
(`trusted_proxy_cidrs`), and compose stacks built from
`deploy/runtime/auth.form.yml` (#1359).

**What changed.**

* elitea-main is now the compose browser edge, with no proxy in front. The
  compose document used to trust all of RFC1918 (the compose network), so a
  browser's own `X-Forwarded-For` was believed and a client could name a
  fresh address per request to get a fresh login attempt window.
  `deploy/runtime/auth.form.yml` now trusts loopback only (`127.0.0.1/32`,
  `::1/128`), which in a compose stack means no proxy at all.
* A request from a peer outside `trusted_proxy_cidrs` is keyed by its socket
  address, and its `X-Forwarded-For` is ignored. Before, the Form login and
  logout routes answered 503 to such a peer.

**What to do.**

* Compose: nothing. Recreate the stack to pick up the new document.
* Kubernetes: `trusted_proxy_cidrs` (and `ELITEA_TRUSTED_PROXY_CIDRS`) must
  name only the ranges your Gateway or ingress controller forwards from. The
  example overlays (`values-auth-minimal.yaml`, `values-standalone.yaml`,
  `deploy/kind/values-kind.yaml`) name the whole RFC1918 space as a pod
  network placeholder; narrow it to your pod CIDR. Never include a range a
  browser reaches elitea-main from directly (a NodePort or LoadBalancer
  Service). The chart sets no default.
