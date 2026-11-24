# Administration: the last administrator cannot be removed (#2500, ADR-0059)

**What changed.** A deployment now always keeps one administrator: the last
active holder of `admin.auth.users.super_admin` or, when nobody holds it, the
last active holder of the administration `admin` role. These requests now
answer `409` with the code `last_administrator` when they would remove that
person, for every caller, a `super_admin` included:

- `POST /api/v2/admin/auth_users/administration` with `set_admin_role` (a set
  without the person's last administrator role) or `delete` (single or bulk; a
  bulk delete is refused whole);
- `DELETE /api/v2/admin/modes/administration`;
- `PUT /api/v2/admin/user_suspend/administration/{id}` with `suspended: true`.

A SCIM deactivation (`PATCH` or `PUT` with `active: false`, a create that
adopts the account with `active: false`, or `DELETE`) of the last
administrator answers a SCIM error with status `409`. The account stays
active and keeps its roles, but its browser sessions, device sessions and
personal access tokens are revoked in the same transaction, and one `critical`
security audit event (`admin.access.deprovision_refused`) is written (a
retry that finds nothing left to end writes none). The
organisation said that the person left, so the access ends.

**Who is affected.** Automation that demotes, suspends or deletes the only
global administrator, and identity providers that deprovision it. A new
deployment where nobody holds `super_admin` or `admin` is not affected.

**What to do.** Give a successor the role first, then remove the old
administrator. When nobody else can, run
`elitea-main admin-role grant <user> super_admin` in the main workload (see
"Administrative roles from the command line" in the deployment operations
guide). A security information system that reads the security audit export
sees the new action `admin.access.deprovision_refused` (event type
`ai.elitea.security.v1.grant.changed` until the `admin.access.changed` type
ships).
