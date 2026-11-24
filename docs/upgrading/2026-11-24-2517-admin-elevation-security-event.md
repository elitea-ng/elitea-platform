# Security audit: break-glass elevation steps are `admin.elevation` events (#2517, ADR-0059)

**What changed.** A new security audit event type,
`ai.elitea.security.v1.admin.elevation` (OCSF class 3005, User Access
Management), records each step of a break-glass elevation. The elevation
ledger (`elitea_auth.admin_elevations`, shared 0269) writes the event for
`started`, `ended` and `expired` in the same transaction and under the same
savepoint as the ledger row. If the event cannot be appended, the elevation
does not start or end. The event carries ids, codes and numbers: the reason
code but never the reason text, and a review note's id and length but never
its text.

The audit export (`elitea.audit.v1`) carries the step under
`details.elevation`. The schema in `libs/jsonschema/audit/v1/event.schema.json`
gains the `elevationStep` definition. The full list of fifteen actions is
in the audit event schema reference. The other twelve actions are written as
the request, review, sealed-credential and local sign-in features arrive.

**Who is affected.** Deployments that consume the security audit export or
the OCSF stream. Nothing can start an elevation in this release, so no event
of the new type is written until a later release adds the request routes or
the sealed sign-in.

**What to do.** If your SIEM validates exported events against a copy of
`elitea.audit.v1`, update that copy so `details.elevation` is accepted. To
alert on break-glass use, filter on the `ai.elitea.security.v1.admin.elevation`
type. No migration and no configuration change is needed.
