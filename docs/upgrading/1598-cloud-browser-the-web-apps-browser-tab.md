---
title: "Cloud browser: the web app's Browser tab (#1598, ADR-0049 decision 6)"
issue: 1598
date: 2026-11-06
adr: ADR-0049
order: 86
---

No operator action. The web chat's side panel shows a **Browser** tab for
an answer whose run used the cloud browser, and opens the view WebSocket
`GET /api/v2/browser_sessions/{session_id}/view` from #1597. It shows a
page only where `main.runtime.browserView` is on; elsewhere the tab shows
why there is no browser. Whatever fronts `/api/v2` must pass
WebSocket upgrades through (see the entry below). When an open is refused,
the tab sends one plain GET to the same route to learn why. Main answers
it with the upgrade's own refusal, or 426 where the view would be
admitted, without taking a view slot or reaching the supervisor; the 426
is expected in access logs.
