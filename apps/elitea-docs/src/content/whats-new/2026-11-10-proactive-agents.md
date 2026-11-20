---
date: 2026-11-10
order: 1
---
## Proactive agents

An agent can now have a **heartbeat**: the platform wakes it on a timer, a small model reads its goals and the new messages, and the agent does nothing, sends you a message in its home thread with a notification, or starts a run under your mandate that waits for you at every approval. Add a `proactivity` block to the agent, publish it, and turn the heartbeat on in the agent editor's new **Heartbeat** tab, which also shows each wake. The agent must be at least medium risk with a reviewed release, except in your personal project. Daily limits on wakes, runs and spend, quiet hours, the kill switch and **Configuration › Proactive agents** keep it in bounds. Cloud agents only. See [Proactive agents](/how-tos/agents-pipelines/agent-heartbeat/).
