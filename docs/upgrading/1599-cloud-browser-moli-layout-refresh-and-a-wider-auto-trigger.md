---
title: "Cloud browser: Moli layout refresh and a wider auto trigger (#1599, ADR-0049)"
issue: 1599
date: 2026-11-08
adr: ADR-0049
order: 104
---

No migration or configuration change; `platformconfig.BrowserLiteAdmitted`
stays false, so `lite` and `auto` are still not offered to projects.

- On Moli (`lite`, and `auto` before a switch) the browser session agent now
  takes a one-pixel screenshot before it reads an element's box, waits for
  text, reads the text snapshot or probes the page for `auto`. Moli
  publishes layout only for a paint, so clicks after a scroll landed off the
  page and text the page added was missing. Chrome (`full`) is unchanged.
- A click whose element centre is still outside the viewport uses the
  element's own activation instead of a mouse press, on both engines.
- A whole tree snapshot with no ref while requests are still in flight waits
  up to 3 seconds for network idle and reads once more, on both engines. A
  snapshot of a booting app can take up to 3 seconds longer.
- `auto` also leaves Moli (`empty_snapshot`) when a zero-ref snapshot is over
  custom elements the page never defined.
- Bake-off corpus revision 4: `atlassian-status-history` is replaced by
  `atlassian-agile-kanban`. Compare bake-off reports only within one
  revision.
