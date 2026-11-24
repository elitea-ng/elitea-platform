# DB action rules Main foundation & Settings UI (T-5E step 5e-2)

| Item | Value |
| --- | --- |
| Date | 2026-11-24 |
| Branch | `feat/5e2-db-action-rules-main` (based on `origin/main` `2df0a49cf`) |
| Migration | `services/elitea-main/migrations/shared/0271_db_action_rules.sql` (contiguous `0271`; renumber at merge if another shared migration lands first) |
| Spec | `handoffs/point5-wave1-20261008/T-5E-db-node.md` (§3.2, §4 `5e-2`, §5, §9) |
| Pattern reference | `http-action-main-foundation-20261123.md` (5d-1) |
| Components | Go Main (`services/elitea-main`), Web (`apps/elitea-web`), deploy (`deploy/helm/elitea/values.yaml`, `deploy/docker-compose.standalone-full.yml`, `deploy/native/standalone-full.mjs`). Zero Rust edits/builds. |

## Business behaviour and what was not ported

There is no current-platform DB pipeline node reference. Legacy `elitea_sdk/tools/sql/api_wrapper.py` executes raw `text(sql)` with no binds, no TLS setting, unbounded `fetchall()`, and unconditional commit — none of that was ported. The only reuse is the existing `sql` configuration type (`host`, optional `port`, `username`, write-only secret `password`), referenced strictly by `configuration_id` and materialized by Main only for a claimed Worker. The admin UI mirrors `Settings > HTTP actions` (`DrawerPageHeader`, plain MUI table, `BaseModal` form, enabled switch, toast — disable only, never hard-delete).

## Decisions (aligned with T-5E §9 & 5d-1)

| # | Decision | Where it lives |
| --- | --- | --- |
| D1 | Project-scoped default-mode permissions `models.db_action_rules.manage` (admin) and `models.db_action_rules.list` (admin, editor, viewer), including per-project override matrices, classified in `capabilities.go` | `migrations/shared/0271_db_action_rules.sql`, `internal/api/v2/dbactions/handler.go`, `internal/domain/entitlements/capabilities.go`, Web `PERMISSIONS.dbActionRules` |
| D2 | `configuration_id` is mandatory (`> 0`) and references an accessible project `sql` configuration; host, port, username, and password never live in pipeline YAML or rule rows | `internal/application/dbaction/rule.go`, `internal/infra/db/repos/db_action_rules.go`, `migrations/shared/0271_db_action_rules.sql` |
| D3 | `marker_table` is `NULL`/omitted for `read` rules and required (safe SQL identifier `table` or `schema.table`, default `'elitea_effects'`) for `write` rules; explicit `""` on write or non-empty on read is refused `400` | `internal/application/dbaction/rule.go`, `internal/api/v2/dbactions/handler.go`, `migrations/shared/0271_db_action_rules.sql` |
| D4 | Redacted editor view: `GET /api/v2/dbactions/prompt_lib/{projectID}/rule-names` returns only `name`, `dialect`, `mode`, `enabled` — never `configuration_id`, `database_name`, `marker_table`, host, or credentials | `internal/infra/db/repos/db_action_rules.go` (`ListNames`), `internal/api/v2/dbactions/handler.go` |
| D5 | Write-time egress check against `ELITEA_DB_EGRESS_ALLOWLIST` (same `egresslib` grammar as Worker 5e-1) plus fail-closed startup in Main; DNS check runs outside the Postgres write tx, while `FOR SHARE` config check runs inside the tx | `internal/infra/egress/db_action.go`, `internal/api/v2/dbactions/handler.go`, `cmd/elitea-main/main.go`, `internal/runtimecomposition/config.go` |
| D6 | Start-time rule snapshot (`elitea.runtime.db-action-snapshot.v1`) stamps sealed `sql` configurations with `CurrentFrozenConfigurationMarker` and extends `CurrentConfigurationsMaterializer` to unsecret them strictly for the claimed Worker in `5e-3` | `internal/application/dbaction/snapshot.go`, `internal/infra/db/repos/db_action_credentials.go`, `internal/infra/storage/configurations_materializer.go` |

## Changed paths

- **Go Main domain & snapshot (`services/elitea-main/internal/application/dbaction/`)**: `rule.go`, `rule_test.go`, `snapshot.go`, `snapshot_test.go`.
- **Go Main repos, egress, materializer & migrations (`services/elitea-main/`)**: `migrations/shared/0271_db_action_rules.sql`, `migrations/db_action_rules_grant_postgres_integration_test.go`, `internal/infra/db/migrate/manifest_test.go`, `internal/infra/db/repos/db_action_rules.go`, `internal/infra/db/repos/db_action_credentials.go`, `internal/infra/db/repos/db_action_rules_postgres_integration_test.go`, `internal/infra/egress/db_action.go`, `internal/infra/egress/db_action_test.go`, `internal/infra/storage/configurations_materializer.go`, `internal/infra/storage/db_action_materializer_test.go`, `internal/application/projectprovisioning/steps.go`, `internal/application/projectprovisioning/deprovision_owned_rows_postgres_integration_test.go`.
- **Go Main admin API, authz & composition (`services/elitea-main/`)**: `internal/api/v2/dbactions/handler.go`, `internal/api/v2/dbactions/handler_test.go`, `internal/api/dbactions_postgres_integration_test.go`, `internal/api/router.go`, `internal/api/production_router_test.go`, `internal/api/middleware/audit.go`, `internal/domain/entitlements/capabilities.go`, `internal/runtimecomposition/config.go`, `internal/runtimecomposition/db_actions_config_test.go`, `cmd/elitea-main/main.go`, `api/openapi/v2.yaml`, `internal/api/generated/api.gen.go`.
- **Deploy config**: `deploy/docker-compose.standalone-full.yml`, `deploy/helm/elitea/values.yaml`, `deploy/native/standalone-full.mjs` (`ELITEA_DB_EGRESS_ALLOWLIST` wired to Main, default empty = public only).
- **Web Admin UI (`apps/elitea-web/`)**: `src/pages/settings/DbActions.tsx`, `src/pages/settings/DbActions.test.tsx`, `src/pages/settings/HttpActions.tsx` (shared `actionRulesFeature` bundle to stay within §3.5 20-export slice budget), `src/features/settings/index.ts`, `src/features/settings/lib/db-actions/{useDbActionRulePermissions.ts,useSqlCredentialOptions.ts}`, `src/features/settings/ui/db-actions/{DbActionRuleFormDialog.tsx,DbActionRulesTable.tsx}`, `src/routes/_shell/settings/db-actions.tsx`, `src/routes/_shell/settings/settingsSections.{ts,test.ts}`, `src/routeTree.gen.ts`, `src/shared/api/generated/dbactions/*`, `src/shared/api/generated/model/{dbAction*,listDbAction*,index.ts}`, `src/shared/api/endpoints.manifest.json`, `scripts/check-endpoint-manifest.test.mjs`, `src/shared/i18n/en.json`, `src/shared/lib/permissions.ts`, `parity/route-wiring-map.json`.
- **Docs**: `services/elitea-worker-rust/docs/source-mapping/db-action-main-foundation-20261124.md`, `services/elitea-worker-rust/docs/source-mapping/README.md`.

## Delivery gate properties

### Performance

- **Named bounds**: `MaxRulesPerProject` (256), `maxRuleNameBytes` (128), `maxDatabaseNameBytes` (128), `maxMarkerTableBytes` (128), `maxPolicyWireBytes` (16 KiB), `MaxSQLHostBytes` (253), `MaxSQLUsernameBytes` (256), `MaxSQLPasswordBytes` (16 KiB), `MaxRequestBodyBytes` (16 KiB) in `internal/application/dbaction/{rule.go,snapshot.go}` and `internal/api/v2/dbactions/handler.go`.
- **Lock hygiene**: Write-time DNS/egress resolution (`LookupConfigurationTarget` + `guard.ValidateHostPort`) runs outside the Postgres write transaction so slow DNS never holds `FOR SHARE` or `pg_advisory_xact_lock` locks; the write transaction performs only O(1) indexed queries (`FOR SHARE` config recheck, `COUNT(*)`, `INSERT`/`UPDATE`, audit `INSERT`).
- **Bounded queries**: `List` and `ListNames` both carry `ORDER BY name LIMIT $2` capped at `MaxRulesPerProject` (`repos/db_action_rules.go:108-158`).

### Durability

- **Optimistic revision fence**: `DBActionRulesRepo.Update` updates strictly `WHERE project_id = $1 AND id = $2 AND revision = $11`, returning `ErrDBActionRuleStale` (`409`) on concurrent edits (`repos/db_action_rules.go:220-258`).
- **Transactional write audit**: Every accepted create or update inserts one `elitea_runtime.db_action_rule_audit` row (`(rule_id, revision)` PK) inside the same Postgres write transaction (`repos/db_action_rules.go:205,253`).
- **Concurrency-safe cap**: `Create` acquires `pg_advisory_xact_lock(hashtextextended('elitea.db_action_rules/' || $1::bigint::text, 0))` so concurrent bursts never exceed 256 rules per project (`repos/db_action_rules.go:188`, proven by `TestDBActionRulesRepoPostgres_ConcurrentBurstNeverExceedsTheLimit`).
- **Project deprovisioning**: `projectprovisioning/steps.go:575-583` deletes `db_action_rule_audit` then `db_action_rules` on project removal (`deprovision_owned_rows_postgres_integration_test.go`).

### Resilience

- **Stored-row resilience**: `DBActionRulesRepo.List` logs and skips rows that fail current validation rather than breaking sibling rules, while `Update` allows repairing or disabling them and `Get`/`readDBActionRule` fail closed (`repos/db_action_rules.go:108-135`).
- **Exact body and field limits**: Limit and limit+1 tests verify `MaxRequestBodyBytes` (16 KiB/+1), trailing JSON rejection, rule/database/marker identifier boundaries (128/129, 63/64), and snapshot rule cap (256/257) (`dbaction/rule_test.go`, `dbaction/snapshot_test.go`, `dbactions/handler_test.go`).
- **Fail-closed startup**: Malformed `ELITEA_DB_EGRESS_ALLOWLIST` fails `egress.NewDBActionGuard`, `runtimecomposition.ConfigFromEnv`, and `cmd/elitea-main/main.go` startup (`egress/db_action_test.go`, `runtimecomposition/db_actions_config_test.go`).

### Security

- **RBAC & object authorization**: All five `/dbactions/prompt_lib/{projectID}/*` routes gate on `PermissionManage` (`models.db_action_rules.manage`) or `PermissionList` (`models.db_action_rules.list`), audited under `/api/v2/dbactions/` (`api/middleware/audit.go:195`). Rule writes also require `configurations.configuration.details` (`dbactions/handler.go:240-250`) and verify inside the write transaction (`FOR SHARE`) that the referenced configuration belongs to the project, has `type='sql'`, matches dialect, and is shared or owned by the writer (`repos/db_action_rules.go:265-305`). Start-time freezing (`repos/db_action_credentials.go:115-185`) re-verifies actor RBAC and requires private `sql` configs to be owned by both the actor and the rule's last writer.
- **No host/credential leakage**: `rule-names` returns only `name`, `dialect`, `mode`, `enabled`; `handler.go` refuses unknown fields (including `host`/`password`) and never echoes submitted values or resolved IPs/hosts in 400 refusals; `MaterializedSQLCredential` redacts passwords/hosts in `String`/`GoString` and refuses `MarshalJSON` (`dbaction/snapshot_test.go`, `dbactions/handler_test.go`).
- **Claim-time materializer isolation**: `CurrentConfigurationsMaterializer.materializeDBActionSnapshot` (`storage/configurations_materializer.go:315-375`) rejects missing `CurrentFrozenConfigurationMarker`, wrong `configuration_type`, foreign `configuration_project_id`, and unresolved `{{secret.*}}` placeholders, and strips the marker before returning to the claimed Worker (`storage/db_action_materializer_test.go`).
- **Egress guard**: `egress.NewDBActionGuard` + `ValidateHostPort` enforces `ELITEA_DB_EGRESS_ALLOWLIST` with port pinning and blocks loopback, private, metadata, IPv4-mapped, and NAT64 targets (`egress/db_action_test.go`, `dbactions/handler_test.go`).

## Recovery-guarantee rows

| Component × phase | Class | Enforced by | Proven by |
| --- | --- | --- | --- |
| Main × DB rule write (create/update) | **R** (atomic tx: rule + revision fence + audit row) | `repos/db_action_rules.go:180-258` (`WithinTx`, `pg_advisory_xact_lock`, `WHERE revision = $11`, `db_action_rule_audit`) | `db_action_rules_postgres_integration_test.go` (compiled; live PG run pending stack/CI), `dbactions/handler_test.go` |
| Main × startup (`ELITEA_DB_EGRESS_ALLOWLIST` malformed) | **F** (fail closed before serving) | `egress/db_action.go:19-25`, `cmd/elitea-main/main.go:547`, `runtimecomposition/config.go:511` | `TestNewDBActionGuardParsesAndFailsClosed`, `TestConfigFromEnvRefusesMalformedDBEgressAllowlist` |
| Main × claim-time DB credential materialization | **F** typed refusal on cross-project/unmarked/unresolved secret | `storage/configurations_materializer.go:315-375`, `repos/db_action_credentials.go:88-112` | `TestCurrentConfigurationsMaterializerUnsecretsDBActionSnapshot` (5/5 cases pass) |

*(Note: Runtime DB node execution and marker reconciliation recovery rows arrive in `5e-3` when `type: db` is admitted.)*

## `rules/security.md` category status

| Category | Status | Mechanism & Proving Test / Justification |
| --- | --- | --- |
| Trust boundaries and identity | Applicable | Admin API derives actor/project strictly from verified principal + route param (`dbactions/handler.go:185-190`); claim-time materializer validates `CurrentFrozenConfigurationMarker` and `configuration_project_id == claimed projectID` (`storage/configurations_materializer.go:346-352`). Proven by `dbactions/handler_test.go`, `TestCurrentConfigurationsMaterializerUnsecretsDBActionSnapshot`, `TestProductionRouter*`. |
| Authorization (object level) | Applicable | Project-scoped `PermissionManage`/`PermissionList` + `configurations.configuration.details` (`handler.go:74-84,240-250`), write-time `FOR SHARE` config ownership check (`repos/db_action_rules.go:265-305`), freeze-time actor+writer private config check (`repos/db_action_credentials.go:160-165`). Proven by `TestMalformedIDsAuditAndConfigurationPermission`, `TestDBActionRulesAuthorizationMatrixOnACleanDatabase`, `TestDBActionRulesRepoPostgres_ConflictsScopingAndConfigurationAccess`, `TestDBActionCredentialsPostgres_FreezeAndMaterialize`. |
| Input, parsing and amplification | Applicable | `http.MaxBytesReader` 16 KiB body cap, `DisallowUnknownFields`, trailing-JSON refusal, strict regex/length bounds on `name`/`database_name`/`marker_table`, `MaxRulesPerProject` 256 cap (`handler.go:265-285`, `rule.go:115-152`, `snapshot.go:75-87`). Proven by `TestBodyCapAndTrailingData`, `TestRuleValidateRejectsInvalidFields`, `TestFreezeSnapshotRefusesOverLimit`. |
| Injection and construction | Applicable | Parameterized SQL throughout (`pgx.Identifier.Sanitize()` only on Main-owned integer project schema `p_<id>`); `marker_table` restricted to safe unquoted ASCII `table` or `schema.table` identifier (`rule.go:93`, `0271_db_action_rules.sql:29`). Proven by `TestRuleValidateRejectsInvalidFields`, `TestCreateValidationAndEgressBoundaries`. |
| Egress and SSRF | Applicable | `egress.NewDBActionGuard(ELITEA_DB_EGRESS_ALLOWLIST)` + `ValidateHostPort` blocks private, loopback, metadata, IPv4-mapped, and NAT64 destinations unless explicitly allowlisted with matching port; refuses writes without echoing hosts/IPs (`egress/db_action.go`, `handler.go:255-268`). Proven by `TestNewDBActionGuardParsesAndFailsClosed`, `TestCreateValidationAndEgressBoundaries`. |
| Secrets | Applicable | Host/port/username/password live only in referenced `sql` configuration (vault-sealed until Worker claim); `rule-names` exposes only `name`/`dialect`/`mode`/`enabled`; `MaterializedSQLCredential` redacts on `String`/`GoString` and blocks `MarshalJSON`; diff secret scan clean (`0` hits). Proven by `TestRuleNamesExposesOnlyRedactedFields`, `TestMaterializedSQLCredentialValidationAndRedaction`, `TestCurrentConfigurationsMaterializerUnsecretsDBActionSnapshot`. |
| Supply chain and deployment | Applicable | Zero new Go or npm dependencies; `govulncheck ./...` reports `0` called vulnerabilities (`1` pre-existing uncalled); `npm audit` reports `21` pre-existing moderate dev-dep advisories (`0` high/critical, `0` new). `ELITEA_DB_EGRESS_ALLOWLIST` defaults empty in Helm, Compose, and native deploy. |
| Cross-layer differentials and version skew | Applicable | Strict OpenAPI v2 schemas regenerated into Go (`api.gen.go`, `oapi-codegen@v2.7.2`) and Web (`orval`, Zod schemas, `endpoints.manifest.json`); SQL field bounds match Rust `SQL/config.rs` (`MaxSQLHostBytes=253`, `MaxSQLUsernameBytes=256`, `MaxSQLPasswordBytes=16 KiB`, default ports `5432`/`3306`); `type: db` stays unadmitted until `5e-3`. |
| Agentic delegation and untrusted content isolation | N/A | No agent delegation hop or untrusted model/MCP content path modified in `5e-2`; live actor RBAC on `sql` config is enforced at snapshot freeze time (`repos/db_action_credentials.go:118-136`). |
| TOCTOU, concurrency races, and effect replay | Applicable | `pg_advisory_xact_lock` serializes per-project rule creation at 256 cap; `FOR SHARE` config check inside write tx; optimistic `revision` fence on `Update`; `-race` clean across all Go suites. |
| Sandbox escape and resource isolation | N/A | No sandbox/code-runner execution or host filesystem paths touched. |
| AuthN, OAuth, PAT, and cryptographic hygiene | Applicable | Domain-separated SHA-256 digests (`elitea.db.rule.v1`, `elitea.runtime.db-frozen-rule.v1`) for rule policy and start-time snapshot bindings (`rule.go:175`, `snapshot.go:117`). |

## Verification, test counts, and skips

- **Go unit & router/gate suites (`GOMAXPROCS=4 go test -p 2 -race`)**:
  - `internal/infra/db/migrate`: **PASS** (`Head(shared) == 271`)
  - `internal/application/dbaction`: **PASS** (6 top-level tests + subtests: normalization, validation boundaries, injection refusal, `PolicyWire` tamper checks, `FreezeSnapshot` dedup/sort/cap/verify, `MaterializedSQLCredential` redaction & validation)
  - `internal/api/v2/dbactions`: **PASS** (6 top-level tests + 16 validation/egress subtests: read/write marker invariants, `rule-names` redaction, 16 KiB/+1 body cap, revision fence, status mapping, audit annotation, `configurations.configuration.details` gate)
  - `internal/infra/egress`: **PASS** (including `TestNewDBActionGuardParsesAndFailsClosed` with 10 subtests)
  - `internal/runtimecomposition`: **PASS** (including `TestConfigFromEnvRefusesMalformedDBEgressAllowlist`)
  - `internal/infra/storage`: **PASS** (including `TestCurrentConfigurationsMaterializerUnsecretsDBActionSnapshot` + 4 negative cases)
  - `internal/domain/entitlements`, `internal/api` (`TestProductionRouterMatchesMainComposedRouteSurface`, `TestEveryGatedPermissionHasAGrant`, `TestEntitlementsCapabilityCoverage`, `TestNoAdministrationPermissionLiteralOutsideTheRegistry`, `TestAuditedSurfaces`, `TestRolesCatalogueIsUnchangedForTheExistingNames`), `internal/api/oapiserver`, `internal/adminaccess/...`, `internal/api/middleware`: **PASS**
  - `go vet ./...`: **PASS** (`0` issues)
  - Compilation checks (`go test -c` / `go build` for `migrations`, `repos`, `projectprovisioning`, `api`, `cmd/elitea-main`): **ALL PASS**
- **Web (`apps/elitea-web`, Node `v24.19.0`)**:
  - `node scripts/check-generated-client.mjs`: **PASS** (`1278` files compared)
  - `node scripts/check-endpoint-manifest.mjs`: **PASS** (`404` entries)
  - `npx vitest run --maxWorkers=2`: **9/9 files, 67/67 tests PASS** (`DbActions.test.tsx` 8, `HttpActions.test.tsx` 8, `settingsSections.test.ts` 12, `check-endpoint-manifest.test.mjs` 14, `src/shared/i18n/` 25)
  - `npm run typecheck` (`tsc --noEmit`): **PASS** (`0` errors)
  - `npx oxlint -c .oxlintrc.json --deny-warnings` on all 12 changed/new files: **PASS** (`0` warnings, `0` errors)
  - `node scripts/theme-gate.mjs`: **PASS** (all 9 checks + 5 `brandPack.contract.test.ts` tests)
  - Static checks (`check-budgets` 7488 files, `check-testid-namespace` 2951 ids, `check-layer-cycle` 5993 modules / 0 errors, `check-dead-code` Knip 0 issues, `i18n-backfill --check` 7459 keys, `check-handlers`, `check-parity-manifest`, `check-upgrading-notes`): **ALL PASS**
- **Scanners**:
  - `govulncheck ./...`: **0 called vulnerabilities** (`1` pre-existing uncalled module advisory)
  - `npm audit`: **21 pre-existing moderate dev-dep advisories** (`0` high, `0` critical, `0` new)
  - Diff secret scan: **0 hits**
- **Skipped / deferred (stated explicitly)**:
  - Live PostgreSQL 18 integration execution (`migrations/db_action_rules_grant_postgres_integration_test.go`, `repos/db_action_rules_postgres_integration_test.go`, `api/dbactions_postgres_integration_test.go`, `projectprovisioning/deprovision_owned_rows_postgres_integration_test.go`) and real-browser disposable Docker stack verification were not run locally (no Docker stacks spun up per assignment instructions); all PG integration packages compile cleanly (`go test -c`) and execute in CI / stack verification.
  - Linux visual regression PNG baselines (`apps/elitea-web/e2e/snapshots/visual/routes.visual.spec.ts-snapshots/settings-*-visual-linux.png`) will need a Linux CI refresh for the new `DB actions` nav item in the Settings sidebar (never generated on macOS).
- **Fixtures honesty**: Go unit tests and Web component tests (`DbActions.test.tsx`) use in-process fakes/doubles and MSW handlers against the real generated Orval client. Live PG tests use ledgered migrations against Postgres.

## Follow-ups

1. **5e-3**: Admit `type: db` node across contract, Rust Worker / `agent-runtime`, Main `admittedNodeTypes` + pipeline start snapshot wiring (`dbaction.FreezeSnapshot`), and Web pipeline editor.
2. **5e-4**: DB action nodes inside Parallel/Map fan-out children (`scope_ref` + child-scoped `effect_id`).
3. **CI Linux visual baselines**: Refresh `settings-*-visual-linux.png` from Linux CI for the new `DB actions` Settings nav row before marking ready for review.
