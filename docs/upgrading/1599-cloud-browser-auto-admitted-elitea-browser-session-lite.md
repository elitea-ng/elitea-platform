---
title: "Cloud browser: `auto` admitted, `elitea-browser-session-lite` published (#1599, ADR-0049 decisions 2 and 9)"
issue: 1599
date: 2026-11-08
adr: ADR-0049
order: 107
---

The engine bake-off (run 42513255705, ADR-0049 Appendix A) admitted `auto`:
Moli first, Chrome when Moli cannot serve the page. `lite` (Moli alone) is
still not admitted. No migration, no contract change.

- The project **Cloud browser** setting now offers `full` and `auto`. Saving
  `lite` is refused with `engine_not_admitted`, and a stored `lite` still
  leaves the browser unavailable for the run.
- Each release now publishes `ghcr.io/elitea-ng/elitea-browser-session-lite`
  (multi-arch, signed, scanned like `elitea-browser-session`).
- **Action:** to let projects run `auto`, make the lite image pullable from the
  cluster (mirror it with `elitea-browser-session` if you mirror images) and
  set `sandboxKubernetes.browserSessions.profile.lite` with
  `profile.liteImage` (`elitea-browser-session-lite` by digest, from the same
  release as `profile.image`). Optionally size lite sessions with
  `worker.runtime.browserSessions.lite_cpu_millis` and `lite_memory_bytes`.
  Without them, an `auto` session fails with `browser_engine_unavailable` and
  `full` is unaffected.
