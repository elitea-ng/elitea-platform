---
title: "Cloud browser: sessions refused under current gVisor (#1599)"
issue: 1599
date: 2026-11-07
order: 96
---

Affects deployments that run cloud browser sessions under gVisor (runsc)
with the engine sandbox off (`engine_sandbox: false`, the production
arrangement).

- The session agent recognised gVisor only by its old `/proc/version`
  release, `4.4.0`. Current runsc releases (20261102 among them) report
  `4.19.0-gvisor`, or `5.15.0-gvisor` with RDMA, so on them every such
  session was refused at start as `isolation_refused`. It now accepts
  `4.4.0` or a `*-gvisor` release with gVisor's fixed build line.
- **Action:** if cloud browser sessions fail to start with
  `isolation_refused` on a current runsc, move to this release. A refused
  start now logs `not_gvisor kernel="…"` with the kernel string it saw.
