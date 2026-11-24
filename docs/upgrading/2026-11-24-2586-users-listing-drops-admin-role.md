# Administration: the Users listing drops `admin_role`, the Users page assigns several roles (#2586, ADR-0059 R0.8c)

**What changed.** `GET /api/v2/admin/auth_users/administration` no longer
returns the single `admin_role` field (the highest of `super_admin`, `admin`,
`editor` and `viewer`). Each row keeps `admin_roles`, every administration role
of the person sorted by name, and `is_admin`, which is true when that list is
not empty. The Users page now shows and edits the whole set of roles with one
picker per person. This completes the change announced in
[Several administrative roles per person](2026-11-23-2574-several-administrative-roles-per-person.md)
and [Role-name checks](2026-11-24-2502-role-name-checks.md).

**What to do.** A script or integration that reads `admin_role` must read
`admin_roles` instead. For the old value, take the first of `super_admin`,
`admin`, `editor` and `viewer` that the list contains. There is no schema
change and nothing to migrate.
