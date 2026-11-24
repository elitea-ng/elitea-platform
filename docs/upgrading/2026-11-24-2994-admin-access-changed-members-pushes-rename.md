# admin.access.changed covers central members, pushes and renames (#2994, ADR-0059)

**What changed.** Three more administration writes append an
`ai.elitea.security.v1.admin.access.changed` event to the security audit
outbox, in the same transaction as the change:

| Write | Kind | Event |
|---|---|---|
| An invite, role change or removal on the central member routes (`/admin/users/administration/{projectID}`) | `project_membership` | One per membership the request changed, in the project's partition, with `actor_is_member`, `member_holds_admin_role` and `bulk` (set when the request writes more than one membership). A removal's `after` is empty. The project-gated `/admin/users/default/{projectID}` routes append none. |
| The personal-project and team-project matrix save (`PUT /admin/user_project_permissions/administration`) and **Apply to Projects** (`POST /admin/permissions/administration/default`) | `project_permissions_pushed` (new) | One summary event per role the push changed, not one per project. The subject is `project_roles/personal:<role>` or `project_roles/team:<role>`; `before` lists the permissions removed from the role in at least one project, `after` those added; `fields: ["roles_assigned"]` when `append_user_role` gave the role to members. A push that changes nothing appends nothing. |
| A role rename (`PUT /admin/roles/{scope}/{mode}`) | `role_renamed` (new) | One event, subject the new name, `before` the old name and `after` the new one; `fields: ["project_role_copies"]` when a central `default` role was renamed in every project with it. |

The two new kinds are added to `details.access.kind` of the `elitea.audit.v1`
export (`libs/jsonschema/audit/v1/event.schema.json`). It is an added enum
value of an optional field, not a schema version change. This replaces the
"Not yet recorded" list of the #2509 note, except for the IdP re-bind, which
keeps its own `identity.link.changed` event.

**Fail closed.** When the outbox cannot take the event, the central member
write, the push and the rename fail with `500` and nothing is written.

**Who is affected.** SIEM consumers that switch on `details.access.kind` see
two new values; a consumer that ignores unknown kinds needs nothing. No
migration, no configuration.

**Rollback.** The previous release does not write these events. Events
already appended stay in the chain and still verify.
