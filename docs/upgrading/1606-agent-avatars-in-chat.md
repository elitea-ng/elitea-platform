---
title: "Agent avatars in chat (#1606, ADR-0050 decision 6)"
issue: 1606
date: 2026-11-06
adr: ADR-0050
order: 83
---

No action is needed. This is a client change only: the web app, the desktop
app and the mobile app read the participant `kind` and `persona`, the
message `author_kind`, `addressed_to`, `visibility` and `cause`, and the
`agent_state` frame of client contract 1.38 (#1605). Upgrade Main first.
Against an earlier Main, the clients show every answer with the Elitea mark
and no motion, as before. The shared view model is the new
`@elitea/client-core/conversation` entry point.

The Rust worker's Kubernetes client moves to kube 5 and the Kubernetes 1.33
API (`k8s-openapi` `v1_33`, was `v1_32`). It is used only by the
`sandbox-supervisor` feature (workspace runs and browser sessions). Pod,
exec and attach calls are unchanged; run those features on Kubernetes 1.33
or newer.
