package migrate

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bootstrapScope keys the advisory lock this file takes. Deliberately distinct
// from ScopeShared and ScopeTenant: it is not a history, it has no ledger and
// no checksum, and giving it their key would let a bootstrap and a shared
// migration in two different processes believe they held the same lock.
const bootstrapScope Scope = "bootstrap"

// Bootstrap applies the legacy-era schema to a database that does not carry it,
// and reports whether it did.
//
// WHY THE BINARY DOES THIS. The numbered histories cannot run against an empty
// database — shared/0030 has a foreign key to centry.project, and that table is
// created by the legacy-era schema rather than by any migration. Until this
// existed, only the compose stack and the E2E seeder could supply it, both by
// running psql against a file on disk. A Kubernetes install therefore could not
// reach a migrated database at all without a human, and the Helm chart's
// workaround was to carry its own copy of the SQL — a second source of truth
// held in step by a CI gate. Applying it here removes both.
//
// It also fixes an ownership problem that the external approaches could not.
// Whoever applies this SQL OWNS the objects it creates, and the histories then
// ALTER those objects, which PostgreSQL only allows the owner to do. Applied by
// an administrator — the only role that could, from outside — the chain died at
// shared/0041 with "must be owner of table project", and the tenant half at
// "tenant schema p_1 is not the effective schema". Applied here it runs as the
// migrating role itself, so ownership is right by construction and no hand-over
// step exists to get wrong.
//
// A DATABASE THE LEGACY RUNTIME BUILT already has centry.project, so the
// legacy-era schema is not applied there; instead the same transaction adds
// the fixed columns of legacyAdoptionColumns that its tables lack, so the
// numbered histories can run on it (adoptLegacyBuiltTables).
//
// IDEMPOTENT, and cheap when there is nothing to do: the presence of
// centry.project is a question, not an error path — to_regclass returns NULL
// for a missing relation rather than raising — so an already-bootstrapped
// database costs one query and returns false.
//
// The whole thing runs in ONE transaction under an advisory lock, so two
// migration Jobs racing on a fresh install cannot both apply it: the loser
// blocks, then finds centry.project present and does nothing.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, schema string) (bool, error) {
	if pool == nil {
		return false, fmt.Errorf("migrate: bootstrap requires a database pool")
	}
	if schema == "" {
		return false, fmt.Errorf("migrate: bootstrap schema is empty")
	}

	applied := false
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := acquireLock(ctx, tx, advisoryLockKey(bootstrapScope, "platform")); err != nil {
			return err
		}

		// Re-checked INSIDE the lock, not before it. Checking first and locking
		// second is the race this ordering exists to close: both processes see
		// an empty database, both proceed, and the second fails partway through
		// on objects the first created.
		var present bool
		if err := tx.QueryRow(ctx,
			`SELECT to_regclass('centry.project') IS NOT NULL`,
		).Scan(&present); err != nil {
			return fmt.Errorf("migrate: probe for the legacy-era schema: %w", err)
		}
		if present {
			return adoptLegacyBuiltTables(ctx, tx)
		}

		if _, err := tx.Exec(ctx, schema); err != nil {
			return fmt.Errorf("migrate: apply the legacy-era schema: %w", err)
		}

		// Proven rather than assumed. The SQL is applied as one multi-statement
		// Exec, so a file that stopped creating this table — an edit, a
		// truncated embed — would otherwise commit quietly and leave the very
		// next migration to fail with a foreign-key error naming a table nobody
		// realised was missing.
		if err := tx.QueryRow(ctx,
			`SELECT to_regclass('centry.project') IS NOT NULL`,
		).Scan(&present); err != nil {
			return fmt.Errorf("migrate: verify the legacy-era schema: %w", err)
		}
		if !present {
			return fmt.Errorf("migrate: the legacy-era schema applied but centry.project is absent")
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// legacyAdoptionColumn is one column the legacy-era schema declares on a table
// that a database built by the legacy runtime may carry without it.
type legacyAdoptionColumn struct {
	schema, table, column, sqlType string
}

// legacyAdoptionColumns is the fixed list Bootstrap adds to a database the
// legacy runtime built. It is deliberately a named list, never a diff against
// the legacy-era schema: each entry is a measured gap that a ledgered
// migration depends on, and each type is the one the legacy-era schema
// declares.
//
// The legacy-era schema creates centry.audit_events with CREATE TABLE IF NOT
// EXISTS (migrations/001_initial.sql:149), so on a legacy-built database the
// table keeps its older shape, without the token and cost columns
// (001_initial.sql:167-171). Shared 0206 builds the audit chain's canonical
// row function over those columns and fails with 42P01 there, and a new
// migration cannot repair it first: it would be numbered above 0206 and run
// after it.
var legacyAdoptionColumns = []legacyAdoptionColumn{
	{"centry", "audit_events", "input_tokens", "INTEGER"},
	{"centry", "audit_events", "output_tokens", "INTEGER"},
	{"centry", "audit_events", "llm_cost", "NUMERIC(18, 8)"},
	{"centry", "audit_events", "token_source", "VARCHAR(16)"},
	{"centry", "audit_events", "cost_source", "VARCHAR(64)"},
}

// adoptLegacyBuiltTables adds each column of legacyAdoptionColumns that is
// missing from a table that exists, inside Bootstrap's transaction and lock.
//
// It reads the catalog first and alters only when a column is absent, so a
// database that already has the columns (every native one, and every one past
// shared 0206) takes no ACCESS EXCLUSIVE lock on the audit table. A table that
// does not exist is left alone; the numbered histories decide what happens to
// it.
func adoptLegacyBuiltTables(ctx context.Context, tx pgx.Tx) error {
	for _, c := range legacyAdoptionColumns {
		table := pgx.Identifier{c.schema, c.table}.Sanitize()
		var tableExists, columnExists bool
		if err := tx.QueryRow(ctx, `
			SELECT to_regclass($1) IS NOT NULL,
			       EXISTS (
			           SELECT 1 FROM pg_attribute
			            WHERE attrelid = to_regclass($1)
			              AND attname = $2
			              AND attnum > 0
			              AND NOT attisdropped)`,
			table, c.column,
		).Scan(&tableExists, &columnExists); err != nil {
			return fmt.Errorf("migrate: probe %s.%s.%s on a legacy-built database: %w", c.schema, c.table, c.column, err)
		}
		if !tableExists || columnExists {
			continue
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
			table, pgx.Identifier{c.column}.Sanitize(), c.sqlType,
		)); err != nil {
			return fmt.Errorf("migrate: add %s.%s.%s on a legacy-built database: %w", c.schema, c.table, c.column, err)
		}
		slog.InfoContext(ctx, "migrate: added a column the legacy-era schema declares to a legacy-built table",
			"table", c.schema+"."+c.table, "column", c.column, "type", c.sqlType)
	}
	return nil
}
