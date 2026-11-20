---
date: 2026-11-20
---
## Administrators can grant only what they hold

The server now stops an administrator from giving themselves, or anybody else, more than they hold. An administrator can add a permission to a role only if they hold it, and can assign or remove a role only if they hold every permission of that role. An administrator cannot change the permissions of a central role that they hold. `admin.auth.users.super_admin` goes only on the `super_admin` role. A change to a project role, to the Standard Roles template or to Apply to Projects cannot widen the administrator's own access in a project. These rules apply to the Roles matrix, the Users page, `/admin/modes`, project members, bulk invites, the personal and team project matrix and SCIM group bindings. A super admin keeps full power. Each refusal is a `403` with a stable code, the Roles page says why, and the Audit Trail records it as `role.escalation_refused`. See [No escalation](/admin/users-and-roles#no-escalation).
