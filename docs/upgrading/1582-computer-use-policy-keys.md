---
title: "Computer use policy keys (#1582, ADR-0047 decisions 3 and 4)"
issue: 1582
date: 2026-11-05
adr: ADR-0047
order: 74
---

Client contract 1.36. The native client policy's `local_work` group gains
three keys. They are additive, and an app of an earlier release ignores
them:

- `local_work_computer_use` (default off): local turns may see and act on
  the user's other applications.
- `local_work_computer_use_apps_allow` (default empty, no ceiling): when
  not empty, the only bundle ids computer use may touch.
- `local_work_computer_use_apps_deny` (default empty): bundle ids it never
  touches. The desktop always adds its own list (Elitea, password
  managers, terminals, Screen Time, the login window and the security
  panes of System Settings). System Settings is refused when the pane it
  shows is unknown.

Nothing changes until an administrator turns `local_work_computer_use` on
under Configuration, *Native client policy*. The `computer` tools
themselves ship later (#1583). No migration. A save refuses an app list
entry that is not a bundle id (`com.apple.mail`) or a bundle id prefix
(`com.example.*`), or a list of more than 200 entries. A stored allow list
whose every entry is invalid loads as one id no application has, so it
admits nothing rather than everything.

The desktop's remembered-choices file (`choices.json` in each workspace's
data directory) gains an optional `bundle_id` field for "always for this
app" answers. Older choice files load unchanged.
