# Desktop: approvals in a trusted folder and the outside-content gate (#3061, ADR-0061)

**What changed.** In a folder the person trusted, the desktop decides a
command that runs outside the sandbox with these layers, in order: the
organisation's policy (`local_work_command_deny` and the rest), plan mode and
hooks, then every **Forbidden** and **Ask** rule (a repository's included);
then an **Allow** rule the person wrote, which runs it without a question;
then an **Always allow** made in a trusted folder (`sandbox: host`,
`requires_trust: true`); then the defaults, which allow only a routine command
that starts no known network-capable program (best effort: a build script can still reach the network). A repository's rule never allows a
command outside the sandbox, and a rule with a login grant keeps its sandbox.

After a turn reads content from outside the folder (a remote toolkit, an MCP
server, the web, the browser pane, another app, or the answers before a later
agent's turn), every host command that no rule of the person's allows is
asked with `source: "taint"`. **Bypass mode and the unattended grant do not
answer that question**; only the person does.

With **Ask for every command** on, every host command is asked, even one a
rule or an earlier **Always allow** covers; deny rules still deny.

Rule files gain two fields: `sandbox: "host"` on an allow rule, and
`requires_trust: true`. Both are refused with `credential`, `hosts`,
`network`, a non-allow decision or `origin: repository`. An older desktop refuses a rules file that uses them
(unknown fields), so a file edited by this release loads no rules on an older
one until the fields are removed.

**What to do.** Nothing. A folder is never trusted unless the policy allows it
(`local_work_folder_trust`, #3059) and the person grants it.
