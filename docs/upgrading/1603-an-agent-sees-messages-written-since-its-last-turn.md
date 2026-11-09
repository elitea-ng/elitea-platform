---
title: "An agent sees messages written since its last turn (#1603, ADR-0050 decision 9c)"
issue: 1603
date: 2026-11-06
adr: ADR-0050
order: 78
---

Upgrade the workers (`elitea-worker-rust`) before Main. Main now sends
`AgentExecutionInputV1.chat_history_refs` (field 71): one entry per
`chat_history` message, with its message id and whether it is the
reader's own. A worker from an earlier release refuses the unknown field,
so the turn fails.

Before this change, the worker seeded an agent's durable session from the
history once, at its first turn. After that it never saw messages that
other agents or people wrote between its turns. Now each turn appends to
the session, once each, the messages it has not seen.

There is nothing else to do. A session created before this release has no
message ids. For such a session, the first turn appends the messages
written after the agent's last answer, then marks the session, and later
turns match by id.
