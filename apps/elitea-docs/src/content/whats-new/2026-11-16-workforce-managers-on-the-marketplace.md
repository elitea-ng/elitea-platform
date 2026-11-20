---
date: 2026-11-16
order: 4
---
## Workforce managers on the Marketplace

Agents with the `can_review_agents` capability get `marketplace_search` and `marketplace_listing` (read-only, feedback as untrusted quoted text, at most 20 items), `propose_hire` and `suggest_feedback`. A hire proposal opens the hire wizard filled in under **Settings → Agent workforce → Marketplace proposals**, and the hire settles it. With **Hire proposed agents automatically** on, a proposal is hired by the platform only for an install of a low-tier, read-only listing whose requirements can be bound, with enough reviews, a high enough rating and a low enough dismissal rate (defaults 3, 4.0 and 0.2, now settings), on probation with a 14-day undo. A workforce manager's report on a dismissed or demoted installed or forked agent is drafted for a person to edit and publish, or published when the stage is applied under the new `agents.workforce.auto_feedback` policy (off by default). A report now publishes only from the proposal the agent drafted: the report endpoint takes `proposal_id` instead of an agent. The mobile app shows the proposals and the probation badge, read-only (client contract 1.56). **Run the migrations** (shared `0253`) and upgrade Main, the worker and the clients together. See [Agent workforce](/how-tos/agents-pipelines/agent-workforce/#workforce-managers-and-the-marketplace).
