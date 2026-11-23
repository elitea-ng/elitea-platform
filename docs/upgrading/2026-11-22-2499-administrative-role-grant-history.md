# Administration: a history of role grants (#2499, ADR-0059)

**What changed.** Shared migration `0260_admin_role_grants.sql` adds
`elitea_auth.admin_role_grants`, an append-only history of central role
assignments: the account, the role name and mode, `granted` or `revoked`, the
actor, the time (`timestamptz`), an optional reason, the source (`console`,
`api`, `scim`, `migration`, `bootstrap`, `operator` or `review`) and a source
reference. A trigger refuses UPDATE, DELETE and TRUNCATE on it. The admin Users
route (`set_admin_role`), the `/admin/modes` routes and the first-login grant of
`identity.initial_global_admins` write one row per change, in the same
transaction as the change. The two admin routes no longer delete and re-insert
the assignment rows: a role the account already holds keeps its row.

The same migration adds an index on `auth_core__project_user_role (user_id)`,
which the permission resolver reads by user. It is a plain `CREATE INDEX` inside
the migration's transaction; the table holds one row per project membership role
and is small on every known deployment. On a database without the `auth_core`
tables the index is skipped.

**Who is affected.** Every deployment. Nothing is removed and no column is added
to `auth_core__user_role`.

**What to do.** Nothing. History starts at this upgrade: assignments made before
it have no row. If you back up or replicate `elitea_auth`, include the new table.
A tool that removed rows from `auth_core__user_role` by hand still works, but
leaves no history; use the admin routes instead.
