---
date: 2026-11-14
order: 11
---
## Agent Evaluation: run cases as agent runs, with steps, time and tool output controls

An evaluation case can now run as a real agent run on the worker instead of one model call. Choose **Agent run** in **How a case runs**, in **Run limits** or in the start form: each case runs the pinned agent version with its tools through the same path as a chat, and the judges score its final answer. Saved limits gain a **Steps per case** limit, a **Time per case** limit (the agent run is cancelled and the case stores a timeout) and **Store tool outputs in the results**, shown only for agent runs. A finished agent run's scorecard adds a **Trajectory** card (steps per case p50 and p95, tool calls, errors, retries, and each case's steps), and the **Efficiency** card adds the tool metrics. One model call per case stays the default, and nothing changes unless you choose the mode. **Run the tenant migration `0246` and upgrade Main and the web app together.** See [Evaluation](/menus/agents#agent-runs).
