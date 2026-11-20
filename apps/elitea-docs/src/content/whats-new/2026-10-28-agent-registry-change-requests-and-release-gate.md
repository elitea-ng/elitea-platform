---
date: 2026-10-28
order: 6
---
## Agent registry, change requests and release gate

Every agent now has a registry record with its owners, risk tier and certification state. Above the low tier, publishing needs a change request approved by someone other than the author, after the agent's evaluation gate passes. **Agents that existed before this release start at tier `medium`, so publishing them now needs one approval**; agents in personal projects and new agents start at `low` and publish as before. Certifications come due on a cadence and lapse if they are not renewed. Reviews done outside the platform are recorded as attestations. See [Agent lifecycle](/admin/enterprise-controls/agent-lifecycle).
