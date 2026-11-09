---
title: "Agent memory tab and chip (#1608, ADR-0050 decision 1b)"
issue: 1608
date: 2026-11-07
adr: ADR-0050
order: 90
---

No operator action. No migration.

- The agent editor has a Memory tab (web and desktop) and a chat answer
  shows a "remembered" line for each note its agent saved, with Approve,
  Edit and Forget.
- `GET /api/v2/elitea_core/agent_memory/prompt_lib/{project_id}/{application_id}`
  takes three more optional filters (`source`, `created_after`,
  `created_before`) and answers a new `people` array (`{id, name}` of the
  people its rows name). A client that does not read it is unaffected.
