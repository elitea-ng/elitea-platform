# Agent memory marks: tag per conversation and rolling deploy (issue 1900, ADR-0050)

The notes an agent recalls are shown as `[memory N·TAG, ...]`, and a `spawn_agent`
brief cites one as `N·TAG`. The tag is now derived per conversation from a key
each elitea-main instance draws at start (it was drawn per turn), so the notes
block keeps its text from turn to turn and provider prompt caching matches.

Who is affected: operators rolling elitea-main and the Rust worker at different
times.

What to do: roll the worker first. A new worker accepts a bare `N` from a run
whose elitea-main sent no tag (an older elitea-main). An older worker does not
know the tag field and refuses a brief that cites `N·TAG` with
`BRIEF_MEMORY_NOT_RECALLED` until it is upgraded; nothing else is affected.
A conversation changes tag (one prompt-cache miss) when its next turn is served
by another elitea-main instance or after a restart.
