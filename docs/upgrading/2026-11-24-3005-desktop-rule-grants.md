# Desktop: approval rules can grant a login, named hosts and argument conditions (#3005, #3006, #3007, #3008)

**What changed.** In the desktop app's approval rules (`rules.json`), an
`allow` rule may now carry three new fields:

- `hosts` (#3005): the host names a matching command may reach. The command
  runs with the network limited to those hosts through a per-command
  filtering proxy; the Seatbelt sandbox lets it connect to that proxy only.
- `credential` (#3006): a credential profile, `gh` for now. The app reads the
  GitHub CLI token with `gh auth token` outside the sandbox and gives it to
  that one command as `GH_TOKEN`; the token is redacted from the command's
  output. Without `hosts`, the profile's hosts are used (`api.github.com`,
  `github.com`, `uploads.github.com`).
- `conditions` (#3007): `forbid_flags`, `require_flags`, `flag_values` and
  `graphql: "query"`, which must all hold for the rule to match.

A command covered by such a rule runs without a question in Act mode
(#3008), but only when the rule is the person's own (never imported from a
repository), the organization's policy allows the network, and the command
is one plain foreground command. Forbidden and Ask rules, the policy's
`local_work_command_deny` and plan mode still win. "Always allow" never
writes these fields; an import of a repository's `.elitea/rules.json` drops
`credential` and `hosts`.

**Who is affected.** Desktop users on macOS who add such rules. On Linux and
Windows the fields load but grant nothing (the sandbox cannot enforce a host
list there): those rules behave as ordinary offline allows, and network
calls are asked as before. Existing rules files, the server, its charts and
its configuration are unchanged.

**What to do.** Nothing for operators. To stop people from using grants on
managed devices, keep the client policy's `local_work_network` off: a grant
needs it. Rule files written with the new fields are refused by an older
desktop build (unknown field), so the rules panel shows the error there and
no rule of that file applies until the app is updated.
