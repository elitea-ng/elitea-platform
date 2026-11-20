---
date: 2026-11-14
order: 1
---
## Agent evaluation: Enhance with AI and run insights

A finished evaluation run's scorecard now has **Enhance with AI**: the model reads the agent version the run scored and the cases that missed their targets, and proposes an instruction patch, rubric or target changes for dimensions, and corrected expected outputs for cases. You review them step by step and nothing is saved until the last step; an instruction patch creates a draft version that still goes through the agent's change request and release gate. The scorecard also shows the run's consumption (agent and judge tokens and cost), an efficiency strip (latency p50 and p95, timeouts, tokens and cost per case) and a comparison with the previous run on the same dataset. **Upgrade Main and the web app together.** See [Evaluation](/menus/agents#evaluation).
