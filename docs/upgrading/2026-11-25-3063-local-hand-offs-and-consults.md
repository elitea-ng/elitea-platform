# Local hand-offs and consults (#3063, #3064, #3065, ADR-0061)

**What changed.** Client contract 1.68. Two new desktop-host operations:
`handLocalFloor` (`POST /api/v2/elitea_core/local_turn_hand_floor/prompt_lib/{project_id}/{execution_id}`)
and `skipLocalFloorHands`
(`POST /api/v2/elitea_core/local_turn_hand_floor_skip/prompt_lib/{project_id}/{question_id}`).
`startLocalTurn`'s `floor_turn` accepts `hand_id` in place of `position` and
answers `hand_off`; `commitLocalTurn` accepts `consults`. A new shared
migration creates `elitea_runtime.local_floor_hands` (the queue of local
hand-offs) and adds `local_turn_executions.floor_stopped`. Project deletion
removes the new rows.

**Who is affected.** Operators upgrading elitea-main, and desktop builds.
A desktop build before 1.68 never hands off locally; against an older
server a 1.68 desktop's `hand_floor` call gets 404, which the model reads as
a hand-off that could not be made, and the agent answers the person itself.

**What to do.** Nothing beyond the usual migration step: the migration is
additive and runs with the others at start-up. No configuration, no new
environment variable, no restart order.
