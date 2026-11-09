---
title: "Desktop: the browser pane's agent drivers on Windows and Linux (#1593, ADR-0048 decision 6)"
issue: 1593
date: 2026-11-08
adr: ADR-0048
order: 99
---

No operator action. No migration, no contract change. Desktop app only.

- The agent of a Local work thread can now use the browser pane on Windows
  (`WebView2`, through its own CDP and the cloud browser's driver) and on
  Linux (WebKitGTK, in an isolated script world). Before this release the
  browser tools answered `unsupported` there.
- Each thread's pane now keeps its site data in a folder of its own,
  `<app data>/browser/stores/<store id>`. Before this release Windows and
  Linux panes used a non-persistent store, so sign-ins did not survive a
  closed pane. Forget site data, a thread's delete or archive, and sign-out
  delete the folder; a folder still locked is deleted at the next launch.
- The pane's Back, Forward and Stop buttons are now the engine's own calls
  on Windows and Linux, so a page cannot override them.
- **Limits.** On Linux under Wayland the agent cannot post trusted input:
  a page that accepts real clicks only is handed to the person
  (`browser_request_user`). The GDK fallback works on X11 only. On Linux
  the agent cannot reach cross-origin frames. On Windows and Linux the pane
  blocks navigations to private addresses, but not yet subresource loads,
  and Windows downloads do not yet get the Mark of the Web. See
  `development/browser-driver-windows-linux` in the docs.
