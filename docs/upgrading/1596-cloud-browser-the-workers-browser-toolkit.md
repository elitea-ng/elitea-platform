---
title: "Cloud browser: the worker's browser toolkit (#1596, ADR-0049 decisions 3, 5, 7 and 8)"
issue: 1596
date: 2026-11-06
adr: ADR-0049
order: 80
---

Nothing changes unless you turn it on. Upgrade the workers before Main:

- `worker.runtime.browserSessions` is new and empty by default. Set it to
  the supervisor's `browser` profile (`target`, `audience`, `cpu_millis`,
  `memory_bytes`, `wall_seconds`, `max_pages`) to give the worker a
  browser. The audience must be in `main.runtime.sandboxAudiences`. See
  [Cloud browser on Kubernetes](../../apps/elitea-docs/src/content/docs/deployment/cloud-browser-kubernetes.mdx).
- A project uses the browser only when its new `browser` project setting
  is enabled. Main serves the runtime-context `browser` field only to a
  worker that asks for it, so a worker of an earlier release is not
  affected.
- The new platform section `browser` sets `browser_egress` (`allowlist`,
  the default, or `open`), `max_run_seconds` (default 1800) and
  `monthly_quota_seconds` (default 0, no quota). A governed run always
  gets an allowlist. To let the browser reach any public host, set
  `browser_egress` to `open` yourself.
- If Main cannot build a run's browser settings (the allowlist is over 128
  entries, or a database read fails), the run still starts. Its browser
  tools answer `browser_allowlist_too_large` or `browser_unavailable`, and
  Main logs `browser context:` with the reason. A run is never refused for
  the browser.
- The monthly quota counts the usage ledger `gateway.llm_usage_events`,
  one row per settled call. A call that never reached a browser bills 0
  seconds. The gateway writes metered units on a request-log row only for
  the first settle of a call, not for a retry.
- A run that a person starts under an unattended grant counts as
  unattended, like a schedule or webhook run: `browser_request_user`
  answers `no_person`.
- The run cap override
  (`POST /elitea_core/run_cap_overrides/prompt_lib/{project_id}`) refuses
  the browser caps with 422 `cap_not_overridable`, not 404 `not_capped`.
  The web app does not offer the action for them.
- Migration `0226_browser_engine_seconds` adds the `second` unit to
  `gateway.metered_operation_prices` and seeds the `browser` /
  `engine_seconds` price at $0. Set your own price in the Tools pricing
  tab.
- Client contract 1.37: unattended grants take `browser_submits`, the
  hosts on which a scheduled or webhook run may submit without asking.
  `UnattendedGrantV1` gains field 9 (`browser_submits`). An earlier
  worker ignores it, so it then asks for every submit.

The admin UI has no form yet for the project setting and the platform
section. Set them through the configuration API.
