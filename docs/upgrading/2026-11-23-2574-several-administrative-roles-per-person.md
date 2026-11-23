# Several administrative roles per person (#2574, ADR-0059 R0.8b)

**What changed.** A person can hold several administration roles, and both
assignment routes accept any role defined in the `administration` mode, not
only `super_admin`, `admin`, `editor` and `viewer`.

**`POST /api/v2/admin/auth_users/administration`, `action: set_admin_role`.**

- New set form: `role_names`, the roles the person holds after the request
  (`[]` for none). The server applies the difference between the person's
  current roles and the set: a role in both is not touched, and each role
  given or taken writes one grant history row.
- Kept: the single form `role_name` still replaces every administration role
  of the person with that one role (`null` or `""` for none). Existing
  automation keeps working unchanged.
- A request that sends both `role_name` and `role_names` is a `400`.
- A role the deployment does not define is a `400` that names it. Before, a
  name outside the four legacy names was a `400` with a fixed list; now
  every defined role is accepted. Assigning the legacy `system` role is now
  `403 role_set_locked` on this route too (it was a `400`).
- Every role rule applies to each role given or taken. If one role fails,
  the whole request is refused, nothing is written, and the `403` body names
  the role in `role`.
- The answer carries `role_names`, the person's roles after the change.
  `role_name` echoes the single form, and is `null` in the set form.

**`POST /api/v2/admin/modes/administration` adds one role.** It used to take
every other role of the person in the mode first (single role per mode); now
the other roles stay. `DELETE` still removes one role. A caller that relied on
the POST to replace a role must also DELETE the old one, or use
`set_admin_role`.

**`GET /api/v2/admin/auth_users/administration`.** Each row has a new field,
`admin_roles`: every administration role of the person, the four legacy names
first (highest first), then the others by name. The single field
`admin_role` (the highest of the four legacy names, or `null`) is kept for one
release so existing clients keep working; read `admin_roles` instead. It will
be removed by ADR-0059 R0.13 (#2502). `is_admin` is now true for any
administration role, so a person who holds only `security_auditor` has
`is_admin: true` and `admin_role: null`.

**The Users page** keeps its single-role picker. It now sends `role_names`, so
picking a legacy role swaps only that role and keeps the person's other roles,
which the picker does not show.

**Contract.** `v2.yaml` now describes `GET` and `POST /admin/auth_users/{mode}`
(`listAdminAuthUsers`, `adminAuthUsersAction`) with examples, and the role
refusal body (`AdminRoleRefusal`). The web client calls the generated
operations. The mobile client does not use these routes and is unchanged.

**What to do.** Nothing for the web console: the Users page keeps its
single-role control (the control for several roles is ADR-0059 R0.8c, #2586).
Review any script that calls `POST /admin/modes/administration` expecting it
to replace a role.
