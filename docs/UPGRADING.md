# Upgrading

Changes that need an operator to act when a deployment moves to a newer
release. Each entry says what changed, who is affected, and what to do. The
newest entry is first.

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
address — or with an address in `@centry.user` — is a configuration error:

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
