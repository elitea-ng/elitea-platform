---
date: 2026-11-15
order: 0
---
## "HR agent" is now "Workforce manager"

The agent kind that reviews other agents' scorecards and proposes dismissals, called the reviewer (HR) agent, is now a **workforce manager** everywhere: the Workforce tab badge, proposal sources and the documentation. Nothing about what it may do changed, and the `can_review_agents` capability keeps its name. **Run the migrations** (shared `0249`), which rewrites stored proposals, and upgrade Main and the web app together. See [Agent workforce](/how-tos/agents-pipelines/agent-workforce/).
