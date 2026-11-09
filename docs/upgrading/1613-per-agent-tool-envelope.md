---
title: "Per-agent tool envelope (#1613, ADR-0050 decision 3)"
issue: 1613
date: 2026-11-08
adr: ADR-0050
order: 109
---

**Upgrade every worker before Main.** Main now freezes an agent's
`meta.tool_envelope` and the new `agents` Configuration section into each run
input (`AgentExecutionInputV1.tool_envelope`, field 74); a worker of an
earlier release refuses an input with a field it does not know. Main sends the
field only for an agent whose version sets an envelope or once an
administrator saved the `agents` section, so before that inputs are unchanged
byte for byte.

- No migration. The `agents` section uses the existing
  `configuration.governance` permission.
- Unattended (agent-to-agent) turns still bind the read-only tools by default;
  the agent's `unattended_tools` and the platform's
  `agents.unattended_tools_allowlist` replace the #1611 read-only filter.
- A version save, import or internal MCP version update with an invalid block
  is refused with `422 tool_envelope_invalid`.
