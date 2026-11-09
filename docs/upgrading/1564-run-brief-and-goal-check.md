---
title: "Run brief and goal check (#1564, ADR-0045 decision 11)"
issue: 1564
date: 2026-11-03
adr: ADR-0045
order: 66
---

New shared migration `0222_run_brief_goal_check`. It adds the columns
`brief`, `brief_state`, `brief_sha256`, `brief_claimed_at` and `goal_check_failures` to
`elitea_runtime.run_chains`, and the column `done_check` to
`elitea_runtime.run_chain_links`. It also adds the link kind
`goal_not_met`. The migration only adds objects. An existing chain gets
`brief_state` `none` and ends as before.

New approvals setting: `run_brief` (default on). Client contract 1.31:
`goal_check` on the grant of the chat send and of the local turn start
(absent is true), `kind` on a local turn continuation, and the operations
`getRunBrief` and `checkRunGoal`.

**Extra model calls.** Each chain whose grant has `goal_check` makes one
call to write the brief. Each time the chain would end as completed, and
the brief has conditions, it makes one more call to check them. Main uses
the project's low-tier default model. When the project has none, Main
uses the project's default model. Each call has a 20-second timeout. The
gateway meters each call under the chain owner's project, and the call
counts against the chain's spend limit. To stop these calls for the
platform, set **Configuration › Approvals › Run Brief and Goal Check** to
off. A chain then ends as before.

**Behaviour.** A chain can now end `failed` when a condition is impossible,
or when one condition is not met on 3 consecutive checks. A check that
finds a condition not met starts one more continuation. The continuation
counts against the chain limits. No operator action is necessary.
