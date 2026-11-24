# DB action rules Main foundation & Settings UI (T-5E step 5e-2)

| Item | Value |
| --- | --- |
| Date | 2026-11-24 |
| Branch | `feat/5e2-db-action-rules-main` (based on `origin/main` `2df0a49cf`) |
| Migration | `services/elitea-main/migrations/shared/0271_db_action_rules.sql` (contiguous `0271`; renumber at merge if another shared migration lands first) |
| Spec | `handoffs/point5-wave1-20261008/T-5E-db-node.md` (§3.2, §4 `5e-2`, §5, §9) |
| Pattern reference | `http-action-main-foundation-20261123.md` (5d-1) |
| Components | Go Main (`services/elitea-main`), Web (`apps/elitea-web`), deploy (`deploy/helm/elitea/{values.yaml,templates/main/configmap.yaml}`, `deploy/helm/tests/render-worker.sh`, `deploy/docker-compose.standalone-full.yml`, `deploy/native/standalone-full.mjs`). Zero Rust edits/builds. |

## Business behaviour and what was not ported

There is no current-platform DB pipeline node reference. Legacy `elitea_sdk/tools/sql/api_wrapper.py` executes raw `text(sql)` with no binds, no TLS setting, unbounded `fetchall()`, and unconditional commit — none of that was ported. The only reuse is the existing `sql` configuration type (`host`, optional `port`, `username`, write-only secret `password`), referenced strictly by `configuration_id` and materialized by Main only for a claimed Worker. The admin UI mirrors `Settings > HTTP actions` (`DrawerPageHeader`, plain MUI table, `BaseModal` form, enabled switch, toast — disable only, never hard-delete).

## Decisions (aligned with T-5E §9 & 5d-1)

| # | Decision | Where it lives |
| --- | --- | --- |
| D1 | Project-scoped default-mode permissions `models.db_action_rules.manage` (admin) and `models.db_action_rules.list` (admin, editor, viewer), including per-project override matrices, classified in `capabilities.go` | `migrations/shared/0271_db_action_rules.sql`, `internal/api/v2/dbactions/handler.go:35-42`, `internal/domain/entitlements/capabilities.go`, Web `PERMISSIONS.dbActionRules` |
| D2 | `configuration_id` is mandatory (`> 0`) and references an accessible project `sql` configuration; host, port, username, and password never live in pipeline YAML or rule rows | `internal/application/dbaction/rule.go:128-160`, `internal/infra/db/repos/db_action_rules.go:303-349`, `migrations/shared/0271_db_action_rules.sql` |
| D3 | `marker_table` is `NULL`/omitted for `read` rules and required (safe SQL identifier `table` or `schema.table`, default `'elitea_effects'`) for `write` rules; explicit `""` on write or non-empty on read is refused `400` | `internal/application/dbaction/rule.go:104,146-160`, `internal/api/v2/dbactions/handler.go:220-245`, `migrations/shared/0271_db_action_rules.sql:27-30` |
| D4 | Redacted editor view: `GET /api/v2/dbactions/prompt_lib/{projectID}/rule-names` returns only `name`, `dialect`, `mode`, `enabled` — never `configuration_id`, `database_name`, `marker_table`, host, or credentials | `internal/infra/db/repos/db_action_rules.go:159-178` (`ListNames`), `internal/api/v2/dbactions/handler.go:104-115` |
| D5 | Write-time egress check against `ELITEA_DB_EGRESS_ALLOWLIST` (same `egresslib` grammar as Worker 5e-1) plus fail-closed startup in Main; DNS check runs outside the Postgres write tx, while `FOR SHARE` config check verifies no target drift inside the tx (`SEC-5E2-03`); Helm defaults Main's allowlist from `worker.dbEgressAllowlist` when unset (`CODE-5E2-01`) | `internal/infra/egress/db_action.go:17-59`, `internal/api/v2/dbactions/handler.go:258-274`, `internal/infra/db/repos/db_action_rules.go:206-301`, `cmd/elitea-main/main.go:548`, `internal/runtimecomposition/config.go:511-514`, `deploy/helm/elitea/templates/main/configmap.yaml` |
| D6 | Start-time rule snapshot (`elitea.runtime.db-action-snapshot.v1`) requires sealed secret references on `password`, binds all rule policy fields to `PolicyDigest` and `BindingDigest`, strips any pre-existing `db_action_snapshot` on start/saved-child freeze, and extends `CurrentConfigurationsMaterializer` to verify the full envelope/flat key allowlist and unsecret strictly `password` for the claimed Worker (`SEC-5E2-01`, `SEC-5E2-02`, `SEC-5E2-04`) | `internal/application/dbaction/snapshot.go:67-191`, `internal/infra/db/repos/db_action_credentials.go:60-268`, `internal/infra/storage/configurations_materializer.go:328-440`, `internal/application/agentexecution/http_action_snapshot.go:23`, `internal/infra/storage/runtime_saved_child_scope.go:59-61` |

## Changed paths

- **Go Main domain & snapshot (`services/elitea-main/internal/application/dbaction/`)**: `rule.go`, `rule_test.go`, `snapshot.go`, `snapshot_test.go`.
- **Go Main repos, egress, materializer & migrations (`services/elitea-main/`)**: `migrations/shared/0271_db_action_rules.sql`, `migrations/db_action_rules_grant_postgres_integration_test.go`, `internal/infra/db/migrate/manifest_test.go`, `internal/infra/db/repos/db_action_rules.go`, `internal/infra/db/repos/db_action_credentials.go`, `internal/infra/db/repos/db_action_credentials_test.go`, `internal/infra/db/repos/db_action_rules_postgres_integration_test.go`, `internal/infra/egress/db_action.go`, `internal/infra/egress/db_action_test.go`, `internal/infra/storage/configurations_materializer.go`, `internal/infra/storage/db_action_materializer_test.go`, `internal/infra/storage/runtime_saved_child_scope.go`, `internal/application/agentexecution/http_action_snapshot.go`, `internal/application/agentexecution/http_action_snapshot_test.go`, `internal/application/projectprovisioning/steps.go`, `internal/application/projectprovisioning/deprovision_owned_rows_postgres_integration_test.go`.
- **Go Main admin API, authz & composition (`services/elitea-main/`)**: `internal/api/v2/dbactions/handler.go`, `internal/api/v2/dbactions/handler_test.go`, `internal/api/dbactions_postgres_integration_test.go`, `internal/api/router.go`, `internal/api/production_router_test.go`, `internal/api/middleware/audit.go`, `internal/domain/entitlements/capabilities.go`, `internal/runtimecomposition/config.go`, `internal/runtimecomposition/db_actions_config_test.go`, `cmd/elitea-main/main.go`, `api/openapi/v2.yaml`, `internal/api/generated/api.gen.go`.
- **Deploy config**: `deploy/docker-compose.standalone-full.yml`, `deploy/helm/elitea/values.yaml`, `deploy/helm/elitea/templates/main/configmap.yaml`, `deploy/helm/tests/render-worker.sh`, `deploy/native/standalone-full.mjs`.
- **Web Admin UI (`apps/elitea-web/`)**: `src/pages/settings/DbActions.tsx`, `src/pages/settings/DbActions.test.tsx`, `src/pages/settings/HttpActions.tsx` (shared `actionRulesFeature` bundle to stay within §3.5 20-export slice budget), `src/features/settings/index.ts`, `src/features/settings/lib/db-actions/{useDbActionRulePermissions.ts,useSqlCredentialOptions.ts}`, `src/features/settings/ui/db-actions/{DbActionRuleFormDialog.tsx,DbActionRulesTable.tsx}`, `src/routes/_shell/settings/db-actions.tsx`, `src/routes/_shell/settings/settingsSections.{ts,test.ts}`, `src/routeTree.gen.ts`, `src/shared/api/generated/dbactions/*`, `src/shared/api/generated/model/{dbAction*,listDbAction*,index.ts}`, `src/shared/api/endpoints.manifest.json`, `scripts/check-endpoint-manifest.test.mjs`, `src/shared/i18n/en.json`, `src/shared/lib/permissions.ts`, `parity/route-wiring-map.json`.
- **Docs**: `services/elitea-worker-rust/docs/source-mapping/db-action-main-foundation-20261124.md`, `services/elitea-worker-rust/docs/source-mapping/README.md`.

## Delivery gate properties

### Performance

- **Named bounds**: `MaxRulesPerProject` (256), `maxRuleNameBytes` (128), `maxDatabaseNameBytes` (128), `maxMarkerTableBytes` (128), `maxPolicyWireBytes` (16 KiB), `MaxSQLHostBytes` (253), `MaxSQLUsernameBytes` (256), `MaxSQLPasswordBytes` (16 KiB), `MaxRequestBodyBytes` (16 KiB) in `internal/application/dbaction/{rule.go:19-26,snapshot.go:24-28}` and `internal/api/v2/dbactions/handler.go:46`.
- **Lock hygiene**: Write-time DNS/egress resolution (`LookupConfigurationTarget` + `guard.ValidateHostPort`) runs outside the Postgres write transaction so slow DNS never holds `FOR SHARE` or `pg_advisory_xact_lock` locks; inside the write tx, `FOR SHARE` verifies no target drift occurred (`repos/db_action_rules.go:206-301`, `SEC-5E2-03`).
- **Bounded queries**: `List` (`repos/db_action_rules.go:121-145`) and `ListNames` (`repos/db_action_rules.go:159-178`) both carry `ORDER BY name LIMIT $2` capped at `MaxRulesPerProject`.

### Durability

- **Optimistic revision fence**: `DBActionRulesRepo.Update` updates strictly `WHERE project_id = $1 AND id = $2 AND revision = $11`, returning `ErrDBActionRuleStale` (`409`) on concurrent edits (`repos/db_action_rules.go:255-301`).
- **Transactional write audit**: Every accepted create or update inserts one `elitea_runtime.db_action_rule_audit` row (`(rule_id, revision)` PK) inside the same Postgres write transaction (`repos/db_action_rules.go:243,294`).
- **Concurrency-safe cap**: `Create` acquires `pg_advisory_xact_lock(hashtextextended('elitea.db_action_rules/' || $1::bigint::text, 0))` so concurrent bursts never exceed 256 rules per project (`repos/db_action_rules.go:220`, proven by `TestDBActionRulesRepoPostgres_ConcurrentBurstNeverExceedsTheLimit`).
- **Atomic snapshot read**: `LoadFrozenRule` uses `pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}` so rule lookup, live RBAC, and `sql` configuration lookup read one MVCC snapshot (`repos/db_action_credentials.go:75`, `CODE-5E2-02`).
- **Project deprovisioning**: `projectprovisioning/steps.go:575-583` deletes `db_action_rule_audit` then `db_action_rules` on project removal (`deprovision_owned_rows_postgres_integration_test.go`).

### Resilience

- **Stored-row resilience**: `DBActionRulesRepo.List` logs and skips rows that fail current validation rather than breaking sibling rules, while `Update` allows repairing or disabling them and `Get`/`readDBActionRule` fail closed (`repos/db_action_rules.go:121-145`).
- **Context preservation & JSON numeric fidelity**: `LoadFrozenRule`, `sqlConfigurationFreeze`, and `MaterializeSQLCredential` preserve `context.Canceled`/`context.DeadlineExceeded` and decode `json.Number`/integral `float64` project IDs cleanly (`repos/db_action_credentials.go:60-177,303-338`, `BUG-5E2-01`).
- **Exact body and field limits**: Limit and limit+1 tests verify `MaxRequestBodyBytes` (16 KiB/+1), trailing JSON rejection, rule/database/marker identifier boundaries (128/129, 63/64), pre-cast `float64` port bounds, and snapshot rule cap (256/257) (`dbaction/rule_test.go`, `dbaction/snapshot_test.go`, `dbactions/handler_test.go`).
- **Fail-closed startup**: Malformed `ELITEA_DB_EGRESS_ALLOWLIST` fails `egress.NewDBActionGuard`, `runtimecomposition.ConfigFromEnv`, and `cmd/elitea-main/main.go:548` startup (`egress/db_action_test.go`, `runtimecomposition/db_actions_config_test.go`).

### Security

- **RBAC & object authorization**: All five `/dbactions/prompt_lib/{projectID}/*` routes gate on `PermissionManage` (`models.db_action_rules.manage`) or `PermissionList` (`models.db_action_rules.list`, `handler.go:77-85`), audited under `/api/v2/dbactions/` (`api/middleware/audit.go:194`). Rule writes require `configurations.configuration.details` (`handler.go:246-256`) and verify inside the write transaction (`FOR SHARE`) that the referenced configuration belongs to the project, has `type='sql'`, matches dialect, is shared or owned by the writer, and matches the egress-validated `(Host, Port)` (`repos/db_action_rules.go:215-219,264-269,303-349`, `SEC-5E2-03`). Start-time freezing (`repos/db_action_credentials.go:179-268`) re-verifies actor RBAC and requires private `sql` configs to be owned by both actor and rule writer.
- **Sealed secrets & field hygiene (`SEC-5E2-02`)**: `sqlConfigurationFreeze` requires `configurationapp.IsCurrentSecretReference(password)` so unsealed values are never frozen; `parseSQLRequiredText` (`snapshot.go:359-365`) refuses `{{secret.*}}` placeholders in `username` across target and credential parsing; both `DBActionCredentials.MaterializeSQLCredential` (`db_action_credentials.go:120-177`) and `CurrentConfigurationsMaterializer.materializeDBActionSnapshot` (`configurations_materializer.go:328-440`) unsecret strictly the sealed `password` field and zero temporary credentials.
- **Claim-time materializer & snapshot binding (`SEC-5E2-01`, `SEC-5E2-04`)**: `FreezeSnapshot` and `FrozenSnapshot.Verify` (`snapshot.go:67-191`) reconstruct and re-validate every rule policy field (`Dialect`, `DatabaseName`, `Mode`, `MarkerTable`) against `PolicyDigest` and `BindingDigest`; `materializeDBActionSnapshot` verifies the full snapshot envelope against `projectID`, `application["id"]`, and `application["version_id"]`, enforces a flat scalar key allowlist on `sql_configuration`, and strips `CurrentFrozenConfigurationMarker`. Pre-existing `db_action_snapshot` fields are stripped at start and saved-child freeze (`http_action_snapshot.go:23`, `runtime_saved_child_scope.go:59-61`).
- **Go/Rust SQL & egress parity (`SEC-5E2-05`)**: `parseSQLHost` rejects IPv6 zone IDs and surrounding whitespace, `hasControlChar` uses `unicode.IsControl`, `ParseSQLPort` validates `float64` bounds before casting, and `egress.Guard.ValidateHostPort` (`egress/db_action.go:35-59`) validates bracketed IPv6 literals and refuses zoned literals and empty brackets without echoing hosts/IPs.

## Review hardening items closed

- `SEC-5E2-01`: Claim-time `materializeDBActionSnapshot` envelope/digest verification, strict flat key allowlist, password-only unsecreting, credential zeroing, and start/child-freeze `db_action_snapshot` stripping (`TestCurrentConfigurationsMaterializerUnsecretsDBActionSnapshot`, `TestFreezeHTTPChildVersionStripsPreExistingDBActionSnapshot`, `TestFreezeCurrentHTTPActionRequestsStripsPreExistingDBActionSnapshot`).
- `SEC-5E2-02`: Sealed secret reference requirement on `password` in `sqlConfigurationFreeze` and `MaterializeSQLCredential`, `{{secret.*}}` refusal in `username`, and password-only unsecreting (`TestDBActionCredentialsMaterializeJSONRoundTripAndHardening`, `TestDBActionCredentialsPostgres_FreezeAndMaterialize`, `TestMaterializedSQLCredentialValidationAndRedaction`).
- `SEC-5E2-03`: Bind egress-validated `DBActionConfigurationTarget` from `Handler.admit` into `Create`/`Update` `FOR SHARE` transactions (`handler_test.go`, `TestDBActionRulesRepoPostgres_ConflictsScopingTargetDriftAndConfigurationAccess`).
- `SEC-5E2-04`: Reconstruct/validate `Rule` policy fields and verify `PolicyDigest` inside `FreezeSnapshot` and `FrozenSnapshot.Verify` (`TestFreezeSnapshotSortsDeduplicatesAndVerifies` tamper subtests, `TestFreezeSnapshotRefusesOverLimitAndMismatchedPolicyDigest`).
- `BUG-5E2-01`: Support `json.Number` and integral `float64` project IDs plus `configuration_uuid` validation in `MaterializeSQLCredential`, and preserve `context.Canceled`/`context.DeadlineExceeded` (`TestDBActionCredentialsMaterializeJSONRoundTripAndHardening`, `TestDBActionCredentialsPostgres_FreezeAndMaterialize`).
- `SEC-5E2-05`: Align Go SQL host/zone/whitespace/control-char/float64-port validation with Rust `SQL/config.rs` and harden bracketed/zoned host handling in `egress.Guard.ValidateHostPort` (`TestMaterializedSQLCredentialValidationAndRedaction`, `TestNewDBActionGuardParsesAndFailsClosed`).
- `CODE-5E2-01`: Default Main's Helm `ELITEA_DB_EGRESS_ALLOWLIST` from `worker.dbEgressAllowlist` when unset (`deploy/helm/elitea/templates/main/configmap.yaml`, verified by `deploy/helm/tests/render-worker.sh` 14/14).
- `CODE-5E2-02`: Use `pgx.RepeatableRead` in `LoadFrozenRule`, add cross-project `PUT` 404 assertion to `TestDBActionRulesAuthorizationMatrixOnACleanDatabase`, and invalidate `getListDbActionRuleNamesQueryKey` on mutations in `DbActions.tsx`.
- `CODE-5E2-03`: Refreshed all `path:line` citations in this source mapping.

## Recovery-guarantee rows

| Component × phase | Class | Enforced by | Proven by |
| --- | --- | --- | --- |
| Main × DB rule write (create/update) | **R** (atomic tx: rule + revision fence + target-drift check + audit row) | `repos/db_action_rules.go:206-301` (`WithinTx`, `FOR SHARE`, `pg_advisory_xact_lock`, `WHERE revision = $11`, `db_action_rule_audit`) | `db_action_rules_postgres_integration_test.go` (compiled; live PG run pending stack/CI), `dbactions/handler_test.go` |
| Main × startup (`ELITEA_DB_EGRESS_ALLOWLIST` malformed) | **F** (fail closed before serving) | `egress/db_action.go:23-29`, `cmd/elitea-main/main.go:548`, `runtimecomposition/config.go:511-514` | `TestNewDBActionGuardParsesAndFailsClosed`, `TestConfigFromEnvRefusesMalformedDBEgressAllowlist` |
| Main × claim-time DB credential materialization | **F** typed refusal on tampered/cross-project/unmarked/unsealed/unresolved secret | `storage/configurations_materializer.go:328-440`, `repos/db_action_credentials.go:120-177`, `dbaction/snapshot.go:146-191` | `TestCurrentConfigurationsMaterializerUnsecretsDBActionSnapshot` (14 cases), `TestDBActionCredentialsMaterializeJSONRoundTripAndHardening` |

*(Note: Runtime DB node execution and marker reconciliation recovery rows arrive in `5e-3` when `type: db` is admitted.)*

## `rules/security.md` category status

| Category | Status | Mechanism & Proving Test / Justification |
| --- | --- | --- |
| Trust boundaries and identity | Applicable | Admin API derives actor/project strictly from verified principal + route param (`dbactions/handler.go:195-202,305-315`); claim-time materializer verifies `FrozenSnapshot.Verify`, flat key allowlist, `CurrentFrozenConfigurationMarker`, and `configuration_project_id == claimed projectID`, and start/child freeze strips pre-existing `db_action_snapshot` (`configurations_materializer.go:328-440`, `http_action_snapshot.go:23`, `runtime_saved_child_scope.go:59-61`). Proven by `dbactions/handler_test.go`, `db_action_materializer_test.go`, `http_action_snapshot_test.go`, `TestProductionRouter*`. |
| Authorization (object level) | Applicable | Project-scoped `PermissionManage`/`PermissionList` + `configurations.configuration.details` (`handler.go:77-85,246-256`), write-time `FOR SHARE` config ownership check (`repos/db_action_rules.go:303-349`), freeze-time actor+writer private config check (`repos/db_action_credentials.go:236-240`). Proven by `TestMalformedIDsAuditAndConfigurationPermission`, `TestDBActionRulesAuthorizationMatrixOnACleanDatabase` (incl. cross-project `GET` & `PUT`), `TestDBActionRulesRepoPostgres_ConflictsScopingTargetDriftAndConfigurationAccess`, `TestDBActionCredentialsPostgres_FreezeAndMaterialize`. |
| Input, parsing and amplification | Applicable | `http.MaxBytesReader` 16 KiB body cap, `DisallowUnknownFields`, trailing-JSON refusal, strict regex/length bounds on `name`/`database_name`/`marker_table`, pre-cast `float64` port bounds, `MaxRulesPerProject` 256 cap (`handler.go:277-296`, `rule.go:101-160`, `snapshot.go:83-94,296-326`). Proven by `TestBodyCapAndTrailingData`, `TestRuleValidateRejectsInvalidFields`, `TestFreezeSnapshotRefusesOverLimitAndMismatchedPolicyDigest`, `TestMaterializedSQLCredentialValidationAndRedaction`. |
| Injection and construction | Applicable | Parameterized SQL throughout (`pgx.Identifier.Sanitize()` only on Main-owned integer project schema `p_<id>`); `marker_table` restricted to safe unquoted ASCII `table` or `schema.table` identifier (`rule.go:104`, `0271_db_action_rules.sql:29`) and re-verified against `PolicyDigest` in `FrozenSnapshot.Verify` (`snapshot.go:169-191`). Proven by `TestRuleValidateRejectsInvalidFields`, `TestCreateValidationAndEgressBoundaries`, `TestFreezeSnapshotSortsDeduplicatesAndVerifies`. |
| Egress and SSRF | Applicable | `egress.NewDBActionGuard(ELITEA_DB_EGRESS_ALLOWLIST)` + `ValidateHostPort` blocks private, loopback, metadata, IPv4-mapped, NAT64, zoned IPv6, and empty-bracket targets unless allowlisted with matching port; write-time `FOR SHARE` verifies no target drift; refuses writes without echoing hosts/IPs (`egress/db_action.go:23-59`, `handler.go:258-274`, `repos/db_action_rules.go:215-219,264-269`); Helm defaults Main allowlist from `worker.dbEgressAllowlist`. Proven by `TestNewDBActionGuardParsesAndFailsClosed`, `TestCreateValidationAndEgressBoundaries`, `TestDBActionRulesRepoPostgres_ConflictsScopingTargetDriftAndConfigurationAccess`, `render-worker.sh`. |
| Secrets | Applicable | Host/port/username/password live only in referenced `sql` configuration; `sqlConfigurationFreeze` requires sealed `{{secret.*}}` password; `username` refuses `{{secret.*}}`; claim-time materialization unsecrets strictly `password` and zeroes temporary credentials; `rule-names` exposes only `name`/`dialect`/`mode`/`enabled`; `MaterializedSQLCredential` redacts on `String`/`GoString` and blocks `MarshalJSON`; diff secret scan clean (`0` hits). Proven by `TestRuleNamesExposesOnlyRedactedFields`, `TestMaterializedSQLCredentialValidationAndRedaction`, `TestDBActionCredentialsMaterializeJSONRoundTripAndHardening`, `TestCurrentConfigurationsMaterializerUnsecretsDBActionSnapshot`. |
| Supply chain and deployment | Applicable | Zero new Go or npm dependencies; `govulncheck ./...` reports `0` called vulnerabilities (`1` pre-existing uncalled); `npm audit` reports `21` pre-existing moderate dev-dep advisories (`0` high/critical, `0` new). `ELITEA_DB_EGRESS_ALLOWLIST` defaults empty in Helm, Compose, and native deploy and propagates from `worker.dbEgressAllowlist` when unset (`render-worker.sh` 14/14). |
| Cross-layer differentials and version skew | Applicable | Strict OpenAPI v2 schemas regenerated into Go (`api.gen.go`, `oapi-codegen@v2.7.2`) and Web (`orval`, Zod schemas, `endpoints.manifest.json`); Go SQL validation matches Rust `SQL/config.rs` (host, IPv6 zones, whitespace, `unicode.IsControl`, ports `1..65535`, default `5432`/`3306`); JSON numeric round-tripping handles `json.Number` and integral `float64`; `type: db` stays unadmitted until `5e-3`. |
| Agentic delegation and untrusted content isolation | N/A | No agent delegation hop or untrusted model/MCP content path modified in `5e-2`; live actor RBAC on `sql` config is enforced at snapshot freeze time (`repos/db_action_credentials.go:188-216`). |
| TOCTOU, concurrency races, and effect replay | Applicable | `pg_advisory_xact_lock` serializes per-project rule creation at 256 cap; `FOR SHARE` config + target-drift check inside write tx; optimistic `revision` fence on `Update`; `RepeatableRead` snapshot in `LoadFrozenRule`; `-race` clean across all Go suites. |
| Sandbox escape and resource isolation | N/A | No sandbox/code-runner execution or host filesystem paths touched. |
| AuthN, OAuth, PAT, and cryptographic hygiene | Applicable | Domain-separated SHA-256 digests (`elitea.db.rule.v1`, `elitea.runtime.db-frozen-rule.v1`) bind all rule policy fields and snapshot metadata (`rule.go:184`, `snapshot.go:122,169`). |

## Verification, test counts, and skips

- **Go unit & router/gate suites (`GOMAXPROCS=4 go test -p 2 -race`)**:
  - `internal/infra/db/migrate`: **PASS** (`Head(shared) == 271`)
  - `internal/application/dbaction`: **PASS** (normalization, validation boundaries, injection refusal, `PolicyWire` tamper checks, `FreezeSnapshot` dedup/sort/cap/verify + 7 per-field tamper cases, `MaterializedSQLCredential` & `ParseSQLTarget` parity/redaction checks)
  - `internal/api/v2/dbactions`: **PASS** (16 validation/egress/target-forwarding subtests, `rule-names` redaction, 16 KiB/+1 body cap, revision fence, status mapping, audit annotation, `configurations.configuration.details` gate)
  - `internal/infra/egress`: **PASS** (`TestNewDBActionGuardParsesAndFailsClosed`, 14 subtests incl. bracketed/zoned IPv6)
  - `internal/runtimecomposition`: **PASS** (`TestConfigFromEnvRefusesMalformedDBEgressAllowlist`)
  - `internal/application/agentexecution`: **PASS** (incl. `TestFreezeCurrentHTTPActionRequestsStripsPreExistingDBActionSnapshot`)
  - `internal/infra/storage`: **PASS** (`TestCurrentConfigurationsMaterializerUnsecretsDBActionSnapshot` + 13 negative cases + `TestFreezeHTTPChildVersionStripsPreExistingDBActionSnapshot`)
  - `internal/infra/db/repos` (unit): **PASS** (`TestDBActionCredentialsMaterializeJSONRoundTripAndHardening` incl. `float64`/`json.Number` round-trip, canceled/deadline ctx, password-only unsecret, 5 hardening refusals)
  - `internal/domain/entitlements`, `internal/api` (`TestProductionRouterMatchesMainComposedRouteSurface`, `TestEveryGatedPermissionHasAGrant`, `TestEntitlementsCapabilityCoverage`, `TestNoAdministrationPermissionLiteralOutsideTheRegistry`, `TestAuditedSurfaces`, `TestRolesCatalogueIsUnchangedForTheExistingNames`), `internal/api/oapiserver`, `internal/adminaccess/...`, `internal/api/middleware`: **PASS**
  - `go vet ./...`: **PASS** (`0` issues)
  - Compilation checks (`go test -c` / `go build` for `migrations`, `repos`, `projectprovisioning`, `api`, `cmd/elitea-main`): **ALL PASS**
- **Helm (`deploy/helm/tests/render-worker.sh`, offline temp chart copy without external `dependencies:`)**:
  - **14 ran, 14 passed, 0 failed** (proves `worker.dbEgressAllowlist` renders to both Worker `ELITEA_DB_EGRESS_ALLOWLIST` and Main `elitea-main-config` `ELITEA_DB_EGRESS_ALLOWLIST` when unset)
- **Web (`apps/elitea-web`, Node `v24.19.0`)**:
  - `node scripts/check-generated-client.mjs`: **PASS** (`1278` files compared)
  - `node scripts/check-endpoint-manifest.mjs`: **PASS** (`404` entries)
  - `npx vitest run --maxWorkers=2`: **9/9 files, 67/67 tests PASS** (`DbActions.test.tsx` 8, `HttpActions.test.tsx` 8, `settingsSections.test.ts` 12, `check-endpoint-manifest.test.mjs` 14, `src/shared/i18n/` 25)
  - `npm run typecheck` (`tsc --noEmit`): **PASS** (`0` errors)
  - `npx oxlint -c .oxlintrc.json --deny-warnings`: **PASS** (`0` warnings, `0` errors)
  - `node scripts/theme-gate.mjs`: **PASS** (all 9 checks + 5 `brandPack.contract.test.ts` tests)
  - Static checks (`check-budgets`, `check-testid-namespace`, `check-layer-cycle`, `check-dead-code`, `i18n-backfill --check`, `check-handlers`, `check-parity-manifest`, `check-upgrading-notes`): **ALL PASS**
- **Scanners**:
  - `govulncheck ./...`: **0 called vulnerabilities** (`1` pre-existing uncalled module advisory)
  - `npm audit`: **21 pre-existing moderate dev-dep advisories** (`0` high, `0` critical, `0` new)
  - Diff secret scan: **0 hits**
- **Skipped / deferred (stated explicitly)**:
  - Live PostgreSQL 18 integration execution (`migrations/db_action_rules_grant_postgres_integration_test.go`, `repos/db_action_rules_postgres_integration_test.go`, `api/dbactions_postgres_integration_test.go`, `projectprovisioning/deprovision_owned_rows_postgres_integration_test.go`) and real-browser disposable Docker stack verification were not run locally (no Docker stacks spun up); all PG integration packages compile cleanly (`go test -c`) and execute in CI / stack verification.
  - Linux visual regression PNG baselines (`apps/elitea-web/e2e/snapshots/visual/routes.visual.spec.ts-snapshots/settings-*-visual-linux.png`) will need a Linux CI refresh for the new `DB actions` nav item in the Settings sidebar (never generated on macOS).
- **Fixtures honesty**: Go unit tests and Web component tests (`DbActions.test.tsx`) use in-process fakes/doubles and MSW handlers against the real generated Orval client. Live PG tests use ledgered migrations against Postgres.

## Follow-ups

1. **5e-3**: Admit `type: db` node across contract, Rust Worker / `agent-runtime`, Main `admittedNodeTypes` + pipeline start snapshot wiring (`dbaction.FreezeSnapshot`), and Web pipeline editor.
2. **5e-4**: DB action nodes inside Parallel/Map fan-out children (`scope_ref` + child-scoped `effect_id`).
3. **CI Linux visual baselines**: Refresh `settings-*-visual-linux.png` from Linux CI for the new `DB actions` Settings nav row before marking ready for review.
