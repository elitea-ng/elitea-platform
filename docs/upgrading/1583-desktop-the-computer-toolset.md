---
title: "Desktop: the `computer` toolset (#1583, ADR-0047)"
issue: 1583
date: 2026-11-06
adr: ADR-0047
order: 81
---

Nothing changes unless you turn it on. On macOS, the desktop app's Local
work threads can now see and operate the person's other applications. Two
switches must both be on:

- the client policy key `local_work_computer_use` (off by default), with
  the optional `local_work_computer_use_apps_allow` and
  `local_work_computer_use_apps_deny` lists;
- the person's device switch, `"computer_use": true` in the app's
  `settings.json` (off unless it is `true`).

With either one off, a turn has no `computer` tools, and the rules deny a
forged call, and turning either off stops a running turn's next call
(`computer_use_off`). The hard limits (locked screen, secure input and
password fields, denied chords and the Quit, Log Out, Shut Down, Restart,
Sleep, Lock Screen, Force Quit and Switch User menu items, Elitea's own
windows, input only into the named app while it is frontmost, 10 acts and
2 screenshots a second) hold in every mode, bypass included. See the how-to page "Let the
agent use your other apps (desktop)".
