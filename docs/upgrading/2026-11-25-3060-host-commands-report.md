# Local turn commit: commands that ran outside the sandbox (#3060, ADR-0061)

**What changed.** A desktop local turn's commit report (`local_work`,
`LocalTurnWorkReport`, client contract 1.67) may carry `host_commands` (how
many of the turn's commands ran outside the OS sandbox in a trusted folder)
and `trusted` (the folder was trusted when the turn ran), and each reported
command may carry `execution: host`. Main stores them on the answer's
`local_work` meta and adds `ran_outside_sandbox: true` when `host_commands` is
above 0, so web and mobile can mark such turns. As with everything in the
report, this is what the client says; the server cannot verify it.

**Compatibility.** The fields are omitted when absent, so a report from an
older desktop is stored and digested exactly as before (a replayed commit
still matches).

**What to do.** No operator action.

**On the desktop.** In a trusted folder a command runs on the host on every
platform (Windows included, where an untrusted folder still refuses a
confined command with `sandbox_unavailable`): with the person's login-shell
environment, read once per app session (variables the app sets, its egress
proxy's URL, grant logins and the app's own tokens are left out; when the
shell cannot be read in 5 s, a fallback with the passthrough list and the real
`PATH`, and the result says `env_fallback: true`), and with its output
redacted of the app's tokens and of known token formats (GitHub, GitLab,
Slack, AWS, Google OAuth, JWTs, bearer values, PEM private keys). A
command's result carries `execution: "host"`.
