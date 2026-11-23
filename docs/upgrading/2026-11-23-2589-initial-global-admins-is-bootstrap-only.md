# `identity.initial_global_admins` is a bootstrap only (#2589, ADR-0059 R0.12b)

**What changed.** `identity.initial_global_admins` (and its environment
fallback `ELITEA_INITIAL_GLOBAL_ADMINS`) grants `super_admin` at first sign-in
only while the deployment has no unsuspended account that holds
`admin.auth.users.super_admin`. Once one exists, a listed account that signs in
gets no role, and the server logs a warning that names the shape of the entry
(`entry_shape=reference` or `entry_shape=email`), never the address, and how to
add an administrator instead: an existing administrator, or the operator
command `elitea-main admin-role grant` (ADR-0059 R0.12a, a later release).

Both sign-in planes follow the rule: the OIDC and SAML first login of an
account, and the provisioning of a new account. The check and the grant run in
the sign-in's transaction under the role-writes lock, so a global
administrator created at the same moment by another role write is seen.

A suspended `super_admin` does not count, so the list still recovers a
deployment whose only global administrator is suspended.

Before, the list granted `super_admin` to a matching account at its first
sign-in whatever administrators existed. Anyone who could edit the Helm values
and owned an account at the identity provider could add an address and become
global administrator.

**What to do.**

- A deployment that adds administrators by appending to the list must stop:
  the list now grants nothing once an administrator exists. An existing
  administrator covers every later case (the console, or
  `POST /api/v2/admin/auth_users/administration`). A deployment whose
  administrators are all suspended still recovers through the list, because a
  suspended `super_admin` does not count. The operator command
  `elitea-main admin-role grant` (ADR-0059 R0.12a) is not in this release.
- Empty the list once the first administrator has signed in.
- A new deployment needs no change: the list names the first administrator as
  before.
