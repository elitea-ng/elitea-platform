---
date: 2026-11-10
order: 2
---
## Automatic hiring and dismissal of agents

A project can now hire and dismiss agents automatically, under **Settings → Agent workforce**, which only agent managers (`agents.workforce.manage`) see. Both are off until a manager turns them on. Automatic hiring creates an agent only from a low-tier, read-only proposal whose spec ran successfully enough times, unpublished and with no tools bound, on probation until a manager keeps it; for 14 days a manager can undo the hire. Automatic dismissal applies the project's rules to each agent's scorecard: watch and suspend apply at once, and retire waits for a grace period during which the owner can object. The policy never demotes or deletes and leaves critical-tier agents and agents under legal hold alone. Agents whose owners left the project are flagged for a new owner and moved to watch after 30 days. See [Automatic hiring and dismissal](/how-tos/agents-pipelines/agent-workforce/#automatic-hiring-and-dismissal).
