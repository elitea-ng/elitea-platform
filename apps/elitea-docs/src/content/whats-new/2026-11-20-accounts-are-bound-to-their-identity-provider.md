---
date: 2026-11-20
---
## Accounts are bound to their identity provider

Each account that signs in through OIDC or SAML is now bound to its provider, the issuer that provider trusts and the subject. A provider whose issuer changed can no longer sign in to an account that was linked under the old issuer, on the web, the desktop app or the mobile app; the sign-in gets `409` with `provider_not_bound`. Only a super admin can create an identity provider, turn it on or off, replace its secret, change its issuer, metadata, certificates or identity attributes, or create, rotate or delete a SCIM client. Each refusal is a `403` with a stable code, the Authentication screen says why, and the Audit Trail records it. After an issuer change the screen warns that the accounts are still bound to the old issuer; to rotate an issuer on purpose, a super admin re-binds them with one audited request. The upgrade binds existing links to the enabled provider. See [Each account is bound to its identity provider](/admin/authentication#each-account-is-bound-to-its-identity-provider).
