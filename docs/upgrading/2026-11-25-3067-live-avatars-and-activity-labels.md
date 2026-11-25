# Live avatars, activity labels and the hand-off baton (#3067, ADR-0061)

**What changed.** The web and desktop participants panels show each agent's state as a label ("Running cargo",
"Waiting for you"), built by client-core `activityLabel` from the `agent_state` frame's `activity`. Only the active
agent's avatar animates; under `prefers-reduced-motion` none does and the hand-off baton (`motion.handoff`) is not
drawn. One polite live region per thread announces changes, at most once per agent per second. A Local work thread's
title bar names the active agent.

**Who is affected.** Web and desktop users.

**What to do.** No operator action.
