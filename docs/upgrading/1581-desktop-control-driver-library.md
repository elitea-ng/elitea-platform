---
title: "Desktop control driver library (#1581, ADR-0047)"
issue: 1581
date: 2026-11-05
adr: ADR-0047
order: 71
---

Nothing to do. `libs/rust/desktop-control` (`elitea-desktop-control`) is a
new library. It holds the `DesktopDriver` trait and a macOS driver on a
vendored copy of agent-desktop (`libs/rust/vendor/agent-desktop/`). No
service, worker or app calls it yet, so no image, configuration or
migration changes.
