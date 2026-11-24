# Administration: role-name checks reviewed, the Users listing ranks no role (#2502, ADR-0059 R0.13)

**What changed.**

- `GET /api/v2/admin/auth_users/administration` returns `admin_roles` sorted
  by name. Before, the four legacy names (`super_admin`, `admin`, `editor`,
  `viewer`) came first, highest first. No role ranks above another now, and a
  person who holds only a narrow role (for example `llm_manager`) is listed
  with it.
- The single `admin_role` field is **still served** in this release, with the
  same value (the highest of the four legacy names, or `null`). The Users page
  still reads it; it is removed when the page shows the set of roles (R0.8c,
  #2586). No server check reads it. Read `admin_roles`.
- The checks that test the role **name** `super_admin` stay, by decision: a
  standing holder of the administration `super_admin` role passes the project
  access check of every project and is listed as a member of every project. A
  custom role with the same permissions does not, and a break-glass elevation
  will not. `admin` or `super_admin` still gets the extra public-project roles
  at sign-in. Giving or taking the `super_admin` role needs the permission
  `admin.auth.users.super_admin`, as before.

**Who is affected.** API consumers of the Users listing that relied on the
order of `admin_roles` (for example, taking its first element as the highest
role). Nothing else changes.

**What to do.** No operator action. A script that wants the highest legacy
role reads `admin_role` for this release, then derives it from `admin_roles`.
