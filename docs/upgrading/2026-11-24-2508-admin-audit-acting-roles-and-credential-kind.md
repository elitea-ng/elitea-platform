# Admin audit rows record the acting roles and the credential kind (#2508, ADR-0059)

**What changed.** Shared migration `0268_admin_audit_role_use.sql` adds seven
nullable columns to `centry.audit_events`:

| Column | Type | Meaning |
|---|---|---|
| `acting_roles` | `text[]` | The caller's standing administration roles at request time. `{}` for a credential that acts with none. |
| `elevation_id` | `uuid` | The break-glass elevation the request was made under. Always NULL until break-glass ships. |
| `credential_kind` | `text` | `session`, `form`, `sealed`, `token`, `native`, `run`, `system` or `scim`. NULL when the kind is unknown. |
| `central_override` | `boolean` | The write went through the central override. Not produced yet. |
| `reason_code`, `reason` | `text` | The reason given for a dangerous or critical action. Not produced yet. |
| `reason_missing` | `boolean` | A required reason was not given. Not produced yet. |

From this release, every audit row of a request that checked administration
permissions fills `acting_roles` and `credential_kind`. A SCIM client's rows
carry `credential_kind` `scim`. Every other row, and every row written before
the upgrade, keeps NULL.

**The upgrade.** The columns are added with no default, so PostgreSQL changes
only the catalogue and does not rewrite the table, however large it is. The
role names come from the permission check the request already made, so no
query is added per request.

**Not in the chain hash yet.** The admin audit chain (shared 0206) does not
cover the new columns, and the append-only guard does not protect them. The
chain still verifies every row written before and after the upgrade. A later
release (ADR-0059 R1.4b, #2576) moves the columns into a versioned chain form
and into the `elitea.audit.v1` export. Until then the columns are not
tamper-evident, and the SIEM export does not carry them.

**Who is affected.** Nobody needs to act. A tool that reads
`centry.audit_events` with `SELECT *` gets seven more columns.

**Rollback.** The previous release does not name the columns and keeps
working. Its rows leave them NULL.
