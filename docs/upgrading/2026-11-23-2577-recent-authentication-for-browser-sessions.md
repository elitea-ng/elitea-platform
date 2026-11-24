# Browser sessions: recent authentication (#2577, ADR-0059)

**What changed.** Shared migration `0262_browser_session_authenticated_at.sql`
adds `authenticated_at timestamptz` to `elitea_auth.browser_sessions`. It
records when the person behind a session last authenticated. The migration fills
existing rows from `created_at`. The column can be NULL, so a replica of the
previous release can still create sessions during a rolling upgrade. NULL means
the time is unknown, and such a session never counts as a recent sign-in.

An OIDC sign-in now records the id_token's `auth_time` claim as that time. If
the provider does not send `auth_time`, the time is unknown (NULL). A SAML
sign-in records the time of the sign-in. A request never moves this time. In
this release only an OIDC session can satisfy the recent-authentication check.
SAML and Form sessions cannot satisfy it yet.

The OIDC login accepts `max_age=<seconds>` (0 to 86400). The platform sends it
to the identity provider. The callback then refuses an id_token whose
`auth_time` is missing or older than the limit, and sends the person back to
the sign-in page with an error. A limited login also refuses a
`target_to` that is not a same-origin relative path, with 400.

The new setting `ELITEA_SESSION_REAUTHENTICATION_MAX_AGE` (default `15m`, range
`1m` to `24h`) is the limit for actions that need a recent sign-in. The Helm
chart and the compose file list it.

**Who is affected.** Every deployment gets the column. No action requires a
recent sign-in in this release, so nobody is asked to sign in again yet. Later
ADR-0059 stories use the check for administration scopes on tokens, break-glass
elevation and critical actions.

**What to do.**

- Nothing is required for the upgrade.
- Before those features ship, make sure your OIDC identity provider honours
  `max_age` and returns `auth_time` in the id_token. A provider that ignores
  `max_age` returns the old `auth_time`, and the platform refuses that sign-in.
- If you back up or replicate `elitea_auth`, the table has one more column.
