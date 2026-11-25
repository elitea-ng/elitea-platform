# Administration: the security auditor no longer sees the Roles link (#3042, ADR-0059)

**What changed.** With the six labelled areas on (`admin_console_areas`), the
temporary **Roles** link under **People and access** (see
`2026-11-24-2534-admin-view-only-mode.md`) is now shown only to a caller who
can also read the permission matrix, through `configuration.roles.permissions.view`
or `configuration.roles.permissions.edit`. Today's Roles page is that matrix.
The security auditor's `access.roles.view` is for who holds which role, and
the server never let it read the matrix, so the auditor's Roles link opened a
page whose matrix read failed. The auditor no longer sees the link. If the
auditor opens `/admin/app/roles` by its address, the page is in view-only
mode with every control disabled, and the matrix read is still refused.

**Who is affected.** Security auditors, and any custom role that opens the
Access item only through `access.roles.view`. No one gains or loses a
permission, and no API changes. Auditors get a view of who holds which role
when the Access item ships (ADR-0059 R3.6 and R3.9).

**What to do.** No operator action. If your auditors relied on the Roles link,
tell them it has gone until the Access item ships.
