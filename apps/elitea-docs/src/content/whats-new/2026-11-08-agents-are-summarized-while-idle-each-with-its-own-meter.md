---
date: 2026-11-08
---
## Agents are summarized while idle, each with its own meter

When an agent's answer ends and the conversation goes quiet, the platform summarizes that agent's history just before the provider's prompt cache expires, as a cached continuation of the agent's own last request. Each agent has its own check: a person's message or another agent's answer doesn't cancel it, and the same agent's next turn does. Each agent's row in the participants panel shows how full its own context is. The desktop app does the same for each agent of a local thread. See [Idle compaction in the cloud](/how-tos/chat-conversations/context-management#idle-compaction-in-the-cloud).
