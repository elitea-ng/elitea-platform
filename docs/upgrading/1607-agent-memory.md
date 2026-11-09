---
title: "Agent memory (#1607, ADR-0050 decision 1b)"
issue: 1607
date: 2026-11-07
adr: ADR-0050
order: 89
---

Nothing to do. Migrations tenant 0163 and shared 0229 run on start.

- Tenant 0163 adds `agent_memory_entries` (one agent's notes in one
  project: kind, content, optional subject person, pinned, status
  pending/active/archived, version and provenance), its version history
  `agent_memory_entry_versions`, and `agent_memory_subject_optouts`.
- Shared 0229 adds `models.applications.memory.list` (default admin,
  editor and viewer) and `models.applications.memory.update`, `.delete`
  and `.approve` (default admin), and adds them to every saved project
  role matrix the same way. A custom role that should manage agent memory
  needs them granted.
- A version's `meta.memory` turns it on. A NEW agent's first version gets
  `{enabled: true, write: auto_attended}`; existing agents have no block,
  so their memory is off and their behaviour is unchanged until someone
  turns it on.
- Recall at turn start adds an `<agent_memory>` block after the person's
  memories in the instructions (at most 12 entries and about 1,500 tokens
  by default). Worker tools `remember`, `recall` and `forget` are bound
  when the block is on; they reach Main over the claim-bound content
  listener (three new RuntimeContentService RPCs). A worker of this release
  on an older Main gets Unimplemented and tells the model memory is
  unavailable; an older worker on this Main binds no tools and still gets
  the recall.
- Every change writes an `ai.elitea.security.v1.agent.memory.changed`
  audit event without the note text.
- Client contract 1.43: `VersionMeta.memory` and the six web routes under
  `/api/v2/elitea_core/agent_memory/...` and
  `/api/v2/elitea_core/agent_memory_opt_out/...`.
