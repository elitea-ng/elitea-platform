# Admin roles: an administrator can no longer grant permissions they do not hold (issue 2488, ADR-0059 R0.8)

**What changed.** An administrator can no longer grant permissions they do not hold. Before this
release, a holder of `configuration.roles.permissions.edit` could add any permission to any role,
including `admin.auth.users.super_admin` to the `admin` role, and a holder of `modes.users` could
assign any role except `super_admin`. Main now applies these rules to every admin write that
changes role contents or role assignments:

- A permission can be added to a role only by a caller who holds that permission.
- A role can be assigned or removed only by a caller who holds every permission of that role
  (`/admin/modes/administration`, `set_admin_role` on `/admin/auth_users`, project members).
- A caller cannot change the permissions of a central role that they hold, and cannot update a
  SCIM group binding that they are a member of.
- `admin.auth.users.super_admin` can be granted only to the administration `super_admin` role.
  This rule applies to super admins too.
- A project-plane change (the Standard Roles template, Apply to Projects, a project matrix, a role
  rename, a bulk invite, a membership change) cannot widen the caller's own permissions in a
  project.

A super admin (holder of `admin.auth.users.super_admin`) is exempt from all rules except the
reserved permission rule. A refusal is `403` with a `code` (`role_permission_not_held`,
`role_own_set_locked`, `role_permission_reserved`, `role_super_admin_required`,
`role_self_escalation`, `role_caller_unresolved`) and an audit row with the action
`role.escalation_refused`. A caller whose own permissions cannot be read is refused
(`role_caller_unresolved`), not allowed.

**Who is affected.** Operators who delegate role administration to accounts that are not super
admins. Such an account can no longer edit the `admin` role it holds, or grant what it does not
hold. Automation that edits roles must run as a super admin. There is no migration and no data
change.

**What you must do.**

- Make sure that at least one trusted account holds `super_admin`. The accounts in
  `ELITEA_INITIAL_GLOBAL_ADMINS` get it on their first sign-in.
- Check for grants that were made before this release. This query must return no rows:

  ```sql
  SELECT role.mode, role.name, grant_row.permission
  FROM public.auth_core__role role
  JOIN public.auth_core__role_permission grant_row ON grant_row.role_id = role.id
  WHERE grant_row.permission = 'admin.auth.users.super_admin'
    AND NOT (role.mode = 'administration' AND role.name = 'super_admin')
  UNION ALL
  SELECT 'project ' || role.project_id, role.name, grant_row.permission
  FROM public.auth_core__project_role role
  JOIN public.auth_core__project_role_permission grant_row ON grant_row.role_id = role.id
  WHERE grant_row.permission = 'admin.auth.users.super_admin';
  ```

  If it returns a row, remove that grant as a super admin and review the Audit Trail for
  `PUT /api/v2/admin/permissions/` requests by the holders of that role.
