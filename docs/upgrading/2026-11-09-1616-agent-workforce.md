# Agent workforce: scorecards, dismissal ladder, HR agents and appeal (#1616, ADR-0050 decisions 5b and 5g-5m)

Every agent gets a daily scorecard. A person, or a reviewer (HR) agent with
`meta.capabilities.can_review_agents: true`, can propose watch, demote,
suspend, retire or delete for an agent. A person holding the new
`agents.workforce.manage` permission (or the agent's owner) decides. The
owner can appeal to a different manager, and a retired agent can be
reinstated for 30 days.

- **Who is affected:** operators upgrading Main and the Rust worker, and
  admins who maintain custom roles.
- **What to do:**
  - **Upgrade every worker before Main.** The HR agent's proposal travels over
    the existing runtime content gRPC service as a new `ProposeDismissal` RPC.
    Until Main is upgraded, a new worker's `propose_dismissal` call answers
    `workforce_unavailable` to the model and nothing is stored; an old worker
    never offers the tool.
  - Shared migration `0236_agent_workforce.sql` adds
    `elitea_lifecycle.agent_workforce_proposals` and
    `elitea_lifecycle.agent_scorecards`, the workforce columns of the agent
    registry (watch flag, `lifecycle_state` retired/deleted, reinstate and
    purge dates, the retire snapshot), the `dismissal` suspension source, and
    the `agents.workforce.manage` permission. It runs with the other shared
    migrations; nothing to do by hand.
  - `agents.workforce.manage` is granted to the platform `admin` role and to
    every project's `admin` role. Grant it to custom roles that should decide
    workforce proposals.
- **Behaviour:**
  - A new routine, `main.agent_scorecard`, computes scorecards once a day and
    pauses during a maintenance window. Scorecards are kept 400 days.
  - A retired agent's runs are refused with 409 `agent_retired`; an agent
    that nests a retired agent is refused with 409 `nested_agent_retired`
    until someone removes the nesting. This applies to chat, the agent run
    route, the support assistant and pipeline triggers.
  - Retiring an agent turns its heartbeat off; reinstating it turns the
    heartbeat back on. A demotion with "turn the heartbeat off" stops the
    heartbeat at once and sets the draft's `proactivity.mode` to `off`.
  - A critical-tier agent and one under legal hold cannot be proposed for any
    stage; delete is refused before the 30-day retention ends and under legal
    hold.
  - Audit: one `ai.elitea.security.v1.agent.workforce.changed` event per
    proposal, decision, comment, appeal, reinstatement and refused HR-agent
    guard.
- **Docs:** "Agent workforce: scorecards, the dismissal ladder and appeals"
  (How-tos, Agents and pipelines).
