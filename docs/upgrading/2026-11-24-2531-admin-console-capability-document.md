# Administration: the console capability document (#2531, ADR-0059 R3.1a)

**What changed.** Two new read-only routes:

- `GET /api/v2/admin/console` returns, for the caller, the console areas, the
  items of each area that the caller's administration roles open (each at
  `read` or `write`), the caller's administration roles and the active
  elevation (always `null` until break-glass ships). A caller with no
  administration permission gets `200` with an empty `areas` list. The
  document names roles, never permissions or people.
- `GET /api/v2/admin/console/items/{item_id}` returns one item and the roles
  that open it, by role name.

Both are open to every signed-in user and grant nothing: every administration
route keeps its own permission check. During a maintenance window an account
that holds any administration permission can still read the document. The
admin console does not read the document yet; its navigation moves to it in a
later release, and the permission list the console page injects today is
removed after that.

**What to do.** Nothing. There is no configuration, no migration and no
change to an existing route. An integration that wants to know which admin
areas an account can open may read the document instead of matching
permission names.
