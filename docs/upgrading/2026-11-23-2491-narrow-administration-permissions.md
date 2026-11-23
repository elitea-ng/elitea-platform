# Administration: narrow permissions for both administrator roles (#2491, ADR-0059)

**What changed.** Shared migration `0261_narrow_admin_permissions.sql` adds 25
narrow administration permissions (the `llm.*`, `platform.*`, `identity.sso.*`,
`mcp.catalogue.manage`, `projects.members.manage`, `projects.settings.*`,
`access.roles.view` and `access.breakglass.*` names) and grants them to the two
existing administrator roles of the `administration` mode. `super_admin` gets
all 25. `admin` gets 24: `identity.sso.trust.manage` (changing whom an identity
provider vouches for) goes to `super_admin` alone. The migration deletes and
updates nothing, gives nothing to `system`, `editor`, `viewer` or a custom role,
and writes nothing when it runs a second time. On a database without the
`auth_core` tables it does nothing.

No route accepts the new names yet. Later releases let each route accept its
narrow name beside the legacy one it narrows (`runtime.plugins`,
`configuration.governance`, `admin.auth.users` and others), so an administrator
who holds the legacy name sees no change. The Roles matrix lists the new names.
`identity.sso.trust.manage` and the four `access.breakglass.*` names are
seeded-only: only a migration is meant to grant them, and a later release makes
the Roles matrix refuse them on any other role.

**New capabilities, not narrowings.** Most of the new names split a power the
administrator roles already have. Six do not.

Both administrator roles gain `projects.settings.view`
and `projects.settings.manage`. This is a privilege increase: once
the project settings gate accepts them (ADR-0059 story R0.6b, #2585), an
administrator can read and change the guarded settings (cloud executor, worker
egress, AI disclosure, browser, computer use) of a project they are not a member
of. Each such write will be marked as a central override in the Audit Trail.

`identity.sso.trust.manage` (to `super_admin` only) names a power that
`super_admin` already has: the identity trust actions require
`admin.auth.users.super_admin` today. The four `access.breakglass.*` names (to
both roles) belong to break-glass elevation, which does not exist yet. They allow
nothing until it ships. When it does, every `admin` can request, approve, end and
review elevations and read the sealed credential list. Remove those grants from
`admin` before then if that role should not have these powers.

**Who is affected.** Every deployment with the `auth_core` tables. No existing
grant is removed and no session loses a permission.

**What to do.** Nothing. If `admin` should not reach the settings of every
project, a global administrator (`super_admin`) can remove
`projects.settings.view` and `projects.settings.manage` from the `admin` role in
the Roles matrix after the upgrade; the migration runs once and does not add
them back.
