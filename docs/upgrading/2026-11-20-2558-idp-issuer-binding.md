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

**Behaviour change.** Only a super admin may create an identity provider, turn one on or off, replace
its secret, change its trust fields (issuer, client ID, redirect URI, Entra settings; SAML entity ID, SSO
URL, NameID format, e-mail attribute, service provider entity ID, ACS URL, request signing and
certificate; an added IdP certificate or Entra tenant; a raised clock skew; SCIM adoption turned on;
e-mail verification turned off), re-bind accounts, or create, rotate or delete a SCIM client. Other
administrators get `403` with `identity_provider_trust_super_admin_required`,
`identity_binding_super_admin_required` or `scim_client_super_admin_required`. Revoking a SCIM client,
the display name, scopes, login domains and changes that only narrow trust stay open to them. A
provider save made against a row that changed since it was read now gets `409`.

**Rolling upgrade.** A replica of the previous release can still link a first sign-in while the
release rolls out. Such a link is bound at its next sign-in, but only to the stored provider that has
not been saved since the migration ran; after any save, a super admin binds leftovers with
`{"from_unbound": true}`. The local Form sign-in plane never reads or writes an `oidc:` or `saml:` link.

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
