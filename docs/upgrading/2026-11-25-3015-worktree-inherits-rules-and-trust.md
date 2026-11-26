# Desktop: task worktrees inherit their folder's rules and trust (#3015, ADR-0061)

**What changed.** A Local work task worktree (a sub-agent's
`isolation: "worktree"`, or a worktree thread) now runs with its folder's
workspace approval rules: its session loads the folder's `rules.json`, and
**Always allow** in the worktree appends to that file. A worktree also has its
folder's trust: it is trusted, paused or untrusted with the folder, it is
never trusted on its own (`trust_grant` refuses it with `trust_refused`), and
revoking the folder's trust stops the worktree's commands that run outside the
sandbox too. A sub-agent's host commands start behind the "outside content"
gate, because its task comes from another agent.

**Compatibility.** A worktree used to have its own (normally empty) workspace
rules file in the app's data; a rule in it is no longer read. Worktrees are
short-lived, so nothing is migrated.

**What to do.** Nothing.
