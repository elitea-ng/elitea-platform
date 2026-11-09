---
title: "Unattended grants and chains (#1435, #1436, ADR-0045)"
issue: 1435
date: 2026-11-03
adr: ADR-0045
order: 65
---

New shared migration `0221_unattended_run_chains`: the tables
`elitea_runtime.run_chains` and `elitea_runtime.run_chain_links`, and the
column `approval_ledger.grant_id`. The migration only adds objects. New
approvals settings: `unattended_grants` (default on),
`unattended_withheld_categories` (default `payment`) and the `chain_*`
limits. Client contract 1.30: `unattended` on the local turn start and on
the chat send, `source` on a HITL exchange, `chain_outcome` on the commit,
and `source: grant` on a remote toolkit confirmation. New proto field
`AgentExecutionInputV1.unattended_grant` (68) and terminal state
`STEP_LIMIT_REACHED`.

**"Run to completion" is on by default (ADR-0045 decision 10).** The web
and desktop clients send the default grant (continue at the caps, inside
the chain limits) with every request. Main sends field 68 on each cloud
turn that carries a grant, so almost every cloud turn has it. A worker of
an earlier release refuses such a turn. A turn without a grant is
unchanged byte for byte.

**Upgrade the workers before Main**, or set **Configuration › Approvals ›
Unattended Grants** to off for the rollout and on again when every worker
is upgraded. With the switch off, a request with only the default grant
runs with no grant (it is not refused). A request with an opt-in (hook
asks, remote confirmations or named sensitive tools) is refused with `422
unattended_grant_refused`, reason `disabled`.

**Behaviour: a grant answers only what it names.** On the desktop, a grant
does not imply bypass mode. A local rule's ask, and a hook's ask or a
remote toolkit confirmation that the grant does not cover, go to the
person as in a turn without a grant. Past the question's wait (30 minutes
for a rule's or a hook's ask, 15 minutes for a remote confirmation) the
default declines it, and the ledger records `timed_out`. Bypass mode stays
a separate opt-in (`bypass_approvals`). No operator action is necessary.
