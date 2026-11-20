---
date: 2026-10-30
order: 8
---
## Throttles per agent and per user

A **Rate limit** entry under **LLM Governance** can now name agents (`scope.agent_ids`) or users (`scope.user_ids`). Each named agent or user then gets its own per-minute counter at the LLM gateway, in addition to the project rate limit. When the counter is full, the gateway refuses the call with `429` and `Retry-After`; the agent and the user are not suspended. Like the project rate limits, throttles fail open when the shared counter is not available. The toolkit reference pages now also show the category of each tool, generated from the tool category contract. See [Throttles per agent and per user](/admin/enterprise-controls/runtime-safety#throttles-per-agent-and-per-user).
