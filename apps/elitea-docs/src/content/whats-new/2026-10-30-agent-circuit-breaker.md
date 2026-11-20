---
date: 2026-10-30
order: 9
---
## Agent circuit breaker

A new routine checks every agent once a minute. **An agent is stopped when more than 50 % of its latest 20 runs failed, or when one tool failed 5 times in a row in one run.** Limits for spend per hour and runs per hour are also available, but they are off by default. The stop is a kill-switch suspension: new runs are refused, runs in flight are cancelled, and the agent stays stopped until a person resumes it. A platform administrator must resume a high-tier agent, and two different platform administrators must resume a critical-tier agent. The agent's owners and the project admins get a notification, the security audit log gets an `agent.breaker` event, and **Admin › Kill switch** shows the rule and the observed value. When many agents fail at the same time (more than 5), the breaker stops none of them and raises an alert, because a shared dependency is the likely cause. Set the limits per risk tier under **Configuration › Circuit Breaker**; an agent's registry record can only make them tighter. The Helm chart adds Prometheus alerts for breaker trips and active suspensions; a platform stop is critical. See [Agent circuit breaker](/admin/enterprise-controls/runtime-safety#agent-circuit-breaker).
