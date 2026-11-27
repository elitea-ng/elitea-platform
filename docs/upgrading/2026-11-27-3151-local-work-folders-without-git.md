# Desktop: Local work in folders without git — ask before no undo, project instructions fallback (#3151)

**What changed.** Two behaviour changes in desktop Local work:

- **Ask before a change that cannot be undone.** In a folder too large to save for undo, the agent's first change
  of a turn now waits for the person to agree to go on without undo; until now the turn went on and only reported
  afterwards that it could not be undone. A "no" makes the turn change nothing. An unattended turn, or a thread in
  Full auto, has nobody to ask: it makes no change in such a folder and says why. The question is never answered by
  an approval rule, a bypass mode or a remembered grant.
- **Project instructions fallback.** A folder that is not a git repository and has no `AGENTS.md` at its root now
  uses its root `README.md`, else `README.txt`, else `NOTES.md`, as its project instructions, read under the same
  confinement and 32 KiB cap. Git repositories are unchanged: their README is never read as instructions. The
  prompt section is now headed `## Project instructions`. The Participants switch is renamed **Respect project
  instructions**; the stored per-thread setting (`thread_settings`, `thread_respect_agents_md_set`) is unchanged, so
  every thread keeps its choice, and turning it off also leaves out the fallback file. A turn's
  `local_work.project_instructions.files` names the fallback file when one applied.

**Who is affected.** Desktop users. Operators running Main: no configuration change, no contract change.

**What to do.** Nothing. To keep a non-git folder's README out of the agent's prompt, add an `AGENTS.md` at the
folder's root (it wins over the fallback), or turn **Respect project instructions** off for the thread. An unattended run
in a very large non-git folder that is meant to edit files needs the folder made smaller (or a git repository) so
it can be saved for undo.
