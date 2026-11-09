---
title: "Cloud browser: the supervisor's `browser` profile (#1595, ADR-0049 decisions 1 and 10)"
issue: 1595
date: 2026-11-05
adr: ADR-0049
order: 77
---

Nothing changes unless you turn it on. `sandboxKubernetes.browserSessions`
is new and off by default. With it off, the chart renders the same
supervisor, Services and NetworkPolicies as before.

The sandbox supervisor (`elitea-worker-rust`, `sandbox-supervisor` feature)
now accepts a profile with `"purpose": "browser"`. It serves the new
`elitea.runtime.v1.BrowserSessionService` gRPC service. A supervisor of an
earlier release refuses that profile at start (unknown purpose), so upgrade
the supervisor image before you enable the profile. The new proto adds
messages only, and no existing field changes.

`StartBrowserSessionRequestV1.unattended` (field 4) marks a schedule or
webhook run with nobody to ask (ADR-0049 decision 7). The supervisor passes
it to the session agent as `attended: false` on the `start` line, so
`browser_request_user` answers `no_person` at once. It defaults to false,
meaning a person may be asked. It is a field of the request, not of the
spec, so it is not part of the grant's digest.

To turn it on, follow
[Cloud browser on Kubernetes](../../apps/elitea-docs/src/content/docs/deployment/cloud-browser-kubernetes.mdx):

- Create a browser node pool with the gVisor RuntimeClass.
- Add four keys to the supervisor's material Secret:
  `browser-client-ca.pem`, `browser-server.crt` (SAN
  `elitea-sandbox-browser`), `browser-server.key` and
  `browser-grant-keyring.json`.
- Add `dns:elitea-sandbox-browser` to `main.runtime.sandboxAudiences`.
- Set `sandboxKubernetes.browserSessions.enabled`, `nodeSelector` and
  `profile.image`. The image must be given by digest.

The material Secret must not carry a key named `browser.json`, because the
chart reserves that name for the profile it renders.

With `engineSandbox: false` (the default), the supervisor refuses every
session unless the RuntimeClass handler is gVisor's (`runsc` or `gvisor`). On
Kata, set `engineSandbox: true`.

Every session Pod counts toward `sandboxKubernetes.maxPods`. The chart
refuses `profile.concurrency` above `maxPods`.

Main's view relay (#1597) arrives separately.
