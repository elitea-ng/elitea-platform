# Upgrading a database the legacy runtime built (FU-279, FU-280), 2026-11-09

A database the legacy runtime built could not be upgraded to main, and some project pages failed on it once it was.
This change lets that database migrate to head with no hand SQL. Projects whose tenant schema the legacy runtime
built then serve the toolkit and agent pages that failed before.

Branch `fix/legacy-db-upgrade-repairs`, from main `720e41900`. Components: Main only (migrations and the migrate
binary). No Worker, Web or contract change.

## Business behaviour

**Reference.** The current platform built these databases. Its schema is measured from the real-model dump
`product-real-models-main-d5e966074`, whose ledger was at shared 172 and tenant 154.

**Before.** `elitea-migrate --all-tenants` on that dump stopped at shared 0206:

```
apply shared/0206_audit_events_chain.sql: ERROR: missing FROM-clause entry for table "e" (SQLSTATE 42P01)
```

The whole batch rolled back, so Main never started on a restore of that dump. Every disposable stack since 2026-11-03
had to add five columns by hand.

**Cause, shared.**
- The legacy-era schema creates `centry.audit_events` with `CREATE TABLE IF NOT EXISTS`
  (`services/elitea-main/internal/infra/db/migrations/001_initial.sql:149`). A legacy-built table therefore keeps its
  older shape, without `input_tokens`, `output_tokens`, `llm_cost`, `token_source` and `cost_source` (`:167-171`).
- 0206 builds the audit chain's canonical row function over those columns.

**Cause, tenant.** The same `IF NOT EXISTS` in `create_tenant_schema` left legacy-built p_1 and p_2 without parts of
the schema Main reads:
- `elitea_tools.owner_id`, `env_vars` and `uuid`;
- the tables `index_meta` and `social_likes`.

The tenant history completed there, but these paths then failed:
- toolkit create, fork and import;
- attaching an agent or pipeline as a tool;
- toolkit export, which answered 404 "toolkit not found";
- the index pages;
- the agent list's liked and trending filters and like counts.

**How the drift was measured, not guessed.** I compared `information_schema.columns` of the upgraded dump against:
- a fresh bootstrap migrated to head, for the shared schemas;
- a fresh p_1 built by `create_tenant_schema` plus the tenant history, which equals the native p_118 and p_119 in the
  dump, for the tenant schemas.

| Area | Found | Done here |
|---|---|---|
| Shared, missing columns | the five `centry.audit_events` columns | added by `Bootstrap` |
| Shared, type and nullability | json vs jsonb on `centry.project.keycloak_groups`/`secrets_json` and `centry.schedule.rpc_kwargs`; nullability on `centry.notifications` and `project_group_association` | none: Main uses no jsonb operator on these, and nullability is the legacy owner's shape |
| Shared, `auth_core__*` | in `public` on the dump; a fresh bootstrap also creates copies in `centry` | none: Main reads `public.auth_core__*` (`internal/infra/identityrepo`) |
| Tenant, `elitea_tools` | no `owner_id`, `env_vars` or `uuid` | added by tenant 0164 |
| Tenant tables | no `index_meta`, `social_likes`, `social_pins` or `social_feedbacks` | `index_meta` and `social_likes` created; `social_pins` and `social_feedbacks` are not, because Main keeps those rows in `centry` (`internal/infra/db/repos/social_pins.go`, `social_feedbacks.go`) |

**After.** On the branch, the same dump migrates to head (shared 232, tenant 164 on all four projects) with no hand
SQL. The diff then shows no missing column, and no missing table other than the two left out on purpose.

**Not ported:**
- **Legacy index metadata.** It lived as a special document (`type: index_meta`) inside each toolkit's vector
  collection (`elitea_sdk/tools/base_indexer_toolkit.py:2980-2997` on the current platform), not in a table. The dump
  carries no vector objects since #1378. So the new `index_meta` starts empty on a legacy project, and its index pages
  show no history until the toolkit is re-indexed. This is a stated gap, not fixed here.
- **Parity for unused tenant tables** (`social_pins`, `social_feedbacks`): not created.

## Changed paths

- `services/elitea-main/internal/infra/db/migrate/bootstrap.go`:
  - `Bootstrap` adopts a legacy-built database (`:76-77`);
  - `legacyAdoptionColumns` is the fixed list (`:125`);
  - `adoptLegacyBuiltTables` (`:141`): catalog probe (`:157`), `ALTER` (`:161`), INFO log per column (`:166`).
- `services/elitea-main/migrations/tenant/0164_legacy_built_tenant_shape.sql`:
  - `owner_id`: add, backfill and NOT NULL (`:52-55`);
  - `env_vars` (`:64`) and `uuid` (`:73`);
  - `index_meta` (`:76`) and `social_likes` (`:87`).
- `services/elitea-main/internal/infra/db/dbtest/legacyshape/legacyshape.go`: the legacy-built fixture, measured
  from the dump's DDL. Test support only.
- Tests (below) and `internal/infra/db/migrate/manifest_test.go`, with the tenant head at 164.

**Why not an ordinary migration, and why 0206 is not edited.**
- A new shared file is numbered above head, and the runner applies pending files in version order in one transaction
  (`internal/infra/db/migrate/runner.go`). On a database at 172, 0206 would run first, fail, and roll the repair back
  with it.
- A free number below 0206 is refused by `scripts/database/check-migration-version.sh` (rationale at
  `internal/infra/db/migrate/manifest_test.go:139-148`).
- `released.go` accepts only comment-only rewrites of a released file.
- `Bootstrap` already completes the legacy-era schema outside the ledger, under its own advisory lock, so the adoption
  step lives there.
- The tenant drift fails no earlier migration, so it is an ordinary ledgered tenant migration. It follows 0124
  (guarded on the table) and 0131:165 (project id from `current_schema()`).

**Every entry path to the shared history (C3).** The production path is `cmd/elitea-migrate`, which runs `Bootstrap`
(`services/elitea-main/cmd/elitea-migrate/main.go:61`), then `ApplyShared` (`:70`), then `ApplyTenant` (`:96`,
`:103`). Every deployment reaches it:

| Entry path | Where |
|---|---|
| Helm | migrate Job `deploy/helm/elitea/templates/main/migrate-job.yaml:113-126`, `command: ["/elitea-migrate"]` |
| Compose | `deploy/docker-compose.standalone-full.yml:358-373`, `elitea-migrate` with `-all-tenants` |
| Standalone stack | `deploy/scripts/standalone-stack.sh:1042`, `run --rm elitea-migrate -all-tenants` |
| Local `stack.sh` | runs the checkout's own standalone compose, so the same service |

Other callers of `ApplyShared` exist only in test support (`internal/infra/db/dbtest/template.go:301`,
`internal/infra/authstatetest/pool.go:87`). Main never runs the shared history at boot:
- `composition.go:317` only checks the head;
- `projectprovisioning` (`router.go:1057`) applies the tenant history to a project Main creates itself, and that
  project gets the full `create_tenant_schema` shape;
- the developer-only `ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA` path (`cmd/elitea-main/main.go:191-197`,
  `internal/infra/db/migrate.go:147`) does not apply 0206.

## Tests

All tests run against real PostgreSQL 18 (`ELITEA_TEST_DATABASE_URL`). Each starts from `legacyshape.BuiltSQL` and is
upgraded by the production sequence: `Bootstrap`, `ApplyShared`, `ApplyTenant(1)`.

`internal/infra/db/migrate/legacy_built_upgrade_postgres_integration_test.go`:

1. **`TestALegacyBuiltDatabaseUpgradesToHeadAndItsAuditChainVerifies`**
   - Without adoption, `ApplyShared` fails at 0206 with 42P01, as main does. After `Bootstrap` it migrates to head.
   - The five columns carry 001's types.
   - Main's own `audit.VerifyChain`, run as the admin route runs it (REPEATABLE READ, read only), passes: 1 checked
     (a row written after the upgrade with token and cost set), 2 unchained (the legacy rows), no break.
   - The tenant side:
     - `owner_id` is NOT NULL and equals 1 on both legacy toolkits;
     - the uuids are distinct;
     - `env_vars` is `{}`;
     - the legacy `settings` are kept;
     - `index_meta` and `social_likes` exist and `social_pins` does not;
     - an index row can be written.
2. **`TestBootstrapTakesNoLockOnAnAlreadyMigratedAuditTable`**
   - A second session holds ROW EXCLUSIVE on `centry.audit_events`, as every audit insert does.
   - `Bootstrap` on a migrated database still finishes within a 10 s deadline and leaves the ledger unchanged. An
     ALTER would wait for ACCESS EXCLUSIVE.
3. **`TestConcurrentBootstrapsAdoptALegacyBuiltDatabaseOnce`**
   - Two concurrent `Bootstrap` calls on a legacy-built database both succeed.
   - The INFO log shows each of the five columns adopted exactly once.
   - The shared history then completes.

Proof that tests 1 and 3 fail without the fix: with the adoption call disabled, test 1 fails with the 0206 42P01
error and test 3 adopts nothing.

Behaviour through Main's real handlers and repository on an upgraded legacy-built p_1. Each test fails with tenant
0164 removed; the failure is quoted.

| Test | Asserts | Without 0164 |
|---|---|---|
| `internal/api/v2/toolkits/legacy_built_tenant_postgres_integration_test.go` `TestAnUpgradedLegacyBuiltProjectExportsItsToolkits` | `ExportToolkit` returns 200 with the legacy name, its type, and `env_vars` `{}` | 404 `{"error":"toolkit not found"}` |
| same file, `…ListsNoIndexesForALegacyToolkit` | `IndexMeta` returns 200 with an empty list (C5) | 500, `relation "p_1.index_meta" does not exist` |
| same file, `…CreatesAndForksToolkitsOwnedByTheProject` | `Create` succeeds with `owner_id = 1`; `ForkToolkit` returns 200, and the copy has `owner_id = 1` and `env_vars` `{}` | 500, `column "owner_id" … does not exist` (42703) |
| `internal/infra/db/repos/legacy_built_tenant_likes_postgres_integration_test.go` `TestAnUpgradedLegacyBuiltProjectListsApplicationsLikedByTheActor` | `ApplicationsRepo.List` with `MyLiked` returns only the liked agent, and sorting by likes puts it first | `relation "p_1.social_likes" does not exist` (42P01) |

The likes test adds a minimal `public.auth_core__user` stand-in. The list joins that table for author names, and on a
real legacy database it lives in `public`, owned by the auth service.

## Performance

- **No-op cost.** On a database that has the columns (every native database, and any past 0206), adoption costs five
  catalog probes (`to_regclass` plus `pg_attribute`) inside `Bootstrap`'s existing transaction. It takes no lock on
  `centry.audit_events`, as test 2 proves.
- **Adoption itself.** Five `ADD COLUMN` statements on nullable columns with no default. These are catalog-only and
  do not rewrite the table: 237,050 rows on the dump.
- **Tenant 0164.** It runs once per project, through the ledger.
  - The `uuid ADD COLUMN … DEFAULT gen_random_uuid()` rewrites `elitea_tools`. On the dump that is p_1 with 0 rows
    and p_2 with 57 rows. The native p_118 and p_119 already have the column, so nothing is rewritten there (N1).
  - The full migrate of the dump, shared 173→232 plus four tenants, took about 0.55 s.

## Durability

- **Forward-only.**
  - The adoption step runs in `Bootstrap`'s single transaction and fails whole.
  - 0164 runs in the runner's batch transaction with its ledger row.
  - No shipped migration is edited, so no checksum moves.
- **A crash mid-run** leaves the database as it was. The next run repeats the same idempotent steps.

## Resilience

- **Concurrent runners** serialise on `Bootstrap`'s advisory lock (`advisoryLockKey(bootstrapScope, "platform")`).
  The loser re-probes inside the lock and adds nothing (test 3).
- **Fails closed.** Each probe and ALTER error is returned with the table and column named, and the migrate Job
  fails.
- **Visible.** Every added column is logged at INFO with its table, column and type. No payload is logged.
- **Bounded.** The list is fixed (five entries) and is never computed from a diff, so the step cannot grow by
  accident.

## Security

- **Identity, authorization, egress and credentials:** unchanged. No route, RPC or credential path changes.
- **SQL construction.** The identifiers come from a compile-time list and are quoted with `pgx.Identifier.Sanitize`.
  The probe is parameterised. 0164 builds no SQL from input.
- **The `owner_id` backfill widens no visibility (C4).**
  - The backfill is this schema's project, taken from the schema name (`0164:47`, as `0131:165`), never from input.
  - The column means PROJECT, not user (`migrations/tenant/0128_owner_id_column_meanings.sql:54-60, 164-165`).
  - A legacy toolkit had no owner. It was visible to everyone in its project because toolkit reads are scoped by the
    project's schema, and no read filters on `elitea_tools.owner_id`. The column is written only on insert
    (`toolkits/handler.go:1765,1963`, `eliteacore/application_relation.go:68`, `eliteacore/import_owner.go:85`).
  - Naming the same project changes no reader's result. That is the credential ownership model: strictly per project,
    shared in a team project, private in the personal one.
- **Data exposure.** No secret, prompt or payload is logged. The adoption log names only the table, column and type.
- **The `security-review` pass** finds no new trust-boundary, injection, egress or secret issue (see the PR). The
  `rules/security.md` categories that apply are injection and construction (checked above) and secrets (diff scan).

## Recovery guarantee

| Component × phase | Class | Enforced by | Proven by |
|---|---|---|---|
| Main migrate Job × admission (schema upgrade) | **I**, idempotent retry | `bootstrap.go:141-166` (probe, then add once); `0164` guarded per column and table; ledger plus advisory lock | tests 2 and 3; rerun on the dump adds nothing (0 adoption logs) |

No other component or phase is touched.

## Real-dump evidence (local, not committed)

| Step | Result |
|---|---|
| Restore the dump into a throwaway `postgres:18` | ok |
| Main's migrate binary from `720e41900` | fails at 0206 (42P01) |
| The branch's migrate binary | five adoption INFO lines, then `migrations applied … tenant_count 4`; ledger shared 232, tenant 164 ×4 |
| Rerun | zero adoption lines |
| Diff against fresh | no missing column; only the two intended tables |
| p_2 toolkits | 57 rows, 57 distinct uuids, `owner_id = 2` on all, `env_vars = {}` |
| `audit.VerifyChain` after one new row | checked 1, unchained 237,050, no break (C2) |

Browser evidence: see below.

## Follow-ups

- Index history on legacy projects starts empty until a re-index (C5). This is a stated gap.
- The fresh real-model baseline is cut after this merges: from merged main, restore `d5e966074`, migrate with no hand
  SQL, then `product-real-models-main-<merge sha>.dump`, which `stack.sh` picks as the newest.
