# Accounts are bound to their identity provider (issue 2558)

**What changed.** An OIDC or SAML account link is now bound to the provider key, the issuer and the
subject. Before, a link was only `oidc:<sub>` or `saml:<NameID>`, so an administrator who pointed the
enabled provider at an identity provider they control could sign in as any account, a super admin
included. A sign-in now uses an account only when the provider and the issuer match its binding;
otherwise it gets `409` with the cause `provider_not_bound`. Web, desktop and mobile sign-ins are bound
the same way.

Shared migration `0258_identity_provider_bindings.sql` adds `elitea_auth.identity_bindings` and binds
every existing link to the **enabled** stored provider of its protocol, with that provider's issuer
(OIDC) or entity ID (SAML). It is additive and idempotent. A link of a protocol with no enabled stored
provider stays unbound.

**Behaviour change.** Only a super admin may create an identity provider, change its trust fields
(issuer, client ID, Entra tenants, SAML entity ID, SSO URL, certificates, e-mail attribute, turning on
SCIM adoption, turning off e-mail verification), re-bind accounts, or create, rotate or delete a SCIM
client. Other administrators get `403` with `identity_provider_trust_super_admin_required`,
`identity_binding_super_admin_required` or `scim_client_super_admin_required`. Revoking a SCIM client
and editing the other provider fields are unchanged.

**What you must do.**

- Run the migration with the release (the chart's migrate job does).
- A deployment that signs in through `OIDC_ISSUER_URL` only needs nothing: each unbound OIDC link is
  bound to the environment provider at its next sign-in.
- If you use a stored provider and some accounts were linked while no stored provider of that protocol
  was enabled, a super admin binds them once:
  `POST /api/v2/admin/identity_providers/administration/<key>/rebind` with `{"from_unbound": true}`.
  If you move from the environment provider to a stored one, send `{"from_provider_key": "@environment"}`.
- To rotate an issuer on purpose, save the new issuer as a super admin, then re-bind with
  `{"from_issuer": "<old issuer>"}`. See Admin > Authentication > Each account is bound to its identity
  provider.
- Tell administrators who are not super admins that trust changes and SCIM client changes now need a
  super admin.

**Rolling back.** The table is additive. An older release ignores it and matches links by subject alone,
which is the behaviour this release removes.
