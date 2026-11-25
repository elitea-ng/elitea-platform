# Break-glass access notices: an outbox table and a sender routine (#2519, ADR-0059)

**What changed.** Shared migration `0277_admin_access_notices.sql` creates
`elitea_auth.admin_access_notices`, the outbox of the break-glass notices. A
request, a start, a `critical` change under an elevation, an overdue review,
an elevation left without a reviewer and a review outcome `unauthorized` each
write one row per recipient in the transaction of the event; the new routine
`main.access_notice_sender` (every 30 seconds, keeps running in maintenance)
sends them by e-mail through the existing mailer and its templates. The same
table carries the notice to owners of `inherit` tokens (#2619).

Recipients come from standing roles: for a request, the holders of
`access.breakglass.approve` other than the requester; for the other events,
the holders of the `admin` or `super_admin` role and of
`access.breakglass.review`. The request e-mail links to the approval page
under `DEPLOYMENT_URL` (or the e-mail settings' public base URL when it is
not set).

**The upgrade.** One new, empty table and three indexes. No data change. No
route writes the events yet (they arrive with the request, start and review
stories), so nothing is sent until break-glass is used.

**Who is affected.** Break-glass notices need outbound e-mail. A deployment
that plans to use break-glass configures it (Configuration → E-mail, or the
`SMTP_*` environment). A mailer that is missing or down never blocks an
elevation: the notices stay in the table with `sent_at` NULL, `attempts` and
`last_error`, and are retried with a growing delay (one minute, doubling, at
most one hour); the routine logs how many wait while no mailer is configured.

**Rollback.** The previous release ignores the table; rows written meanwhile
stay unsent until the release that sends them runs again.
