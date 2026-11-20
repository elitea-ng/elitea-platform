---
date: 2026-10-28
order: 3
---
## Worker egress allowlist

Platform administrators can limit the public hosts that agents, toolkits and MCP servers reach from the worker. **Configuration › Worker Egress** shows the hosts that runs reached in the last 30 days and copies them as an allowlist. In `allowlist` mode, a run may reach only those entries, its project's entries and the hosts of its own toolkits; any other host is a tool error that names the host. The default stays `open`. See [Least privilege and trust](/admin/enterprise-controls/least-privilege-and-trust).

A stop of an agent's answer before it writes anything no longer deletes the question on a conversation under legal hold.
