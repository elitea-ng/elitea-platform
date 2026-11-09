---
title: "Desktop: browser pane control and step records (#1591, ADR-0048 decisions 4 and 9)"
issue: 1591
date: 2026-11-07
adr: ADR-0048
order: 88
---

No operator action. No migration, no new operation.

- Client contract 1.42: a local turn commit's tool step may carry
  `browser_step`, which Main stores as an ADR-0042 step record of toolkit
  `browser`: the tool, outcome, host, URL path (no query or fragment),
  ref with its role and name, typed text as length and HMAC digest only,
  and the screenshot's size. Other keys are dropped. An older Main
  ignores the key; the turn still commits.
- The desktop pane's control pill (Agent / You / Idle, take over, hand
  back with a note, Let the agent watch) and `browser_request_user` are
  on-device only.
