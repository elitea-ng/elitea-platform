---
title: "Send and continue, take back when done (#1439, ADR-0045 decision 8)"
issue: 1439
date: 2026-11-04
adr: ADR-0045
order: 68
---

New shared migration `0224_handoff_take_back_when_done`. It adds the column
`take_back_when_done` (boolean, default false) to
`elitea_runtime.workspace_handoffs`, and the link kind `handoff` to
`elitea_runtime.run_chain_links`. The migration only adds objects. An
existing handoff keeps false.

Client contract 1.33: the complete of a send (`completeWorkspaceHandoff`)
takes `take_back_when_done` and `continue` (`prompt`, `mode`, `grant`).
With `continue`, Main completes the send, writes the person's message and
starts the cloud turn in one transaction, and answers `continued`. A
refused grant or chain answers 422 `unattended_grant_refused`, 409
`chain_limit_reached` or 409 `chain_ended`, and the send stays
HANDING_OFF. A `continue` in mode `plan` answers 422 `continue_refused`
(`reason` `mode`, `field` `mode`): a cloud turn has no plan mode. A
complete without `continue` may name `stopped_chain_root_execution_id`, the
chain of the local turn the desktop stopped for the send: Main ends that
chain as `waiting` (origin `cloud`) in the same transaction, instead of the
chain sweep ending it later as `failed`. The state (`getWorkspaceHandoff`)
adds `take_back_when_done` and `last_cloud_run`; `last_cloud_run` is read
only for a handoff with `take_back_when_done` (null otherwise).

A grant that names the chain of the running desktop turn continues that
chain in the cloud. Its limits count across the desktop and the cloud, and
its `run_finished` notification names `origin` `cloud`. Main never takes a
thread back by itself: the owner's desktop takes it back. No setting and no
operator action.
