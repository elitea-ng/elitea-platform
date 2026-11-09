---
title: "Cloud browser: the lite engine (Moli) and the engine bake-off (#1599, ADR-0049 decisions 2 and 9)"
issue: 1599
date: 2026-11-07
adr: ADR-0049
order: 95
---

No operator action. No migration, no contract change.

- The project **Cloud browser** setting still offers only `full`. `lite`
  (Moli) and `auto` (Moli, then Chrome) stay closed until the ADR-0049
  bake-off admits Moli and a later release records it. Until then a
  stored `lite` or `auto` is refused on save, and Main serves every
  session as `full`.
- New, optional chart values for when Moli is admitted:
  `sandboxKubernetes.browserSessions.profile.liteImage` (the
  `elitea-browser-session-lite` image by digest, required with
  `profile.lite` and refused without it), and
  `worker.runtime.browserSessions.lite_cpu_millis` and
  `lite_memory_bytes` (both or neither). A chart that sets `profile.lite`
  without `liteImage` now fails to render. Before, it rendered a `lite`
  ceiling that no session could use.
- `elitea-browser-session-lite` is built but not published yet.
- Upgrade the supervisor and the worker together, as for every
  `BrowserEngineV1` change. The proto gains `BROWSER_ENGINE_V1_AUTO`. An
  older supervisor refuses an `auto` spec as an unknown engine.
