# Administration: the break-glass elevation ledger (#2515, ADR-0059)

**What changed.** Shared migration `0269_admin_elevations.sql` adds
`elitea_auth.admin_elevations`, the ledger of break-glass elevations: one row
per elevation, from its request to its review. A row holds the person, the
reference of the one browser session it is bound to, the level (`admin` or
`super_admin`), the path (`approved`, `self` or `sealed`), a reason code
(`incident`, `identity_outage`, `recovery`, `other`), the reason text, an
optional ticket, the requester, the approver, the request, decision, start,
expiry and end times (all `timestamptz`), the end reason and the review state.

A trigger makes the table append-only. DELETE and TRUNCATE are refused. An
UPDATE can only fill in the decision, the start, the end and the review, each
of them once, and the review state only moves forward. The person, the session
reference, the reason, the level and the expiry of a row never change. CHECK
constraints hold the rules of ADR-0059 decision 5 in the database as well:
nobody approves their own request, approval switched off never gives the
`super_admin` level, and no elevation lasts longer than 480 minutes (240 on the
sealed path).

**Who is affected.** Every deployment. Nothing existing is changed.

**What to do.** Nothing. The table stays empty until break-glass is used, and
in this release nothing can start an elevation yet: the routes and the sealed
sign-in come in later releases. If you back up or replicate `elitea_auth`,
include the new table. Do not delete its rows by hand. The trigger refuses
that on purpose, because the ledger is the record of who held emergency
access, and when.
