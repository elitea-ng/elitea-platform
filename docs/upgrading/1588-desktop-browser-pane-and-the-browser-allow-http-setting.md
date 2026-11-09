---
title: "Desktop: browser pane and the `browser_allow_http` setting (#1588, ADR-0048 decisions 1 and 2)"
issue: 1588
date: 2026-11-05
adr: ADR-0048
order: 72
---

Desktop app only; no server change. Local work threads get a Browser tab
(⇧⌘B) whose page is a child webview with no IPC and its own cookies and
site data per thread (macOS 14+: a persistent store per thread; macOS
12–13: a private store, gone when the page closes).

- New optional key `browser_allow_http` (default `false`) in the app's
  `settings.json` (`~/Library/Application Support/ai.elitea.desktop/`).
  Plain `http` pages load only when it is `true`; loopback and private
  addresses stay refused. The client policy key `local_work_browser_http`
  (#1590) replaces it; until then, a managed rollout that needs `http`
  pages sets the key on each machine.
- The capability `capabilities/default.json` now grants by webview label
  (`"webviews": ["main"]`) instead of by window (`"windows": ["main"]`).
  A fork that adds its own capability file must do the same: a `windows`
  entry would grant every browser page in the main window.
- Downloads from the pane are saved to `~/Downloads/Elitea/<thread>/`
  without a prompt. On macOS each one is quarantined
  (`com.apple.quarantine`, agent `Elitea`, so Gatekeeper checks it on first
  open) and records its URL (`kMDItemWhereFroms`). Unicode format
  characters (bidi overrides, zero-width marks) are removed from the name.
- Private-network blocking: top-level pages and frames whose address is
  `localhost`, a private name or a private or loopback IP are refused. On
  macOS, subresources (`fetch`, images, scripts, WebSockets) to those
  addresses are blocked too, by a `WKContentRuleList` installed in each
  pane before its first page loads (#1634). If the rules cannot be
  installed, the pane does not open (`browser_unprotected`). Public names
  that resolve to private addresses are NOT blocked (the rules match URLs,
  not resolved addresses). Do not rely on the pane alone to protect local
  services on managed machines.
- The pane's Back, Forward and Stop are native on macOS: a page cannot
  override them (#1634).
- Signing out now also deletes every thread's browser site data.
