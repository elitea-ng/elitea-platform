---
title: "Turn-taking for conversations with several agents (#1611, ADR-0050 decisions 7c and 7d)"
issue: 1611
date: 2026-11-08
adr: ADR-0050
order: 100
---

**What changed.** Main decides who answers a message in a conversation with
several agents (the conversation meta `floor`: `addressed`, the default, or
`lead` with `lead_participant_id`). A message may name several agents
(`addressed_to` on the start body); they answer one at a time. An agent can
hand the floor to other agents with the `hand_floor` tool, served on the
worker's claim-bound content listener (`RuntimeContentService.HandFloor`).
An agent-to-agent turn binds only the agent's read-only tools until the
per-agent tool envelope (#1613) names its unattended tools. Loop guards per
human message: 6 hand-offs, 2 turns per agent, 3 hand-offs between the same
two agents.

**Migrations.** Shared `0231_agent_floor_turns.sql`: two new tables
(`agent_floor_exchanges`, `agent_floor_turns`), the chain link kind
`agent_turn` and three chain end limits (`agent_hops`, `consecutive_turns`,
`pair_exchanges`). Additive; no backfill.

**Order.** Upgrade the workers before Main: a worker from an earlier release
refuses the new input tag `floor` (73). A conversation with one agent gets no
`floor` and is unchanged.

**Who is affected.** Only conversations with two or more agents.
