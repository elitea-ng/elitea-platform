# Main hardening residuals: save-time YAML budget, legacy edge auth, membership suspension, private egress entries (2026-10-09)

Branch `fix/main-hardening-residuals`, cut from `origin/main` 58abb650c and rebased onto 2b55242fd. Components: Go Main (`services/elitea-main`),
the shared Rust `elitea-agent-runtime` test suite (test only), a shared fixture under `testdata/`, and one Helm
values comment. The Worker runtime is not changed; this file lives here per the source-mapping rule.

Hardens SEC-11, SEC-12, SEC-14 and SEC-10 (follow-ups F8, F9, F12, F11). Finding details are kept out of this
repository. SEC-13 (toolkit test-connection egress) is out of scope and waits for a decision.

## Business behaviour

Taken from the current platform and kept:

- Pipeline create, version create/update, agent-type change, import, fork and the MCP instruction patch keep their
  request and response shapes; the existing 512 KiB / 128-node refusals are unchanged.
- `GET /auth` keeps answering a token credential check with 200 or 403 and the same headers and body.
- Project-scoped social reads and writes (authors, trending authors, likes, pins, feedback) keep their shapes and
  member / administrator semantics for active accounts and projects.
- Webhook, MCP, MCP OAuth/DCR and Code workspace egress keep their allowlist grammar (egresslib) and their refusal
  text.

Changed on purpose:

- **SEC-11.** A pipeline definition whose anchor/alias expansion exceeds the budget the Worker parses it under
  (`PIPELINE_YAML_BUDGET`: 131 072 nodes, 1 MiB scalar bytes, depth 64, plus the parser's 100-replays-per-event
  guard) is refused at save with a typed 400 `PIPELINE_YAML_EXPANSION_TOO_LARGE`, on every save path. Before, it was
  stored and refused only when a run started.
- **SEC-12.** The legacy `GET /auth` handler no longer emits an `X-Auth-*` identity projection. A `target` mapper
  parameter (any value) is refused with 403. The signed projection from `/internal/auth/main` is the only one Main
  trusts. No edge in `deploy/` used the legacy projection (every forward-auth address is `/internal/auth/main`).
- **SEC-14.** The shared in-statement membership predicate refuses a suspended user and a suspended project, the
  administrator branch included, matching the default-mode RBAC resolver. A NULL flag fails closed.
- **SEC-10.** A private egress allowlist entry permits only its own range: a CIDR its block, an IP literal that
  address, `localhost` loopback, each only on the entry's port when it pins one. Host-name and wildcard entries
  permit no private address. Before, any private entry permitted every private class for the path.

Not ported: nothing from the current platform; these are platform-only hardening changes.

## Changed paths and enforcing code

| Item | Enforcing code |
| --- | --- |
| SEC-11 budget and typed refusal | `services/elitea-main/internal/domain/pipelinelimits/expansion.go:38` (`ErrExpansionTooLarge`), `:59` (`checkExpansion`), `:70` (`parserEvents`), `:85-88` (metered walk, alias replay cap) |
| SEC-11 wiring (all save paths share `check`) | `internal/domain/pipelinelimits/limits.go:119`; `Refusal` recognises the new error so start-time admission reports it too |
| SEC-11 shared boundary cases | `testdata/pipeline-yaml-budget/generate.py`, `cases.json` (11 cases) |
| SEC-12 no projection | `internal/api/v2/auth/edge_auth.go:145-157` (`writeSuccess`: no `X-Auth-*`, `target` refused) |
| SEC-14 predicate | `internal/infra/db/projectaccess/projectaccess.go:31-37` |
| SEC-10 per-entry permits | `internal/infra/egress/classify.go:131` (`privatePermits`), `:170` (`canonicalIP`), `:182` (`canonicalBlock`); `guard.go:112`, `:145` and `:153` (Validate with the URL port), `:231` (dial with the dialled port), `:258`/`:275` (one filter for both) |
| Docs | `services/elitea-main/docs/browser-auth-urls.md` (`/auth` row), `deploy/helm/elitea/values.yaml` (egress upgrade note) |

## Tests

All run against a throwaway PostgreSQL 18 (pgvector image) via `ELITEA_TEST_DATABASE_URL`.

| Suite | Result |
| --- | --- |
| `go test ./...` in `services/elitea-main` (with PostgreSQL) | 195 packages ok, 22 without tests, 0 failures (after the two fixture fixes below) |
| New and changed tests (`-json`, top-level) | 111 pass, 0 skip, 0 fail |
| `cargo test -p elitea-agent-runtime --lib bounded_yaml` | 12 pass (incl. `shared_pipeline_budget_cases_match_mains_save_check`) |
| `cargo clippy -p elitea-agent-runtime --all-targets -D warnings` | clean |

New or changed proving tests:

- SEC-11: `pipelinelimits/expansion_test.go` (`TestExpansionBudgetMatchesTheWorker` pins the numbers to the Rust
  constant; `TestExpansionBudgetSharedCases` limit and limit+1 for nodes, scalar bytes and depth, alias-reached depth,
  self-reference, parser replay guard at n=3/n=4, alias bomb; `TestExpansionBoundaryCasesSitExactlyOnTheLimit`;
  `TestExpansionRefusesALargeBombWithinTheBudget`), `expansion_internal_test.go` (meter allocates 0),
  `api/v2/applications/pipeline_expansion_postgres_integration_test.go` (create, version create, update with stored
  and body type; at-limit saves; refused writes store nothing), `api/v2/eliteacore/pipeline_expansion_postgres_integration_test.go`
  (import and fork), `api/v2/mcp/internal_applications_pipeline_expansion_postgres_integration_test.go` (instruction
  patch: refused, no change, no backup), Rust `bounded_yaml_tests.rs` (same 11 cases, same verdict and limit kind).
- SEC-12: `api/v2/auth/edge_auth_test.go` (`TestEdgeAuthSuccessTargetContract`: every target refused, no `X-Auth-*`
  on any answer; `TestEdgeAuthLegacyRouteIgnoresForgedIdentityHeaders`: forged identity headers with no, rejected
  or valid credential are never trusted or reflected; `TestEdgeAuthLegacyProjectionShapeIsRefusedDownstream`: the
  former projection shape and a forged one from a trusted-CIDR peer get 401 from the real auth middleware and
  forwarded-identity verifier).
- SEC-14: `api/v2/social/suspension_matrix_postgres_integration_test.go` (HTTP authorization matrix: owner, member,
  suspended member, NULL-flag member, foreign-project member, user with no project, administration super_admin,
  unauthenticated; active, suspended and NULL-flag project; lift restores; unknown project stays 404 for the
  administrator; refused writes change no rows; no e-mail in refusals), `infra/db/repos/social_pins_membership_postgres_integration_test.go`
  (in-statement: Pin/Unpin and the write statement alone refuse a suspended member, a member of a suspended project
  and the administrator on a suspended project; feedback create statement likewise), `projectaccess_test.go`,
  `social_feedbacks_test.go`. Mutation check: removing the two suspension terms fails all five new SEC-14 tests.
- SEC-10: `infra/egress/private_entries_test.go` (`TestGuardPrivateEntryPermitsOnlyItsOwnRange`: 34 cases, CGNAT
  entry refuses loopback/RFC 1918/ULA/benchmarking incl. mapped, NAT64 and 6to4 forms, subnet entries, IP and port
  pinning, `localhost`, names and wildcards, forbidden inside a wide block; `TestGuardDialTimeRefusesPrivateOutsideTheNamedEntry`
  on a live listener; `TestGuardPerEntryFilterBudget`), `hardening_test.go` (each liftable class now named).
- Fixture fixes: `api/v2/conversations/authority_postgres_integration_test.go` and
  `api/v2/folders/pinned_listing_postgres_integration_test.go` now model `centry.project.suspended` and
  `auth_core__user`, as production does.

## Performance

- SEC-11: the meter visits at most the budget (131 072 nodes plus at most 100 replays per event) and allocates
  nothing (`TestExpansionMeterAllocatesNothing`). A 280 KB document expanding to about 3×10⁹ nodes is refused in
  27–31 ms and about 25 MB per call (`BenchmarkCheckRefusesBomb`, M-series, 20 runs). That cost is the existing
  yaml.v3 parse of the written document that `Check` already did; the walk adds no allocation. The parse is
  bounded by the 512 KiB byte limit, which is checked first.
- SEC-14: two primary-key lookups added to one statement; no extra round trip.
- SEC-10: `permittedIPs` stays at one allocation per call with 64 entries and 32 answers
  (`TestGuardPerEntryFilterBudget`); `BenchmarkGuardPermittedIPs` 4.6 µs, 1 alloc.

## Durability

- SEC-11: the refusal happens before any write. A refused create, version create, update, import, fork or patch
  stores nothing (row counts asserted), so no over-budget definition can be replayed into a run.
- SEC-14: the decision runs inside the effect's own statement, so a suspension that lands after the route gate
  still refuses the write (`TestSocialPinWriteStatementRefusesSuspensionOnItsOwn`,
  `TestSocialFeedbackCreateStatementRefusesSuspensionOnItsOwn`).
- SEC-10, SEC-12: no persisted state.

## Resilience

- Every new refusal is typed and readable: `PIPELINE_YAML_EXPANSION_TOO_LARGE` names the three limits and the
  remedy and echoes no content; the legacy route answers its existing `Access Denied`; the egress refusal keeps its
  text; the membership refusal keeps `forbidden`.
- Limits are bounded and proven at limit and limit+1 on both Main and the Worker parser.
- Fail closed: an unknown YAML node kind or nil alias refuses; NULL suspension refuses; an unparsable allowlist
  entry permits nothing.

## Security

| `rules/security.md` category | Applies | How checked |
| --- | --- | --- |
| Trust boundaries and identity | Yes (SEC-12) | Legacy route emits no projection; forged headers ignored; former shape refused by the real verifier (tests above; browser and in-network evidence below) |
| Authorization (object level) | Yes (SEC-14) | HTTP matrix incl. suspended member, foreign project, other user, unauthenticated; in-statement tests; mutation check |
| Input, parsing and amplification | Yes (SEC-11) | Alias bomb, depth, scalar bytes and replay guard at limit/limit+1, shared with the Rust suite; time and allocation bound |
| Injection and construction | Checked, no change | Predicate placeholders are integer `fmt` indices only; no user data is interpolated |
| Egress and SSRF | Yes (SEC-10) | Per-entry permit tests incl. IPv4-mapped, NAT64, 6to4, port pinning, forbidden-inside-block, dial time |
| Secrets | Checked | Diff secret scan before each commit: clean. Browser tokens were minted and used inside one script, never printed, and deleted |
| Supply chain | Checked | No dependency change. `govulncheck` on elitea-main: 0 called; 1 uncalled pre-existing module advisory (GO-2026-5932, golang.org/x/crypto, no fix available). `cargo deny` and `npm audit` not applicable (no Rust or Web dependency change) |

`security-review` on the branch: no findings. Residual assumption noted by the review: edge stripping of
caller-supplied `X-Auth-*` is the SEC-01 work already on main, not this change.

## Recovery guarantees

| Component × phase | Class | Evidence |
| --- | --- | --- |
| Main × admission (pipeline save) | F | Typed 400 before any write; nothing stored. `pipeline_expansion_postgres_integration_test.go` (three packages) |
| Main × admission (run start of a pre-existing over-budget pipeline) | F | `CheckStart` uses the same `check`; `Refusal` maps the new error to the readable start refusal (`limits.go`) |
| Worker × compile | F (unchanged) | Worker refuses the same documents with the same limit kind (`bounded_yaml_tests.rs`); Main now stops them earlier |
| Main × admission (project-scoped social read/write) | F | 403 decided in the effect statement; no partial write. `suspension_matrix_postgres_integration_test.go` |
| Main × edge auth (legacy route) | F | 403, no projection. `edge_auth_test.go` |
| Main × tool call (webhook / MCP egress, effectful) | F | Refused before connect at validation and at dial time. `private_entries_test.go` |

No L rows. These changes add no new durable phase.

## Real-browser evidence

Standalone stack `elitea-mh`, browser host `mh.localhost:18420` (`STANDALONE_HOST`), real-model dump
`product-real-models-main-c0f2e5f9b.dump` (shared 158, tenant 148), logged in through the OIDC mock as
`admin@centry.user`. Images: `elitea-main`/migrate built from this branch at cbf7373cd (the pre-rebase SHA of
8c8fff013; the rebase onto 2b55242fd brought in no change to the files this PR touches)
(`ghcr.io/elitea-ng/elitea-main:mh-cbf7373cd`, image `sha256:f6dc4c9dd8cf…`); its extracted binary contains
`PIPELINE_YAML_EXPANSION_TOO_LARGE`, `edge-auth identity projection is not offered on this route` and
`suspended IS NOT FALSE`, and not the removed legacy text. Every other service ran merged-main `main-c0f2e5f9b-verify`
images (main 58abb650c differs from c0f2e5f9b only by documentation). Main ran with
`ELITEA_WEBHOOK_EGRESS_ALLOWLIST=100.64.0.0/10`. No response mocks.

- SEC-11 create: Pipelines › New, the default graph plus a six-line anchor chain; Save →
  `POST /api/v2/elitea_core/applications/prompt_lib/2` 400, the page shows the expansion message; after reload a
  name search returns 0 rows.
- SEC-11 update: the dump's pipeline 164 ("verify-1ab920d yaml expansion bo", stored before this change) opens
  normally; a description edit + Save → `PUT …/version/prompt_lib/2/164/190` 400 with the same message; after
  reload the description is unchanged.
- SEC-12: from the signed-in page through the edge, `/auth`, `/auth?target=rpc` and `/auth?target=header`, with and
  without forged `X-Auth-*` and with a freshly minted test token: every answer 403 `Access Denied`, no `X-Auth-*`
  header (the edge strips client `X-Forwarded-*`, so the legacy check never passes from a browser). Inside the stack
  network (`alpine` on `elitea-mh_default` → `elitea-main:8080`) with a valid token and the five forwarded headers:
  `/auth` 200 with no `X-Auth-*`; `/auth?target=rpc` 403, with and without forged headers; a protected API with a
  forged unsigned identity and no credential: 401. Test tokens were deleted (204).
- SEC-14: project 118 (the admin is a member): social authors and feedback list 200 → admin console › Projects ›
  Suspend project → authors 403, feedback list 403 `forbidden`, feedback create 403 and no row stored (0 rows) →
  Unsuspend → both 200 after reload.
- SEC-10: Settings › Webhooks › New webhook: `http://127.0.0.1:8080/hook` 400 and `http://10.0.0.5/hook` 400
  ("does not resolve to a permitted destination"), `http://100.64.1.5/hook` 201; after reload the list holds only
  the CGNAT webhook and the pre-existing public one.

## Fixtures

- Unit and PostgreSQL tests create their own databases and rows (SQL fixtures), as listed above.
- Browser: real-model dump restored into the stack's own database; the project suspension and the webhooks were
  made through the UI; test tokens through the token API from the signed-in page; nothing was written to the
  database directly. The stack and its volumes were torn down afterwards.

## Follow-ups

- SEC-13 (toolkit test-connection egress through the guard) waits for a decision on self-hosted private hosts.
- The SEC-14 HTTP matrix stubs the feedback RBAC resolver (`feedbackGrantAll`); real RBAC refusal of a suspended
  project is covered by `legacyrbac` tests and the browser pass. Conversation pins use the same predicate but have
  no dedicated suspension test.
- An administrator who needs a suspended project's data must lift the suspension first (this was already true for
  every route with a named permission).
