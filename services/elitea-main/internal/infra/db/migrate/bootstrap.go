package migrate

import (
	"context"
	"fmt"

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
			return nil
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
