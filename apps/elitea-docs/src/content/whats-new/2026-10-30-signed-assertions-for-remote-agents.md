---
date: 2026-10-30
order: 2
---
## Signed assertions for remote agents

An agent's calls to a remote MCP server can now carry a short-lived signed assertion of which agent, run and user called, and with which toolkits (header `Elitea-Agent-Assertion`). The receiver verifies it against this deployment's published keys. Nothing is sent by default: administrators choose `allowlist` (named servers only) or `on` under **Configuration › Remote Agents**; governed runs always send one. The platform's MCP endpoint accepts these assertions from the issuers that an administrator lists under **Configuration › Remote Agents**, refuses replays and expired assertions, and limits every agent and tool that the caller calls to the caller's toolkits. In the governed profile, an agent invoked through MCP needs an OAuth token or a verified assertion; a personal access token alone is refused. A2A is not supported. See [Remote agents](/admin/enterprise-controls/least-privilege-and-trust#remote-agents).
