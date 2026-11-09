---
title: "Desktop: the streamed cloud browser in the pane (#1592, ADR-0048 decision 7)"
issue: 1592
date: 2026-11-07
adr: ADR-0048
order: 94
---

No operator action. No migration, no contract change.

- The desktop app now opens the view WebSocket
  `GET /api/v2/browser_sessions/{session_id}/view` (#1597) itself, with
  its bearer access token and `X-Client-Version`, over HTTP/1.1 (an
  upgrade cannot run on HTTP/2). Whatever fronts `/api/v2` must pass
  WebSocket upgrades for native clients as it does for browsers. The
  upgrade carries no `Origin` header (a native client's), which the
  route admits.
- The desktop reads a refused upgrade's status itself, so it sends no
  diagnosing GET; a 426 on the upgrade (an app older than the
  deployment's minimum) shows the app's update notice.
- The tab shows in a chat and, for a Local work thread that runs in the
  cloud, in the thread's Browser tab in place of the local page.
