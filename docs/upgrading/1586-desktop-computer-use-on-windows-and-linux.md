---
title: "Desktop computer use on Windows and Linux (#1586, ADR-0047 decision 10)"
issue: 1586
date: 2026-11-08
adr: ADR-0047
order: 101
---

Desktop app only; no server, migration or configuration change. The desktop
app's computer use (`computer` tools of a Local work thread) now runs on
Windows (UI Automation) and Linux (AT-SPI), not only on macOS. It stays off
until the organisation's client policy (`local_work_computer_use`) and the
person's device switch both allow it, as before.

- An organisation that allows computer use now allows it on Windows and Linux
  desktops too, once the person turns it on there. The policy has no per-OS
  switch; review `local_work_computer_use_apps_allow` / `_apps_deny` with
  the Windows and Linux application ids below in mind.
- Application ids on Windows are executable images (`notepad.exe`), on Linux
  desktop entry ids (`org.gnome.TextEditor`). Policy app lists and remembered
  "always for this app" answers name them that way.
- Each OS has its own deny list of chords, menu items and applications
  (terminals, password managers, system and security tools). A denied app
  fails with the new error code `app_denied`.
- Linux on Wayland can see apps but not act in them: there is no global
  input watch for the pause-on-your-input guard, so every act is refused with
  `user_active_guard_unavailable`. X11 sessions act normally. Wayland is
  told by the environment, logind's `Session.Type` (of the
  `XDG_SESSION_ID` session, the user's display session, or the process's
  own), or an Xwayland X server (its `XWAYLAND` extension, or RandR outputs
  named `XWAYLAND*`), so a stripped environment does not get past it. Settings shows computer
  use as read-only there, with the reason (`read_only_reason` in the
  settings reply).
- Windows judges each window, not only its owner: a packaged app's frame
  (`applicationframehost.exe`) by the app it hosts, so Windows Settings and
  Windows Security are denied; of `explorer.exe`'s windows only File
  Explorer folder windows pass (the Run box and the Control Panel are
  refused). File Explorer folder windows are read and clicked but never
  typed into: its address bar runs what is typed, so typing, `set_value`
  and an inserting chord there fail with `app_denied`.
  `rundll32.exe`, `control.exe` and `sechealthui.exe` join the
  deny list. A policy allow entry for `explorer.exe` or
  `applicationframehost.exe` no longer opens those.
- Linux denies GNOME Settings and KDE System Settings whole
  (`org.gnome.Settings`, `gnome-control-center`, `org.kde.systemsettings`,
  `systemsettings`). An app with no desktop entry is named by its
  executable (`xterm`), never by its display name: review policy app lists
  that used a display name. An interpreted app (Python, Node, a shell
  script) is named by its script (`terminator`, `secrets`), never by its
  interpreter: an id such as `python3.12` or `node` is refused
  (`invalid_args`) and cannot be remembered, and an app with no readable
  script name has no id. Policy entries naming an interpreter match nothing.
- Linux fails closed on the lock screen: with no screensaver service that
  answers (i3lock, slock, xsecurelock), every call is refused with
  `screen_unavailable`. There is no override; a desktop without one cannot
  use computer use.
- The user-active guard's watch dying mid-session (display gone, a
  Windows hook that cannot be re-armed) now refuses every act with
  `user_active_guard_unavailable` until the next session, a new
  `DesktopError` code from the driver.
- Windows cannot detect a low-level hook it removed silently (after
  `LowLevelHooksTimeout`): the watch re-arms its hooks every 30 seconds, so
  the person's input can go unseen for at most that long. A re-arm that
  fails ends the watch as above.
- On Windows and Linux, a key chord that types or pastes is refused
  (`action_failed`) when the focused control cannot be found; navigation
  keys still pass. Win+. and Win+; (the emoji and symbol panel) and Win+V
  (the clipboard history) count as typing.
- Other programs' injected input (remote-control tools, `xdotool`) does not
  pause the agent on Windows or Linux; the how-to says so.
