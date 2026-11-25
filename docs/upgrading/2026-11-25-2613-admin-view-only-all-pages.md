# Administration: every admin page in view-only mode (#2613, ADR-0059 R3.4b)

**What changed.** The view-only mode that started on Budgets (see
`2026-11-24-2534-admin-view-only-mode.md`) now covers every admin page. A page
whose console item a role opens only to read shows the **View only** notice,
which names the roles that can change it. Every control that would change
something stays on screen but is disabled. Search, tabs, paging and detail
drawers keep working, and the page sends no change.

- **Roles**, **Users**, **Identity providers**, **SCIM**, **Projects**,
  **App requests**, **LLM governance** and the LLM Proxy tabs (providers and
  models, models and pricing, tool prices, budget alerts) are converted in this
  release. The remaining pages (agents and governance, retention, and the rest
  of Platform) follow in the next one.
- Controls that used to be **hidden** from a role without the permission are
  now shown **disabled**: New role, rename and delete on Roles, the role
  picker, suspend, delete, bulk invite and device revoke on Users, create,
  delete, suspend and members on Projects, and approve and reject on App
  requests. A role that can read a page sees what it could do there, and the
  notice says who can.
- Each tab of the LLM Proxy and of identity configuration follows its own
  console item, so a role can be view only on one tab and able to change
  another.
- The security auditor sees **Roles** in view-only mode. Until the Access item
  lands (R3.6), the console derives the Roles level from the roles that grant
  it.
- If the console cannot read the capability document, no page turns view only:
  the server answers each change, as before.

**Who is affected.** Security auditors and any custom role that reads an admin
page without the permission to change it. No one gains or loses a permission,
and no API changes. The server still refuses every change a role may not make.

**What to do.** No operator action. If your runbooks or training material say a
read-only administrator "does not see" a button, update them: the button is now
shown disabled.
