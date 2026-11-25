# Administration: the separation matrix covers the write paths (#2587)

**What changed.** The tests of the separation matrix now cover the writes that
change who holds what. Before this release those lines were marked
`pending #2587`. A new file,
`services/elitea-main/internal/api/testdata/separation_matrix_writes.txt`,
records the answer the server gives each built-in administration role for
these writes:

- role assignment
- the Roles matrix and **Apply to Projects**
- role create, rename and delete
- project members, the bulk invite and the project matrix
- SCIM group bindings and SCIM clients
- each identity provider action
- the central project settings routes

The file also has a column for a holder of only `identity.sso.settings.manage`.
The answers are read against a migrated database, so they include the
refusal code of a rule that runs after the route check, such as
`role_self_escalation` or `role_permission_not_held`. The route-gate file
`separation_matrix.txt` has no `pending` lines now.

**Who is affected.** No one. The server answers the same way as before; the
change is in the tests and the documentation.

**What to do.** No operator action.
