---
date: 2026-10-30
order: 7
---
## Run caps per risk tier

Every agent run now has caps on its tool calls, model turns, spend and records read, set for each risk tier under **Configuration › Run Caps**, next to the existing maximum run time. **A run now stops after 200 toolkit or MCP tool calls by default.** The other caps are off until you set them. Agent authors can lower a cap for one agent in `meta.run_caps`; a save that raises one is refused with `RUN_CAP_ABOVE_TIER`. A run that reaches a cap ends with `CAP_REACHED` and a message that names the cap. A user with the new `safety.override_cap` permission (project admins and platform administrators) can let the conversation's next run pass that cap once; each override is a security audit event. Save the per-tier caps after every worker runs this release. See [Run caps](/admin/enterprise-controls/runtime-safety#run-caps).
