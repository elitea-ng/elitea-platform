# Identity trust and settings permissions (#2584, ADR-0059)

- **What changed:** the identity provider, SCIM client and SCIM group binding
  routes are split by action. The reads accept `identity.sso.view`. The
  settings actions (a provider's display name, scopes, login domains and name
  attribute, a SCIM client revoke, the group bindings) accept
  `identity.sso.settings.manage`. The trust actions accept
  `identity.sso.trust.manage` or `admin.auth.users.super_admin`; both belong
  to `super_admin` only. Every route still accepts the permission it accepted
  before (`runtime.plugins`, `admin.auth.users`,
  `configuration.roles.user_project_permissions.view` / `.edit`), except for
  the trust actions.
- **Identity trust does not change at this upgrade.** The #2558 fix already
  took the trust actions from `admin`. This release names the gate and opens
  the settings half to a narrow permission. One trust action had no
  super admin check before: deleting an identity provider was open to every
  holder of `runtime.plugins`. It now needs a super admin, like creating one,
  and a refusal is `403 identity_provider_trust_super_admin_required` with the
  Audit Trail action `identity_provider.trust_change_refused`.
- **Unchanged on purpose:** turning a provider on or off and deleting a SCIM
  client stay trust actions, as the #2558 fix made them. A group binding still
  needs every permission of the role it grants in that project
  (`role_permission_not_held`), whatever permission opened the route.
- **Who is affected:** an `admin` who deleted identity providers. Ask a
  super admin to do it.
- **What to do:** nothing. No migration and no configuration. The built-in
  roles already hold the new names (shared migration 0261); a custom role
  can be given `identity.sso.view` or `identity.sso.settings.manage` on the
  Roles page. `identity.sso.trust.manage` is refused on every role but
  `super_admin`.
