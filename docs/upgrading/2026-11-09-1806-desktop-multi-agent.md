# Desktop: several agents in a local thread, local and cloud (#1806, ADR-0050 decision 7f)

- **What changed.**
  - A desktop **Local work** thread can hold several agents. A message names the ones it is for (`@Name`); Main
    routes it by the conversation's floor at `startLocalTurn` (`addressed_to`), answers the order (`floor`), and the
    desktop starts each later agent itself as a `floor_turn` of the same message, one at a time. Main admits a later
    turn only for the agent at that position of a message it routed, and only while no newer message has started
    (`409 floor_turn_refused` / `floor_turn_superseded`).
  - An agent participant whose mapping says `entity_settings.executor: "cloud"` is a **cloud agent**: the desktop
    relays the conversation's text to it through `startLocalChildTurn` on the turn's own agent version, never the
    workspace, and Main stamps its answer `agent_executor: cloud` (its `executed_by` stays `desktop`). Its turn's remote toolkit calls are refused (403).
  - Client contract **1.47** (`x-elitea-client-contract`): `LocalTurnStart.addressed_to`, `floor_turn`;
    `LocalTurnStarted.floor`, `executor`.
  - Shared migration **0238** (`0238_local_turn_floor.sql`) adds `floor_question_id`, `floor_position`,
    `floor_targets` and `executor` to `elitea_runtime.local_turn_executions`. No backfill: existing rows read as
    single-agent local turns.
- **Who is affected.** Deployments whose people use the desktop app. A thread with one agent behaves as before.
- **What to do.**
  - **Run the migrations**, as for every release, before the new Main serves
    traffic.
  - **Upgrade Main before the desktop app.** A desktop app of this release sends `addressed_to` and `floor_turn`;
    an older Main ignores them, so only the first addressed agent answers a message. An older desktop app keeps
    working against the new Main: it never sends the fields.
