# Governance request-body and CEL bounds

Branch `fix/governance-body-bounds`, base `origin/main` (`fcf86c31`). Component: Go Main only
(`services/elitea-main`). Brief: `handoffs/point5-wave1-20261008/00-COMMON.md`.

## Problem

`POST/PUT /api/v2/admin/gateway/governance` and `POST /api/v2/admin/gateway/governance/validate-cel`
decoded request bodies with `json.NewDecoder(r.Body).Decode` and no size limit. The routes sit behind the
platform `administration` permission (`internal/api/router.go:2640`), so the caller is an authenticated
administrator, but an unbounded decode still lets one request allocate without limit on a Main replica.
Neighbouring handlers already bound their bodies
(`internal/api/v2/configurations/handler.go:2130`, `internal/api/nativeauth/admin.go:66`).

## Business behaviour

The current platform (Pylon/EliteaUI) has no governance CEL authoring surface, so there is no business
behaviour to port. The Web client (`apps/elitea-web/src/pages/admin/api/adminGovernanceApi.ts:206,222,262`)
sends only `type`, `name`, `data`, `enabled` for writes and `{cel}` for validation; all of them are still
accepted. `useValidateCel` treats a `{valid:false,error}` 200 as a result and any non-2xx as a transport
failure, which matches the new responses below.

Deliberately not changed: the response envelope (`{"error": "..."}` from `writeError`, same as the rest of
`internal/api/gateway`), the List/Delete handlers (no body), and the gateway's load-time compiler.

## Changed paths

- `services/elitea-main/internal/api/gateway/governance.go:30` — `maxGovernanceRequestBytes = 256 << 10`.
- `services/elitea-main/internal/api/gateway/governance.go:227` — `decodeGovernanceJSON`, the one bounded
  decoder: `http.MaxBytesReader` (`:228`), `DisallowUnknownFields` (`:230`), exactly one JSON value
  (`:234`), `*http.MaxBytesError` → **413** `request body too large` (`:240`), anything else → **400**
  `invalid request body`.
- `governance.go:212` (`ValidateCEL`) and `governance.go:251` (`decodeGovernanceBody`, used by Create and
  Update) call it. Both return before any database statement.
- `services/elitea-main/internal/api/gateway/routing_cel.go:141` — `maxRoutingCELBytes = 8 << 10`;
  `:144` typed `ErrRoutingCELTooLong`; `:160` checked in `CompileRoutingCEL` before the parser runs. A
  too-long predicate is `{valid:false}` on validate-cel and a 400 on a routing-rule write.

### Why a separate CEL cap

cel-go is pinned at **v0.29.0** in `services/elitea-main/go.mod` (not v0.30.0). Its parser default is an
expression size limit of **100,000 code points** and a recursion depth of 250
(`cel-go@v0.29.0/parser/parser.go:55,70`); it is not a node-count limit. Without the request cap, those are
the only bounds. With the 256 KiB body cap alone, a predicate could still be up to ~100 KB, and the
gateway recompiles every stored rule on each policy load
(`services/elitea-llm-gateway/internal/policy/routing.go:292`). A routing rule is one boolean over nine
variables, so 8 KiB is far above any hand-written predicate and gives the author a readable refusal instead
of a parser error. The cap is stricter than the gateway, so "a rule that type-checks here type-checks in the
gateway" (`routing_cel.go:14`) still holds.

## Tests

`services/elitea-main/internal/api/gateway/governance_test.go`, written first and seen failing for the right
reason (200 instead of 413/400, `nil` instead of `ErrRoutingCELTooLong`):

| Test | Proves |
|---|---|
| `TestGovernanceBodyBound` (`:536`) | create, update, validate-cel: body of exactly `maxGovernanceRequestBytes` → 200; limit + 1 → 413 `request body too large`, no SQL issued |
| `TestCompileRoutingCELLengthBound` (`:579`) | predicate of exactly `maxRoutingCELBytes` compiles; limit + 1 → `errors.Is(err, ErrRoutingCELTooLong)` |
| `TestValidateCELActionOverCELCap` (`:589`) | over-cap predicate → 200 `{valid:false}`; the refusal does not echo the expression |
| `TestCreateRoutingRuleOverCELCap` (`:607`) | over-cap routing rule → 400, no SQL issued |
| `TestGovernanceBodyStrictDecode` (`:623`) | second JSON value, trailing garbage, unknown top-level field (row and validate-cel) → 400 with no SQL; trailing whitespace → 200; a valid value followed by whitespace past the limit → 413 |

Results (Go 1.25.13, `GOTOOLCHAIN=go1.25.13`, darwin/arm64):

- `go vet ./internal/api/gateway/` and `go vet ./...`: clean.
- `go test -race -count=1 ./internal/api/gateway/`: pass — 125 tests/subtests pass, 13 skip. The skips are
  the pre-existing real-PostgreSQL budget-alert tests (`budget_alerts_postgres_integration_test.go`), which
  need `ELITEA_TEST_DATABASE_URL`; they do not touch the changed code.
- `go test -count=1 ./...` in `services/elitea-main` (final commit): exit 0 — 185 packages pass, 0 fail,
  22 packages without tests; 13,047 tests/subtests pass, 0 fail, 1,987 skip. The skips are pre-existing
  PostgreSQL integration tests that need `ELITEA_TEST_DATABASE_URL`, which was not set for this run.
- No new real-PostgreSQL test: the change runs strictly before the first statement and its tests assert that
  no SQL is issued.

## Performance

Budget: one governance request reads at most `maxGovernanceRequestBytes + 1` bytes and runs the CEL parser on
at most `maxRoutingCELBytes` bytes. Zero database round trips on a refused body.

Measured with a scratch benchmark (not committed) on the first commit, before the strict single-value check
was added (that check adds one `Decode` of the remaining whitespace), `-benchtime=2s -count=3`, Apple
M-series, 16 threads:

| Case | ns/op | B/op | allocs/op |
|---|---|---|---|
| `CompileRoutingCEL` at the 8 KiB cap | 223,000–231,000 | ~51,000 | 185 |
| Create with a body exactly at 256 KiB (fake querier) | 1.14–1.34 ms | ~1.93 MB | 115 |
| validate-cel with a 1 MiB body (refused 413) | 0.80–0.84 ms | ~3.18 MB | 88 |

The 413 row's allocation is dominated by the benchmark building and copying its own 1 MiB request; the
handler reads only up to the limit. Before the change the same handler would decode the whole body whatever
its size.

## Durability

Not applicable to state: the change only refuses input before the first database statement and adds no
write. No crash window is created or touched.

Recovery-guarantee rows for what this touches (Main × admission):

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Main × admission, oversized or malformed governance body | F — typed failure (413 / 400), nothing persisted; the input can never be accepted, so resume or retry would not help | `governance.go:227-245` | `TestGovernanceBodyBound`, `TestGovernanceBodyStrictDecode` |
| Main × admission, over-cap routing CEL | F — typed `ErrRoutingCELTooLong`, 400 on write, `{valid:false}` on validate | `routing_cel.go:160` | `TestCompileRoutingCELLengthBound`, `TestCreateRoutingRuleOverCELCap`, `TestValidateCELActionOverCELCap` |
| Main crash during a governance write (unchanged) | I — single-statement INSERT/UPDATE; a repeated Create is deduplicated by the section/type/name unique key (409), Update is a full replace | `governance.go` Create/Update (unchanged) | existing `TestCreateUniqueViolation`, `TestUpdateUniqueViolation` |

## Resilience

- Every input on these routes is now bounded: body bytes (256 KiB), CEL bytes (8 KiB), and, via cel-go,
  recursion depth 250.
- Typed, readable failures: 413 `request body too large`; 400 `invalid request body`; `ErrRoutingCELTooLong`
  with the byte count and the limit. No silent coercion: unknown top-level fields and trailing values are
  refused instead of dropped.
- `http.MaxBytesReader` also tells the server to close the connection after an oversized body, so the
  remaining bytes are not drained.

## Security

- Threat: an authenticated administrator (or a stolen admin session) sends a very large body or CEL
  expression to exhaust Main memory/CPU. Mitigated at `governance.go:228` and `routing_cel.go:160`, proven by
  the limit/limit+1 tests above.
- Strict schema: `DisallowUnknownFields` on the top-level object (`governance.go:230`); `data` stays a free
  JSON object because its shape depends on `type` and is validated per type in `validateGovernanceRow`.
- Authorization unchanged: the routes stay behind the `administration` permission group
  (`internal/api/router.go:2640`); no new read or write.
- No sensitive data in errors: the 413/400 messages are constants, and the CEL refusal reports only the byte
  count and the limit, never the expression (`TestValidateCELActionOverCELCap`).
- No new dependency; `go.mod`/`go.sum` unchanged. `govulncheck ./...`: 0 vulnerabilities reachable from
  our code; 5 in required modules that our code does not call (pre-existing, unchanged by this PR).

## Reviews

- `code-review` (high) on the branch diff: 6 findings. Fixed: trailing values were accepted, unknown top-level
  fields were dropped, and the validate-cel limit test padded with an unknown field. Kept as follow-ups (below):
  the gateway load path has no CEL cap, over-cap rules saved earlier cannot be updated, and each package keeps
  its own private bounded decoder.
- `security-review` on the branch: no findings. The authorization group is unchanged, no SQL was touched, error
  strings are constants or byte counts, and decoding uses the same typed structs.

## Real-browser evidence

Collected 2026-10-09 in the desktop app's built-in browser against a fresh local rehearsal stack. Real backend,
no response mocks.

- **Stack:** compose project `elitea-govbb`, built from `deploy/docker-compose.standalone-full.yml` and
  `docker-compose.standalone-rust-agent.yml` via `deploy/scripts/standalone-stack.sh` (`build elitea-main`,
  `certs`, `up`, `seed`). Browser host `http://govbb.localhost:18170`, own subnet `10.231.70.0/24`.
- **Images:**
  - `elitea-main`: `ghcr.io/elitea-ng/elitea-main:govbb-20261009`, image
    `sha256:311add35bb3c1851ce937c7b17e71632d28d8f18e32b5cd66cf2e01b42c3202c`, built from branch commit
    `12c1984b` (this PR merged with `main` at `85cabcc8`).
  - Every other service reuses the locally built `dtfx-20261008` images unchanged, for example `elitea-web`
    `sha256:241e61287dc23e38cfa8c0cdd23ada154c64534f349424b017e35f93a12fe2b5`.
- **Actor:** the seeded test administrator `e2e-admin@autotest.local`, signed in through the stack's OIDC mock.
- Admin → LLM Governance → New entry → type **CEL routing rule**:
  1. Typed `provider == "openai" && budget_used < 0.9`, pressed **Validate CEL**: `POST .../validate-cel` → 200,
     and the page shows "The expression compiles."
  2. Replaced it with a valid 8,214-byte predicate (`provider == "a…"`), pressed **Validate CEL**: 200, and the
     page shows "CEL expression is too long: 8214 bytes, limit 8192" beside the field. The expression is not
     echoed.
  3. With name `govbb-route` and target `openai / gpt-4o / 1`, pressed **Save**: `POST .../governance` → 400, the
     dialog stays open showing the same message, and no row is written.
  4. Restored the short predicate and pressed **Save**: 200; the list shows `govbb-route` (`routing_rule`,
     all projects, enabled).
  5. **Reload** of `/admin/app/governance`: the row is still listed. The list API returns id
     `c36e9d2e-ee35-413a-b120-782087abbab0` with the same predicate and target.
- Same-origin requests with the signed-in session, against the same stack:
  - a 307,226-byte validate-cel body → **413** `{"error":"request body too large"}`;
  - an unknown top-level field (`enabeld`) on create → **400** `invalid request body`;
  - a second JSON value after the first → **400**.

  The list afterwards still holds only `global` and `govbb-route`.
- **Gateway:** on its next 30-second refresh, `elitea-llm-gateway` logged
  `governance definitions loaded … routing_rules:1 … rejected:0`, so the rule accepted under the cap also
  compiles on the enforcement side.
- **Logs:** `elitea-main` logged no error for these requests, and no CEL text (no run of the padding character
  appears in its logs).

## Fixtures

Unit-test fixtures are in-process: `httptest` requests through the real chi routes against the `fakeQuerier` seam
(`governance_test.go:84`).

On the browser stack:
- users, RBAC and the `global` budget-alert row came from `standalone-stack.sh seed`, which writes to the
  database;
- the `govbb-route` rule was created through the UI;
- the over-limit and strict-decode requests were sent from the signed-in page as same-origin requests.

## Follow-ups

- The gateway's load-time compiler (`services/elitea-llm-gateway/internal/policy/routing.go:93`) has no
  length cap of its own, so a row written straight into `gateway.governance_config` bypasses the 8 KiB cap.
  Out of scope for this Main-only change; a matching cap there would bound gateway policy loads for
  hand-written rows too.
- A routing rule saved before this change with a predicate over 8 KiB now fails validation on every Update,
  including an enable toggle; it can still be deleted or shortened. No such rule is expected (a routing
  predicate is a short boolean), but none was searched for in a deployed database.
- Each Main package keeps its own private bounded decoder (`configurations/handler.go:2130`,
  `artifacts/objects.go:123`, this one); a shared helper would stop them drifting.
