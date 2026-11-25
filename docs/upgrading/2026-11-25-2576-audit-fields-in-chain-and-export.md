# Admin audit role-use fields are in the chain hash and the SIEM export (#2576, ADR-0059)

**What changed.** Shared migration `0276_audit_events_chain_form_v2.sql`
versions the canonical form of the admin audit chain (`centry.audit_events`).
Rows chained from the upgrade on are hashed in form `elitea.audit_events.v2`,
which covers the role-use columns of #2508 and #2522: `acting_roles`,
`elevation_id`, `credential_kind`, `central_override`, `reason_code`,
`reason_missing` and `elevation_flagged`. On those rows the columns are
tamper-evident: the append-only guard refuses a change, and a change made past
it breaks the chain verification. The reason text (`reason`) is never hashed,
so it stays erasable.

Rows chained before the upgrade keep form `elitea.audit_events.v1`. Their
links are not rewritten and they verify as before; their role-use columns
stay outside their hash. The new table
`elitea_runtime.audit_events_chain_forms` records the first chain position of
form v2, and the verification picks the form by the row's chain position, so a
chain that spans the upgrade verifies end to end.

The `elitea.audit.v1` SIEM export carries the same fields in the `details` of
an `admin_audit` event, after `is_error`; each is omitted when it was not
recorded. The reason text is never exported. `schema` stays `elitea.audit.v1`:
the fields are optional additions.

**The upgrade.** No table rewrite and no data change: the migration creates
one small table and replaces four functions. It holds the chain head lock for
the length of its transaction, so admin audit inserts wait for a moment.

**Who is affected.** A SIEM mapping of `admin_audit` events may add the new
`details` fields (the Audit event schema reference page lists them). A consumer that ignores unknown fields, as the schema asks, needs
no change. Nobody needs to act on the database.

**Rollback.** The previous release's binaries keep writing rows; the database
function hashes them in form v2, which verifies. Restoring the previous
database functions without the cut-over table would make every v2 row fail
verification, so do not revert the migration by hand.
