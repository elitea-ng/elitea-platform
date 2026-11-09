---
title: "Schedules and webhooks on a handed-off workspace (#1440, ADR-0045 decision 9)"
issue: 1440
date: 2026-11-04
adr: ADR-0045
order: 69
---

New tenant migration `0159_workspace_thread_targets`. It adds the columns
`target_kind` (default `pipeline`), `target_conversation_uuid`,
`target_prompt` and `target_grant` to `pipeline_schedules`, and the last
three and `last_execution_id` to `pipeline_triggers`. The trigger's
`target_kind` check admits `workspace_thread`, and the schedule's
`last_result` check admits `skipped_thread_busy`. The migration only adds
objects. An existing schedule or trigger keeps its kind.

Client contract 1.34: `savePipelineSchedule` and
`rotatePipelineInboundTrigger` take `target_kind: workspace_thread` with
`conversation_uuid`, `prompt` and an optional `grant` (the `continue` and
`chain` parts and `goal_check`; `sensitive_tools`, `hook_asks` and
`remote_confirmations` answer 422 `unattended_grant_refused`). The save
answers 404 for a conversation that is not in the project or not the
caller's, and 422 `not_handed_off` for one never sent to the cloud. The
reads add the same fields, and the trigger read adds `last_execution_id`.
An occurrence on a thread that is not idle in the cloud records
`skipped_thread_busy`, and a webhook call answers 409 `thread_busy`.

A new question on a handed-off thread now takes a share lock on the
thread's handoff row in the admission transaction and checks CLOUD_IDLE
again. A posted message and an occurrence cannot both start a turn. No
setting and no operator action. A deployment without the cloud executor
answers 503 to a `workspace_thread` save.
