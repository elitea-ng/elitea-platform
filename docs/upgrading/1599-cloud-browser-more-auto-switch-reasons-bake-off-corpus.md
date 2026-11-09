---
title: "Cloud browser: more `auto` switch reasons, bake-off corpus revision 3 (#1599)"
issue: 1599
date: 2026-11-08
order: 98
---

No operator action. No migration, no contract change for Main or the
supervisor.

- `auto` is still closed to projects until the ADR-0049 bake-off admits
  Moli. When it opens, an `auto` session now also moves to Chrome when
  Moli dies or drops its connection during a call (`engine_lost`), does
  not answer a command or finish a `browser_open` in time (`timeout`),
  or fails a call with an internal engine error (`engine_error`). The
  `empty_snapshot` rule now also applies to a snapshot with no ref on a
  page whose DOM has links, buttons or fields, after a second read on
  Moli.
- A `browser_wait` that hits one of these is no longer run again on
  Chrome: the result comes back with `engine_switched.repeated: false`.
  A switch keeps within the agent line protocol's 90 s call limit.
- The `engine_switched.reason` field of a tool result can carry the
  three new values. The `engine_switched` notice to viewers is
  unchanged.
- The bake-off corpus (`libs/rust/browser-session/bakeoff/corpus.json`)
  is at revision 3: 11 tasks whose sites had moved were fixed or replaced
  in the same class and kind. Bake-off numbers from revisions 1 and 2 do not
  compare task for task with later runs.
