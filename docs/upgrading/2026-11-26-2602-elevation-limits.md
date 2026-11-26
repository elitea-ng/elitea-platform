# Break-glass: what an elevation can no longer do (#2602, ADR-0059)

**What changed.** A person whose access comes from a break-glass elevation,
rather than from their standing roles, gets `403` for these requests:

- Demoting, suspending or deleting anyone who holds `access.breakglass.approve`
  or `access.breakglass.review`. The code is `elevation_protected_person`.
- Changing their own roles. The code is `elevation_restricted`.
- Writing a gateway governance (guardrail) rule:
  `POST`, `PUT` and `DELETE /api/v2/admin/gateway/governance…`. The code is
  `elevation_restricted`.
- Deleting a project: `DELETE /api/v2/projects/project/{mode}/{projectID}`.
  The code is `elevation_restricted`.

The last two are refused because they cannot yet be announced in the same
transaction as the change. Every other `critical` change an elevation makes
is now announced as it happens, with an `admin.elevation.critical_change`
security event and a notice to the administrators and auditors. If that
announcement cannot be written, the change is rolled back.

**Who is affected.** Only people acting under an elevation. A person whose
standing roles already allow these actions is not affected.

**What to do.** Nothing for the upgrade. During an incident, a governance
rule or a project delete needs a person who holds the permission in a
standing role. See
[Break-glass access](../../apps/elitea-docs/src/content/docs/admin/break-glass.mdx).
