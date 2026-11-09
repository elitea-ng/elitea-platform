---
title: "Cloud browser: the live view (#1597, ADR-0049 decision 6)"
issue: 1597
date: 2026-11-06
adr: ADR-0049
order: 85
---

Nothing changes unless you turn it on. Upgrade Main before the workers:

- Migration 0228 adds `elitea_runtime.browser_sessions`, one row per
  cloud browser session (the run, its owner and conversation, the
  supervisor audience, the spec, the state, its start), with indexes for
  the retention pass. It holds no data from before the upgrade and grants
  nothing. A live row is watchable only within its spec's wall time plus
  the 10-minute linger; ended rows, and open rows past that bound, are
  deleted a day later by the replay-progress retention pass.
- The new control RPC `ReportBrowserSession`: the worker reports each
  session it starts and ends, under its live claim, and ends the session
  before it gives the claim up. A Main without the RPC, or without the
  view configured, answers `UNSUPPORTED_CAPABILITY`; the worker logs
  `browser_session_reports_unsupported` once at debug and stops
  reporting, and the browser tools go on working. So a worker of this
  release on an older Main loses only the view.
- The supervisor now sends a `session_live` or `session_lingering`
  notice when it admits a view. Upgrade the sandbox supervisor with Main;
  an older supervisor still works, its first frame standing in for the
  admission.
- `main.runtime.browserView` (new, off by default) turns the relay on. It
  renders `ELITEA_RUNTIME_BROWSER_VIEW_CONFIG` (now refused in
  `main.env`) and adds `browser-view-client.crt` and `.key` to Main's
  runtime material Secret: issue Main a client certificate from the
  runtime CA with its workload identity. Every supervisor `audience` must
  be in `main.runtime.sandboxAudiences`. Main refuses to start with a bad
  value. See
  [Cloud browser on Kubernetes](../../apps/elitea-docs/src/content/docs/deployment/cloud-browser-kubernetes.mdx),
  "Step 4: the live view".
- Client contract 1.40 adds `viewBrowserSession`
  (`GET /api/v2/browser_sessions/{session_id}/view`, a WebSocket). Every
  refusal is an HTTP status before the upgrade (404, 410, 429, 502, 503).
  Access is checked again every 30 seconds: a lost watcher is closed with
  4403, a lost driver gets a `control_revoked` notice. Driving and taking
  over are audited. The web app's viewer comes later (#1598); nothing in
  the web app changes yet.
- A WebSocket must reach Main through your ingress: allow the `Upgrade`
  header on `/api/v2/browser_sessions/`, with an idle timeout above 30
  seconds (Main pings an idle view every 30 seconds). Browser pages must
  come from the deployment's public origin; the desktop app's origins are
  admitted.
