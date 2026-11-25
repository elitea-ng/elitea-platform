# Administration: role definitions and comparison on Access (#2540, ADR-0059 R3.9a)

**What changed.** Two new read-only routes describe the administration roles:

- `GET /api/v2/admin/access/role-definitions` lists every role of the central
  administration mode with its status (`built_in`, `as_found`, `unassignable`
  or `custom`), whether its set is locked, its permission count, the console
  areas and items it opens at `read` or `write`, whether the caller holds it,
  and whether the caller may assign it to another person. When the caller may
  not, `refusal_code` gives the reason with the codes the assignment routes
  already return: `role_super_admin_required`, `role_set_locked` or
  `role_permission_not_held`. `self_refusal_code` is the answer for changing
  one's own roles: `role_self_assignment` for every administrator but a global
  administrator, who may.
- `GET /api/v2/admin/access/role-definitions/compare?a=<role>&b=<role>`
  compares two roles: the console items either opens, with each role's level,
  and the permissions both, only `a` or only `b` hold. A missing name answers
  `400`, an unknown role `404`.

Both routes need a permission that opens the console's Access item (the same
gate as `GET /api/v2/admin/access/roles`, which includes `access.roles.view`).
The **Access** page lists the role definitions. The console capability
document now marks the Access item available.

**Who is affected.** Nobody loses anything. Tools that build access reviews can
read the new routes instead of reconstructing the rules.

**What to do.** Nothing.
