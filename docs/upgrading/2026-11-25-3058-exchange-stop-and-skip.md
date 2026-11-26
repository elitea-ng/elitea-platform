# Desktop host: Stop exchange and Skip (#3058, ADR-0061)

**What changed.** Two new desktop IPC commands, `agent_exchange_stop {turn_id}` and
`agent_exchange_skip {turn_id, participant_id} -> {remaining}`, stop a message's remaining agents or skip one of them.
The host keeps the routed order: `status` `running`'s `floor` gains `exchange` (the participant ids still to answer),
a new `exchange` event (`{remaining}`) follows a skip, and a stop ends with `floor_stopped` code `stopped_by_person`.
Both commands are in the desktop capability allowlist. Main needs no change: it already admits a later floor position
whose target matches.

**Who is affected.** Desktop users.

**What to do.** No operator action.
