# Agent workforce: automatic hiring and dismissal (#1617, ADR-0050 decisions 5c and 5k)

A project can turn on an automatic hiring policy and an automatic dismissal
policy for its agents, under Settings → Agent workforce. Both are off by
default: nothing changes for a project until a person with
`agents.workforce.manage` turns one on.

- **Who is affected:** operators upgrading Main. Nothing to do for the worker.
- **What to do:**
  - Shared migration `0237_agent_workforce_policies.sql` adds
    `elitea_lifecycle.agent_workforce_policies` (one row per project that
    saved a policy) and `elitea_lifecycle.agent_automatic_actions`, the
    probation and orphan columns of the agent registry, and a `policy` source
    and `policy_rule` column on `agent_workforce_proposals`. It runs with the
    other shared migrations; nothing to do by hand.
  - No new permission: the policy uses `agents.workforce.manage` from the
    agent workforce upgrade.
- **Behaviour:**
  - A new routine, `main.agent_workforce_policy`, runs every hour (at most
    every 5 minutes if reconfigured) and pauses during a maintenance window.
    It only reads projects whose policy has a switch on.
  - Automatic hiring creates an agent only from an open agent proposal whose
    tools are all read-only (low tier), whose spec completed the configured
    number of ephemeral runs without a failure, while the project is under its
    `max_auto_hired`. The agent is unpublished, has no tools bound and stays on
    probation until a manager keeps it; a manager can undo the hire for 14
    days.
  - Automatic dismissal applies the project's rules: watch and suspend at
    once, retire after `grace_days` with an objection window. It never demotes
    or deletes, never goes past retire, and skips critical-tier agents and
    agents under legal hold. Agents whose owners left the project are flagged
    and moved to watch after 30 days, never retired.
  - Audit: the policy's steps are `ai.elitea.security.v1.agent.workforce.changed`
    events with actions `agent.workforce.policy_changed`, `.auto_hired`,
    `.auto_hire_undone`, `.auto_hire_kept`, `.policy_applied`,
    `.policy_objected`, `.policy_held` and `.orphan_flagged`; the actor is
    `policy:<rule>` when the policy acted.
- **Docs:** "Agent workforce: scorecards, the dismissal ladder and appeals",
  section "Automatic hiring and dismissal" (How-tos, Agents and pipelines).
