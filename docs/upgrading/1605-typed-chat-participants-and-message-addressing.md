---
title: "Typed chat participants and message addressing (#1605, ADR-0050 decision 7)"
issue: 1605
date: 2026-11-06
adr: ADR-0050
order: 82
---

No action is needed. Workers and Main can be upgraded in any order.

- Tenant migration `0162_chat_participant_kinds.sql` adds
  `chat_participants.kind`, `chat_participant_mapping.persona` and, on
  `chat_message_group`, `author_kind`, `addressed_to`, `visibility` and
  `cause`. It fills them for existing rows from `entity_name`,
  `sent_to_id`, the reply link and the version souls. The backfill turns
  the tenant's user triggers off for its own updates, so it does not
  re-deliver every message to every synced client. The migration holds an
  exclusive lock on `chat_message_group` from the column change to the end
  of the backfill, so chat sends in that project wait until it commits. On
  a project with millions of messages, run the upgrade in a quiet window.
- Two `BEFORE INSERT` triggers fill the new columns when a writer leaves
  them unset. A writer that sets `addressed_to` to one participant also
  gets `sent_to_id`, and the other way round. `sent_to_id` is deprecated
  and stays for one release.
- Shared migration `0227_agent_execution_participant.sql` adds
  `participant_id` and `addressed_to` to
  `elitea_runtime.agent_execution_jobs`. Admission writes them for chat
  runs. A run admitted before the upgrade has neither, so its frames
  carry no participant.
- Client contract 1.38. The execution event stream adds `participant_id`
  and `addressed_to` to each frame of a chat run, and a derived
  `agent_state` frame after each frame that moves the participant to a new
  state. Main adds both when it serves the stream. The stored frames, and
  the digests the result check reads, do not change. Clients of an earlier
  contract ignore the new frame type.
