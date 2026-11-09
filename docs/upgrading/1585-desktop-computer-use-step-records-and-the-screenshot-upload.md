---
title: "Desktop: computer use step records and the screenshot upload (#1585, ADR-0047 decision 9)"
issue: 1585
date: 2026-11-07
adr: ADR-0047
order: 87
---

Nothing to do unless a project wants its screenshots kept. No migration.

- Each desktop `computer` call is now an ADR-0042 step record of its
  turn, written in the turn commit's transaction: the tool, bundle id,
  window title, action, control ref, outcome, and, for typed text, its
  length and an HMAC-SHA256 under a per-turn key that never leaves the
  device (the platform cannot recover a short PIN from it). Keys outside
  that list are dropped; a record that is not an object or is too large is
  dropped with a warning, and the turn is still committed. The records are
  anchored and swept like a worker run's.
- A new project setting, configuration type `computer_use` in section
  `project_settings` (under `models.project_settings.edit`), holds
  `upload_screenshots` (off by default). Off, screenshots stay on the
  device and the commit records only that each was taken, its size and
  its digest. On, the desktop app uploads each screenshot as an attachment
  of the turn's conversation, through the chat attachment bucket, size
  limit (`ARTIFACT_ATTACHMENT_MAX_IMAGE_MB`) and retention, at most 64
  screenshots and 64 MiB per turn (429 or 413 past it).
- Client contract 1.41: `startLocalTurn` answers `computer_use`, and
  `uploadLocalTurnScreenshot`
  (`POST /api/v2/elitea_core/local_turn_screenshot/prompt_lib/{project_id}/{execution_id}`)
  is new. A desktop app of this release on an older Main reads the
  missing `computer_use` as off and uploads nothing.
