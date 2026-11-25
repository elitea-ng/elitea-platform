# Native client policy: `local_work_folder_trust` (#3059, ADR-0061)

**What changed.** The native client policy's **Local work** section gains
`local_work_folder_trust` (**Allow Trusted Folders (No Sandbox)**), a boolean
that defaults to **on**. It is delivered to the desktop app as
`local_work.folder_trust` (client contract 1.66). With it, a person can mark
one desktop folder trusted: that folder's commands run as the person's own
processes, outside the OS sandbox, still under approvals, the person's rules
and `local_work_command_deny`. Trust is per folder, granted by the person
through a warning dialog, recorded in a local ledger on the device, and
revocable at any time.

**When it takes effect.** Only while `local_work_max_sandbox_mode` is
`full-access` and `local_work_shell` and `local_work_network` are on. The
default ceiling is `workspace-write`, so a deployment that never raised it has
no trusted folders, whatever this key says. If the policy stops allowing trust
(this key turned off, the ceiling lowered, the shell or the network turned
off), every trusted folder on a device is suspended at its next policy refresh
(at most the access token lifetime) and resumes only when the policy allows it
again and the person confirms again.

**What to do.** Nothing, to keep trust unavailable on the default ceiling. If
you raised the ceiling to `full-access` and want a question for every command
without trusted folders, turn `local_work_folder_trust` off. See **Admin >
Features and configuration** for how the keys combine.
