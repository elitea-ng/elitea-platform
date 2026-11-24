# HTTP action Main foundation (T-5D step 5d-1)

| Item | Value |
| --- | --- |
| Date | 2026-11-24 |
| Branch | `feat/http-action-main-foundation` (merged `origin/main` `ca4a50234`, `2d5e9ceac`) |
| Migration | `services/elitea-main/migrations/shared/0270_http_action_rules_and_effects_v3.sql` |
| Design | `services/elitea-worker-rust/docs/http-action-node-design.md` (updated: "Implementation status (5d-1)") |
| Closes | FU-073 (transport port and egress guard); the Main half of FU-072. The `http` node is admitted in 5d-2. |
| Components | Go Main (`services/elitea-main`), Web (`apps/elitea-web`), contracts (`libs/jsonschema/runtime/v1`), deploy (`deploy/helm`, `deploy/docker-compose.standalone-full.yml`). No Rust change. |

## Business behaviour and what was not ported

There is no current-platform reference. Legacy EliteaUI has no HTTP node, and the legacy SDK reaches HTTP only through
the openapi/postman toolkits without an SSRF guard. Nothing of that path was ported. The only reuse is the existing
`openapi` configuration type as the credential source (API key Bearer/Basic/custom header, OAuth2 client
credentials), resolved by Main from the project vault. The admin page follows the existing Settings > Webhooks page
(same table, dialog, switch, and toast components; no new visuals).

## Decisions (D1-D6, pending user confirmation, each kept in one place)

| # | Decision | Where it lives |
| --- | --- | --- |
| D1 | Revision 1 deleted (snapshot v2, invocation v3 only) | `internal/application/httpaction/frozen.go` (`SnapshotSchema`), `contract.go` (`Schema`) |
| D2 | Effect classes: GET/HEAD/OPTIONS I; other methods C unless the rule says `replay: idempotency_key` and the node binds a key (I) | `internal/application/httpaction/rule.go` `Rule.Retryable`, enforced again in `repos/http_action_effects.go` Begin |
| D3 | Auth in v1: none, Bearer, Basic, custom header, OAuth2 client credentials via `openapi` configurations; delegated OAuth and mTLS refused | `internal/infra/db/repos/http_action_credentials.go` `headers` |
| D4 | Project-scoped default-mode permissions `models.http_action_rules.manage` (admin) / `.list` (admin, editor, viewer), classified in `internal/domain/entitlements/capabilities.go` | `internal/api/v2/httpactions/handler.go`; grants in `migrations/shared/0270_*.sql`; Web `PERMISSIONS.httpActionRules` |
| D5 | Pool per Main replica, in process: 32 slots, 8 per execution | `internal/infra/storage/runtime_http_action.go` `MaxHTTPActionSlots`, `MaxHTTPActionsPerExecution` |
| D6 | Origins/paths only on the admin rules page; the editor endpoint returns names and enabled only | `GET /api/v2/httpactions/prompt_lib/{projectID}/rule-names` |

## No feature flags (§8.1)

There is no switch. `HTTP_ACTION_INTEGRATION_READY` does not exist and was not added. The `ExecuteHTTPAction` gRPC RPC
on `ContentServer` is live (mTLS claim, dedicated pool, full authority) wherever agent dispatch and the configurations
vault are composed (`internal/runtimecomposition/composition.go`, `http_actions.go`); it has no Worker caller until
5d-2. That is a runtime RPC, not an admitted node family: `type: http` remains refused at save and start by
`pipelinelimits` until 5d-2 admits it in Worker, Main, and Web together.

## Changed paths

- **Go Main application (`services/elitea-main/internal/application/httpaction/`)**: `canonical.go`, `contract.go`, `frozen.go`, `node.go`, `render.go`, `rule.go`, `service.go` (+ unit/vector tests).
- **Go Main storage & egress (`services/elitea-main/internal/infra/`)**: `egress/http_action.go`, `storage/http_action_artifacts.go`, `storage/runtime_http_action.go` (+ `runtime_http_action_test.go`), `db/repos/http_action_rules.go`, `db/repos/http_action_effects.go`, `db/repos/http_action_credentials.go`, `db/repos/http_action_recovery.go` (+ unit & PostgreSQL integration tests).
- **Go Main admin API, authz, composition & migrations**: `internal/api/v2/httpactions/handler.go`, `internal/api/router.go`, `internal/api/middleware/audit.go`, `internal/domain/entitlements/capabilities.go`, `internal/application/projectprovisioning/steps.go`, `internal/runtimecomposition/composition.go`, `config.go`, `http_actions.go`, `cmd/elitea-main/main.go`, `api/openapi/v2.yaml`, `migrations/shared/0270_http_action_rules_and_effects_v3.sql`, `migrations/http_action_rules_grant_postgres_integration_test.go`, `internal/infra/db/migrate/manifest_test.go`.
- **Contracts & deploy**: `libs/jsonschema/runtime/v1/http-action-invocation.schema.json`, `http-action-rule.schema.json`, `fixtures/http-action-invocation-v3*.json`, deleted v1 `libs/proto/elitea/runtime/v1/http_frozen_request.fixture.json`; `deploy/helm/elitea/values.yaml`, `deploy/docker-compose.standalone-full.yml`.
- **Web Admin UI (`apps/elitea-web/`)**: `src/pages/settings/HttpActions.tsx`, `HttpActions.test.tsx`, `src/features/settings/lib/http-actions/*`, `src/features/settings/ui/http-actions/*`, `src/routes/_shell/settings/http-actions.tsx`, `settingsSections.ts`, `settingsSections.test.ts`, `src/routeTree.gen.ts`, `src/shared/api/generated/httpactions/*`, `src/shared/api/generated/model/*`, `src/shared/api/endpoints.manifest.json`, `src/shared/i18n/en.json`, `src/shared/lib/permissions.ts`, `parity/route-wiring-map.json`.
- **Docs**: `services/elitea-worker-rust/docs/http-action-node-design.md`, `recovery-guarantees.md`, `source-mapping/http-action-main-foundation-20261123.md`, `source-mapping/README.md`.

## Delivery gate properties

### Performance

- **Named limits**: `MaxInputsBytes` (256 KiB), `MaxInvocation` (768 KiB), `MaxRequest` (384 KiB), `MaxResponse` (2 MiB), `MaxInline` (512 KiB), `MaxSegmentBytes` (1024), `maxHeaderLiteralString` (8192), `MaxRulesPerProject` (256), `MaxQueryNames` (32), `MaxAttempts` (16), `MaxHTTPActionSlots` (32), `MaxHTTPActionsPerExecution` (8) in `internal/application/httpaction/{contract.go,render.go,rule.go}` and `internal/infra/storage/runtime_http_action.go`.
- **Single-pass input decoding**: `ParseInvocation` (`contract.go:177-215`) stores `ParsedInputs` so `Service.Execute` (`service.go:154-160`) calls `RenderDecoded` without running `CanonicalJSON` and `json.Decoder` twice per invocation.
- **OAuth2 token coalescing**: `HTTPActionCredentials.clientCredentialsToken` (`repos/http_action_credentials.go:255-282`) uses `singleflight.Group` keyed by `(project, config, revision)` with a bounded 256-entry cache so concurrent calls share one outbound token fetch.
- **Write count**: 1 `INSERT` (`Begin`) + 1 atomic `UPDATE` (`Commit`, single statement without an extra claim-lock round trip) in `repos/http_action_effects.go:187-255`.
- **Pool isolation**: `TestHTTPActionPoolIsolationFromContentReads` (`storage/runtime_http_action_test.go`) verifies 16 concurrent blocked HTTP actions add `< 50 ms` p95 (measured sub-millisecond) to content reads; `TestHTTPActionPoolBoundaries` verifies 8/9 per-execution and 32/33 global slot caps.
- **Overhead budget (`-tags loadgate`)**: `TestHTTPActionMainOverheadBudgetPostgres` compiled cleanly; prior noisy-host measurement was p50 8.8 ms / p95 20.4 ms (> 15 ms target under load avg ~28). Re-measurement on a quiet host / in-network remains open before merge.

### Durability

- **Durable intent before dispatch**: `HTTPActionRepository.Begin` (`repos/http_action_effects.go:151-225`) writes `state='dispatching'`, `request_bytes`, `inputs_bytes`, `rule_wire`, `dispatch_claim_id`, `dispatch_lease_epoch`, and `dispatch_deadline` with `INSERT ... ON CONFLICT DO NOTHING` before `RoundTrip`.
- **Detached post-dispatch persistence**: Both response artifact spill (`service.go:295-315`, bounded by `artifactWriteTimeout` and `intent.DispatchDeadline`) and receipt commit (`service.go:322-330`, `commitTimeout`) run on `context.WithoutCancel(ctx)` so caller disconnect never discards a known remote outcome.
- **Atomic commit & lookup fencing**: `HTTPActionRepository.Lookup` (`repos/http_action_effects.go:91-125`) reads unlocked first for fast empty/terminal lookups and, whenever a row is `dispatching`, acquires `lockHTTPActionClaim` (`execution_claims, execution_jobs FOR SHARE`) before re-reading `execution_http_effects FOR UPDATE` so a released/superseded claim cannot settle a live claim's `dispatching` row (`SEC-5D1-01`, `TestHTTPEffectLookupFencesReleasedClaimAgainstLiveDispatchPostgres`). `HTTPActionRepository.Commit` (`repos/http_action_effects.go:240-265`) updates only when `state='dispatching' AND dispatch_claim_id=$4 AND dispatch_lease_epoch=$5`. Another claim cannot commit the row, a late commit after takeover settlement to `uncertain` returns `ErrHTTPEffectFenced`, and a job cancel before takeover settles still preserves the true remote outcome.
- **Attempt retry invariants**: `Begin` for `attempt > 0` (`repos/http_action_effects.go:176-204`) requires the prior attempt row to exist, have identical `inputs_digest`, `frozen_binding_digest`, and `request_digest` (else 409 `nondeterministic_inputs`), and have a pinned `rule_wire` that was itself retryable for its recorded `request_bytes` (so a post-hoc rule edit cannot unlock retries on an earlier non-retryable attempt).
- **Recovery re-render**: `http_action_recovery.go` re-renders from stored `inputs_bytes` and `rule_wire` and verifies `request_bytes` equality (`TestHTTPActionRecovery*`, `TestNodeRecoveryMainTransactionUsesActualHTTPCommittedProof`).

### Resilience

- **Bounds & limits**: Limit and limit+1 unit/component tests cover inputs (256 KiB/+1), invocation (768 KiB/+1), response (2 MiB/+1), inline spill (512 KiB/+1), header count/size/value cap (8192/+1), timeout (30000/30001), rules per project (256/257), query names (32/33), path segment (1024/1025), and admin write body (16 KiB/+1).
- **Stored-row resilience**: `HTTPActionRulesRepo.List` (`repos/http_action_rules.go:110-132`) logs and skips rows that fail current validation rather than failing listing for the whole project, while `Update` allows repairing or disabling them and `readHTTPActionRule` still fails closed on runtime execution.
- **Typed failures**: Dial-time egress refusals wrap `ErrEgressRefused` (`storage/http_action_artifacts.go:20-31`) and commit `state='failed', failure_code='policy_denied'` instead of `uncertain` (`service.go:255-261`). OAuth2 token endpoint 4xx responses (except 408/429) and egress refusals return typed `policy_denied` instead of a 503 retry loop (`repos/http_action_credentials.go:315-332`). Live dispatching attempt N-1 returns `ErrDispatchInFlight` (`repos/http_action_effects.go:184-189`).
- **Fail-closed startup**: `egress.NewHTTPActionGuard` refuses malformed `ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST` entries at startup (`runtimecomposition/http_actions_config_test.go`).

### Security

- **Hardened egress (FU-073)**: `httpaction.Transport` is `http.RoundTripper` (an `*http.Client` cannot satisfy it; proven by `TestTransportPortRejectsHTTPClient`). Both action dispatch and OAuth2 token fetches use `egress.NewHTTPActionGuard(ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST)` with `RequireHTTPS()`, `Proxy=nil`, zero redirects followed, and dial-time IP re-resolution (`TestHTTPActionEgress*` in `storage/runtime_http_action_test.go`, `internal/infra/egress/...`).
- **Header & media-type strictness**: Restricted headers are refused at parse/freeze time (`node.go:244`) and again at render/validate time; header field names and values are validated with `httpguts.ValidHeaderFieldName`/`ValidHeaderFieldValue` at parse, render, validate, and pre-Begin (`node.go:250`, `render.go:404`, `contract.go:290`, `service.go:203-218`). JSON bodies accept only empty, `application/json`, or `*+json` media types consistently across parse, validate, and outbound dispatch (`node.go:286`, `contract.go:333`, `service.go:220-226`).
- **Credential & configuration authorization**: Rule writes with `configuration_id > 0` require the writer to hold `configurations.configuration.details` (`api/v2/httpactions/handler.go:235-245`) and verify inside the write transaction (`FOR SHARE`) that the configuration belongs to the project, has `type='openapi'`, and is shared or owned by the writer (`repos/http_action_rules.go:260-289`). At runtime, private configurations are admitted only when owned by both the executing actor and the rule's last writer (`repos/http_action_credentials.go:142-145`).
- **Admin API & audit**: Every route under `/api/v2/httpactions/prompt_lib/{projectID}` is gated on `PermissionManage` or `PermissionList` (`auth.PermissionModeDefault`), audited under `/api/v2/httpactions/` (`api/middleware/audit.go:195`), and covered by `TestNoAdministrationPermissionLiteralOutsideTheRegistry`, `TestEveryGatedPermissionHasAGrant`, `TestEntitlementsCapabilityCoverage`, and `TestHTTPActionRulesAuthorizationMatrixOnACleanDatabase`.
- **Encoding & redaction**: Path segments and query pairs use `encodeComponent` (RFC 3986 unreserved set); 25/25 cross-language render vectors pass (`TestRenderVectors`). Sentinel tests (`TestReceiptAndErrorsCarryNoSecretOrBody`, `TestHTTPActionRouteLogsCarryNoSensitiveFieldsPostgres`) prove secrets, headers, bodies, and origins never reach receipts, errors, or logs.

## Recovery-guarantee rows

| Component × phase | Class | Enforced by | Proven by |
| --- | --- | --- | --- |
| Main × P05 HTTP action effect ledger (5d-1): crash between Begin and Commit, lease loss, duplicate RPC | **C** (safe method or `replay: idempotency_key` + bound key: **I**) | `repos/http_action_effects.go` `Begin`/`Lookup`/`Commit` (atomic `INSERT ... ON CONFLICT DO NOTHING`, `dispatch_deadline`, atomic `UPDATE` predicate fencing) | `TestHTTPEffectCrashWindowsPostgres`, `TestHTTPEffectBegin32ConcurrentOneDispatchPostgres`, `TestHTTPEffectDispatchDeadlinePostgres`, `TestHTTPActionRouteWorkerReplacementGetsStoredReceiptPostgres` (compiled on rebased head; live PG rerun pending Docker slot) |
| Main × P05 rule/credential/RBAC change mid-run | **F** typed (`policy_denied`) | `repos/http_action_effects.go` `Begin` (`FOR SHARE` rechecks on claim, rule, and configuration) + `repos/http_action_credentials.go` `httpConfigurationAccess` | `TestHTTPEffectBeginRechecksAuthorityPostgres`, `TestHTTPEffectConcurrentDisableIsOrderedPostgres`, `TestHTTPCredentialsLiveAuthorityPostgres` |
| Main × P05 retry attempt N>0 after uncertain/failed | **I** (for class I only; **F** `409`/`403` on drift or non-retryable prior rule) | `repos/http_action_effects.go:176-204` (digest/binding/request equality + prior `rule_wire` retryable check) | `TestHTTPEffectAttemptRetryHardeningPostgres`, `TestExecuteEffectClasses` |

## `rules/security.md` category status

| Category | Status | Mechanism & Proving Test / Justification |
| --- | --- | --- |
| Trust boundaries and identity | Applicable | Runtime RPC requires verified mTLS peer cert + claim fence (`storage/runtime_http_action.go:194-200`, `content_server.go:1198`); admin API derives actor/project from verified principal + route param (`api/v2/httpactions/handler.go:194-200`). Proven by `TestHTTPActionRouteStatusMapping`, `TestProductionRouter*`, `internal/transport` listener allowlist tests. |
| Authorization (object level) | Applicable | Project-scoped `PermissionManage`/`PermissionList` + `configurations.configuration.details` (`handler.go:74-84,235-245`), write-time config ownership check (`repos/http_action_rules.go:260-289`), runtime actor+writer private config check inside `Begin` tx (`repos/http_action_credentials.go:100-145`). Proven by `TestConfigReferenceRequiresConfigurationAccessPermission`, `TestHTTPActionRulesRepoPostgres_ConfigurationAccessCheckedAtWrite`, `TestHTTPActionRulesAuthorizationMatrixOnACleanDatabase`. |
| Input, parsing and amplification | Applicable | Strict JSON (`strictJSON` depth/node caps, `DisallowUnknownFields`, duplicate key rejection), upfront raw node byte cap (`node.go:33-41`), `response.max_bytes: null`/`0` refusal (`node.go:335`), `DecodeTemplate` operation length and `timeout_ms` bounds (`node.go:116-148`, `SEC-5D1-02`), 16 KiB rule body cap, 256 KiB inputs cap, 768 KiB invocation cap, 384 KiB request cap, 2 MiB response cap (`contract.go`, `node.go`, `handler.go:42`). Proven by `TestBodyCapAndTrailingData`, `TestBodyAtTheCapIsAccepted`, `TestParseNodeRefusesURLMethodCredentialAndUnknownFields`, `TestExecuteResponseBoundaries`. |
| Injection and construction | Applicable | Parameterized SQL throughout (`pgx.Identifier.Sanitize()` only on Main-owned integer project schema); `encodeComponent` + path placeholder grammar + `httpguts.ValidHeaderFieldValue` (`render.go`, `rule.go`, `node.go`). Proven by `TestRenderVectors` (25/25), `TestRenderEncodingCannotEscapeOperation`, `TestParseNodeHeaderCapAndJSONContentType`. |
| Egress and SSRF | Applicable | `egress.NewHTTPActionGuard` (`RequireHTTPS`, `Proxy=nil`, no redirects, dial-time re-resolution, forbidden/private/NAT64/IPv4-mapped blocks) for rule write, action dispatch, and OAuth2 token fetch; `Transport = http.RoundTripper` (`storage/http_action_artifacts.go:14-31`). Proven by `internal/infra/egress/...`, `TestHTTPActionEgress*`, `TestTransportPortRejectsHTTPClient`. |
| Secrets | Applicable | Credentials by reference (`configuration_id`) resolved server-side from vault, zeroed after header copy (`service.go:191,332`), never in YAML/receipts/errors/logs; diff secret scan clean (0 hits). Proven by `TestReceiptAndErrorsCarryNoSecretOrBody`, `TestHTTPActionRouteLogsCarryNoSensitiveFieldsPostgres`. |
| Supply chain and deployment | Applicable | Zero new Go or npm dependencies added; `govulncheck ./...` reports 0 called vulnerabilities (1 pre-existing uncalled); `npm audit` reports 21 pre-existing moderate dev-dep advisories (`glob`/`minimatch` via storybook/orval, 0 new). `ELITEA_HTTP_ACTION_EGRESS_ALLOWLIST` defaults empty (public only) in Helm & Compose. |
| Cross-layer differentials and version skew | Applicable | Canonical JSON (`canonical.go`) + 25 shared render vectors (`http-action-render-vectors-v1.json`) + strict JSON schema fixtures (`libs/jsonschema/runtime/v1/`); unknown fields & v1 snapshots fail closed (`node.go`, `frozen.go`, `contract.go`). Proven by `TestRenderVectors`, `TestFrozen*`, `internal/runtimecontracts/...`. |
| Agentic delegation and untrusted content isolation | Applicable | Frozen toolkit guardrails (`ToolPolicy.Permits`, sensitive tools fail closed in `storage/runtime_http_action.go:167-173`); fan-out `scope_ref` fails closed `policy_denied` until 5d-3 (`contract.go:194-198`). Proven by `TestExecute*`, `TestFrozen*`. |
| TOCTOU, concurrency races, and effect replay | Applicable | Live claim/rule/config rechecked `FOR SHARE` inside `Begin` tx; `Lookup` fences `dispatching -> uncertain` on `lockHTTPActionClaim` in canonical lock order (`SEC-5D1-01`); `INSERT ... ON CONFLICT DO NOTHING`; atomic `Commit` fence; attempt N>0 requires exact prior digests and retryable pinned `rule_wire`; OAuth2 token cache keyed by revision + `singleflight` with overflow-safe `expires_in` clamp (`DOC-01`). Proven by `-race` runs across all packages + live PG 18 concurrency/crash/attempt/fencing tests (`p5d1v`). |
| Sandbox escape and resource isolation | N/A | No sandbox/code-runner execution or host filesystem paths touched; response artifacts use content-addressed keys `http-effects/<effect>/<sha256>.bin` (`storage/http_action_artifacts.go:50-71`). |
| AuthN, OAuth, PAT, and cryptographic hygiene | Applicable | OAuth2 client-credentials token fetch enforces HTTPS, no redirects, egress guard, RFC 6749 §2.3.1 encoding, typed 4xx refusal, revision-scoped cache bounded by token expiry/1h (`repos/http_action_credentials.go:255-350`); domain-separated SHA-256 digests (`elitea.http.effect.v3`, `elitea.http.credential-revision.v1`, `elitea.graph.http.activation.v1`). |

## Verification, test counts, and skips

Executed on rebased branch (`feat/http-action-main-foundation` over `origin/main` `876c4ce6f`):

- **Go unit & gate suites (`GOMAXPROCS=4 go test -p 2 -race`)**:
  - `internal/application/httpaction`: **PASS** (including 25/25 render vectors, header cap/restricted header parse checks, `+json` content-type, detached bounded artifact-write ctx, dial-time `ErrEgressRefused` -> `failed/policy_denied`, `httpguts` validation)
  - `internal/api/v2/httpactions`: **PASS** (including config-access permission check & `ErrHTTPActionRuleConfigurationInvalid` -> 400)
  - `internal/runtimecomposition`: **PASS** (HTTP actions config & fail-closed startup) — 67 total top-level PASS across `httpaction`, `httpactions`, and `runtimecomposition`
  - `internal/infra/egress`: **PASS** (all hardening, address class, private allowlist, dial-time tests)
  - `internal/infra/storage` (`-run TestHTTPAction`): **PASS** (pool 8/9 & 32/33 boundaries, pool isolation from content reads, gRPC status mapping, all 5 egress hardening tests)
  - `internal/infra/db/repos` (non-PG: `TestHTTPActionRecovery*`, `TestNodeRecoveryMainTransactionUsesActualHTTPCommittedProof`): **PASS**
  - `internal/infra/db/migrate`: **PASS** (`Head(shared) == 269`; re-verified `npm run typecheck`, `node scripts/check-endpoint-manifest.mjs` (`399` entries), and `go vet ./...` after `origin/main` `ca4a50234` merge / `api.gen.go` regeneration)
  - `internal/adminaccess` & `internal/adminaccess/rolegrants`: **PASS**
  - `internal/domain/entitlements`: **PASS**
  - `internal/api` & `internal/api/v2/admin` (`TestAdministrationSurface*`, `TestNoAdministrationPermissionLiteralOutsideTheRegistry`, `TestEveryGatedPermissionHasAGrant`, `TestEntitlementsCapabilityCoverage`, `TestRolesCatalogueIsUnchangedForTheExistingNames`, `TestProductionRouter*`): **PASS**
  - `internal/application/agentexecution` & `internal/runtimecontracts`: **PASS**
  - `internal/transport/...` (`#2919` internal listener allowlist checks): **PASS**
  - `go vet ./...`: **PASS** (0 issues)
  - `go test -c` compilation check for PostgreSQL integration packages (`repos`, `repos -tags loadgate`, `migrations`, `projectprovisioning`, `api`): **PASS**
- **Web (`apps/elitea-web`, Node 24.19.0)**:
  - `npx tsc --noEmit`: **PASS** (0 errors)
  - `npx oxlint` on all 9 changed/new HTTP actions files: **PASS** (0 warnings, 0 errors)
  - `npx vitest run --maxWorkers=2 src/pages/settings/HttpActions.test.tsx src/routes/_shell/settings/settingsSections.test.ts src/shared/i18n/`: **7 files, 45 tests PASS** (8 `HttpActions`, 12 `settingsSections`, 25 `i18n`)
  - `node scripts/theme-gate.mjs`: **PASS** (all 9 checks + 5 `brandPack.contract.test.ts` tests)
  - Static checks (`check-endpoint-manifest`, `check-testid-namespace`, `check-budgets`, `check-layer-cycle`, `check-handlers`, `check-parity-manifest`, `scripts/ci/check-upgrading-notes.mjs`): **ALL PASS**
- Linux visual baselines (`apps/elitea-web/e2e/snapshots/visual/routes.visual.spec.ts-snapshots/settings-*-visual-linux.png`): Refreshed all **10** `settings-*` baselines from Linux CI (`ci-web-e2e / Visual regression`, run `45391699663`, artifact `14923416731`) after verifying the diffs reflect only the new `HTTP actions` navigation item in the Settings sidebar.
- **Scanners**:
  - `govulncheck ./...`: **0 called vulnerabilities** (1 pre-existing uncalled module advisory)
  - `npm audit`: **21 pre-existing moderate dev-dep advisories** (`glob`/`minimatch` via `@storybook/react-vite` and `orval`); 0 new
  - Diff secret & trailer scan (`/usr/bin/grep`): **0 hits**

### Live PostgreSQL 18 integration suite & disposable `p5d1v` stack (`2026-11-24`, torn down)

- **Live PostgreSQL 18 integration tests (`ELITEA_TEST_DATABASE_URL`, scratch DB `httpaction_test` on `elitea-p5d1v-postgres-1`, dropped before teardown): 34/34 top-level tests PASS, 0 failed, 0 skipped**:
  - `migrations` (`-run 'TestHTTPAction|TestCleanDatabaseGrantsHTTPActionRulePermissions|TestProjectRolesResolveHTTPActionRulePermissions'`): **3/3 PASS** (`6.903s`)
  - `internal/infra/db/repos` (`-run 'TestHTTP|TestNodeRecoveryHTTP'`): **27/27 PASS** (`15.564s`)
  - `internal/infra/db/repos` (`-run TestNodeRecoveryMainTransactionUsesActualHTTPCommittedProof`): **1/1 PASS** (5/5 subtests, `0.484s`)
  - `internal/infra/db/repos` (`-tags loadgate -run TestHTTPActionMainOverheadBudgetPostgres`): **1/1 PASS** (`1000` runs: `p50=6.730333ms`, `p95=11.131417ms`, `max=43.43525ms`, `8.285s`)
  - `internal/api` (`-run TestHTTPAction`): **1/1 PASS** (`TestHTTPActionRulesAuthorizationMatrixOnACleanDatabase`, `50` authenticated route × principal × shape cases, `1.821s`)
  - `internal/application/projectprovisioning` (`-run TestDeprovisionRemovesTheProjectOwnedRowsThatHaveNoForeignKey`): **1/1 PASS** (`5.609s`)
- **Built images & `stack.sh binary-check` receipts (commit `2d5e9ceac`)**:
  - `ghcr.io/elitea-ng/elitea-main:p5d1-2d5e9ceac` (`id=9bfbc4861dba29f91aec7963d21adc6f8cee2e612955fc2695d013ad347e561a`, `/elitea-main`, `marker=elitea.http.effect.v3`, `count=1` OK)
  - `ghcr.io/elitea-ng/elitea-web:p5d1-2d5e9ceac` (`id=12ac49d84e3952983b90e3130e6278cd573d1e1fc0d828615f59ea69f72b4c4e`, `/usr/share/nginx/html/spa`, `marker=httpactions/prompt_lib`, `count=1` OK)
- **Disposable production-profile Docker stack (`p5d1v` at `http://p5d1v.localhost:18440/app/`)**:
  - `stack.sh verify-profile --name p5d1v`: **`50 passed, 0 failed`** (exit `0`; live run on shared migration `0269` prior to renumbering to `0270` after `#3002`, tenant migrations, and agentstate migrations at worktree head; real-model dump restored).
- **Real-browser acceptance (`Settings > HTTP actions`, `admin@centry.user`, zero mocks)**:
  - **Project & permission scoping**: On `Private` (`projectID: 2`) UI fetched `GET /api/v2/httpactions/prompt_lib/2/rule-names` (`200`) with no Add button; on `promptlib_public` (`projectID: 1`) UI fetched `GET /api/v2/httpactions/prompt_lib/1/rules` (`200`) and rendered `Add an HTTP action rule`.
  - **Create, reload, disable, & no hard-delete**: Created rule `github_status_check` (`https://api.github.com`, `/status`, `GET`, toolkit `github`, tool `status_check`) via `POST .../1/rules` -> **`201 Created`** (`id: 1`, `revision: 1`, `enabled: true`). First hard browser reload preserved `github_status_check` (`enabled: true`). Toggled `Toggle enabled` off via `PUT .../1/rules/1` -> **`200 OK`** (`revision: 2`, `enabled: false`); verified no hard-delete button/option in table Actions or Edit dialog; second hard browser reload preserved `github_status_check` (`enabled: false`).
  - **Private & metadata origin refusal**: Submitting `https://127.0.0.1` (`test_loopback_deny`) and `https://169.254.169.254` (`test_metadata_deny`) each returned **`400 Bad Request`** with readable error `HTTP action rule origin does not resolve to a permitted destination` and left only `github_status_check` in the table (`0` unexpected console errors).
  - **`rule-names` redaction check**: `GET http://p5d1v.localhost:18440/api/v2/httpactions/prompt_lib/1/rule-names` returned **`200`** with `{"items":[{"name":"github_status_check","enabled":false}]}` and zero `origin`, `path_template`, or credential fields.
- **Fixtures honesty**: Unit and Web component tests (`HttpActions.test.tsx`) use in-process fakes/doubles and MSW mocks. Live PostgreSQL 18 tests run ledgered migrations against `elitea-p5d1v-postgres-1`, and all `p5d1v` browser rules were created and verified directly through the UI (`0` DB seeding, `0` response mocks).

## Follow-ups

1. **5d-2**: Admitted `type: http` node in Worker, `agent-runtime`, Main `admittedNodeTypes`, and Web pipeline editor (closes remaining half of FU-072 and browser acceptance §6 items 2–5).
2. **5d-3**: HTTP actions inside Parallel/Map fan-out children (`scope_ref` + `execution_saved_child_scopes`).
3. **D3 follow-up**: Delegated user OAuth and mTLS client certificates for `openapi` configurations.
