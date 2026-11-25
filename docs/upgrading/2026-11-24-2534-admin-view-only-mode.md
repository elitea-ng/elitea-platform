# Administration: view-only pages, starting with Budgets, and a Roles link (#2534, ADR-0059)

**What changed.** The admin console reads each page's level from its
capability document (`GET /api/v2/admin/console`): an item is open to read or
to change. A page in view-only mode shows a **View only** notice that names the
roles that can change it, from `GET /api/v2/admin/console/items/{item_id}`, and
disables every control that would change something. Its changes are not sent.

- **Budgets** is the first page in view-only mode. A role that opens the
  Budgets item only to read it, for example a custom role with
  `models.admin.project_budgets.view` and no edit permission, now sees the
  budgets with the edit and clear controls disabled, instead of controls the
  server refused with `403`.
- The other admin pages keep their controls until a later release converts
  them. The server still refuses a change a role may not make.
- If the console cannot read the capability document, no page turns view
  only: the server answers each change, as before.
- With the six labelled areas on (`admin_console_areas`), **Roles** is listed
  again, under **People and access**, for a caller whose roles open it: the
  administrator roles, the security auditor (through `access.roles.view`) and
  any custom role that holds a role-management permission. Until now Roles
  was reachable only by its address, `/admin/app/roles`. The link is
  temporary; the **Access** item replaces it in a later release.

**Who is affected.** Administrators with a custom role that can read budgets
but not change them, and security auditors, who now see the Roles link. No one
gains or loses a permission, and no API changes.

**What to do.** No operator action.
