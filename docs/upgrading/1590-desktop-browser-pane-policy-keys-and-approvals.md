---
title: "Desktop: browser pane policy keys and approvals (#1590, ADR-0048 decision 8)"
issue: 1590
date: 2026-11-06
adr: ADR-0048
order: 84
---

Client contract 1.39. The browser pane is now governed by the client
policy, and it is **on by default**:

- `local_work_browser` (default `true`) and `local_work_browser_agent`
  (default `true`): the pane, and the agent's browser tools. To keep the
  pane off for your organisation, set `local_work_browser` to `false`; to
  let people browse but not the agent, set `local_work_browser_agent` to
  `false`. A desktop of an earlier release keeps its own behaviour.
- `local_work_browser_allow_domains` and `local_work_browser_deny_domains`
  (empty): site lists, `example.com` or `*.example.com`, at most 200 each.
- `local_work_browser_http` and `local_work_browser_private_network`
  (`false`).
- **The `browser_allow_http` setting is gone.** The app no longer reads
  `browser_allow_http` from its `settings.json` (#1588); plain `http`
  follows `local_work_browser_http`. A device that relied on the setting
  refuses `http` pages until the policy key is on.
- The agent asks before a form submit, an upload or a download (the person
  can allow it for the site in the thread), and every time before a buy,
  pay, delete, send or sign control. An unattended desktop grant may carry
  `browser_submits` (hosts); the commit ledgers those answers with kind
  `browser_submit`.

Nothing to migrate. Upgrade Main before the desktops: an earlier Main
sends no browser keys, and a desktop of this release then keeps the pane
and its tools off (absent is off).
