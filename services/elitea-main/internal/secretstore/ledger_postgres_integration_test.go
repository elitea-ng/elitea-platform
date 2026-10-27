package secretstore

// The staged-write ledger against a real PostgreSQL, applying shared migration
// 0182 from its FILE so this test proves the schema that deploys.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
)

func newLedgerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	databaseName := fmt.Sprintf("elitea_ledger_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
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
	migration, err := os.ReadFile("../../migrations/shared/0182_secret_write_ledger.sql")
	if err != nil {
		t.Fatalf("the migration file must be readable: %v", err)
	}
	for range 2 { // applying it twice must be a no-op
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("apply 0182: %v", err)
		}
	}
	return pool
}

// recordingStore answers Write with a scripted result and records retires.
type recordingStore struct {
	SecretStore
	mu        sync.Mutex
	writeErr  error
	version   int64
	retireErr error
	retired   []int64
}

func (s *recordingStore) Write(_ context.Context, _ StoreContext, _ SecretRef, _ RuntimeSecret, cas int64) (int64, error) {
	if s.writeErr != nil {
		return s.version, s.writeErr
	}
	if s.version != 0 {
		return s.version, nil
	}
	return cas + 1, nil
}

func (s *recordingStore) Retire(_ context.Context, _ StoreContext, _ SecretRef, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired = append(s.retired, version)
	return s.retireErr
}

// versionScoped is a recordingStore that, like the Vault adapter, retires
// exactly one version, so the sweeper may retire through it.
type versionScoped struct{ *recordingStore }

func (versionScoped) RetiresOnlyTheWrittenVersion() {}

func ledgerRef() SecretRef {
	return SecretRef{Backend: BackendVault, Mount: "secret", Path: "tenant7/db"}
}

func ledgerStates(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT state, count(*) FROM elitea_identity.secret_write_ledger GROUP BY state`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := map[string]int{}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			t.Fatal(err)
		}
		states[state] = count
	}
	return states
}

func TestPostgresLedgerStagedWriteOutcomes(t *testing.T) {
	pool := newLedgerPool(t)
	ledger, err := NewLedger(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	value := NewRuntimeSecret([]byte("v"))

	ok := &recordingStore{}
	if version, err := ledger.StagedWrite(ctx, ok, governedContext(), ledgerRef(), value, 0); err != nil || version != 1 {
		t.Fatalf("committed write = %d, %v", version, err)
	}
	conflict := &recordingStore{writeErr: Errorf(ErrVersionConflict, OpWrite, "x")}
	if _, err := ledger.StagedWrite(ctx, conflict, governedContext(), ledgerRef(), value, 0); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("conflict = %v", err)
	}
	unknown := &recordingStore{writeErr: Errorf(ErrUnavailable, OpWrite, "x")}
	if _, err := ledger.StagedWrite(ctx, unknown, governedContext(), ledgerRef(), value, 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown = %v", err)
	}
	states := ledgerStates(t, pool)
	if states["committed"] != 1 || states["abandoned"] != 1 || states["staged"] != 1 {
		t.Fatalf("states = %v", states)
	}

	// A committed row never moves again, and no row returns to staged.
	if _, err := pool.Exec(ctx, `UPDATE elitea_identity.secret_write_ledger SET state = 'abandoned' WHERE state = 'committed'`); err == nil {
		t.Fatal("a committed row must not change state")
	}
	if _, err := pool.Exec(ctx, `UPDATE elitea_identity.secret_write_ledger SET locator_path = 'x' WHERE state = 'staged'`); err == nil {
		t.Fatal("a row's identity must not change")
	}
}

func insertStale(t *testing.T, pool *pgxpool.Pool, ref SecretRef, version int64, state string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
INSERT INTO elitea_identity.secret_write_ledger
    (credential_ref_id, tenant_project_id, workload_scope, policy_revision_id,
     backend, locator_mount, locator_path, intended_version, state, created_at)
VALUES ('cref-1', 7, 'agent-runtime', '2026-10-r1', $1, $2, $3, $4, $5, clock_timestamp() - interval '1 hour')
RETURNING id`, ref.Backend, ref.Mount, ref.Path, version, state).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPostgresLedgerSweepStale(t *testing.T) {
	pool := newLedgerPool(t)
	ledger, err := NewLedger(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref := ledgerRef()

	// v2: staged and stale, nobody committed it -> retire and abandon.
	insertStale(t, pool, ref, 2, "staged")
	// v3: a loser of a CAS race left staged; the winner committed v3.
	insertStale(t, pool, ref, 3, "committed")
	insertStale(t, pool, ref, 3, "staged")
	// A young staged row is not touched.
	if _, err := ledger.StagedWrite(ctx, &recordingStore{writeErr: Errorf(ErrUnavailable, OpWrite, "x")}, governedContext(), ref, NewRuntimeSecret([]byte("v")), 4); err == nil {
		t.Fatal("expected unavailable")
	}
	// A row for a backend that is not in the map stays staged.
	other := SecretRef{Backend: BackendPGVault, Path: "name"}
	insertStale(t, pool, other, 1, "staged")

	store := &recordingStore{}
	result, err := ledger.SweepStale(ctx, map[string]SecretStore{BackendVault: versionScoped{store}}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.Abandoned != 1 || result.Superseded != 1 || result.Deferred != 1 {
		t.Fatalf("result = %+v", result)
	}
	if len(store.retired) != 1 || store.retired[0] != 2 {
		t.Fatalf("retired = %v; a committed version must never be retired", store.retired)
	}
	states := ledgerStates(t, pool)
	if states["staged"] != 2 || states["abandoned"] != 2 || states["committed"] != 1 {
		t.Fatalf("states = %v", states)
	}

	// A retire that fails leaves the row staged for the next sweep.
	insertStale(t, pool, ref, 9, "staged")
	failing := &recordingStore{retireErr: Errorf(ErrUnavailable, OpRetire, "x")}
	result, err = ledger.SweepStale(ctx, map[string]SecretStore{BackendVault: versionScoped{failing}}, 0, 100)
	if err != nil || result.Deferred != 2 || result.Abandoned != 0 {
		t.Fatalf("failing retire = %+v, %v", result, err)
	}
	// A not-found retire counts as retired.
	notFound := &recordingStore{retireErr: Errorf(ErrNotFound, OpRetire, "x")}
	result, err = ledger.SweepStale(ctx, map[string]SecretStore{BackendVault: versionScoped{notFound}}, 0, 100)
	if err != nil || result.Abandoned != 1 {
		t.Fatalf("not-found retire = %+v, %v", result, err)
	}
	// A store that removes by name (pgvault) is never retired through: the
	// row is abandoned and counted as a possible orphan.
	byName := &recordingStore{}
	result, err = ledger.SweepStale(ctx, map[string]SecretStore{BackendPGVault: byName}, 0, 100)
	if err != nil || result.Unretired != 1 || result.Abandoned != 0 || len(byName.retired) != 0 {
		t.Fatalf("name-scoped store = %+v, %v, retired %v", result, err, byName.retired)
	}
}

// A write the backend answered with another version leaves the row staged
// with that version recorded, and the sweeper retires the recorded version,
// not the intended one.
func TestPostgresLedgerUnexpectedVersionIsRetired(t *testing.T) {
	pool := newLedgerPool(t)
	ledger, err := NewLedger(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	value := NewRuntimeSecret([]byte("v"))
	for _, store := range []*recordingStore{
		{version: 5, writeErr: Errorf(ErrUnexpectedVersion, OpWrite, "x")}, // what the Vault adapter returns
		{version: 6}, // an adapter that answered success with the wrong version
	} {
		version, err := ledger.StagedWrite(ctx, store, governedContext(), ledgerRef(), value, 0)
		if !errors.Is(err, ErrUnexpectedVersion) || version != 0 {
			t.Fatalf("unexpected version = %d, %v", version, err)
		}
	}
	var staged, recorded int
	if err := pool.QueryRow(ctx, `
SELECT count(*), count(created_version) FROM elitea_identity.secret_write_ledger
WHERE state = 'staged' AND intended_version = 1 AND created_version IN (5, 6)`).Scan(&staged, &recorded); err != nil {
		t.Fatal(err)
	}
	if staged != 2 || recorded != 2 {
		t.Fatalf("staged = %d, recorded = %d", staged, recorded)
	}
	// created_version is set once.
	if _, err := pool.Exec(ctx, `UPDATE elitea_identity.secret_write_ledger SET created_version = 9 WHERE created_version = 5`); err == nil {
		t.Fatal("created_version must be immutable once set")
	}
	// The trigger keeps created_at immutable, so age the rows past
	// MinSweepAge with it disabled.
	if _, err := pool.Exec(ctx, `
ALTER TABLE elitea_identity.secret_write_ledger DISABLE TRIGGER secret_write_ledger_transition;
UPDATE elitea_identity.secret_write_ledger SET created_at = created_at - interval '1 hour';
ALTER TABLE elitea_identity.secret_write_ledger ENABLE TRIGGER secret_write_ledger_transition;`); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{}
	result, err := ledger.SweepStale(ctx, map[string]SecretStore{BackendVault: versionScoped{store}}, 0, 100)
	if err != nil || result.Abandoned != 2 {
		t.Fatalf("sweep = %+v, %v", result, err)
	}
	if len(store.retired) != 2 || store.retired[0]+store.retired[1] != 11 {
		t.Fatalf("retired = %v; the recorded versions 5 and 6 must be retired, never the intended 1", store.retired)
	}
}
