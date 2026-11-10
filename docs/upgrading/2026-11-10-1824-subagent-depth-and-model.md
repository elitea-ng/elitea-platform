# Agents: sub-agent depth and sub-agent model (#1824, #1825, ADR-0050 decision 4)

Sub-agents can now nest one level more, and a sub-agent can run on another model of the project.

- **What changed.**
  - **New setting `subagents_max_depth`.** It is in **Configuration › Agent tools** (the `agents` section), with the
    permission `configuration.governance`. The value is 1 (the default) or 2.
    - 1: the agent's sub-agents cannot start sub-agents. This is the behaviour before this change.
    - 2: each sub-agent may start sub-agents of its own, and those cannot start more. All of them share the run's
      sub-agent limits (at most 12 started in a turn, at most 4 running for one agent) and the run's delegation
      depth. Each one gets only tools its parent has; tools that write still need the agent's sub-agent tools.
    - Admission reads the setting and freezes it into each turn. A change does not reach a turn that already started.
  - **`profile.model` can name a model.** A sub-agent may run on another model of the project catalogue that the
    agent may use. Main freezes these models (with their limits) into the turn when the agent has sub-agents turned
    on. Any other name is refused at spawn with `SUBAGENT_MODEL_NOT_ALLOWED`. `"inherit"` (the default) is the
    parent's model. The desktop app keeps `"inherit"` only.
  - **New input field.** `AgentExecutionInputV1.subagent_policy` (field 77) carries the depth and the models.
  - **Proposals.** A proposed agent's profile digest now includes the sub-agent's model.
- **Who is affected.** Every deployment that runs the Rust worker. A worker from an earlier release refuses an input
  that has field 77.
- **What to do.** Upgrade the workers before Main. Then set **Sub-Agent Depth** to 2 if you want nested sub-agents.
  No migration is necessary.
