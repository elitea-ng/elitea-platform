# Agent activity on `agent_state` frames (#3068, ADR-0061)

**What changed.** Client contract 1.66. The `agent_state` frame of a cloud
run's event stream may carry an `activity` object: the tool a `working` agent
runs (`tool`, a sanitised name of at most 64 characters, and `tool_kind`, its
category) and on whom a `waiting` agent waits (`waiting_on`: `person` or
`agent`). A sub-agent's frames now put the calling agent in `waiting` on an
agent instead of `working`, and a change of the activity alone (a second,
different tool) writes a new frame. The frame never carries tool arguments or
command text, and it still has no SSE id.

**Who is affected.** Clients that read the execution event stream. Clients
before 1.66 ignore the new key; a client that renders `working` for a
delegating agent shows `waiting` there now.

**What to do.** Nothing. No configuration, migration or restart order is
involved.
