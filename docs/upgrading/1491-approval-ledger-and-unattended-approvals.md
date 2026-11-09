---
title: "Approval ledger and unattended approvals (#1491, on by default)"
issue: 1491
date: 2026-10-30
order: 56
---

ADR-0041 decision 5. Shared migration **0211** adds
`elitea_runtime.approval_ledger` (append-only), `unattended_run_policies`,
`unattended_approval_settings` and `unattended_approval_actions`, and adds
`approval_required_unattended` and `approval_timed_out` to the allowed
`execution_jobs.cancel_reason` values. No backfill: pauses that started
before the upgrade have no `pause` row, and a decision on them is recorded.

- **Flag.** The per-interrupt ledger is always on. The environment variable
  `ELITEA_RUNTIME_EXECUTION_INTERRUPTS_API_ENABLED` was already deleted and is
  not read; remove it from your values if you still set it. No chart value
  changes.
- **Evidence.** Each pause, decision, timeout and unattended outcome writes one
  ledger row and one `ai.elitea.security.v1.agent.approval` event to the
  security audit outbox, in the decision's transaction, for every run (not
  only governed runs). Expect more security audit events in the audit
  export. A decision that cannot be recorded is refused.
- **Behaviour change: unattended runs.** A run that a schedule or a webhook
  trigger started and that gets to a sensitive tool or a review node used to
  wait for a person indefinitely. It now takes the run's
  `on_approval_required`: `fail` (the default: the run ends with
  `approval_required_unattended`), `skip` or `notify_and_wait` (the owner is
  notified; the run ends with `approval_timed_out` after
  `approvals.unattended_wait_hours`, default 24). If your schedules need a
  person to approve steps, set `notify_and_wait` on them, or change the
  platform default under Configuration › Approvals
  (`configuration.governance`).
  This also applies to a static pipeline pause (`interrupt_before` or
  `interrupt_after`). A static pause has no refusal, so `skip` on it uses
  `fail`. When a `notify_and_wait` wait on a static pause expires, the run
  ends with `approval_timed_out` and the pause cannot be continued.
- **Argument digest.** The worker sends `argument_digest`, the SHA-256 of the
  arguments before masking, with each root pause and in
  `display.argument_digest` of each fan-out card (0157). The card schema
  (`fanout-interrupt-card.v1`) and `ExecutionInterruptCardDisplay` gain this
  optional field. Main no longer digests the masked arguments: a pause from an
  earlier worker records no digest.
- **API.** `PipelineSchedule`, `PipelineScheduleUpdate`,
  `PipelineInboundTrigger` and `PipelineInboundTriggerModeRequest` gain the
  optional field `on_approval_required`. No operation was added.
- **Upgrade order.** Upgrade Main first, then the workers. The chart's Argo CD
  sync waves do this: Main is wave 0, the workers are wave 1. A new worker
  puts `argument_digest` in a fan-out card, and Main of an earlier release
  refuses that card, because the card's `display` is closed. A new Main
  accepts cards from an earlier worker (no digest). If you do not use the
  chart's Argo CD application, upgrade Main and wait until it is ready before
  you upgrade the workers.
