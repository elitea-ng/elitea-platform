# Proactive agents: the agent heartbeat (#1618, ADR-0050 decision 2)

Agents can wake on a timer and do nothing, notify their owner, or start a run
under the owner's mandate. Main runs a new job, `agent.heartbeat.scan.v1`,
every minute on its platform scheduler.

- **Migrations:** shared `0235_agent_heartbeats` (the table
  `elitea_runtime.agent_heartbeats`) and tenant `0166_agent_heartbeat_settings`
  (the tables `agent_heartbeat_settings` and `project_proactivity_settings`).
  Both are additive. No backfill.
- **Who is affected:** nobody until an owner turns on an agent's heartbeat. No
  agent wakes before that.
- **What to do:**
  - Upgrade every worker before Main. A heartbeat run carries the new floor
    cause `heartbeat`, and an older worker refuses it.
  - To keep proactive agents off for now, set **Configuration › Proactive
    agents › Allow Proactive Agents** to off.
  - Check the platform maximums in the same section: 96 wakes, 24 runs and
    10 USD per agent per day, and 2 runs per day in a personal project.
  - A run that an agent starts needs credential governance and a delegated
    mandate of the agent's owner. Without them, the agent can only notify.
  - The triage uses the project's low-tier default model, and the gateway
    meters it under the execution id `heartbeat:<wake id>`.
- **Notifications:** the new event type `agent_heartbeat`. Clients older than
  this release show it as a plain notification.
- **Out of scope:** desktop agents have no heartbeat. Event wakes and
  `workspace_thread` act targets are not available yet.
