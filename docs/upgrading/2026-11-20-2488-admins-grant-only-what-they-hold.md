# Admin roles: an administrator can no longer grant permissions they do not hold (issue 2488, ADR-0059 R0.8)

**What changed.** An administrator can no longer grant permissions they do not hold. Before this
release, a holder of `configuration.roles.permissions.edit` could add any permission to any role,
including `admin.auth.users.super_admin` to the `admin` role, and a holder of `modes.users` could
assign any role except `super_admin`. Main now applies these rules to every admin write that
changes role contents or role assignments:

- A permission can be added to a central role, or removed from it, only by a caller who holds
  that permission.
- A central role can be assigned or removed only by a caller who holds every permission of that
  role (`/admin/modes/administration`, `set_admin_role` on `/admin/auth_users`).
- A project member (`/admin/users/default/{project}`) can give or take a project role only when
  they hold every permission of that role in that project.
- A SCIM group binding can be created, moved or deleted only by a caller who holds every
  permission of its role in its project, because the binding gives the role to members pushed
  later, the caller possibly among them.
- A caller cannot change the permissions of a central role that they hold.
- `admin.auth.users.super_admin` can be granted only to the administration `super_admin` role.
  This rule applies to super admins too.
- A central administrator keeps managing the members and roles of other accounts in every project
  (the admin Projects page, bulk invites). These writes, and the project-plane contents changes
  (the Standard Roles template, Apply to Projects, a project matrix, a role rename), cannot widen
  the caller's own permissions in a project.
- `append_user_role` on `/admin/user_project_permissions` is super admin only.

A super admin (holder of `admin.auth.users.super_admin`) is exempt from all rules except the
reserved permission rule. The checks of the matrix save and of the central assignments run inside
the write transaction, under one advisory lock that the role writes share, so a concurrent role
write cannot outdate them. A refusal is `403` with a `code` (`role_permission_not_held`,
`role_own_set_locked`, `role_permission_reserved`, `role_super_admin_required`,
`role_self_escalation`, `role_caller_unresolved`) and an audit row with the action
`role.escalation_refused`. A caller whose own permissions cannot be read is refused
(`role_caller_unresolved`), not allowed.

**Who is affected.** Operators who delegate role administration to accounts that are not super
admins. Such an account can no longer edit the `admin` role it holds, or grant what it does not
hold. A central administrator that authors SCIM group bindings for projects it is not a member of
must be a super admin. Automation that edits central roles or SCIM bindings must run as a super
admin. There is no migration and no data
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
