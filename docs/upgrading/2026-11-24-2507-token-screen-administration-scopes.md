# Web: the token screen sets administration scopes (#2593, ADR-0059 R1.3b)

**What changed.** The personal token form of the web app offers
**Administration access** to a person who holds administration permissions
and is signed in with a browser: the scopes the server offers
(`GET /api/v2/auth/token/admin_scopes`), nothing else. The token list gains an
**Administration** column when a token has `scoped` or `inherit` access, and
marks an `inherit` token with its end date once the sunset date is set. A
create that answers `401 reauthentication_required` sends the person through
the sign-in named by `X-Elitea-Reauthentication-Method` (OIDC with `max_age`,
SAML with `ForceAuthn`, or the Form password prompt) and restores the form.
The desktop and mobile apps show no scope control: a native device session
cannot set scopes.

**Who is affected.** People who rely on `inherit` tokens, which end at the
sunset date of ADR-0059 decision 2.

**What to do: move an `inherit` token to `scoped`.**

1. List your tokens (`GET /api/v2/auth/token/`, or the token screen) and note
   each `inherit` token and what uses it. A holder of `access.roles.view` sees
   every such token of the deployment at `GET /api/v2/admin/access/tokens`.
2. Either keep the token and narrow it, from a browser session signed in
   within the limit:

   ```
   PUT /api/v2/auth/token/<uuid>/admin_scopes
   {"admin_access": "scoped", "admin_scopes": ["access.roles.view"]}
   ```

   or create a new token on the token screen with only the scopes it needs,
   point the automation at it, and delete the old token.
3. A token that needs no administration permission at all:
   `{"admin_access": "none"}` on the same route (no recent sign-in needed).

The OIDC identity provider must honour `max_age` and return `auth_time`, and a
SAML provider must honour `ForceAuthn`, for the recent sign-in to succeed; see
"Recent authentication" in the authentication guide.
