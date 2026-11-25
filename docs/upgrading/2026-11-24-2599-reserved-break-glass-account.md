# The address `break-glass@centry.user` is reserved; unbound sealed credentials (#2599)

**What changed.**

- `elitea-main break-glass issue --unbound --ticket ID --label TEXT` issues a
  sealed credential bound to the reserved account `break-glass@centry.user`
  instead of a named person, for an envelope in a safe. The first unbound
  issue creates the account with no role; later ones reuse it. The
  `credential_issued` event carries the reason codes `credential_unbound`
  and `ticket:<ticket>`. `--unbound` without `--ticket` or `--label`, or
  beside `--user`, exits 2 and stores nothing. `issue --ticket` is accepted
  on a bound issue too.
- The address is reserved. These are refused for it:
  - a role assignment (`set_admin_role`, the Users role editor, a mode
    assignment, `elitea-main admin-role`): `403 reserved_account`;
  - a project invitation or a project role given to it:
    `403 reserved_account`;
  - an admin user invitation of any `@centry.user` address:
    `400 reserved_account`;
  - a token create from its session: `403 token_mint_reserved_account`
    (the database refuses the row too);
  - an OIDC or SAML sign-in that asserts the address, or a subject linked to
    the account: `403`. Form users in `@centry.user` were refused already.
- The admin Users listing (both tabs and their counts) does not show the
  account.
- A bound `issue --user` that names the account is refused: use `--unbound`.

**Who is affected.** A deployment that already has an account with this
address (unlikely: the domain is the system-identity domain) can no longer be
given a role, a token or a sign-in; if it holds a role or is suspended, `issue
--unbound` refuses until an operator clears it. No migration runs.

**Rollback.** The previous release has no `--unbound` and does not refuse
the address. Unbound credentials and the reserved account stay; the account
holds no role, so it gives nothing to anyone.
