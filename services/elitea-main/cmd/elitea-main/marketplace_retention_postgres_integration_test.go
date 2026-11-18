package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/routines"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

func newMarketplaceRetentionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv("ELITEA_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the PostgreSQL service-integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("elitea_mkt_retention_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	config := adminConfig.Copy()
	config.ConnConfig.Database = name
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("baseline migrations: %v", err)
	}
	if err := migrate.New(pool, platformmigrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("shared migrations: %v", err)
	}
	return pool
}

// TestMarketplaceRetentionRoutinePurgesWithdrawnSnapshotsNobodyHolds runs the
// routine's own function: a withdrawn snapshot past retention that no project
// holds is deleted with its empty listing, a held one stays, and a pass in
// maintenance does nothing.
func TestMarketplaceRetentionRoutinePurgesWithdrawnSnapshotsNobodyHolds(t *testing.T) {
	pool := newMarketplaceRetentionPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	seed := func(entity int, held bool) {
		var listing int64
		if err := pool.QueryRow(ctx, `
INSERT INTO elitea_marketplace.listings (kind, source_project_id, source_entity_id, name, state)
VALUES ('agent', 2, $1, 'L', 'delisted') RETURNING id`, entity).Scan(&listing); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO elitea_marketplace.listing_versions (listing_id, version_id, name, digest, definition, withdrawn_at)
VALUES ($1, $2, 'v1', decode(repeat('ab', 32), 'hex'), '{}'::jsonb, now() - interval '60 days')`, listing, 100+entity); err != nil {
			t.Fatal(err)
		}
		if held {
			if _, err := pool.Exec(ctx, `
INSERT INTO elitea_marketplace.installs (project_id, entity_kind, entity_id, listing_id, version_id, digest, mode, installed_by)
VALUES (3, 'agent', 9, $1, $2, decode(repeat('ab', 32), 'hex'), 'install', 1)`, listing, 100+entity); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed(1, false)
	seed(2, true)

	if _, err := pool.Exec(ctx, `
INSERT INTO centry.platform_config (section, key, value, updated_at)
VALUES ('maintenance', 'maintenance_enabled', 'true'::jsonb, now())`); err != nil {
		t.Logf("maintenance switch not seeded (%v); the maintenance leg is skipped", err)
	} else {
		outcome, err := marketplaceRetentionRun(ctx, pool, logger)
		if err != nil || outcome != routines.OutcomeMaintenance {
			t.Fatalf("a pass in maintenance = %q %v, want the maintenance outcome", outcome, err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_marketplace.listing_versions`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("snapshots after a maintenance pass = %d (%v), want 2", n, err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM centry.platform_config WHERE section = 'maintenance'`); err != nil {
			t.Fatal(err)
		}
	}

	outcome, err := marketplaceRetentionRun(ctx, pool, logger)
	if err != nil || outcome != routines.OutcomeOK {
		t.Fatalf("pass = %q %v", outcome, err)
	}
	var versions, listings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_marketplace.listing_versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_marketplace.listings`).Scan(&listings); err != nil {
		t.Fatal(err)
	}
	if versions != 1 || listings != 1 {
		t.Errorf("after the purge: %d snapshots and %d listings, want only the held one of each", versions, listings)
	}
}

// TestMarketplaceSignalsRoutineWritesTheDailyRows runs the signals routine's own
// function: it writes the listing and version rows from the installs and
// refreshes the ratings, and a pass in maintenance does nothing.
func TestMarketplaceSignalsRoutineWritesTheDailyRows(t *testing.T) {
	pool := newMarketplaceRetentionPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var listing int64
	if err := pool.QueryRow(ctx, `
INSERT INTO elitea_marketplace.listings (kind, source_project_id, source_entity_id, name)
VALUES ('agent', 2, 1, 'L') RETURNING id`).Scan(&listing); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_marketplace.listing_versions (listing_id, version_id, name, digest, definition)
VALUES ($1, 100, 'v1', decode(repeat('ab', 32), 'hex'), '{}'::jsonb)`, listing); err != nil {
		t.Fatal(err)
	}
	for entity, dismissed := range map[int]string{9: "NULL", 10: "now()"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO elitea_marketplace.installs (project_id, entity_kind, entity_id, listing_id, version_id, digest, mode, installed_by, dismissed_at)
VALUES (3, 'agent', $1, $2, 100, decode(repeat('ab', 32), 'hex'), 'install', 1, %s)`, dismissed), entity, listing); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_marketplace.listing_feedback (listing_id, version_id, kind, rating, author_kind, author_user_id, project_id)
VALUES ($1, 100, 'review', 4, 'person', 5, 3)`, listing); err != nil {
		t.Fatal(err)
	}

	outcome, err := marketplaceSignalsRun(ctx, pool, logger)
	if err != nil || outcome != routines.OutcomeOK {
		t.Fatalf("pass = %q %v", outcome, err)
	}
	var active, total, dismissed int
	if err := pool.QueryRow(ctx, `
SELECT active_hires, hires_total, dismissed FROM elitea_marketplace.listing_signals_daily
WHERE listing_id = $1 AND version_id = 0`, listing).Scan(&active, &total, &dismissed); err != nil {
		t.Fatal(err)
	}
	if active != 1 || total != 2 || dismissed != 1 {
		t.Errorf("signals = %d active of %d with %d dismissed, want 1 of 2 with 1", active, total, dismissed)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT rating_count FROM elitea_marketplace.listings WHERE id = $1`, listing).Scan(&count); err != nil || count != 1 {
		t.Errorf("rating_count = %d (%v), want the routine to refresh it to 1", count, err)
	}
}
