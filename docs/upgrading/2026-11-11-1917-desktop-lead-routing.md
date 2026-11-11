# Desktop: the lead answers an unaddressed message in a lead thread (#1917, ADR-0050 decision 7c)

In a desktop **Local work** thread with several agents and a lead, a message that names no agent now goes to the lead.

- **What changed.**
  - **The composer's agent is no longer an address.** The desktop host used to send every message that named no agent
    as addressed to the composer's agent (`addressed_to: [participant_id]`), so a lead thread never reached its lead.
    The host now sends `addressed_to` only for the agents the message names (**@Name**). This matches Main's
    `turntaking.Route` and the web chat (#1804).
  - **The host follows Main's route.** When Main routes a message to another agent than the composer's (under `lead`,
    the lead), the host starts the same message again, with the same question id, as that agent and runs the turn as it.
    Before, the host refused such a start with `floor_routed_elsewhere`. The turn's `status` `running` carries
    `floor.followed: true`, and the desktop shows **<Agent> answers: the thread routed this message to it**.
  - **A followed cloud lead takes no sticky modes.** Plan mode, Bypass, a model choice and a **Run to completion**
    grant are dropped for it. A chain Main opened for the grant ends with that turn's commit.
  - `floor_routed_elsewhere` now means only that the host could not follow: Main routed the followed start elsewhere
    again, or a later agent's turn was routed to another agent. No agent runs then.
- **Who is affected.** People using desktop threads that have a lead (`meta.floor.policy: lead`, a saved team's
  thread). Threads without a lead behave as before: the composer's agent answers an unaddressed message.
- **What to do.** Nothing. To have a particular agent answer in a lead thread, name it with **@Name**.
