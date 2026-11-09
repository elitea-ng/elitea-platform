---
title: "Composing for several agents on web and mobile (#1612, ADR-0050 decisions 7e and 7f)"
issue: 1612
date: 2026-11-08
adr: ADR-0050
order: 105
---

No operator action. No migration, no contract change: the web and mobile
clients now send `addressed_to` (added in #1611), write the conversation meta
`floor` for the lead, fold agent-to-agent messages into a side-conversation
chip, and show loop-guard lines. Roll out Main with #1611 before these
clients. A local desktop thread still holds one agent (#1806).
