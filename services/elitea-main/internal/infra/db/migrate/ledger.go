package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const ensureLedgerSQL = `
CREATE SCHEMA IF NOT EXISTS elitea_runtime;
CREATE TABLE IF NOT EXISTS elitea_runtime.schema_migrations (
    target_kind TEXT NOT NULL,
    target_id TEXT NOT NULL,
    version BIGINT NOT NULL,
    name TEXT NOT NULL,
    checksum BYTEA NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (target_kind, target_id, version),
    CONSTRAINT schema_migrations_target_kind
        CHECK (target_kind IN ('shared', 'tenant', 'agentstate')),
    CONSTRAINT schema_migrations_checksum_length
        CHECK (octet_length(checksum) = 32)
)`

func ensureLedger(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, ensureLedgerSQL); err != nil {
		return fmt.Errorf("migrate: ensure ledger: %w", err)
	}
	return nil
}

type ledgerQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type recordedMigration struct {
	version  int64
	name     string
	checksum []byte
}

func readValidatedLedger(
	ctx context.Context,
	queryer ledgerQueryer,
	scope Scope,
	targetID string,
	manifest []Migration,
	requireComplete bool,
) (map[int64]struct{}, error) {
	rows, err := queryer.Query(ctx, `
SELECT version, name, checksum
FROM elitea_runtime.schema_migrations
WHERE target_kind = $1 AND target_id = $2
ORDER BY version`, string(scope), targetID)
	if err != nil {
		return nil, fmt.Errorf("migrate: read ledger: %w", err)
	}
	defer rows.Close()

	recorded := make([]recordedMigration, 0, len(manifest))
	for rows.Next() {
		var entry recordedMigration
		if err := rows.Scan(&entry.version, &entry.name, &entry.checksum); err != nil {
			return nil, fmt.Errorf("migrate: scan ledger: %w", err)
		}
		recorded = append(recorded, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: iterate ledger: %w", err)
	}
	return validateRecordedLedger(manifest, recorded, requireComplete)
}

func validateRecordedLedger(
	manifest []Migration,
	recorded []recordedMigration,
	requireComplete bool,
) (map[int64]struct{}, error) {
	expected := make(map[int64]Migration, len(manifest))
	for _, migration := range manifest {
		expected[migration.Version] = migration
	}
	applied := make(map[int64]struct{}, len(recorded))
	for _, entry := range recorded {
		migration, ok := expected[entry.version]
		if !ok {
			return nil, fmt.Errorf("migrate: database version %04d is not present in the binary manifest", entry.version)
		}
		if _, duplicate := applied[entry.version]; duplicate {
			return nil, fmt.Errorf("migrate: duplicate database version %04d", entry.version)
		}
		if releasedText(migration, entry.checksum) {
			applied[entry.version] = struct{}{}
			continue
		}
		if entry.name != migration.Name {
			return nil, fmt.Errorf("migrate: name mismatch for %s", migration.Path)
		}
		if err := verifyChecksum(migration, entry.checksum); err != nil {
			return nil, err
		}
		applied[entry.version] = struct{}{}
	}
	if requireComplete {
		for _, migration := range manifest {
			if _, ok := applied[migration.Version]; !ok {
				return nil, fmt.Errorf("migrate: expected %s is not applied", migration.Path)
			}
		}
	}
	return applied, nil
}

func recordMigration(
	ctx context.Context,
	tx pgx.Tx,
	targetID string,
	migration Migration,
) error {
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.schema_migrations
    (target_kind, target_id, version, name, checksum)
VALUES ($1, $2, $3, $4, $5)`,
		string(migration.Scope), targetID, migration.Version, migration.Name, migration.Checksum[:],
	); err != nil {
		return fmt.Errorf("migrate: record %s: %w", migration.Path, err)
	}
	return nil
}
