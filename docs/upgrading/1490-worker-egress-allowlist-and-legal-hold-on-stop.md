---
title: "Worker egress allowlist (#1490, off by default) and legal hold on stop (#1507)"
issue: 1490
date: 2026-10-29
order: 43
---

* **elitea-main: worker public egress policy (ADR-0044 decision 2).** A new
  console section, **Configuration › Worker Egress** (`worker_egress`:
  `mode` `open` | `allowlist`, `allowlist` up to 512 entries), and a project
  configuration type `worker_egress` (`data.allowlist`, up to 256). The
  default is `open`: commands are unchanged byte for byte, and nothing is
  refused. In `allowlist` mode, and for every governed command whatever the
  mode, Main signs a `WorkerCommandV1.egress_policy` into each command, and
  the worker refuses any other host with the tool error
  `egress_host_not_allowed`. **Before switching**, open the section's hosts
  panel (or `GET /api/v2/admin/worker_egress/hosts`), copy the reached hosts
  as an allowlist and review them. Toolkit, MCP and configuration URL hosts
  are added per run automatically.
* **Upgrade the worker before you switch the mode.** A worker from an
  earlier release rejects a command that carries the new field (unknown
  tags are refused at the wire layer), so in `allowlist` mode, and for
  governed runs, its claims fail. In `open` mode the field is never sent and
  an old worker keeps working.
* **Shared migration 0197** adds `refusal_code` and `refusal_message` to
  `elitea_runtime.command_outbox` (a `NOT VALID` CHECK, so existing rows are
  not scanned). A command whose policy cannot be built (a malformed entry,
  more than 1024 entries or 24 KiB) fails at once with
  `EGRESS_POLICY_INVALID` and a message naming the entry and who fixes it,
  instead of retrying until its deadline. The migration job applies it.
* **Egress summaries are kept 30 days, not 7**, and the merger that turns
  them into one audit event per run turn now always runs, also without the
  egress proxy. Set `ELITEA_EGRESS_AUDIT_RETENTION_DAYS` (1 to 365) on
  elitea-main to change the window. Expect the
  `elitea_runtime.egress_run_summaries` table to grow with the longer window.
* **Legal hold: a stop before output keeps the question.** Stopping an
  agent's answer before it wrote anything used to delete the question and
  the empty answer. On a conversation under a legal hold both are now kept,
  and the answer is settled as stopped. Nothing to do.
