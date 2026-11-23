# Administration: built-in administrative roles, and their sets are locked (#2496, ADR-0059)

**What changed.** Shared migration `0262_builtin_admin_roles.sql` adds four
built-in roles to the central `administration` mode and records which roles are
built-in in a new table, `elitea_auth.admin_role_seeds`.

| Role | Display name | Permissions |
|---|---|---|
| `llm_manager` | LLM manager | 9: the `llm.*` catalogue, policy, usage and alert names, the project budgets, and the project list |
| `project_administrator` | Project administrator (all projects) | 15: projects, their members and guarded settings, member and role listings, bulk invites, moderation, budget view |
| `security_auditor` | Security auditor | `models.admin.audit_trail.view`, `access.roles.view`, `access.breakglass.review` |
| `emergency_access` | Emergency access | `access.breakglass.request` |

The roles are created with their permissions and **assigned to nobody**. Moving
people from `admin` to a narrower role is your decision.

The migration also records `super_admin`, `admin`, `editor` and `viewer` of the
`administration` mode as built-in. On a deployment upgraded from the legacy
platform it records the administration `system` role too, as a role that is
never assigned. It removes no permission from any role, including `system`,
`editor` and `viewer`.

**The built-in sets are locked.** From this release the roles API refuses
(`409`) to rename or delete a built-in role, and the Roles matrix refuses to
change the permissions of one, for every caller, a global administrator
included. A deployment that edited the set of `admin` (or of another built-in
role) before the upgrade keeps the edited set, and can no longer edit it in the
console. The repair is the operator command `elitea-main admin-role reset <role>`
(for example `elitea-main admin-role reset admin`), which puts a built-in set
back to the one Elitea defines. It ships in a later release of this programme
(ADR-0059 story R0.12a, #2501); until then the edited set stays as it is. For a
different set, create a custom role.

This replaces the advice of the #2491 upgrade note: after this release the
`projects.settings.*` grants cannot be removed from `admin` in the Roles matrix.
Give the people who should not reach project settings a custom role instead.

**A custom role with a built-in name.** If the deployment already has an
administration role called `llm_manager`, `project_administrator`,
`emergency_access` or `security_auditor`, the migration leaves it exactly as it
is: it gets no permission, is not recorded as built-in, is not locked and can
still be renamed and deleted. The migration log has a notice naming it. The
built-in role of that name is not created, and the migration does not run
again, so renaming the custom role later does not add it.

**Who is affected.** Every deployment with the `auth_core` tables. On a
database without them the migration does nothing.

**What to do.** Nothing to keep working. Check the migration log for a notice
about a custom role with a built-in name. If you edited a built-in set and want
Elitea's set back, run `elitea-main admin-role reset <role>` once it ships.
