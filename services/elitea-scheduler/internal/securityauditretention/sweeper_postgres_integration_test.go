package securityauditretention

// The sweep against a real PostgreSQL holding the REAL 0183 tables: the
// migration file is applied as is, so a column rename there fails here.
// Runs when ELITEA_TEST_DATABASE_URL is set; skips otherwise.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-scheduler/internal/testpg"
)

const outboxMigrationPath = "../../../elitea-main/migrations/shared/0183_security_audit_outbox.sql"

var databaseSequence atomic.Uint64

func newOutboxPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv("ELITEA_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the security audit outbox retention integration tests")
	}
	migration, err := os.ReadFile(filepath.Clean(outboxMigrationPath))
	if err != nil {
		t.Fatalf("read %s: %v", outboxMigrationPath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer adminPool.Close()
	databaseName := fmt.Sprintf("elitea_secaudit_ret_%d_%d_%d", os.Getpid(), time.Now().UnixNano(), databaseSequence.Add(1))
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse ELITEA_TEST_DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.Database = databaseName
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		dropPool, dropErr := pgxpool.New(dropCtx, databaseURL)
		if dropErr != nil {
			return
		}
		defer dropPool.Close()
		_, _ = dropPool.Exec(dropCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply 0183: %v", err)
	}
	return pool
}

// insertRow writes outbox row seq at chain position seq, optionally with a
// durable copy (whose hash may differ).
func insertRow(t *testing.T, pool *pgxpool.Pool, seq int64, storedAt, exportedAt *time.Time, durable bool, durableHashByte byte) {
	t.Helper()
	ctx := context.Background()
	hash := make([]byte, 32)
	hash[0] = byte(seq)
	event := `{"specversion":"1.0"}`
	id := fmt.Sprintf("0192f0c1-7b6e-7c3a-8f00-%012d", seq)
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.security_audit_outbox
		(seq, partition_key, partition_seq, event_id, event_type, event, prev_hash, hash, stored_at, exported_at)
		VALUES ($1, '1', $1, $2::text::uuid, 'ai.elitea.security.v1.probe', $3::text::jsonb, $4, $5, $6, $7)`,
		seq, id, event, make([]byte, 32), hash, storedAt, exportedAt); err != nil {
		t.Fatalf("insert outbox row %d: %v", seq, err)
	}
	if !durable {
		return
	}
	durableHash := append([]byte{}, hash...)
	durableHash[31] = durableHashByte
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_identity.security_audit_events
		(seq, partition_key, partition_seq, event_id, event_type, event, prev_hash, hash, created_at)
		VALUES ($1, '1', $1, $2::text::uuid, 'ai.elitea.security.v1.probe', $3::text::jsonb, $4, $5, now())`,
		seq, id, event, make([]byte, 32), durableHash); err != nil {
		t.Fatalf("insert durable row %d: %v", seq, err)
	}
}

func TestOnlyExportedStoredExpiredRowsGo(t *testing.T) {
	t.Parallel()
	pool := newOutboxPool(t)
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	recent := now.Add(-time.Hour)

	insertRow(t, pool, 1, &old, &old, true, 0)       // exported, stored, old: goes
	insertRow(t, pool, 2, &old, &old, false, 0)      // no durable copy: stays
	insertRow(t, pool, 3, &old, &old, true, 1)       // durable hash differs: stays
	insertRow(t, pool, 4, &old, nil, true, 0)        // not exported: stays
	insertRow(t, pool, 5, nil, nil, false, 0)        // not stored: stays
	insertRow(t, pool, 6, &recent, &recent, true, 0) // inside the window: stays

	sweeper, err := New(pool, closedGate, Config{RetentionDays: 30, BatchSize: 100}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := sweeper.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 1 || !stats.Drained {
		t.Fatalf("stats %+v, want 1 deleted", stats)
	}
	rows, err := pool.Query(context.Background(), `SELECT seq FROM elitea_runtime.security_audit_outbox ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	seqs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seqs) != "[2 3 4 5 6]" {
		t.Fatalf("surviving outbox rows %v, want [2 3 4 5 6]", seqs)
	}
	var durable int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM elitea_identity.security_audit_events`).Scan(&durable); err != nil {
		t.Fatal(err)
	}
	if durable != 4 {
		t.Fatalf("durable store holds %d rows, want 4 (never deleted from)", durable)
	}
}
