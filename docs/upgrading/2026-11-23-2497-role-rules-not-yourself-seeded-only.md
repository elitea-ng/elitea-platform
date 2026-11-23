# Administration: not yourself, seeded-only permissions and locked sets (#2497, ADR-0059)

**What changed.** The no-escalation rules of the roles API gain four rules
(ADR-0059 decision 3). No migration ships with this change.

- **Nobody but a standing global administrator changes their own
  administration roles.** Giving yourself a central role, or taking one away
  from yourself, is refused with `403` and the code `role_self_assignment`, on
  the Users page (`POST /admin/auth_users/administration`, `set_admin_role`)
  and on `POST` and `DELETE /admin/modes/administration`. A holder of
  `admin.auth.users.super_admin` through a standing role is exempt. Another
  administrator makes the change; the first one still comes from
  `identity.initial_global_admins`.
- **A custom role hands out only what it holds.** A custom role that holds
  `admin.auth.users` or `modes.users` assigns or removes only a role whose
  permissions its holder holds (the rule of #2488, unchanged), and now never
  the holder's own roles.
- **Five more permissions cannot be added to a role through the API.**
  Besides `admin.auth.users.super_admin`, the permissions
  `identity.sso.trust.manage`, `access.breakglass.request`,
  `access.breakglass.approve`, `access.breakglass.manage` and
  `access.breakglass.review` are granted by migrations only. A Roles matrix
  save, a project matrix save or a Standard Roles template save that adds one
  is refused with `403` and the code `role_permission_reserved`, for a global
  administrator too. Grants that migrations already wrote stay.
- **The edit of a built-in set answers `403` `role_set_locked`.** In the
  previous release it answered `409`. Rename and delete of a built-in role
  still answer `409`. The refusal is in the Audit Trail with the action
  `role.escalation_refused`.
- **The legacy `system` role is assigned to nobody.** On a deployment upgraded
  from the legacy platform, `POST /admin/modes/administration` with the role
  `system` answers `403` `role_set_locked` for every caller (the Users page has
  always refused it, with `400`).
- **`break_glass` is a reserved role name.** Creating a role with that name, or
  renaming a role to it, answers `400`.

**What to do.**

- A script or automation that changes the roles of the account it signs in as
  must use another administrator account, or a global administrator.
- A client that handled the `409` of a built-in set edit should read the
  `403` and its `code` instead. The web Roles page shows a message for
  `role_self_assignment` and `role_set_locked`.
- If a custom role should hold one of the five permissions, it cannot: give
  the person the built-in role that holds it.
