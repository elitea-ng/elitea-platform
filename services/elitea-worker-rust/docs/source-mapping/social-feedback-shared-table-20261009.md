# Social feedback in the shared table (2026-10-09)

Branch `fix/main-social-feedback-shared-table`, on `origin/main` after #1175 (access hardening) merged. Component:
Go Main (`services/elitea-main`). The generated Web client is regenerated from the changed OpenAPI. The Worker is not
changed; this file lives here per the source-mapping rule.

## Defect

On merged `main` `0def77b22`, stack `elitea-verify-0def77b2`, `GET /api/v2/social/feedbacks/default/{project}`
answered 500 `{"error":"failed to list feedback"}` for projects 1 and 2. Main logged `relation
"p_2.social_feedbacks" does not exist (SQLSTATE 42P01)`.

- The list handler read `p_N.social_feedbacks`, which no tenant migration creates. Only legacy-carried schemas
  (`p_118`, `p_119`) still hold that table.
- The mounted POST wrote the same missing table.
- The audited write slice (`repos/social_feedbacks.go`) and the current platform use the shared
  `centry.social_feedbacks`. That slice was never mounted.
- The `elitea_core` twin read the same missing table but swallowed the error, so its 200 was an empty list that hid
  the fault.
- No migration in this repository created `centry.social_feedbacks`, so a fresh install had no table at all.

## Business behaviour

Taken from the current platform (pylon `social` plugin: `api/v2/feedbacks.py`, `models/feedbacks.py`,
`rpc/feedbacks.py`):

- Storage is one shared table, `centry.social_feedbacks`: `id, user_id, referrer, description, rating, user_agent,
  created_at`. It has no project column and no tenant table.
- The write overwrites the author with the caller, takes the user agent and `Referer` from the request, requires
  `description` and `rating` 0..5, and answers `201 {"id": n}`. The permission is `models.social.feedbacks.create`.
- The list permission is `models.social.feedbacks.list`, which default-mode admin, editor and viewer hold. The
  response envelope is `{"total": n, "rows": [...]}` with a real count, plus `offset`, `limit`, `sort_by` and
  `sort_order`.
- `with_modes(["", "<int:project_id>"])` also exposes the implicit-default alias `/social/feedbacks/{project}`.
- Callers: EliteaUI's `FeedbackDialog` is the only caller and is commented out (`Sidebar.jsx:49`). The new Web has
  generated client code only. 👍/👎 on a chat message is a different feature
  (`/elitea_core/message_feedback/...`, `chat_message_feedback`) and is not changed here.

Deliberately not ported:

- **Unscoped listing.** The legacy GET ignores the project in the path and returns every row of every user and every
  project to any viewer. Listing is now scoped (decision taken with the user):
  - a project's listing shows the rows submitted in that project;
  - legacy rows, which have no project, appear only to their own author;
  - the public project (`publicproject.ID()`) lists only the caller's own rows.
- **Arbitrary query keys.** Any query key used to be an equality filter, and an unknown key raised a 500. Only
  `limit` (1..200, default 50), `offset` (0..100000), `sort_by` (`id`|`created_at`) and `sort_order` (`asc`|`desc`)
  are accepted. Any other key, a repeated key or a malformed pair gives 400.
- **No empty tenant table.** No `p_N` table is created. An empty one would have hidden the existing shared rows.

## Changed paths and enforcing code

| Rule | Enforced at |
| --- | --- |
| Shared table exists in the legacy shape; nullable `project_id`; list indexes; list grant where `.create` is granted. Existing rows kept, no tenant table | `services/elitea-main/migrations/shared/0158_social_feedbacks_project.sql` |
| Write stamps the project. The INSERT repeats membership (`projectaccess.Membership`) and project existence; no row means forbidden | `internal/infra/db/repos/social_feedbacks.go:28`, `:171` |
| One-statement list: membership, visibility rule, real total and one page in one snapshot. ORDER BY comes from four constant clauses | `social_feedbacks.go:76`, `:186`, `:211` |
| Page bounds also enforced in the repository | `social_feedbacks.go:21`, `:186` |
| Strict query parsing (`url.ParseQuery`, key allowlist, ranges) | `internal/api/v2/social/feedback.go:215`, `:271` |
| Public project lists own rows only | `feedback.go:283` |
| Body cap 64 KiB, strict JSON media type, author from the principal | `feedback.go:117`, `:141` |
| GET/POST on `/feedbacks/default/{projectID}` and `/feedbacks/{projectID}`, behind the membership gate and an RBAC gate (`.list` / `.create`) | `internal/api/v2/social/handler.go:126-128` |
| RBAC gate fails closed (503) without a resolver | `feedback.go:322` |
| Production wires the resolver; the elitea_core twin serves the same listing behind `projectScoped` + `.list` | `internal/api/router.go:4330`, `:3818` |
| Contract | `api/openapi/v2.yaml` (`/social/feedbacks/default/{project_id}`), regenerated `internal/api/generated/api.gen.go` (oapi-codegen v2.7.2) and `apps/elitea-web/src/shared/api/generated/**` (orval 8.33.0) |

Removed: `social.Handler.ListFeedbacks`, `social.Handler.CreateFeedback` and `logTenantReadFault` (tenant table),
and `eliteacore.Handler.Feedbacks`, which swallowed the error.

## Tests

Local runs on 2026-10-09 against `pgvector/pgvector:0.8.1-pg18-trixie`, the same image as CI, via
`ELITEA_TEST_DATABASE_URL`, after the rebase onto `origin/main`.

| Area | Tests | Result |
| --- | --- | --- |
| Write then list (real PG, HTTP) | `TestFeedbackWriteThenListRoundTrip`: POST as a member, GET it back (fields, `project_id`, ISO `created_at`, total), row present in `centry.social_feedbacks` | pass |
| Authorization matrix (real PG, real `legacyrbac` resolver) | `TestFeedbackAuthorizationMatrix`: owner/admin, viewer member, foreign-project member, user with no project, member without the permission, unauthenticated, for GET and POST on both path forms; refusals write no row | pass |
| In-statement authorization | `TestFeedbackStatementsRefuseANonMemberThemselves`: repository refuses a non-member for list and insert; membership revoked after the gate is refused; administrator cannot insert into an absent project | pass |
| Platform administrator | `TestFeedbackPlatformAdministratorPassesTheSQLButNotTheDefaultModeGate`: the real resolver refuses a super_admin with no project role (403, as on every default-mode route); with a grant-all resolver the SQL admits them | pass |
| Isolation and legacy rows | `TestFeedbackIsolationAndLegacyRows`, `TestFeedbackPublicProjectListsOnlyTheCallersOwnRows` | pass |
| Empty project | `TestFeedbackEmptyProjectListsNothing`: 200 `{"total":0,"rows":[]}` | pass |
| Bounds | `TestFeedbackPaginationSortAndBounds`: limit 0/1/200/201, offset -1/100000/100001, unknown, repeated and malformed keys; total independent of the page | pass |
| Fail closed | `TestFeedbackRoutesFailClosedWithoutAResolver`, `TestFeedbackHandlerWithoutPoolIsUnavailable` | pass |
| Statement budget | `TestCurrentSocialFeedbacksRepositoryListUsesOneConstantOrderedStatement`: one round trip per list; invalid input issues no SQL | pass |
| Migration (real runner) | `migrations/social_feedbacks_project_postgres_integration_test.go` (2): fresh DB gets the table and indexes; a legacy-shaped table with rows keeps them with NULL `project_id`; re-apply is a no-op | pass |
| Pre-existing suites updated deliberately | shared-table shape (now nullable `project_id`), absent-project 404/403 ordering, route surface (+2 alias routes), project-access matrix, manifest head 158, remaining-grants count 44 | pass |

Packages run with real PG and `-count=1`, all passing:

- `./internal/api/v2/social/...`
- `./internal/api/`
- `./internal/api/v2/eliteacore/...`
- `./internal/infra/db/repos/`
- `./internal/infra/db/migrate/...`
- `./internal/infra/db/projectaccess/...`
- `./migrations/...`

Other checks:

- `go build ./...` and `go vet` on the changed packages are clean.
- Web: `npm run typecheck` is clean, and `check-generated-client.mjs` and `check-endpoint-manifest.mjs` pass.
- The full repository Go suite was not run.

## Performance

- **Mechanism:** one statement and one round trip per list: membership, count and page in one snapshot
  (`social_feedbacks.go:76`). The insert is one statement. The project listing uses
  `social_feedbacks_project_id_idx (project_id, id)`. Legacy author rows use the partial index
  `social_feedbacks_legacy_author_idx (user_id, id) WHERE project_id IS NULL`. A page holds at most 200 rows.
- **Proving test:** `TestCurrentSocialFeedbacksRepositoryListUsesOneConstantOrderedStatement`.
- **Measured:** no load benchmark was run. The table is one row per human submission.

## Durability

- **Mechanism:** the write is a single INSERT whose authorization predicate is in the same statement, so there is
  no partial state. Legacy rows are untouched by 0158: it uses `IF NOT EXISTS` and adds a nullable column.
- **Proving tests:**
  - `migrations/social_feedbacks_project_postgres_integration_test.go`;
  - `TestCurrentFeedbackCreateHTTPPostgresParityAndNegativeSecurity`, which shows no partial row after a constraint
    failure.
- **Measured:** on the real-model DB, both pre-0158 fixture rows survived the migration with `project_id` NULL.

## Resilience

- **Mechanism:**
  - every input is bounded: body 64 KiB, limit, offset, and a key allowlist;
  - failures are typed and readable: 400 `invalid request`, 401, 403 `forbidden`, 404 for an absent project, 413,
    415, 500 `failed to list feedback` with a server-side log, and 503 when the resolver or pool is missing;
  - there is no silent coercion: `rating` and `description` use strict JSON types.
- **Proving tests:** `TestFeedbackPaginationSortAndBounds`, `TestCurrentFeedbackCreateRouteValidatesBoundedBody`,
  `TestFeedbackRoutesFailClosedWithoutAResolver`.

## Security

Each category in `rules/security.md`, and whether it applies to this change:

- **Trust boundaries and identity:** applies. The author is `principal.OwningUserID()` from the verified session or
  PAT. A body `user_id` or `referrer` is ignored. No new identity source.
- **Authorization (object level):** applies.
  - Membership gate → RBAC `.list`/`.create` → the same membership decision inside the statement (fails closed).
  - Listing is scoped to the project (and legacy rows to their author), where legacy leaked across tenants.
  - Proven by the matrix and in-statement tests above.
  - A non-member reads 403 before any existence signal (404 only after membership), as already proven by
    `TestFeedbacksAnswer403ForANonMemberOfAnAbsentProject`.
- **Input and amplification:** applies. Body and page caps, strict query and JSON parsing. No structured
  expansion.
- **Injection:** applies. SQL is parameterized. The only interpolated fragments are constant ORDER BY clauses and
  `projectaccess.Membership` output. No tenant schema name is built any more.
- **XSS:** not applicable. JSON only; no rendering change.
- **Egress / SSRF:** not applicable.
- **Secrets:** applies (scan only). The diff was scanned before commit (token, key and credential patterns): no
  findings.
- **Supply chain:** no dependency change.
  - `govulncheck ./...` on `elitea-main` reports only Go standard-library advisories of the local `go1.26.5`
    toolchain (fixed in 1.26.6). They are pre-existing and unrelated to this diff.
  - `npm audit` was not run: no Web dependency changed.

## Recovery guarantees

| Component × phase | Class | Evidence |
| --- | --- | --- |
| Main × admission (feedback write) | I | One INSERT whose authorization predicate is in the statement. A crash before commit leaves no row; after commit the 201 carries the id. A blind client retry creates a second submission, as in legacy (feedback is append-only). `TestCurrentFeedbackCreateHTTPPostgresParityAndNegativeSecurity` |
| Main × output delivery (feedback list) | R | Read-only, one snapshot. A retried GET after a Main restart returns the same rows. Browser reload evidence below |
| PostgreSQL × migration 0158 | I | Idempotent DDL plus `ON CONFLICT DO NOTHING` grants. Re-apply is a no-op (migration test) |

No touched row is L.

## Real-browser evidence

Run on 2026-10-09 in the Claude desktop browser pane (Chromium). Real backend, no response mocks.

- **Stack:** standalone compose project `elitea-socialfb`, front door `http://socialfb.localhost:18140`.
  - The OIDC redirect was moved to that host because a session cookie on plain `localhost` was being replaced by
    another local stack's login (`session_unknown` in Main's log).
  - Worker `rust`. The stack was torn down after recording.
- **Database:** restored from the real-model product dump at `main` `0def77b22` (shared 157, tenant 148).
  `elitea-migrate` then applied 0158 (ledger: shared 158).
- **Images built from this branch** (`98fca6167`, rebased on `origin/main` `1ab920dde`):
  - `elitea-main` `sha256:e1fdf3db2b7e…`, binary sha256 prefix `bac3294a5f86de7a`. It was extracted with
    `docker create` + `docker cp` and contains `feedback authorization unavailable` (main) and
    `0158_social_feedbacks_project` (main and `elitea-migrate`), both unique to this branch.
  - `elitea-web` `sha256:e4eafbc43dba…`.
  - Every other service used the `main-0def77b22-verify` images.
- **Fixtures (stated honestly):** before 0158, two legacy-shape rows were inserted with `psql`, with no project:
  - id 1, by user 3 (`admin@centry.user`);
  - id 2, by user 6 (`rust-mcp-restricted-20260913@centry.user`).

  Everything else was created through the UI or the signed-in browser session.

Steps:

1. **👍 on a chat message.**
   - Signed in as `admin@centry.user` (user 3) through the mock OIDC page.
   - In project 2 ("Private") a new chat with `gpt-5.4-mini` answered "pong" (conversation 869).
   - Clicking "Like this answer" sent `POST /api/v2/elitea_core/message_feedback/prompt_lib/2/a6123b2f-…` → 200, and
     the thumb showed "Likes: 1".
   - This is the message-feedback feature, which is unchanged.
2. **Submit and list from the same signed-in session.**
   - `POST /api/v2/social/feedbacks/default/2` `{description, rating: 5}` → 201 `{"id":3}`.
   - `GET /api/v2/social/feedbacks/default/2?sort_order=desc` → 200 `{"total":2,…}`, returning:
     - row 3 (`project_id` 2, the browser user agent);
     - row 1 (the caller's own legacy row, `project_id` null).
   - User 6's legacy row 2 is absent.
   - The same rows came from `GET /api/v2/elitea_core/feedbacks/default/2` (the twin, previously a masked empty 200)
     and from the alias `GET /api/v2/social/feedbacks/2?limit=1` (`total` 2, one row).
   - Project 118 and the public project 1 list only the caller's legacy row (total 1).
   - `?limit=0` → 400. Without credentials → 401.
3. **Reload.**
   - Reloaded `/app/chat/869`: the thumb is still selected (`{"likes":1,"dislikes":0,"mine":{"rating":1}}`).
   - Listing project 2 again, and opening the API URL directly in the tab, returned the same two rows.
4. **Foreign actor.**
   - Signed in as user 6, who belongs to no project.
   - GET project 2 → 403, elitea_core twin → 403, POST project 2 → 403, GET project 118 → 403.
   - `centry.social_feedbacks` still held exactly rows 1–3 afterwards.
5. **Main log:** no `social_feedbacks_list` / `failed to list feedback` / tenant-schema errors during the run. The
   tenant `social_feedbacks` tables present (`p_118`, `p_119`) are the same as on the reference stack. None was
   created.

## Follow-ups

- `projectaccess.Membership` (shared with the project gate since #1175) does not check project or user suspension.
  The route's RBAC resolver does, but the in-statement repeat does not.
- The list returns other members' `referrer` and `user_agent` to any project member. This is legacy parity, now
  project-scoped. Consider trimming them for viewers.
- `internal/infra/db/migrations/001_initial.sql` (the dev/E2E bootstrap) still creates the unused tenant
  `p_N.social_feedbacks` in the entity shape. Nothing reads it now. Remove it in a bootstrap clean-up.
- The standalone `CurrentFeedbackCreateRoute` is still kept for its parity suites. It shares the handler with the
  mounted route; fold the suites onto the mounted route and delete it.
- The alias `/social/feedbacks/{projectID}` is routed but not described in `v2.yaml`. The spec's info block still
  names the removed `CreateFeedback` 500 site. Rewording it touches every generated Web file.
- The elitea_core twin has no real-PostgreSQL test through its own mount. It is covered by the route-surface and
  project-access matrix tests and by the browser run above.
- Migration number 0158 was free on `main` at push time. It is assigned at merge.
