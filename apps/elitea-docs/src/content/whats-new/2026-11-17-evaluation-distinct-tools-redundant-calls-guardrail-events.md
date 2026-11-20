---
date: 2026-11-17
order: 3
---
## Evaluation: Distinct tools, Redundant calls, Guardrail events and Step limit hit

The Trajectory card of an agent-run evaluation now shows **Distinct tools**, **Redundant calls** (an identical call repeated after it already succeeded, with its share of tool calls), **Guardrail events** (gateway guardrail verdicts that blocked, masked or warned, and sensitive-tool approval gates) and **Step limit hit** (cases that ended at the step limit), with each case's own values in its row. Runs stored before this change show **not recorded**, not 0. Upgrade Main and the web app together; no operator action is needed. See [Agents](/menus/agents).
