---
title: "Saved teams of agents"
issue: 1620
date: 2026-11-09
adr: ADR-0050
---

A project can now save a set of agents with its lead as a **team** and start a
conversation with all of them at once (ADR-0050 decision 8).

### Migration

- Shared migration `0232_agent_teams` creates `elitea_chat.agent_teams` and
  `elitea_chat.agent_team_members`. A team is deleted with its project
  (`ON DELETE CASCADE` to `centry.project`).
- The same migration grants the new permissions to the default-mode roles and
  to every saved project role matrix by role name:
  `models.teams.list` to admin, editor and viewer;
  `models.teams.create`, `models.teams.update` and `models.teams.delete` to
  admin and editor.

### Who is affected

- **Custom roles** get none of the new permissions. Grant
  `models.teams.*` to a custom role that should see or manage teams.
- **Projects with per-project permission rows** (a project whose roles carry
  their own permission list) are covered by the migration only for roles named
  admin, editor or viewer.

### API

New public REST routes, all under `/api/v2/elitea_core`:

- `GET`, `POST /agent_teams/prompt_lib/{project_id}`
- `GET`, `PUT`, `DELETE /agent_team/prompt_lib/{project_id}/{team_id}`
- `POST /agent_team_from_conversation/prompt_lib/{project_id}/{conversation_id}`
- `POST /agent_team_conversation/prompt_lib/{project_id}/{team_id}`

A team grants nothing: starting a conversation from one adds each agent
through the ordinary participant checks, under the caller's own access. The
client contract version is unchanged; the routes are not part of it.

Nothing else to do.
