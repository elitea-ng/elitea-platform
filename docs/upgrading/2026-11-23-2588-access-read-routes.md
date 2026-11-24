# Access read routes: holders of administrative roles and the grant history (#2588, ADR-0059 R0.10b)

**What changed.** Two new read routes. No existing behaviour changes.

- `GET /api/v2/admin/access/roles` lists every account that holds at least
  one administration role, once, with all its roles; each role says whether it
  is built-in (and its built-in marker) or custom. Accounts without an
  administration role are not listed, and the route does not page over or
  search the user directory.
- `GET /api/v2/admin/access/history` lists `elitea_auth.admin_role_grants`
  (shared 0260), newest first: the account, the role and mode, `granted` or
  `revoked`, the actor, the time and the source. It filters by `user_id` and
  `role` and pages with `limit` (1 to 200, default 50) and `offset`.

Both are gated on `access.roles.view`, or the names it narrows,
`admin.auth.users` and `modes.users`. Shared 0261 already grants
`access.roles.view` to `super_admin` and `admin`, and shared 0263 to
`security_auditor`, so no migration ships with this change. A security auditor
reads both routes and still gets `403` on the user directory
(`GET /api/v2/admin/auth_users/administration`).

`v2.yaml` describes both routes with examples
(`listAdministrationRoleHolders`, `listAdministrationRoleHistory`). The web
client is regenerated; no page calls them yet (the Access page is ADR-0059
R3.9). The mobile client does not use them and is unchanged.

**What to do.** Nothing.
