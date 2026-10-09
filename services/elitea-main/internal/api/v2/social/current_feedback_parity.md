# Current Social Feedback parity evidence

This slice covers project-scoped feedback creation and listing. Both are
mounted under `/social` (`Handler.Routes`) behind the project-membership gate
and an RBAC gate; the list is also mounted at
`/elitea_core/feedbacks/default/{project_id}`. The earlier tenant-table
`ListFeedbacks`/`CreateFeedback` handlers, which read and wrote a
`p_N.social_feedbacks` that no tenant migration creates, are removed.

## Current-baseline evidence

The audited source snapshots are:

- Social `996b55c95a9469808a33d8b4ad11656917027b54`
  - `api/v2/feedbacks.py`
  - `models/pd/feedbacks.py`
  - `models/feedbacks.py`
- Shared `cb9633806aab2ff37c0be360915509d7a265ad68`
  - `tools/api_tools.py`
- Auth `ff02d66a8858604e6947bb3a52bda8543dbe0e76`
  - `module.py`

`API.url_params = with_modes(["", "<int:project_id>"])` exposes both safe
project-scoped forms:

- canonical UI form: `POST /api/v2/social/feedbacks/default/{project_id}`
- implicit-default alias: `POST /api/v2/social/feedbacks/{project_id}`

The two no-project variants exposed by generic Pylon URL generation are not in
this slice. Without a project identifier they cannot preserve the required
project-membership authorization boundary.

## Behavior matrix

| Concern | Current Python behavior | Go slice |
| --- | --- | --- |
| mode | `default` handler only | exact |
| permission | `models.social.feedbacks.create` | exact |
| recommended project roles | admin, editor, viewer | exact when the persisted role grants the permission |
| authorization order | decorator resolves project permissions before `request.get_json()` | exact and tested with a body that fails the test if read |
| author | `auth.current_user()["id"]` overwrites client `user_id` | active server-validated owning user overwrites client fields; PAT row IDs cannot become authors |
| media type | `request.get_json()` requires JSON, including `application/*+json` | exact |
| input | required description and rating 0..5; unknown fields ignored | same valid EliteaUI contract; unknown fields ignored |
| request metadata | user agent comes from request; `Referer` overwrites body referrer when present | request user agent and `Referer` only |
| storage | one row in shared `centry.social_feedbacks`; no project column or tenant schema | same table, plus a nullable `project_id` (migration `shared/0158`) set from the path; no tenant schema |
| success | `201 {"id": <row id>}` | exact |
| membership at write | route decorator only | route gate, and the INSERT repeats the membership check (and project existence) so a revoked member cannot write; no row returned maps to 403 |
| insert atomicity | one SQLAlchemy transaction | one PostgreSQL statement transaction; error returns no row |

The persisted RBAC tables remain authoritative. Role names do not grant access
by themselves, and the Go route does not cache permissions. Active principal,
active project, project membership, and the exact permission are checked before
the body is allocated or decoded.

## List semantics

`GET .../feedbacks/default/{project_id}` (alias `.../feedbacks/{project_id}`)
requires `models.social.feedbacks.list` and answers
`{"total": <all visible rows>, "rows": [...]}`, paged by `limit` (1..200,
default 50), `offset` (0..100000), `sort_by` (`id` | `created_at`) and
`sort_order` (`asc` | `desc`); any other query key or value is a 400. One
statement checks membership and returns the count and page from one snapshot.
A row is visible when the caller is a member and either the row belongs to the
project (only the caller's own rows in the public project) or the row has no
project and the caller wrote it (legacy rows keep a NULL `project_id`).

## Deliberate security deltas

- The legacy list returned every row of the shared table, across all projects
  and users, to any viewer. That is not ported: a project's list shows that
  project's rows only, and a legacy row shows only to its author.
- With no permission resolver configured the feedback routes answer 503
  rather than skipping the RBAC check.

These deltas preserve all valid EliteaUI requests while rejecting ambiguous or
untrusted inputs accepted by Pydantic v1 coercion:

- The body is capped at 64 KiB.
- Project IDs must be positive canonical base-10 integers.
- Rating and description use strict JSON types. Numeric strings, booleans,
  fractional ratings, and numeric descriptions are rejected rather than
  coerced.
- Validation errors use the bounded envelope
  `{"error":"invalid request"}` instead of reflecting Pydantic field details.
- A body-supplied `referrer` is always ignored. The current Python route leaves
  it intact when the `Referer` header is absent; accepting that forged field is
  not retained.
- Database failures use `{"error":"internal server error"}` and do not expose
  SQL or protected data.
- Forwarded identity is accepted only from a verified ingress peer and is
  revalidated against active PostgreSQL user/token state.

## Verification

`feedback_test.go` provides unit/component coverage for:

- both project-scoped paths and method rejection;
- exact RBAC mode and permission;
- server-derived author and request metadata;
- authorization and tenancy before body read;
- bounded parsing, strict validation, exact success/error envelopes;
- storage failure and incomplete composition.

`feedback_postgres_integration_test.go` crosses the in-process HTTP,
principal validation, current PostgreSQL RBAC resolver, and real PostgreSQL
storage boundaries. It proves:

- active PAT owner attribution rather than token-row attribution;
- admin/editor/viewer success from persisted grants;
- missing, untrusted, suspended, wrong-permission, cross-project, suspended
  project, and invalid-project denial;
- denied bodies are not read;
- shared-table shape and exact stored values;
- no partial feedback row after a constraint-induced statement failure.

`feedback_list_postgres_integration_test.go` adds the write-then-list round
trip, the authorization matrix on both path forms (owner, viewer, foreign
project, no project, no permission, unauthenticated), in-statement refusal of a
revoked member, project isolation, legacy-row and public-project visibility,
pagination bounds and the empty project. `migrations/` carries the 0158
fresh-install and legacy-rows tests.

## Remaining cutover gates

- Add sqlc compiler input/query generation from the authoritative deployed
  `centry.social_feedbacks` schema. This isolated slice was intentionally
  restricted from editing shared schema/query/generated-code files.
- Add the route to generated API documentation/telemetry with the same
  visibility as the current `register_openapi` declaration.
- Run a signed-in browser checkpoint through the existing feedback dialog and
  verify the stored row on the deployment.
