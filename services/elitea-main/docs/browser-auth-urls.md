# Browser authentication URLs

Status: implemented. Code: `internal/api/auth_paths.go`,
`internal/api/browserauth`, `internal/api/v2/auth`.

All browser authentication routes are under `/auth/`. The old prefix
`/forward-auth/` is a **deprecated compatibility alias**. The alias serves the
same handlers. It is not a redirect, because a SAML assertion arrives as a POST
and an OIDC callback carries a one-time code. Existing identity provider
registrations that name an old URL continue to work.

New links and redirects from elitea-main and the web app use `/auth/`. Register
the new URLs for each new identity provider. Move existing registrations when
convenient. The alias will be removed in a later release, after a notice.

## URL migration

| Purpose | Canonical URL | Deprecated alias |
| --- | --- | --- |
| Sign-in page (SSO chooser) | `/auth/login` | `/forward-auth/login` |
| Sign-in page routing | `/auth/login/continue` | `/forward-auth/login/continue` |
| Logout | `/auth/logout` | `/forward-auth/logout` |
| Session probe | `/auth/info` | `/forward-auth/info` |
| OIDC login | `/auth/oidc/login` | `/forward-auth/auth_oidc/login` |
| OIDC redirect URI (callback) | `/auth/oidc/callback` | `/forward-auth/auth_oidc/callback` |
| OIDC logout | `/auth/oidc/logout` | `/forward-auth/auth_oidc/logout` |
| SAML SP metadata | `/auth/saml/metadata` | `/forward-auth/auth_saml/metadata` |
| SAML login (SP-initiated sign-on) | `/auth/saml/login` | `/forward-auth/auth_saml/login` |
| SAML assertion consumer service | `/auth/saml/acs` | `/forward-auth/auth_saml/acs` |
| SAML logout (local session only) | `/auth/saml/logout` | `/forward-auth/auth_saml/logout` |
| Form login page | `/auth/form/login` | `/forward-auth/auth_form/login` |
| Form credential post | `/auth/form/authorize` | `/forward-auth/auth_form/authorize` |
| Form logout | `/auth/form/logout` | `/forward-auth/auth_form/logout` |

These paths do not change:

- `GET /auth` (no further segment) is the edge forward-auth check
  (`v2auth.ForwardAuthHandler`). The browser routes are below it and do not
  collide with it.
- `/forward-auth/auth` is the Form plane's core check. It stays under the old
  prefix only.
- `/internal/forward-auth/main` is the gateway's ForwardAuth target. It is not
  browser-facing.
- `/api/v2/auth/*` is the token and permission API. It is a different prefix.

## What an operator registers

Use the public origin of the deployment, for example
`https://elitea.example.com`.

- OIDC: set `OIDC_REDIRECT_URI` (or the provider's `redirect_uri` on the admin
  Authentication page) to `https://<host>/auth/oidc/callback`, and register the
  same value at the identity provider.
- SAML: set `sp_entity_id` (for example `https://<host>/auth/saml/metadata`)
  and `acs_url` to `https://<host>/auth/saml/acs` on the admin Authentication
  page. Give the identity provider the metadata URL
  `https://<host>/auth/saml/metadata`. The sign-on URL is
  `https://<host>/auth/login` (the sign-in page) or
  `https://<host>/auth/saml/login` (straight to SAML).

The SAML `acs_url` is compared with the `Destination` of each response. A
provider that still posts to the old ACS URL must keep the old `acs_url` value
until its registration moves. Change both at the same time.

## Login state across prefixes

Every login state cookie (OIDC state, nonce and PKCE verifier, the SAML
request cookie, the session cookie and the sign-in page's last-used cookie)
has `Path=/`. The return target is kept in the signed state, not in the URL
prefix. A login that starts under `/auth/` can return under `/forward-auth/`,
and the reverse.

## Edges

Each browser edge must forward `/auth/` to elitea-main, in addition to
`/forward-auth/` and `/auth`. The edge files in this repository do so:
`deploy/traefik/dynamic.yml`, `deploy/traefik/dynamic.e2e.yml` and
`deploy/gateway-api/httproute.yaml`. Update an edge that is maintained in
another repository before you deploy this release. Otherwise `/auth/login`
reaches the web app instead of elitea-main.
