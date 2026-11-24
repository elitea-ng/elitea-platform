# Deleting an account records the revocation of its roles (#2922, ADR-0059)

**What changed.** `POST /api/v2/admin/auth_users/administration` with
`action: delete` now revokes every central role an account holds before it
deletes the account, in the same transaction, and writes one `revoked` row per
role to the grant history (`elitea_auth.admin_role_grants`), with the caller as
the actor and the source `console` or `api`.

Before, `auth_core__user_role` cascaded from the account row, so the delete
dropped every role the account held, a `super_admin` included, and the history
still showed each role as granted.

The delete itself is unchanged: same request, same answer. A caller whose own
account cannot be resolved now gets `403 role_caller_unresolved` and nothing
is deleted, because the history needs an actor.

**What to do.** Nothing. Accounts deleted before this change keep their stale
`granted` rows; the history is append-only.
