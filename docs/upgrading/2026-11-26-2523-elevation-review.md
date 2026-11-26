# Break-glass: the review after use and the revert list (#2523, ADR-0059)

**What changed.** Every elevation is reviewed after it ends, by someone who
was not involved:

- When an elevation ends, the platform records who may review it: every
  holder of `access.breakglass.review` through a standing role (the
  `security_auditor` role by default) other than the person elevated, the
  requester and the approver. That record decides, not the current roles.
- `GET /api/v2/admin/access/elevations/{elevationID}/review` reads the
  review: the elevation, its reviewers, the changes made under it (flagged
  ones first) and, once written, the attestation.
  `POST` on the same path writes the attestation: an outcome, `justified` or
  `unauthorized`, and a note. The review of a sealed elevation also says
  whether a replacement credential was issued. The attestation changes no
  platform state. Its `admin.elevation.reviewed` security event carries the
  outcome, never the note.
- An `unauthorized` outcome notifies as an activation does and opens a
  revert list of the elevation's flagged changes:
  `GET /api/v2/admin/access/reverts` lists it and
  `POST /api/v2/admin/access/reverts/{itemID}/handled` marks an entry done,
  for holders of `access.breakglass.manage`.
- While a review is pending, taking the review permission from one of its
  reviewers, suspending or deleting them needs `admin.auth.users.super_admin`
  (`403 elevation_pending_reviewer` otherwise). The change is listed in the
  review, and the person can still review.

Only a browser session calls the new routes. The shared migration
`0283_admin_elevation_items.sql` adds the table
`elitea_auth.admin_elevation_items` (append-only, except for marking a
revert entry handled). For an elevation already awaiting review it records
the reviewers from today's standing roles; it changes no existing row.

**Who is affected.** Holders of the `security_auditor` role now write review
attestations. Administrators without `super_admin` can no longer remove,
suspend or delete a person while that person is a reviewer of a pending
review.

**What to do.** Nothing for the upgrade. See
[Break-glass access](../../apps/elitea-docs/src/content/docs/admin/break-glass.mdx).
