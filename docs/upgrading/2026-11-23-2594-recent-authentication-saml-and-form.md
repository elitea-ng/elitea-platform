# Recent authentication for SAML and Form sign-in (#2594, ADR-0059)

**What changed.** Shared migration `0263_recent_authentication_saml_and_form.sql`
adds two columns:

- `elitea_auth.browser_sessions.authentication_proven` (`boolean NOT NULL
  DEFAULT false`). It is true when the session's `authenticated_at` is proof of
  an authentication: an OIDC `auth_time`, or a SAML `AuthnInstant` from a
  sign-in that asked for `ForceAuthn`. Only such sessions pass the
  recent-authentication check. Existing rows stay false.
- `elitea_auth.form_sessions.authenticated_at` (`timestamptz`, nullable). It is
  the time the person last typed their Form password. Existing rows stay NULL,
  which means unknown.

The SAML login accepts `max_age=<seconds>` (0 to 86400), as the OIDC login
does. The platform then sets `ForceAuthn="true"` on the authentication request.
The assertion consumer service refuses an assertion whose `AuthnInstant` is
missing or older than the limit, using the provider's clock skew setting. An
ordinary SAML sign-in no longer records an authentication time.

Form sign-in has a password prompt at `/auth/form/reauthenticate?target_to=<path>`.
A correct password records a new authentication time and replaces the session
cookie. A wrong password changes nothing and counts against the existing
sign-in attempt limits.

**Who is affected.** Deployments that sign people in with SAML or Form. No
action requires a recent sign-in in this release, so nobody is asked to sign in
again yet. When later ADR-0059 stories ship (administration scopes on tokens,
break-glass elevation, critical actions), the platform asks SAML users for a
`ForceAuthn` sign-in and Form users for their password. Sessions that existed
before this upgrade, on any sign-in method, need one fresh sign-in before they
count as recent.

**What to do.**

- SAML: make sure your identity provider honours `ForceAuthn="true"`. It must
  ask the person to authenticate again and send an `AuthnStatement` whose
  `AuthnInstant` is the time of that authentication. A provider that ignores
  `ForceAuthn` returns an old `AuthnInstant`, and the platform refuses the
  sign-in.
- Form: nothing.
- If you back up or replicate `elitea_auth`, both tables have one more column.
- A rolling upgrade is safe. A replica of the previous release writes sessions
  without the new columns; those sessions read as not recent, which only asks
  for one more sign-in.
