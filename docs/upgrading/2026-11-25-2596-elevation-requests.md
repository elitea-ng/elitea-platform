# Break-glass: elevation requests, approvals and denials (#2596, ADR-0059)

**What changed.** Six new administration routes under
`/api/v2/admin/access/elevations` let a person ask for a break-glass
elevation and a second person answer it:

- `POST /api/v2/admin/access/elevations` asks, with a reason code, a reason,
  an optional ticket and a duration (the Break-glass section's online
  default when absent, never above its cap). Every holder of
  `access.breakglass.approve` other than the requester is told by e-mail.
- `POST /api/v2/admin/access/elevations/{elevationID}/approve` and `/deny`
  answer. The level is the approver's: `super_admin` for an approver who
  holds `super_admin` by a standing role, `admin` otherwise. Nobody answers
  their own request, and nobody under an elevation answers at all. An answer
  after the request lifetime (30 minutes by default) answers `410` and
  records the expiry.
- `GET .../mine` (the caller's latest request), `GET .../pending` (the
  approval queue) and `GET .../elevations` (the history).

Only a browser session calls them: a personal access token, a native
session, a run token and a sealed session get `403
elevation_credential_refused`. With approval switched off and its delay
over, a request is approved at once by the platform at the `admin` level.

Nothing starts an elevation yet: the start and the end come in a later
release, so an approved request grants nothing on its own.

The shared migration `0280_admin_elevation_requests.sql` adds the requested
duration and the denial reason code to `elitea_auth.admin_elevations`, two
indexes, and the reason code to the ledger's append-only trigger. It changes
no existing row.

**Who is affected.** No existing caller: every route is new. Holders of
`access.breakglass.approve` (the `admin` and `super_admin` roles by default)
start to receive request e-mails once people request elevations.

**What to do.** Nothing for the upgrade. Before you rely on break-glass,
configure outbound e-mail, so that approvers hear of requests. See
[Break-glass access](../../apps/elitea-docs/src/content/docs/admin/break-glass.mdx).
