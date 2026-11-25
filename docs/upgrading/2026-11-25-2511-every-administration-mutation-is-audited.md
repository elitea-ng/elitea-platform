# Every administration mutation writes an admin audit row (#2511, ADR-0059)

**What changed.** A new test walks every administration mutation (the write
routes of the administration surface, the central override's configuration
writes and the separation matrix of the write paths) and fails when one writes
no row to `centry.audit_events`. It found two gaps, both fixed in this
release:

- `PATCH /api/v2/elitea_core/marketplace_listing/prompt_lib/{projectID}/{listingID}`
  (featuring and delisting a Marketplace listing, a platform administrator's
  action) now writes an admin audit row. The prefix
  `/api/v2/elitea_core/marketplace_listing/` joins the audited surfaces. The
  listing page under it is a GET: a successful read stays unrecorded, and a
  refused one (401 or 403) writes a row, as on every audited surface.
- A mutation on any audited route whose handler fails with an unexpected
  error (a panic) now writes its row, with status 500, or with the status the
  handler had already sent (a client that dropped a response). Before, the
  row was lost.

**Who is affected.** The Audit Trail and the SIEM export show these rows from
the upgrade on. A SIEM rule that counts admin audit rows may see a few more.
Nobody needs to act.

**Rollback.** The previous release writes no row for these two cases; nothing
else changes.
