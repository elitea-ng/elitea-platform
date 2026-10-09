# Access hardening: project-scoped reads and pins, unlock admission, vault master key, Web CSP (2026-10-08)

Branch `fix/main-web-access-hardening`, rebased onto `origin/main` on 2026-10-09. Components: Go Main
(`services/elitea-main`), Web (`apps/elitea-web`), Helm chart, local stacks. The Worker is not changed; this
file lives here per the source-mapping rule.

Hardens SEC-05, SEC-06, SEC-07 and SEC-08, and hardening items H-01, H-02, H-03 and H-06. H-05 is covered for pin
writes only. Finding details are kept out of this repository.

## Business behaviour

Taken from the current platform:

- Project feedback, project feature switches, pins, the author card and the author listings keep their response
  shapes.
- Shared-chat links keep their password unlock and grant cookie.
- Markdown keeps rendering the same constructs, including read-only GFM task-list checkboxes.
- Vault keys keep the centry Fernet format.

Hardened beyond the current platform:

- Every `{projectID}` route authorizes project membership, or is allowlisted with a reason.
- Author e-mail is shown only to colleagues.
- Pin writes check membership inside the write.
- The anonymous password check has a cost bound.
- Vault keys must be wrapped.
- Rendered markdown keeps no `style`/`class`/`id` attributes.
- The CSP covers scripts, styles, images and connections.

## Changed paths and enforcing code

| Rule | Enforced at |
| --- | --- |
| Membership gate on elitea_core feedbacks, `platform_settings/{projectID}`, pin POST/DELETE | `services/elitea-main/internal/api/router.go:3800`, `:3932-3933`, `:4000` |
| One SQL meaning of "may act in project" for the gate and for writes | `internal/infra/db/projectaccess/projectaccess.go:20`; used by `internal/api/middleware/project_authorization.go` |
| Pin/unpin decide membership inside the write statement, fail closed | `internal/infra/db/repos/social_pins.go:100` (first statement), `:179`, `:212`, `:300`, `:316` (predicate in each INSERT/DELETE) |
| A non-conversation pin names an entity that exists in the project | `social_pins.go:48` (`pinTenantTables`, `FOR SHARE` on the row) |
| Author e-mail only for the author or a user sharing a non-public project; counts only over shared projects, at most 100 | `internal/api/v2/eliteacore/author_lookup.go:43`, `:124`, `:138`, `:24` |
| Social author listings limited to project members; the public project's listing carries only the caller's e-mail | `internal/api/v2/social/handler.go:719`, `:765`, `:776` |
| Unlock: wrong-shape token refused without a key derivation | `internal/api/v2/sharedchat/handler.go:494` |
| Unlock: attempt budget per client and link (10 per 5 minutes, at most 10,000 tracked) | `handler.go:507`, `admission.go:47`; client key `admission.go:180` (trusted-proxy resolver wired at `internal/api/shared_chat_routes.go:42`, else the socket peer) |
| Unlock: process-wide verification cap (GOMAXPROCS/2 clamped to 1..8), non-blocking, 429 + Retry-After | `handler.go:511`, `admission.go:70` |
| Main refuses to start without `SECRETS_MASTER_KEY` unless `ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=true`; malformed key always refused | `cmd/elitea-main/master_key_gate.go:31`, called at `cmd/elitea-main/main.go:199` before the database pool; `internal/api/v2/secrets/handler.go:81` |
| With a key set, Main refuses to start while unwrapped vault keys exist | `master_key_gate.go:70`, called at `main.go:262` |
| Helm passes the key from a Secret reference and fails the render without a source or with a plaintext value | `deploy/helm/elitea/templates/main/_helpers.tpl:1273`, `templates/main/deployment.yaml` |
| Sanitisers drop `style`, `class`, `id`, `name`, `data-*`, form attributes and controls; `on*` stays stripped | `apps/elitea-web/src/shared/ui/lib/sanitizeMarkdownHtml.ts:49`, `:104`; `sanitizeSplashHtml.ts:46` |
| CSP with hash-pinned inline scripts, no `unsafe-inline`/`unsafe-eval` in script-src | `apps/elitea-web/nginx/security-headers.conf:69` |
| No CSP violations from the app itself | same-origin AudioWorklet `src/features/chat-input/lib/helpers/speechCapture.ts:74`; Zod jitless `src/shared/config/zodJitless.ts:13`, imported first by `src/app/main.tsx` and `src/entries/admin/main.tsx` |
| No bearer token in shipped files | `deploy/scripts/run-migration-tests.sh:42`, `:110`; gate `services/elitea-main/tests/secretscan/committed_token_test.go` |

Local stacks that seed unwrapped vault keys set the opt-out:

- `deploy/docker-compose.e2e-standalone.yml`
- `deploy/docker-compose.standalone-full.yml`, which defaults it to true and still uses a key when one is supplied
- `deploy/kind/values-kind.yaml`

`deploy/README.md` documents the states, the Helm wiring and the rewrap order.

## Tests

Counts are from local runs on 2026-10-08. The real PostgreSQL runs used `pgvector/pgvector:0.8.1-pg18-trixie`,
the same image as CI.

| Area | Tests | Result |
| --- | --- | --- |
| Authorization matrix | `internal/api/router_project_access_matrix_test.go` covers each changed route against owner, member, member of another project, user with no project, unauthenticated, PAT and session. Refused requests send no statement naming the tenant or social tables. | pass |
| Route walk | `router_project_route_walk_test.go` sends each of 333 `{projectID}` routes as a member of another project. 10 routes are allowlisted, each with a reason. Stale allowlist entries, empty reasons and fewer than 250 routes all fail. Mutation-checked: removing a gate fails it. | pass |
| Pins | `internal/infra/db/repos/social_pins_membership_postgres_integration_test.go` (real PG) covers member pin/unpin for 7 types, non-member refused with the row count unchanged, refusal after membership revocation, the write statement refusing on its own, missing entity 404, and administration-mode super_admin only. | pass |
| Author and social | `author_lookup_postgres_integration_test.go`, `social/project_scope_postgres_integration_test.go` (real PG) cover the author, a colleague, a stranger, a fellow member of the public project only, a token principal, an unknown id, the 100-project cap, member-only listings and the public-project listing e-mail. | pass |
| Unlock | `sharedchat/admission_test.go`, `admission_internal_test.go` and `api/shared_chat_routes_test.go` cover cap and cap+1, budget and budget+1, per-link budget, forwarded-header independence, bounded tracking, wrong-shape refusal, and a 1000-request burst with the real KDF. | 35 pass under `-race` |
| Master key | `cmd/elitea-main/master_key_startup_test.go` (10-case table, `run()` refuses before the database), `master_key_rows_postgres_integration_test.go` (real PG), `secrets/unwrapped_rows_test.go`, and `deploy/helm/tests/render-main-master-key.sh` (9 checks) | pass |
| Web | `sanitizeHtml.contract.test.ts` (82 tests), `src/app/securityHeadersConf.test.ts` (22 tests; source and built inline-script hashes), `zodJitless.test.ts`, `speechCapture.test.ts` | pass |
| Token scan | `tests/secretscan` covers the repository plus a planted-token non-vacuity case | pass |

Full suites after the rebase onto `origin/main` (2026-10-09):

- **Main** (`go test -timeout 30m ./...`, real PG): 16,276 passed, 0 failed, 84 skipped.
  - The skips need a secured NATS server, editor or other external binaries.
  - The rebase added membership seeding to the new upstream conversation-pin concurrency test, which now runs
    through the membership check. Upstream's `FOR NO KEY UPDATE` lock order is kept in pin and unpin.
- **Web** (`vitest --project node`, npm 11 / Node 24): 16,812 passed, 3 failed, 6 expected-fail.
  - The 3 failures (2 files) were 5-second timeouts under load. Both files pass alone (20/20).
  - Before the rebase: 16,688 passed, with 5 load timeouts that pass alone.
- `go vet`, `gofmt`, `tsc --noEmit` and `oxlint` are clean on the changed files. `helm lint` passes, and every
  shipped values file renders.

## Performance

| Path | Budget | Measured / mechanism |
| --- | --- | --- |
| Pin/unpin | 1 transaction, 2 statements, 5 s timeout (`socialPinTimeout`). The access check is a single `EXISTS` over indexed auth tables. | no extra round trip beyond the gate statement |
| Author card | 1 profile read, 1 shared-project read, ≤ 2 count statements per shared project, ≤ 100 projects. The previous code issued 4 per project with no cap. | bounded by `authorCountedProjectsMax` |
| Unlock | verifications ≤ cap at any instant | 1000-request burst on 16 cores, cap 2: max in flight 2; probe p99 6–84 ms against a 250 ms budget; 990–993 requests refused with 429 |
| CSP | header only | no runtime cost; worklet served as a hashed asset |

## Durability

- Pins: the membership check and the write commit in one statement. A revoke committed before the statement is
  honoured. Conversation pins keep their `FOR SHARE` lock and sync stamp.
- Vault keys: no stored format changes. Start-up refuses a key/row mismatch instead of serving failing reads. The
  rewrap is the existing `deploy/scripts/rewrap-centry-vault.py`.
- Unlock budgets live in memory per replica and are lost on restart. That is acceptable for a throttle; with N
  replicas the limits are N times the stated values.

## Resilience

- Every refusal is typed: 403 for non-members (before any 404), 404 for a missing entity or project, 400 for an
  invalid id or type, and 429 with Retry-After for unlock admission.
- Start-up failures name the variable and the remedy and never include key bytes.
- Limits are named constants with limit and limit+1 tests.

## Security

| Category (`rules/security.md`) | Applies | How checked |
| --- | --- | --- |
| Identity from verified credential | yes | actor from `OwningUserID()`. A token with no owning user is refused in pins and the author lookup (tests). |
| Object-level authorization in the effect transaction, fail closed | yes | pin/unpin predicate inside the write. The matrix and route-walk tests cover the rest. |
| No user/e-mail enumeration | yes | author and listing tests. The public project never makes users colleagues. |
| Parameterized SQL | yes | identifiers come only from `tenantschema.Quote` and fixed maps, and values are bound. |
| Cost cap on unauthenticated expensive endpoint | yes | burst test |
| XSS sanitizer and CSP | yes | contract tests, CSP directive and hash tests |
| Fail-closed config | yes | master-key gate and unwrapped-row refusal tests, Helm render guard |
| Secrets not committed, not in logs or errors | yes | token removed, repository scan test, error-content assertions |
| Egress | no | no outbound calls added |
| Parsers of untrusted structure | no | none added |
| Dependencies | no | none added |

Reviews:

- **`code-review` (high):** 8 findings.
  - Fixed: the unlock budget is now per client and link, Main refuses to start on unwrapped keys, and the
    public-project rule is applied.
  - The remaining items are follow-ups, listed below or tracked privately.
  - Splash styling is an intended change.
- **`security-review`:** no finding at or above confidence 8.

Audits (no dependency change on this branch, so every finding is pre-existing):

- `govulncheck ./...` (Main, go1.26.5): 7 standard-library advisories, fixed by the go1.26.6 floor in a separate
  open PR.
- `npm audit --package-lock-only` (Web): 4 high, 1 moderate and 2 low. The affected packages are `braces`,
  `micromatch`, `vite-plugin-singlefile`, `source-map-js`, `smol-toml`, `katex` and `mermaid`.

## Recovery guarantees

| Component × phase | Class | Evidence |
| --- | --- | --- |
| Main × admission (pin/unpin write) | I | The upsert is keyed by (entity, project, entity_id), and a repeated unpin is a no-op success. A crash before commit leaves no row. `social_pins_membership_postgres_integration_test.go` |
| Main × admission (unlock) | F | 429 + Retry-After. No state is written before verification. `admission_test.go` |
| Main × start-up (vault key) | F | A typed refusal names the remedy. Nothing is served with a key/row mismatch. `master_key_rows_postgres_integration_test.go` |
| Web × output delivery (markdown render) | R | Pure rendering with no stored state. Reloading re-renders from the server. |

No touched row is L.

## Real-browser evidence

Run on 2026-10-09 in the Claude desktop browser pane (Chromium), against a standalone stack of this branch. Real
backend, no response mocks.

- **Stack:** compose project `elitea-secfix-1175`, served at `http://secfix.localhost:18140`. A separate hostname
  keeps the session cookie apart from other local stacks.
- **Database:** restored from the real-model product dump at `main` `0def77b22` (shared migrations 157, tenant
  148). It holds 4 vault key rows, all wrapped. `SECRETS_MASTER_KEY` is set.
- **Images:**
  - Main: `elitea-main:secfix-1175`, `sha256:a26391aa7964…`, arm64.
  - Web: `elitea-web:secfix-1175`, `d722b3542043`.
  - Every other service: the `main-0def77b22-verify` images. The branch base is `0def77b22`, and only Main and Web
    differ.
- **Image integrity:** the `/elitea-main` binary extracted with `docker create` + `docker cp` contains strings that
  exist only on this branch: `ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS`, the unwrapped-key refusal text, the unlock
  "busy" message, and `projectaccess`. The Web image contains the new `security-headers.conf` (`script-src 'self'`
  plus the two hashes) and `assets/audioChunkProcessor.worklet-*.js`.

| Check | Actor | Observation |
| --- | --- | --- |
| Start-up | — | Main became healthy with the key set and wrapped rows. No master-key warning was logged. |
| Sign-in | `admin@centry.user` (user 3), `rust-mcp-restricted-20260913@centry.user` (user 6) | Both signed in through the OIDC mock. |
| Pin | user 3 | "Pin on top" on conversation 866 (project 2) from the chat list menu. The pin shows after reload, and `centry.social_pins` holds one row (conversation, 2, 866, 3). |
| Pin/unpin refused | user 6 (no project) | POST and DELETE on `elitea_core` and `social` pin routes returned 403. The pin row did not change. |
| Feedback and settings | user 3 / user 6 | `elitea_core` feedbacks/2 returned 200 / 403. `platform_settings/prompt_lib/2` returned 200 / 403. The project-less `platform_settings` returned 200 / 200. `social` feedbacks/2 returned 403 for user 6. For user 3 it returned 500: this database has no `p_2.social_feedbacks` table, and the social list handler, which this branch does not change, reports that as a server error. The `elitea_core` route answers 200. |
| Author card | user 3 viewing self / user 6 viewing user 3 / user 3 viewing user 6 / unknown id | Self has `email`. The other two cards have no `email` key and zero counts. An unknown id returns 200 `{}`. |
| Social authors | user 6 | `social/authors/1` returned 403. |
| Shared-chat unlock | browser on link A | The UI showed "That password did not work". Attempts 1–10 returned 403. Attempt 11 returned 429 with `Retry-After: 288`. The correct password on link A also returned 429, which the UI showed as a generic error. |
| Per-link budget | same browser, link B | The correct password unlocked link B through the UI and showed the read-only transcript. It was still unlocked after reload. |
| Markdown | user 3, gpt-5.4-mini, conversation 867 | Model output with a GFM task list and `<span style onclick class id data-z>` rendered two disabled checkboxes. The span had no attributes left and default colour. Bold still rendered. Unchanged after reload. |
| CSP | user 3 | Full loads of chat, agents, pipelines, settings, toolkits, MCPs, credentials, artifacts, docs with Mermaid, the shared-chat view, and the chat with model output: no CSP violation. As a positive control, an injected inline script was blocked and reported as a `script-src-elem` violation. |
| Microphone worklet | user 3 | `audioWorklet.addModule` of the shipped same-origin asset loaded and registered `audio-chunk-processor`. A Blob-URL worklet is refused by the policy. |

The Web agent's earlier local Chromium check used stubbed API responses. It is superseded by this run.

## Fixtures

All test fixtures are created by SQL in the tests, against isolated databases. None were created through the UI.

## Follow-ups

- Pin existence does not tell agent from pipeline, or toolkit from MCP.
- The shared-chat view shows its generic error for a 429. A "too many attempts, try again later" message would be
  clearer; it needs a new `t()` key.
- The social feedback list answers 500 when a project's tenant schema lacks `social_feedbacks`. This predates this
  branch; the `elitea_core` twin answers 200.
- `api/openapi/v2.yaml` and `API_CONTRACT.md` do not yet describe the new 400/403/404 responses or the conditional
  `email`.
- Author-card visibility for non-colleagues and the `img-src` scope are product decisions; tracked privately.
- Further e-mail visibility rules on the other author listings are tracked privately.
- Apply the start-up master-key requirement to the LLM gateway too.
- Review the desktop (Tauri) entry's CSP and Zod configuration; that entry arrived on `main` after this work began.
- `source-mapping/README.md` is also edited by open PRs; this branch adds one line.
