# A new security audit event: admin.access.changed (#2509, ADR-0059)

**What changed.** A change of who holds what on the administration surface
appends one chained event of the new type
`ai.elitea.security.v1.admin.access.changed` to the security audit outbox
(shared 0183), in the same transaction as the change. `action` is
`admin.access.<kind>`:

| Kind | Recorded when |
|---|---|
| `role_assignment` | A central role is assigned or removed (Users page, `/admin/modes`, account deletion), and the first sign-in grant of `identity.initial_global_admins` (source `bootstrap`, no actor). |
| `role_permissions` | A role's permission set is edited on the Roles page (central roles and the public and support project roles). |
| `role_created`, `role_deleted` | A role is created or deleted. |
| `project_membership` | A bulk invite adds a membership, with `actor_is_member`, `member_holds_admin_role` and `bulk`. |
| `global_provider_route` | A global provider is created, deleted, or its `data` or type changes. |
| `identity_trust` | An identity provider trust action (create, delete, on or off, trust fields, secret) and a SCIM client create or rotate. |
| `token_scopes` | Reserved: wired with administration-scoped tokens (ADR-0059 R1.3a). |

The event names the actor, the subject and the sets before and after by name,
the elevation id and the reason code when there are any. It never carries the
reason text, a credential value or a digest of one. The OCSF class is 3005
User Access Management. `details.access` is a new optional field of the
`elitea.audit.v1` export (`libs/jsonschema/audit/v1/event.schema.json`), so
this is not a schema version change.

**Fail closed.** The event is part of the change: when the outbox cannot take
it, the role change, the matrix save, the role create or delete, the bulk
invite pair, the global provider write, the identity provider save or delete
and the SCIM client create or rotate fail and nothing is written. A
deployment whose shared migrations are applied has the outbox since 0183.

**Who is affected.** SIEM consumers of the security audit export see a new
event type. Ignore it, or route it to the identity and access dashboards; the
`kind` is in `details.access.kind`. No migration, no configuration.

**Not yet recorded.** The project member routes of a project's own Users page
(central membership by route waits on the central project gates, ADR-0059
R0.6), the estate-wide "Apply to Projects" and personal-project permission
pushes, a role rename, and the IdP re-bind (it has its own
`identity.link.changed` event) append no `admin.access.changed` event in this
release. A `role_deleted` event of a central `default` role carries
`fields: ["project_role_copies"]` when the same-named project roles of every
project went with it.

**Rollback.** The previous release does not write the type. Events already
appended stay in the chain and still verify.
