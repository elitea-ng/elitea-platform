package db

import (
	"context"
	"embed"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

//go:embed migrations/*.sql
var migrations embed.FS

// gatewayMigrationPath is the LLM-gateway governance/budget/price-catalog
// schema (BF0.4), inside the LEDGERED corpus.
//
// It used to be a private directory under this package, applied only by
// RunMigrations — i.e. only when ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA=true, which
// production never sets. The Helm migration hook runs elitea-migrate, which
// applies only the ledgered histories, so a Helm-deployed database never got
// these tables while three services read them (issue #306). The DDL now lives
// in migrations/shared/0067 and this file points at it, so the dev bootstrap
// and the production migration apply the exact same bytes — a second copy is
// how the two silently diverge.
const gatewayMigrationPath = "shared/0067_gateway_budget_schema.sql"

// usageDimensionsMigrationPath is the SECOND half of the gateway schema
// (issues #320/#321/#322): the per-request usage ledger, the nullable
// soft_alert_pct that lets a platform default exist, and the seeded global
// budget-alert row.
//
// It is listed beside 0067 rather than folded into it because a shipped
// migration is content-checksummed and must never be edited. Both are needed
// together: the gateway's snapshot query joins the global alert row on every
// /llm call and the write-back consumer inserts into the ledger, so a database
// carrying only 0067 answers 42P01 on the billing path.
const usageDimensionsMigrationPath = "shared/0084_budget_usage_dimensions.sql"

// audioPricesMigrationPath is the THIRD file of the gateway price catalog: the
// four per-1,000,000-unit audio price columns that /llm/v1/audio/transcriptions
// and /llm/v1/audio/speech need.
//
// It is a separate file, not an edit of 0067, because a shipped migration is
// content-checksummed and the ledger rejects any drift in it.
//
// It belongs beside the other two for the reason GatewayMigrationSQL() exists:
// four Postgres integration suites build the gateway schema from that function
// alone. A database built without this file has gateway.gateway_models with no
// audio columns, and the price read answers 42703 at request time — a runtime
// error on the billing path, not a red build.
const audioPricesMigrationPath = "shared/0086_gateway_audio_prices.sql"

// requestLogMigrationPath is the FOURTH file of the gateway schema: the
// per-request log the gateway writes for every request it serves, billed or
// not, succeeded or not.
//
// It is in this list for the reason the three above are. The log is a real
// gateway table with real readers — the admin request-log listing
// (internal/api/gateway/llm_proxy_logs.go) and the project analytics reads
// (internal/infra/db/repos/analytics.go) — so a test database that has the
// budget tables and not this one fails at query time rather than at setup,
// which is the failure mode GatewayMigrationSQL exists to prevent.
const requestLogMigrationPath = "shared/0099_gateway_request_logs.sql"

// meteredOperationsMigrationPath is the FIFTH file of the gateway schema
// (#1238): the metered tool price catalog and the operation/units/unit
// columns of the usage ledger and the request log.
//
// It is here for the reason the four above are: the budget Usage read, the
// analytics reads and the admin request-log listing all select
// operation, so a test database built without it answers 42703 at query
// time. Every statement in it is guarded, and its execution_jobs half is
// skipped when elitea_runtime is absent (a gateway-only schema).
const meteredOperationsMigrationPath = "shared/0174_gateway_metered_operations.sql"

// tokenBindingMigrationPath is the access-token project binding (ADR-0018,
// spec-llm-project-scope §3), inside the LEDGERED corpus.
//
// It is here for the same reason 0067 is, and the failure it prevents is
// harder: internal/db/queries/auth_pat.sql's GetActivePATPrincipalByUUID now
// LEFT JOINs elitea_identity.token_project_binding, and that query runs for
// EVERY credential validation. A database built by this function alone would
// answer 42P01 to every token-authenticated request — not a missing feature, a
// dead API surface. RunMigrations applies migrations/*.sql plus this exempt
// list only; it never applies the ledgered shared corpus, so a file that the
// hot read path depends on must be named here.
//
// Every real deploy path (the Helm migrate-job, deploy/scripts/standalone-stack.sh)
// runs elitea-migrate and applies the whole ledgered
// history, so none of them need this. Only ELITEA_DEV_BOOTSTRAP_LEGACY_SCHEMA
// does.
const tokenBindingMigrationPath = "shared/0071_token_project_binding.sql"

// agentReleasesMigrationPath is shared 0198 (ADR-0040 decision 2): every
// agent version save appends a draft revision to
// elitea_lifecycle.version_revisions in the save's transaction, so a
// database without it refuses every version save with 42P01. The file
// depends on no other schema and every statement in it is guarded.
const agentReleasesMigrationPath = "shared/0198_agent_releases.sql"

// agentLifecycleMigrationPath is shared 0204 (ADR-0040 decisions 1, 3, 5
// and 6): every agent publish reads the agent's registry row and records a
// change request in the publish transaction, so a database without it
// refuses every publish with 42P01. It depends on 0198 (the release
// foreign keys), which this list applies first; every other dependency
// (tenant schemas, centry.project, auth_core) is guarded, and its backfill
// runs once (elitea_lifecycle.lifecycle_backfills).
const agentLifecycleMigrationPath = "shared/0204_agent_lifecycle_governance.sql"

// dumpGuardExemptMigrations are the ledgered shared files RunMigrations applies
// itself, in order. Every one of them MUST be idempotent and MUST tolerate a
// database that already holds its objects: the pool.Exec below leaves NO ledger
// row, so elitea-migrate re-applies and records them later.
//
// Add a file here when code on a read path depends on it. Do not add one for a
// feature that only the ledgered corpus needs — the list is a compatibility
// shim for one dev flag, not a second migration runner.
var dumpGuardExemptMigrations = []string{
	gatewayMigrationPath,
	usageDimensionsMigrationPath,
	// After 0067: it adds columns to the table 0067 creates.
	audioPricesMigrationPath,
	// The gateway's per-request log. It qualifies under the rule above: two
	// read paths depend on it — the admin request-log listing and the project
	// analytics reads — and on a dump-loaded database the baseline set is
	// skipped, so without this entry both answer 42P01 on an instance whose
	// budget tables are present. Every statement in the file is guarded.
	requestLogMigrationPath,
	// After 0084 and 0099: it adds columns to the usage ledger and the
	// request log, which the Usage, analytics and request-log reads select.
	meteredOperationsMigrationPath,
	tokenBindingMigrationPath,
	agentReleasesMigrationPath,
	// After 0198: its foreign keys name 0198's releases.
	agentLifecycleMigrationPath,
}

func RunMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	// Skip the baseline migrations if the database already has tenant schemas
	// (loaded from a dump) — but ALWAYS run the idempotent gateway migrations
	// afterwards so the budget/price tables exist regardless.
	var schemaCount int
	// Tenant schemas are named p_<numeric project id>. Do not use LIKE 'p_%':
	// its underscore is a wildcard and matches PostgreSQL's pg_catalog and the
	// public schema, which would skip baseline migrations on a fresh database.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.schemata WHERE schema_name ~ '^p_[0-9]+$'`).Scan(&schemaCount); err != nil {
		return fmt.Errorf("migrate: count tenant schemas: %w", err)
	}
	if schemaCount > 0 {
		slog.Info("skipping baseline migrations — existing tenant schemas detected", "count", schemaCount)
	} else {
		if err := applyMigrationDir(ctx, pool, migrations, "migrations"); err != nil {
			return err
		}
	}

	// These files are idempotent and dump-guard-exempt (BF0.4): they must land
	// even on dump-loaded instances, where the p_% guard above skipped the
	// baseline set. Applying them here leaves NO ledger row — elitea-migrate
	// will re-apply and record them later, which is safe precisely because every
	// statement in them is guarded.
	for _, path := range dumpGuardExemptMigrations {
		sql, err := ledgeredMigrationSQL(path)
		if err != nil {
			return err
		}
		slog.Info("applying migration", "dir", "shared", "file", path)
		if _, err := pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("migrate: exec %s: %w", path, err)
		}
	}
	return nil
}

// ledgeredMigrationSQL reads one file out of the immutable ledgered corpus.
func ledgeredMigrationSQL(path string) (string, error) {
	data, err := platformmigrations.Files.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("migrate: read %s: %w", path, err)
	}
	return string(data), nil
}

// GatewayMigrationSQL returns the gateway schema migration as SQL.
//
// It exists for integration tests that need the gateway budget tables:
// reading the REAL migration means a schema change cannot pass a test against
// a hand-copied DDL and then fail against production. Runtime code must call
// RunMigrations instead — this returns SQL, it does not run it.
//
// It returns ALL FOUR files that make up that schema, in ledger order. Every
// caller wants "the gateway tables", not "one particular migration", so a
// caller that had to know the other files existed would be a caller that
// eventually forgets: the usage-ledger table added by 0084 is written on the
// billing path, the audio price columns added by 0086 are read on it, and the
// request log added by 0099 is what the admin log listing and the project
// analytics both read — a test database missing any of them fails at runtime
// rather than at setup.
func GatewayMigrationSQL() (string, error) {
	base, err := ledgeredMigrationSQL(gatewayMigrationPath)
	if err != nil {
		return "", err
	}
	dimensions, err := ledgeredMigrationSQL(usageDimensionsMigrationPath)
	if err != nil {
		return "", err
	}
	audio, err := ledgeredMigrationSQL(audioPricesMigrationPath)
	if err != nil {
		return "", err
	}
	requestLog, err := ledgeredMigrationSQL(requestLogMigrationPath)
	if err != nil {
		return "", err
	}
	metered, err := ledgeredMigrationSQL(meteredOperationsMigrationPath)
	if err != nil {
		return "", err
	}
	// A newline between them: the ledgered runner executes each file separately,
	// and concatenating without a separator would splice the last statement of
	// one onto the first of the next. 0086 comes last because it ALTERs the
	// table 0067 creates.
	return base + "\n" + dimensions + "\n" + audio + "\n" + requestLog + "\n" + metered, nil
}

// TokenBindingMigrationSQL returns the token project binding migration as SQL,
// for the same reason GatewayMigrationSQL exists: a test that needs
// elitea_identity.token_project_binding must read the file production applies,
// not a hand-copied CREATE TABLE that can drift from it.
func TokenBindingMigrationSQL() (string, error) {
	return ledgeredMigrationSQL(tokenBindingMigrationPath)
}

// applyMigrationDir executes every *.sql file in dir (lexical order) from fsys.
func applyMigrationDir(ctx context.Context, pool *pgxpool.Pool, fsys embed.FS, dir string) error {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("migrate: read dir %s: %w", dir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := fsys.ReadFile(dir + "/" + entry.Name())
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", entry.Name(), err)
		}
		slog.Info("applying migration", "dir", dir, "file", entry.Name())
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			return fmt.Errorf("migrate: exec %s: %w", entry.Name(), err)
		}
	}
	return nil
}
