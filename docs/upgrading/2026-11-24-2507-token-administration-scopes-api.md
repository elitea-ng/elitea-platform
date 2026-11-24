# Auth: personal access tokens take administration scopes (#2507, ADR-0059 R1.3a)

**What changed.** A personal access token can now carry `admin_access: scoped`
with a list of `admin_scopes` (the columns of shared 0265; no new migration).

- `POST /api/v2/auth/token/` accepts `admin_access` (`none`, the default, or
  `scoped`) and `admin_scopes`. `inherit` is refused with 400
  `invalid_admin_access`: no request sets it.
- `PUT /api/v2/auth/token/{uuid}/admin_scopes` replaces the scopes of one of
  the caller's tokens, or drops them (`{"admin_access": "none"}`). This is how
  an `inherit` token moves off `inherit` before the sunset date.
- `GET /api/v2/auth/token/admin_scopes` lists the scopes the caller may choose.
- `GET /api/v2/admin/access/tokens` (behind `access.roles.view`) lists every
  token with `scoped` or `inherit` access and its owner, with no token value.
- Every token response carries `admin_access` and `admin_scopes`, and an
  `inherit` token carries `inherit_ends_on` once the sunset date is set.

Setting scopes is refused (403, a `token_scope*` code) unless the caller is an
OIDC, SAML or Form browser session that is not sealed and not elevated, every
scope is in the caller's own administration set, and no scope is marked
`token: never` in the registry. It also needs a sign-in within
`ELITEA_SESSION_REAUTHENTICATION_MAX_AGE` (15 minutes by default), otherwise
401 `reauthentication_required`. That 401 now also carries
`X-Elitea-Reauthentication-Method` (`oidc`, `saml` or `form`). Each change
appends one `admin.access.changed` security audit event of kind
`token_scopes`; refusals write `token.mint_refused` or
`token.admin_scopes_refused` to the admin audit trail.

**Token scopes in the registry.** Every administration permission that any
role may hold is now a token scope, except the legacy names `runtime.plugins`
and `configuration.governance`. The never-scopes stay: the global
administrator's own permission, identity trust and the four break-glass
permissions.

**Who is affected.** Automation that reads token responses strictly gains two
fields. Operators whose identity provider does not send `auth_time` (OIDC) or
honour `ForceAuthn` (SAML) cannot set scopes until it does: see "Recent
authentication" in the authentication guide.

**What to do.** Nothing on upgrade. To replace an `inherit` token, give it the
scopes it needs from the token screen, or with the `PUT` route above.
