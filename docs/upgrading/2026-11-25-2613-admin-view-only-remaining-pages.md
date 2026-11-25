# Administration: the remaining admin pages in view-only mode (#2613, ADR-0059 R3.4b)

**What changed.** The view-only mode of `2026-11-25-2613-admin-view-only-all-pages.md`
now covers the rest of the console, so every admin page has it. At the read
level of a page's console item, the page shows the **View only** notice and
disables, never hides, every control that would change something.

- **Agents and governance:** Connector requests (approve, reject, inventory
  requests), Delegations awaiting review (scan), Toolkits (decide, apply to
  listed, project exceptions), Credential inventory (revoke), Permission
  trimming (remove) and Kill switch (suspend, resume). Showing resumed
  suspensions and listing a toolkit's project exceptions are reads and stay on.
- **Security:** Data retention (policy fields, save, project overrides, place
  and release legal holds). The tabs and the active-only filter stay on.
- **Platform:** Configuration and Features (the section form and Save),
  E-mail (fields, save, password removal, test message), Branding (fields,
  uploads, save, reset, import, restore), Schedules and tasks (the schedule
  switch and edit, Stop), Service Descriptors (activate, deactivate) and
  Secrets (create, edit, delete).
- A dedicated editor shown inside Configuration follows its own console item
  wherever it renders: E-mail follows **E-mail**, MCP servers follow
  **Toolkits**, and native clients and Microsoft apps follow
  **Client applications**. A role can therefore be view only on one of them
  and able to change the rest of Configuration, or the other way round.
- On Secrets, **Create secret**, **Edit** and **Delete** used to be hidden from
  a role without that permission. They are now shown disabled. The value
  reveal is a read and keeps working for a role that holds
  `configuration.secrets.secret.view`. The Internal tab stays read-only for
  everyone, as before.
- Downloading the branding package is a read and stays on at the read level.

**Who is affected.** Any role that reads one of these pages without the
permission to change it. No one gains or loses a permission, and no API
changes. The server still refuses every change a role may not make.

**What to do.** No operator action. If your runbooks say a read-only
administrator "does not see" Create secret, Edit or Delete on Secrets, update
them: the buttons are now shown disabled.
