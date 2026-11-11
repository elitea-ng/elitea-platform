# Desktop: the lead answers an unaddressed message in a lead thread (#1917, ADR-0050 decision 7c)

In a desktop **Local work** thread with several agents and a lead, a message that names no agent now goes to the lead.

- **What changed.**
  - **The composer's agent is no longer an address.** The desktop host used to send every message that named no agent
    as addressed to the composer's agent (`addressed_to: [participant_id]`), so a lead thread never reached its lead.
    The host now sends `addressed_to` only for the agents the message names (**@Name**). This matches Main's
    `turntaking.Route` and the web chat (#1804).
  - **The desktop starts the message on the lead.** The desktop reads the thread's lead (`meta.floor`) and starts a
    message that names no agent on the lead, locally or in the cloud as the lead's mapping says. A cloud lead's turn
    takes none of the composer's modes (plan mode, Bypass, model choice, **Run to completion**). The answer is marked
    with the lead's name.
  - **`floor_routed_elsewhere` now means the lead changed.** The host still refuses, before any agent runs, a start
    Main routes to another agent than the one it started on. The desktop then reads the thread again and sends the
    message once more, to the agent Main routes it to.
  - **A chain the host continues stays on its agent.** An unattended chain's next turn and a resumed chain address the
    agent they continue, so **Run to completion** on a named agent in a lead thread keeps that agent.
- **Who is affected.** People using desktop threads that have a lead (`meta.floor.policy: lead`, a saved team's
  thread). Threads without a lead behave as before: the composer's agent answers an unaddressed message.
- **What to do.** Nothing. To have a particular agent answer in a lead thread, name it with **@Name**.
