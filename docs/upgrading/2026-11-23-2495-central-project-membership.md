# Central project membership: `projects.members.manage` and a central removal (#2495)

**What changed.** The administration member routes of a project accept the
narrow permission `projects.members.manage` beside the legacy names
(ADR-0059 R0.6a):

- `GET /admin/users/administration/{projectID}` accepts
  `projects.members.manage` or `configuration.users.users.view`.
- `POST` and `PUT` on the same path accept `projects.members.manage` or
  `configuration.users.users.create` / `.edit`, as before for the legacy
  names.
- New route `DELETE /admin/users/administration/{projectID}?id[]=…` removes
  members of any project (described in the API contract as
  `administrationUserDelete`). It accepts `projects.members.manage` or
  `configuration.users.users.delete`. The accounts survive, as on the
  existing `DELETE /admin/users/default/{projectID}`.

`configuration.users.users.delete` is now a registered permission. No
migration grants it: holders of `projects.members.manage` (the built-in
`super_admin`, `admin` and `project_administrator`) reach the removal through
that name.

**The self rule.** The existing rule (#2555) is the only one: a membership
change that would give the caller permissions it does not hold is refused
with `403` and `code: role_self_escalation`. The body now also carries
`ways_in: ["project_member", "operator_command"]` and
`operator_command: "elitea-main admin-role"`. Removing yourself from a
project is allowed. A second account, or another project administrator, can
still be added; that is detected (the `access.admin_membership_granted`
signal), not prevented.

**Audit.** A membership write by a caller who is not a member of the project
(these routes and `POST /admin/invites_bulk/administration`) marks its audit
record `central_override`. The flag is set on the in-process audit event; its
storage column arrives with ADR-0059 R1.4a.

**Who is affected.** Anyone holding the legacy `configuration.users.users.*`
names sees no change. Holders of the built-in administration roles can now
manage the membership of any project without being a member.

**What to do.** Nothing. Scripts that removed members through the `default`
route keep working; the `administration` route is the one to use without a
membership.
