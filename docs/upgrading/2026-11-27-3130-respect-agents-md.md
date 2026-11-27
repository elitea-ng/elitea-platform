# Desktop: AGENTS.md status in Participants and a per-thread Respect AGENTS.md switch (#3130)

**What changed.** The desktop no longer shows an "AGENTS.md applied" notice under each message. The thread's
Participants tab lists the folder's project instructions (`AGENTS.md`, nested `AGENTS.md`, `SOUL.md`) once, with a
per-thread **Respect AGENTS.md** switch (on by default). Off, the thread's next turns, and their hand-offs, consults
and sub-agents, are assembled without the files. The desktop keeps the choice in its thread history database, which
moves to schema 7 (a new `thread_settings` table) on first start. Client contract 1.70: a committed turn's
`local_work` report gains `project_instructions` (`applied`, `files`), present when the folder held instruction
files, stored bounded (20 files, 400 bytes each) on the answer as `meta.local_work.project_instructions`. Web and
mobile show "AGENTS.md applied" or "AGENTS.md ignored" on such an answer, read-only. A local consult now also gets
the folder's project instructions after the callee's own instructions when the thread respects them.

**Who is affected.** Desktop users. Operators running Main: no configuration change.

**What to do.** Nothing. The schema upgrade runs by itself and keeps every thread. As with every history schema
upgrade, an older desktop build cannot open the upgraded history database (it reports a newer schema and runs
without history); upgrade rather than downgrade the desktop app.
