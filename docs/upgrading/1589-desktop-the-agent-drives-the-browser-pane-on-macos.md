---
title: "Desktop: the agent drives the browser pane on macOS (#1589, ADR-0048 decisions 5 and 6)"
issue: 1589
date: 2026-11-05
adr: ADR-0048
order: 75
---

Desktop app only; no server change, no new setting. On macOS, the agent of
a Local work thread whose Browser tab is open gets the `browser_*` tools
(the shared contract, `libs/rust/browser-tools`). The tools are not offered
to sub-agents and are not workspace tools.

- Plan-mode and unattended turns only read the page: navigating and
  acting are refused there (unattended: `no_person`). In other turns every
  tool call runs without an approval prompt until #1590 adds the approvals
  and the `local_work_browser*` policy keys. A deployment that must not let
  the agent use web pages should hold this release until then.
- `browser_open` follows the pane's own rules: https only unless
  `browser_allow_http` is on, never a loopback, private or link-local host.
- Pages that accept only real input are clicked with a real mouse event
  only when the app has the Accessibility permission (System Settings >
  Privacy & Security > Accessibility). Without it, the agent is told to
  ask the person. Nothing asks for that permission yet.
- The driver's script runs in a private WebKit content world; pages cannot
  see it or call it.
