# Desktop host: local `agent_state` events (#3057, ADR-0061)

**What changed.** The desktop host derives an avatar state for each local turn from its own events (decision 4b) and
sends it to the webview as a new `agent://event` kind, `agent_state` (`{participant_id, state, activity?}`), right
after the event that caused it and only when the state or activity changes. Tool and program names are sanitised
(`[A-Za-z0-9_.:-]`, at most 64 bytes); command text never leaves the host. The events are live-only: the thread
history does not keep them. The rows are pinned by the shared fixture
`libs/proto/contracts/agent-state-activity-v1.json`.

**Who is affected.** Desktop users; nothing is sent to Main.

**What to do.** No operator action.
