# Upgrading

Changes that need an operator to act when a deployment moves to a newer
release. Each entry says what changed, who is affected, and what to do. The
newest entry is first.

## Form sign-in state moved from Redis to PostgreSQL; auth document v2 — BREAKING

**Affects:** every deployment that sets `ELITEA_AUTH_CONFIG_FILE` (Helm:
`main.fileConfig.authConfig.enabled: true`), whether Form sign-in is on or off.

**What changed.**

- The Form graph's sign-in state — browser sessions, one-time login
  transactions and the attempt limiter — is in PostgreSQL now, in the
  `elitea_auth` tables of shared migration 0153. The auth Redis client and the
  `auth` Redis ACL user are gone, and readiness no longer checks Redis.
- The authentication document schema is `elitea.auth.form.v2`. It has no
  `redis:` block, and the attempt key moved to `credentials.attempt_key_file`.
  A `elitea.auth.form.v1` document is **refused**: elitea-main stops at boot,
  `elitea-auth-material` stops at pod start, and the chart refuses to render.
- The material is three files: `attempt_key_file`, `pat_signing_key_file` and
  `users_json_file`. The Redis password and the Redis CA are not read.
- A pre-login session (between the login page and the password submit) lives
  five minutes. An authenticated session still lives `cookie.lifetime_seconds`.
- elitea-scheduler deletes expired sign-in rows, including the OIDC and SAML
  `browser_sessions` rows that nothing deleted before.

**What to do.**

1. In the authentication document: set `schema_version: elitea.auth.form.v2`,
   delete the `redis:` block, and add
   `credentials.attempt_key_file` with the value of the old
   `redis.attempt_key_file`.
2. Run the migrations (the chart's migration Job does) before the new
   elitea-main starts.
3. Optional: drop the `redis-auth-password` and Redis CA keys from the auth
   material Secret, and the `user auth` line from the runtime Redis ACL file.
   An install still works with them present; nothing reads them.

People signed in through Form sessions are signed out once. OIDC and SAML
sessions are not affected.

## Form (username/password) sign-in is off by default — BREAKING

**Affects:** every deployment whose people sign in through the Form plane —
the local users in the Form users JSON file named by the
`ELITEA_AUTH_CONFIG_FILE` document (Helm: `main.fileConfig.authConfig`) — on a
deployment with no OIDC or SAML sign-in configured. Deployments that sign in
through OIDC or SAML are not affected: Form sign-in was already unmounted
there.

**What changed.** The local username/password sign-in is now controlled by
one switch, `ELITEA_FORM_LOGIN_ENABLED` (Helm: `main.env.ELITEA_FORM_LOGIN_ENABLED`),
and it is **off unless set to `true`**. Sign-in is meant to be OIDC or SAML,
with SCIM provisioning. With the switch off:

- the authentication document is still read and still required where it was
  — the gateway edge, personal access token validation and the runtime's
  forwarded-identity check depend on it;
- no `/auth/form/*` route and no Form `/auth/login` page is mounted, and the
  Form handler holds no users, so no configured password is accepted
  anywhere. With OIDC or SAML configured, `/auth/login` is the SSO sign-in
  page; with neither, it answers 404 and elitea-main logs
  `no browser sign-in is enabled` at start-up;
- a Form session created before the upgrade stops authorizing at once
  (people signed in with a Form password are signed out);
- configured Form users are ignored, and elitea-main logs that at start-up
  (`Form sign-in is disabled …; the configured Form users are ignored`).

Any value other than `true`/`false`/`1`/`0` (any letter case) or empty stops
the boot.
OIDC, SAML and SCIM behave exactly as before.

**What to do.**

- *You sign in with Form users and want to keep doing so:* set
  `ELITEA_FORM_LOGIN_ENABLED=true` before upgrading
  (`values-auth-minimal.yaml` already does). Read the next entry too: every
  Form user now needs a real email address.
- *You sign in with OIDC or SAML:* nothing to do. If your authentication
  document's `identity.initial_global_admins` lists Form logins, they no
  longer match anyone; list `oidc:<sub>`, `saml:<nameid>` or
  `email:<address>` entries instead.

**Making the first administrator of a fresh install** without Form sign-in:

- *OIDC:* set `OIDC_ISSUER_URL` (and the client settings) and name yourself
  in `identity.initial_global_admins` or `ELITEA_INITIAL_GLOBAL_ADMINS` —
  `email:<you@example.com>` works when your identity provider marks the
  address verified, `oidc:<sub>` always does. Your first OIDC sign-in receives
  the administration role.
- *SAML only:* a SAML identity provider is configured in the admin console
  (Configuration › Authentication), so it needs an administrator first, and
  there is no environment variable that configures SAML. Either bootstrap
  through OIDC as above, or, once: set `ELITEA_FORM_LOGIN_ENABLED=true` with
  one Form user (with an `email`) listed in `identity.initial_global_admins`,
  sign in, author the SAML provider and add your `saml:<nameid>` to the
  list, then set `ELITEA_FORM_LOGIN_ENABLED=false` and restart — the restart
  also mounts the SSO sign-in page, which is decided at boot.

## Form users must have a real email address

**Affects:** deployments that sign people in through the Form plane (the
local username/password users in the Form users JSON file named by
`provider.form.users_json_file` in the `ELITEA_AUTH_CONFIG_FILE` document).

**What changed.** A Form user's address is now read from the user's
top-level `email` field — the field the admin schema writes — and, when that
is absent, from `attributes.email`. Before, the top-level field was accepted
and ignored, and a user with no `attributes.email` signed in as
`<login>@centry.user`. That domain is the platform's reserved system-identity
domain (`system@centry.user`, `system_user_<n>@centry.user`): the Users page,
analytics, budgets and SCIM all treat an address in it as a platform account,
so the person disappeared from those surfaces.

No address is synthesized any more, on any sign-in plane. A Form user with no
address, with a top-level `email` that is not a usable address (it contains
whitespace or control characters), or with an address in `@centry.user` is a
configuration error:

- the users file still loads, so one bad entry does not lock the others out;
- elitea-main logs a warning at start-up naming the affected logins
  (`Form users have no usable email address and will be refused at sign-in`);
  `elitea-auth-validate` prints the count;
- that user's sign-in is refused with the ordinary "sign-in failed" page, and
  the server log says why (`Form sign-in refused: the Form user has no email
  address configured`, with the login).

**What to do before upgrading.** Give every Form user a real address:

```json
{"users": [
  {"login": "alice", "password": "…", "email": "alice@example.com"}
]}
```

**Accounts already created as `<login>@centry.user`.** Nothing is rewritten
automatically: which real address belongs to each person is your decision,
and changing an address changes how the account matches future sign-ins. At
start-up elitea-main logs a warning listing such accounts
(`sign-in accounts have a synthesized address in the reserved @centry.user
domain`, with their provider references). An existing account keeps signing
in — it is found by its provider link, not by its address — so you can fix
the rows at your own pace:

1. List them:

   ```sql
   SELECT u.id, u.email, p.provider_ref
   FROM public.auth_core__user AS u
   JOIN public.auth_core__user_provider AS p ON p.user_id = u.id
   WHERE lower(u.email) LIKE '%@centry.user'
     AND lower(u.email) <> 'system@centry.user'
     AND lower(u.email) NOT LIKE 'system\_user\_%@centry.user';
   ```

2. For each person, set the address you configured for them in the users
   file, after checking that no other account already holds it:

   ```sql
   UPDATE public.auth_core__user SET email = 'alice@example.com' WHERE id = <id>;
   ```

The warning stops once no such account remains.
