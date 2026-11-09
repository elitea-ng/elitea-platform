---
title: "Desktop: computer use session panel, stop shortcut and Settings page (#1584, ADR-0047 decisions 6 and 7)"
issue: 1584
date: 2026-11-07
adr: ADR-0047
order: 91
---

No operator action. No migration, no new operation, no contract change.

- The desktop's device switch moves from editing `settings.json` to
  Settings › Computer use; the key (`"computer_use": true`) is the same,
  so a switch already on stays on.
- A session now pauses on the person's own keyboard or pointer input and
  stops on Control-Option-Escape. Without the input watch (macOS refused
  it) acts fail with `user_active_guard_unavailable`; reads still run.
- macOS keeps the Accessibility and Screen Recording grants for the
  bundle id `ai.elitea.desktop` and its signing team. A rebuilt or
  re-signed app with another id or team starts with no grants; anyone
  distributing their own build must tell people to grant them again.
