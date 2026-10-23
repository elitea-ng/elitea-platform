# Browser authentication URLs

Status: implemented. Code: `internal/api/auth_paths.go`,
`internal/api/browserauth`, `internal/api/v2/auth`.

All browser authentication routes are under `/auth/`.

| Purpose | URL |
| --- | --- |
| Sign-in page (SSO chooser) | `/auth/login` |
| Sign-in page routing | `/auth/login/continue` |
| Logout | `/auth/logout` |
| Session probe | `/auth/info` |
| OIDC login | `/auth/oidc/login` |
| OIDC redirect URI (callback) | `/auth/oidc/callback` |
| OIDC logout | `/auth/oidc/logout` |
| SAML SP metadata | `/auth/saml/metadata` |
| SAML login (SP-initiated sign-on) | `/auth/saml/login` |
| SAML assertion consumer service | `/auth/saml/acs` |
| SAML logout (local session only) | `/auth/saml/logout` |
| Form login page | `/auth/form/login` |
| Form credential post | `/auth/form/authorize` |
| Form logout | `/auth/form/logout` |

The Form rows exist only when `ELITEA_FORM_LOGIN_ENABLED=true` AND no
single sign-on plane is configured. Form sign-in is off by default; with it
off they answer 404, and `/auth/login` is the SSO chooser when OIDC or SAML is
configured.

There is no forward-auth target any more (#1359). `GET /auth` (the legacy
token check), the Form plane's `/auth/check` and `/internal/auth/main` (which
projected a signed `X-Auth-*` identity for an authenticating edge) are gone.
elitea-main reads every browser credential itself: the single sign-on
`elitea_session` cookie and the Form plane's `elitea_browser_auth` cookie
(`internal/api/browserauth/form_session.go`), and it deletes any inbound
`X-Auth-*` or `X-Elitea-*` identity header before routing
(`internal/api/middleware/strip_identity.go`).

The browser routes are below `/auth`, and each is registered by its full path,
so they do not collide. `/api/v2/auth/*` is the token and permission API, a
different prefix.

## What an operator registers

Use the public origin of the deployment, for example
`https://elitea.example.com`.

- OIDC: set `OIDC_REDIRECT_URI` (or the provider's `redirect_uri` on the admin
  Authentication page) to `https://<host>/auth/oidc/callback`. Register the
  same value at the identity provider.
- SAML: on the admin Authentication page, set `acs_url` to
  `https://<host>/auth/saml/acs` and `sp_entity_id` to a stable identifier,
  for example `https://<host>/auth/saml/metadata`. Give the identity provider
  the metadata URL `https://<host>/auth/saml/metadata`. The sign-on URL is
  `https://<host>/auth/login` (the sign-in page) or
  `https://<host>/auth/saml/login` (straight to SAML).

## Edges

Each browser edge forwards `/auth` and `/auth/` to elitea-main
(`deploy/gateway-api/httproute.yaml`, and the compose edges while they exist).
No edge authenticates a request on elitea-main's behalf.
