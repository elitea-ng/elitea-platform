---
title: "Grounding indicators (#1500, ADR-0043 decision 5)"
issue: 1500
date: 2026-11-02
adr: ADR-0043
order: 64
---

A new agent version meta key, `meta.citations` (`"off"`, `"required"` or
null). Main refuses any other value with 400 on the version saves, and
refuses the agent on an import or a fork; the internal MCP version tools
answer 400. The worker turns citations off, with the warning
`agent_citations_setting_invalid`, for a stored value that it cannot read.
With `"required"`, the worker numbers each tool result of the current turn
(after the last user message) in the model request (the envelope source
becomes `N:TOOL`; results of earlier turns keep their name), adds a citation
rule after the agent's instructions, and sends a
grounding indicator as `response_metadata.grounding` on the final
`full_message` event. Main stores it as the message group's
`meta.grounding` and records no score. No migration and no proto change.

**Upgrade every worker before you turn on citations.** A worker of an
earlier release ignores the key: the agent runs without the citation rule
and the answer has no indicator. An earlier Main keeps the key in the
version meta and drops the indicator.
