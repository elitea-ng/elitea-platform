# Administration: runtime.plugins and configuration.governance get no new grants (#2503, ADR-0059 R0.14a)

**What changed.** The two broad legacy permissions `runtime.plugins` and
`configuration.governance` end on a sunset date. A later release sets and
announces the date, at least 90 days after the last narrow replacement ships.
Until then:

- `PUT /api/v2/admin/permissions/{scope}/{mode}` (the Roles matrix) refuses a
  save that adds either name to a role that does not already hold it:
  `400` with `{"code": "permission_deprecated", "permissions": [...],
  "role": ..., "roles": [...]}`. It binds every caller, a super admin
  included, and every scope and mode. Nothing is written.
- `PUT /api/v2/admin/user_project_permissions/administration` refuses the same
  way when the save adds either name to a project role that does not hold it
  in a target project.
- Existing grants keep working. A save that keeps an existing grant answers
  `200`, and removing one is allowed (the matrix cannot add it back).
- `GET /api/v2/admin/permissions/{scope}/{mode}` gains a `catalogue` array
  beside `rows`: `{"name": ..., "deprecated": true|false}` for each row. The
  two legacy names are `deprecated: true`. `rows` is unchanged.
- The Roles page marks both names **Deprecated**, disables the checkbox of a
  role that does not hold one, and explains the refusal.

**Who is affected.** Automation that grants `runtime.plugins` or
`configuration.governance` to a role through either matrix API, for example
when it sets up a new custom role.

**What to do.** Grant the narrow permission that replaces the legacy one (see
Users and roles, "Narrow administration permissions"). Roles that already hold
a legacy name keep it, and nothing has to change before the sunset date. The
agent governance Configuration sections have no narrow pair yet (R0.15): until
it ships, only a role that already holds `configuration.governance`, such as
`admin` and `super_admin`, opens them.
