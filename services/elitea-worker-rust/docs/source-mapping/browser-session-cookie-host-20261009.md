# Browser sessions refused after sign-in on a standalone stack — 2026-10-09

Post-merge verification of `main` `1ab920dde` reported that a browser session was refused about 30–40 s after
signing in through the OIDC mock, even while idle, with Main logging `authentication refused the request`
`source=session_cookie reason=session_unknown`. The session row was live (not revoked, `expires_at` 7 days out,
`idle_timeout_seconds` 28800, `last_seen_at` still at creation time), and the browser's cookie count on API requests
grew from 1 to 4. Swapping only the Main image back to `0def77b22` appeared to remove the symptom, so the regression
was attributed to the Main parts of #1175 or #1176.

**Finding: it is not a Main regression.** Both Main images behave identically. The symptom is two standalone stacks
browsed on `localhost:<port>` from one browser profile. Browsers scope cookies by host and ignore the port, so
every standalone stack writes the same `elitea_session` cookie on host `localhost`. Signing in to one stack replaces
the other stack's cookie with an identifier that stack never issued, and that Main answers `session_unknown`. The
1 → 4 cookie growth is the other stack's in-flight OIDC login (`oidc_state`, `oidc_nonce`, `oidc_pkce`, five-minute
lifetime) riding on the same host. The image-swap isolation was confounded: during that window other sessions were
signing in to other local stacks in the shared built-in-browser profile (the #1180 browser pass, and the
`elitea-admission-fix` stack, whose Main logged the same `session_unknown`, `cookie_count` 1 then 4, at 14:36 UTC).

This change makes the existing workaround (a `<name>.localhost` host per stack, used by #1158 and #1175's browser
passes) a supported, validated knob of `standalone-stack.sh`, and pins the session behaviour Main must keep.

## Business behaviour

- Unchanged. Main's session cookie (`elitea_session`, host-only, `Path=/`, `SameSite=Lax`) and the server-side
  session read are untouched. No product code changes.
- New, opt-in: `STANDALONE_HOST` names the host the browser uses for one standalone stack. It defaults to
  `localhost`, so an unset variable renders the same compose configuration as before, and CI (which never sets it)
  is unchanged.
- **Deliberately not done:**
  - No per-deployment cookie name. Production deployments are on distinct hosts. A configurable cookie name would
    widen the Main/Web/edge contract to fix a developer-machine shape.
  - No change to the default host. `*.localhost` resolves to loopback in browsers and in curl, but not necessarily
    in every CI runner's resolver. The scripted checks keep calling `localhost`.

## Root-cause evidence

All runs were on 2026-10-09 in Chromium (Playwright `chromium-1234`, a fresh browser context per run, no response
mocks).

| Run | Setup | Result |
|---|---|---|
| Idle, one stack | Sign in to `localhost:18120` (Main `1ab920dde`), stay 90 s on `/app/chat` | Every request authenticated; jar holds one cookie. No refusal in Main. |
| Navigation, one stack | Same, then pipelines 162 and 165, chat 867, 60 s idle, pipeline 162, reload, chat 867 | No 401 on any API request. |
| curl, one stack | Scripted OIDC login; `/auth/info` and the models API every 8 s for 72 s | 200 throughout. |
| Two stacks, new Main is the victim | One context: sign in to `localhost:18120` (Main `1ab920dde`), then to `localhost:18620` (this session's stack) | The jar's single `elitea_session` now holds the 18620 value; 18120 answers 401 and its Main logs `reason=session_unknown cookie_count=1`. |
| Two stacks, old Main is the victim | Same with the 18620 Main set to `main-0def77b22-verify` and signed in first | Identical: 401 and `reason=session_unknown cookie_count=1` on the `0def77b22` Main. |

Binary identity of the two Main images (`docker create` + `docker cp` of `/elitea-main`, then `grep -a`): the
`main-1ab920dde-verify` binary (`sha256:81ffed1c923e…`) contains `elitea-main edge identity projection v1` and
`X-Auth-Signature` (#1176); the `main-0def77b22-verify` binary contains neither. The two control runs therefore ran
different code and failed the same way.

Code reading confirms it. `session_unknown` is produced only by `browsersession.ErrNotFound` or
`ErrMalformedCookie` (`services/elitea-main/internal/api/middleware/auth.go:751-753`), and the store answers
`ErrNotFound` only when no row has the presented identifier (`internal/auth/browsersession/postgres.go:60-61`). Nothing
on the session-cookie path changed between `0def77b22` and `1ab920dde`. #1176's signed projection
(`internal/api/browserauth/identity_projection.go`, 30 s lifetime) applies only to `X-Auth-*` headers from
EdgeAuth, and the standalone edge runs no forward-auth (`deploy/traefik/dynamic.yml`, `strip-client-identity`).

## Changed paths and enforcing code

- `deploy/docker-compose.standalone-full.yml` — `elitea-main` `OIDC_REDIRECT_URI` is
  `http://${STANDALONE_HOST:-localhost}:${STANDALONE_PORT:-8084}/auth/oidc/callback`, so the callback that sets the
  cookie lands on the host the browser uses.
- `deploy/scripts/standalone-stack.sh` — reads `STANDALONE_HOST` (default `localhost`), refuses anything other than
  `localhost` or one `<label>.localhost` (lower-case, RFC 1123 label) with a bash `[[ =~ ]]` whole-string match,
  exports it for compose, and prints the web, docs, admin and API URLs on that host. The header names it for a
  second stack.
- `services/elitea-main/internal/api/browser_session_web_journey_postgres_integration_test.go` — new.
- `services/elitea-main/tests/deployedge/standalone_browser_host_test.go` — new.

## Tests

| Test | What it proves |
|---|---|
| `TestABrowserSessionStaysValidAcrossTheWebAppsRequestsAfterSignIn` (real PostgreSQL, migrated shared schema, production `PostgresStore` and `authsvc.PrincipalValidator`, the real router) | A session minted as the OIDC callback mints it is admitted on all 10 routed requests of the Web's post-sign-in burst (captured from the browser run above) at 0 s, 40 s (past the 30 s projection lifetime and the reported window), 65 s (past `TouchInterval`), after 5 idle minutes and after a reload. `/api/v2/social/author` must answer 200, so the test cannot pass on unregistered routes. The row is never revoked and `last_seen_at` moves forward. A well-formed cookie from another deployment is refused (Main logs `session_unknown`, `cookie_count=1`, the field signature), and the displaced session still works afterwards. |
| `TestStandaloneOIDCRedirectFollowsTheBrowserHost` | The compose redirect URI follows `STANDALONE_HOST` and defaults to `localhost`. |
| `TestStandaloneStackAcceptsOnlyLoopbackBrowserHosts` | Runs the script's own host block in bash: accepts empty (becomes `localhost`), `localhost`, `sessfix.localhost` and `stack-2.localhost`. Refuses `example.com`, `localhost.example.com`, `a.b.localhost`, `-a.localhost`, `A.localhost`, `127.0.0.1`, `x.localhost:1/evil`, and a value with an embedded newline. The newline case failed with the first `grep -Eq` implementation, which is why the check is a bash regex. |

Mutation check: revoking the session after the first burst makes the journey test fail at the 40 s burst with a 401.

Runs: `go test -count=1` with `ELITEA_TEST_DATABASE_URL` against a throwaway PostgreSQL 18 (pgvector 0.8.1) for
`./internal/api/`, `./internal/api/middleware/`, `./internal/auth/browsersession/` and `./tests/deployedge/`:
1,275 + 311 + 21 + 41 = 1,648 tests passed, 0 failed, 0 skipped. `go vet ./internal/api/` is clean, and the new files
are `gofmt`-clean.

## Performance

- No runtime path changed. The journey test pins the existing write budget: one `last_seen_at` UPDATE per
  `TouchInterval` (1 min), not one per request (`internal/auth/browsersession/manager.go:176`). Of the six
  bursts only two write: the one at 65 s and the first one after the 5-minute idle.
- Measured: the journey test runs in 0.5 s, including 6 × 10 + 1 router requests against PostgreSQL.

## Durability

- Unchanged. Sessions are rows in `elitea_auth.browser_sessions` (shared 0117). A foreign or stale cookie is a read
  miss and never writes or revokes, which the journey test proves: the displaced session keeps working after the
  foreign cookie is refused.

## Resilience

- The new variable is validated before anything starts. An invalid value stops `standalone-stack.sh` with a typed
  message, `STANDALONE_HOST must be localhost or <label>.localhost (got '…')`.
- The unset default renders the previous configuration, so existing stacks and CI behave as before.

## Security

Per category in `.claude/rules/security.md`:

- **Trust boundaries and identity:** not changed. Identity still comes only from the session row, the token or the
  signed projection. The journey test runs the production principal validator.
- **Authorization:** not changed.
- **Input, parsing and amplification:** `STANDALONE_HOST` is untrusted operator input that flows into an OAuth
  `redirect_uri`. It is bounded to loopback names by a whole-string regex. The test covers suffix, prefix, nested
  label, case, IP, path and newline injection.
- **Injection and construction:** the value is interpolated by compose into one environment variable and is never
  evaluated by a shell. The script quotes it.
- **Egress and SSRF:** not applicable. The redirect is browser-side, and only loopback names are accepted.
- **Secrets:** none added. The diff was scanned for token and key patterns before commit.
- **Supply chain:** no dependency or image change. Scanners (govulncheck, cargo deny, npm audit) are not affected by
  this diff and were not re-run.

## Recovery guarantees

| Component × phase | Class | Evidence |
|---|---|---|
| Main × admission (browser session read) | R | A browser that holds its own cookie keeps its session across idle time, navigation and reload; a foreign cookie is a read miss that changes no state. `browser_session_web_journey_postgres_integration_test.go` |
| Web/browser × admission (two local stacks) | R | With distinct `STANDALONE_HOST` values each stack keeps its own cookie, so neither sign-in ends the other's session. Real-browser run below. |

No touched row is L. The default `localhost` shape is a known developer-machine limit (two stacks on one host share a
cookie), documented in the script and the compose file. It is not a product guarantee.

## Real-browser evidence

Run on 2026-10-09 in Chromium (Playwright, a fresh context, no response mocks), against this session's own standalone
stack.

- **Stack:** compose project `elitea-sessfix`, ports 18620–18627, OIDC mock 19720, browsed at
  `http://sessfix.localhost:18620` with `STANDALONE_HOST=sessfix.localhost`. Main's `OIDC_REDIRECT_URI` rendered as
  `http://sessfix.localhost:18620/auth/oidc/callback`, and `/auth/oidc/login` redirected with that `redirect_uri`.
- **Database:** restored from the real-model product dump at `main` `1ab920dde` (shared 157, tenant 148), migrated
  by the stack's own `elitea-migrate`.
- **Images:** every image is a `main-1ab920dde-verify` (or `main-0def77b22-verify` where the reference stack uses
  that) image built from merged `main`. This change has no product code, so no branch image was built. Main is
  `sha256:81ffed1c923e…`, whose binary was checked as above.
- **Other stack:** the shared verification stack `elitea-verify-0def77b2` on `localhost:18120`, Main `1ab920dde`.

| t (s) | Step | Observation |
|---|---|---|
| 0–4 | Sign in to `sessfix.localhost:18620` as `admin@centry.user` (user 3) through the OIDC mock | Lands on `/app/chat`; `/auth/info` 200, user 3. Session `s1.pUI4-j…` created 14:49:58 UTC. |
| 4 | Same browser signs in to the other stack, `localhost:18120` | That stack sets its own `elitea_session` on host `localhost`. This is the step that signed the first stack out in the regression report. |
| 8 | Back to `sessfix.localhost:18620/app/chat` | The chat screen loads as user 3. |
| 8–308 | Idle for 5 minutes on the chat screen, `/auth/info` once a minute | 200 at every tick (68, 128, 188, 248, 308 s). |
| 313 | Navigate to pipeline 162 (`verify-main sensitive reverse`) | The editor opens with the pipeline, signed in. |
| 317 | Navigate to chat 867 | The conversation (gpt-5.4-mini, "VERIFIED Tokyo") renders. |
| 320 | Reload chat 867 | Same conversation, signed in. `/auth/info` is 200 on both stacks: neither sign-in ended the other. |

- 0 responses with status 401 from `sessfix.localhost:18620` during the whole run. Main logged no
  `authentication refused the request` line in the 15 minutes around it.
- The session row was not revoked, and `last_seen_at` moved from 14:49:58 to 14:55:02 UTC: the touch throttle
  advanced the idle clock while the session was in use.
- What the browser actually sent, captured from the request headers in a separate run with the same steps:
  `sessfix.localhost:18620` received only its own `elitea_session=s1.2CB4Zm…`, and `localhost:18120` received only
  `elitea_session=s1.A4Psba…`. Both `/auth/info` calls returned 200.
- The negative control is the "two stacks" rows under Root-cause evidence: the same steps with both stacks on
  `localhost` sign the first stack out at once.
- Screenshots of each step were taken and kept locally; they are not committed.

## Fixtures

The journey test creates its database, account and session rows by SQL and through `browsersession.Manager`, in an
isolated database it drops afterwards. The browser runs used the restored product dump and signed in through the
OIDC mock UI. Nothing was seeded.

## Follow-ups

1. **Planning session:** correct regression 1 in `post-merge-verification-1ab920dde-20261009.md` (not on `main`
   yet). It is the shared-host cookie collision above, not a Main defect, and `1ab920dde` is suitable for
   interactive use when each concurrent stack has its own `STANDALONE_HOST`.
2. Runbooks for local stacks (for example the real-model stack handoff) should set `STANDALONE_HOST=<name>.localhost`
   for every stack that runs beside another one.
3. During these runs, requests in flight when the page navigated away were logged as `session_store_unavailable`
   (`cookie_count=4`). This is most likely a cancelled request context reaching the store's "did not answer"
   branch (`internal/api/middleware/auth.go` `serverSessionRefusal` default). It is noise, not a store fault, and
   it predates this change.
