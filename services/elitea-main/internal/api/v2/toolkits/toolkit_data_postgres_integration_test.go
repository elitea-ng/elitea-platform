package toolkits

// The row-first toolkit delete against a real PostgreSQL (#1244): the row
// delete and the journal write are ONE transaction, the type reaches the
// journal from `DELETE ... RETURNING type`, a missing row journals nothing, and
// a journal failure leaves the row in place.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// txJournal records what the repository hands it and whether it came with the
// delete's own transaction; it can also fail, as a journal write would.
type txJournal struct {
	types []string
	inTx  []bool
	err   error
}

func (j *txJournal) Enqueue(ctx context.Context, tx pgx.Tx, _, _ int64, toolkitType string) (string, error) {
	j.types = append(j.types, toolkitType)
	j.inTx = append(j.inTx, tx != nil)
	if j.err != nil {
		return "", j.err
	}
	// Prove the transaction is the delete's: the row is already gone inside it.
	var left int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM p_1.elitea_tools`).Scan(&left); err != nil {
		return "", err
	}
	if left != 0 {
		return "", errors.New("the toolkit row is still visible inside the delete's transaction")
	}
	return "inventory", nil
}

// migratedToolkitsPool is a private database with the baseline AND the
// platform's shared migrations: the row-first delete touches
// elitea_runtime.index_registry (shared/0162), which the baseline alone does
// not create.
func migratedToolkitsPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	if err := migrate.New(pool, platformmigrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("run shared migrations: %v", err)
	}
	return pool, ctx
}

func seedOneToolkit(ctx context.Context, t *testing.T, repo *pgRepo) string {
	t.Helper()
	var id string
	if err := repo.pool.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, owner_id, author_id, created_at)
VALUES ('graph', 'inventory_inventory', '', 1, 1, now() AT TIME ZONE 'UTC') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("seed toolkit: %v", err)
	}
	return id
}

func countToolkits(ctx context.Context, t *testing.T, repo *pgRepo) int {
	t.Helper()
	var n int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM p_1.elitea_tools`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeleteToolkitRowFirstJournalsInTheSameTransaction(t *testing.T) {
	pool, ctx := migratedToolkitsPool(t)
	repo := &pgRepo{pool: pool}
	id := seedOneToolkit(ctx, t, repo)

	journal := &txJournal{}
	if err := repo.DeleteToolkitRowFirst(ctx, "1", id, journal); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if countToolkits(ctx, t, repo) != 0 {
		t.Fatal("the row survived the delete")
	}
	if len(journal.types) != 1 || journal.types[0] != "inventory_inventory" || !journal.inTx[0] {
		t.Fatalf("journal saw types=%v inTx=%v", journal.types, journal.inTx)
	}

	// A row that does not exist: no error, nothing journaled.
	if err := repo.DeleteToolkitRowFirst(ctx, "1", id, journal); err != nil {
		t.Fatalf("delete of a missing row: %v", err)
	}
	if len(journal.types) != 1 {
		t.Fatalf("a missing row was journaled: %v", journal.types)
	}
}

func TestAJournalFailureRollsTheRowDeleteBack(t *testing.T) {
	pool, ctx := migratedToolkitsPool(t)
	repo := &pgRepo{pool: pool}
	id := seedOneToolkit(ctx, t, repo)

	cause := errors.New("write cleanup journal: connection refused")
	err := repo.DeleteToolkitRowFirst(ctx, "1", id, &txJournal{err: cause})
	if !errors.Is(err, cause) {
		t.Fatalf("delete = %v, want the journal's failure", err)
	}
	if countToolkits(ctx, t, repo) != 1 {
		t.Fatal("the row was deleted although the journal write failed")
	}
}

// Without a journal the row-first delete is the plain delete.
func TestDeleteToolkitRowFirstWithoutAJournalDeletesTheRow(t *testing.T) {
	pool, ctx := migratedToolkitsPool(t)
	repo := &pgRepo{pool: pool}
	id := seedOneToolkit(ctx, t, repo)
	if err := repo.DeleteToolkitRowFirst(ctx, "1", id, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if countToolkits(ctx, t, repo) != 0 {
		t.Fatal("the row survived the delete")
	}
}

type countingJournal struct{ calls int }

func (j *countingJournal) Enqueue(context.Context, pgx.Tx, int64, int64, string) (string, error) {
	j.calls++
	return "inventory", nil
}

// seedRegistryIndex writes one live index_registry row for the toolkit, as the
// Rust indexing runtime's admission does, and returns its id.
func seedRegistryIndex(ctx context.Context, t *testing.T, repo *pgRepo, toolkitID int32, name string) string {
	t.Helper()
	registry, err := repos.NewIndexRegistryRepository(repo.pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.InitializeRegistryRun(ctx, indexingapp.RegistryInitialRun{
		ProjectID: 1, ToolkitID: toolkitID, IndexName: name,
		MetaID: name + "-meta", ExecutionID: name + "-exec", CorrelationID: name + "-corr",
		Generation: 1, IndexGeneration: 1,
		Configuration: []byte(`{"index_name":"` + name + `"}`), AdmittedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed index %s: %v", name, err)
	}
	row, found, err := registry.FindByName(ctx, 1, toolkitID, name)
	if err != nil || !found {
		t.Fatalf("find %s: found=%v err=%v", name, found, err)
	}
	return row.IndexID
}

func registryState(ctx context.Context, t *testing.T, repo *pgRepo, indexID string) (exists, tombstoned bool) {
	t.Helper()
	var deleted *time.Time
	err := repo.pool.QueryRow(ctx, `SELECT deleted_at FROM elitea_runtime.index_registry WHERE index_id = $1::uuid`, indexID).Scan(&deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true, deleted != nil
}

// Deleting a toolkit tombstones every live registry row of the toolkit in the
// delete's own transaction (ADR-0031 decision 4), so the tombstone sweeper
// deletes their vector namespaces; another toolkit's rows are untouched, a
// journal failure rolls the tombstones back with the row, and the journal-less
// delete tombstones too.
func TestDeleteToolkitRowFirstTombstonesTheToolkitsRegistryIndexes(t *testing.T) {
	pool, ctx := migratedToolkitsPool(t)
	repo := &pgRepo{pool: pool}
	id := seedOneToolkit(ctx, t, repo)
	var otherID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, owner_id, author_id, created_at)
VALUES ('other', 'github', '', 1, 1, now() AT TIME ZONE 'UTC') RETURNING id::text`).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	toolkit, _ := strconv.ParseInt(id, 10, 32)
	other, _ := strconv.ParseInt(otherID, 10, 32)
	docs := seedRegistryIndex(ctx, t, repo, int32(toolkit), "docs")
	code := seedRegistryIndex(ctx, t, repo, int32(toolkit), "code")
	kept := seedRegistryIndex(ctx, t, repo, int32(other), "kept")

	// A journal failure rolls everything back: the row and the tombstones.
	if err := repo.DeleteToolkitRowFirst(ctx, "1", id, &txJournal{err: errors.New("journal down")}); err == nil {
		t.Fatal("the journal failure was swallowed")
	}
	for _, indexID := range []string{docs, code, kept} {
		if exists, tombstoned := registryState(ctx, t, repo, indexID); !exists || tombstoned {
			t.Fatalf("index %s after a rolled-back delete: exists=%v tombstoned=%v", indexID, exists, tombstoned)
		}
	}

	// A journal that only records (txJournal asserts an EMPTY table, and the
	// other toolkit is meant to survive).
	journal := &countingJournal{}
	if err := repo.DeleteToolkitRowFirst(ctx, "1", id, journal); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if journal.calls != 1 {
		t.Fatalf("journal calls = %d", journal.calls)
	}
	for _, indexID := range []string{docs, code} {
		if exists, tombstoned := registryState(ctx, t, repo, indexID); !exists || !tombstoned {
			t.Fatalf("index %s of the deleted toolkit: exists=%v tombstoned=%v, want a tombstone", indexID, exists, tombstoned)
		}
	}
	if exists, tombstoned := registryState(ctx, t, repo, kept); !exists || tombstoned {
		t.Fatalf("the other toolkit's index: exists=%v tombstoned=%v, want live", exists, tombstoned)
	}
	// The tombstones are the sweeper's: claimable, with the toolkit's ids.
	registry, _ := repos.NewIndexRegistryRepository(pool)
	claimed, err := registry.ClaimTombstones(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("claimed %+v err=%v, want the two tombstones", claimed, err)
	}
	for _, tombstone := range claimed {
		if tombstone.ToolkitID != int32(toolkit) || tombstone.ProjectID != 1 {
			t.Fatalf("tombstone %+v names another toolkit", tombstone)
		}
	}

	// The journal-less delete tombstones too.
	if err := repo.DeleteToolkit(ctx, "1", otherID); err != nil {
		t.Fatalf("plain delete: %v", err)
	}
	if exists, tombstoned := registryState(ctx, t, repo, kept); !exists || !tombstoned {
		t.Fatalf("the plain delete left the index live: exists=%v tombstoned=%v", exists, tombstoned)
	}
	if countToolkits(ctx, t, repo) != 0 {
		t.Fatal("a row survived")
	}
}

// A database on the baseline alone — no shared migrations, so no
// elitea_runtime.index_registry (shared/0162) — still deletes toolkits: the
// tombstone statement is skipped when the table is absent, and the journal
// still runs in the same transaction.
func TestDeleteToolkitRowFirstOnADatabaseWithoutTheIndexRegistryTable(t *testing.T) {
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	var present bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('elitea_runtime.index_registry') IS NOT NULL`).Scan(&present); err != nil || present {
		t.Fatalf("the baseline must not have the registry table (present=%v err=%v)", present, err)
	}
	repo := &pgRepo{pool: pool}
	id := seedOneToolkit(ctx, t, repo)
	journal := &txJournal{}
	if err := repo.DeleteToolkitRowFirst(ctx, "1", id, journal); err != nil {
		t.Fatalf("delete on a baseline-only database: %v", err)
	}
	if countToolkits(ctx, t, repo) != 0 || len(journal.types) != 1 {
		t.Fatalf("toolkits left=%d journaled=%v, want the row gone and the journal written", countToolkits(ctx, t, repo), journal.types)
	}
	// The journal-less path too.
	id = seedOneToolkit(ctx, t, repo)
	if err := repo.DeleteToolkit(ctx, "1", id); err != nil || countToolkits(ctx, t, repo) != 0 {
		t.Fatalf("DeleteToolkit on a baseline-only database: err=%v left=%d", err, countToolkits(ctx, t, repo))
	}
}

// memoryJournal is a journal that also takes the memory toolkit's id.
type memoryJournal struct {
	txJournal
	toolkits []int64
}

func (j *memoryJournal) EnqueueMemory(_ context.Context, tx pgx.Tx, projectID, toolkitID int64) (string, error) {
	if tx == nil || projectID != 1 || toolkitID <= 0 {
		return "", errors.New("unexpected memory enqueue")
	}
	j.toolkits = append(j.toolkits, toolkitID)
	return "memory", nil
}

// A memory toolkit's delete records the toolkit id in the delete's transaction,
// whatever its settings say: the store is the toolkit's, not a namespace
// string's, so a rename or a changed namespace setting orphans nothing.
func TestAMemoryToolkitDeleteRecordsItsIdInTheSameTransaction(t *testing.T) {
	pool, ctx := migratedToolkitsPool(t)
	repo := &pgRepo{pool: pool}
	for _, settings := range []string{
		`{"namespace": "team-notes", "memory_scope": "both"}`,
		`{"memory_scope": "personal"}`,
		`{"namespace": "   "}`,
	} {
		var id string
		if err := pool.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, settings, owner_id, author_id, created_at)
VALUES ('notes', 'memory', '', $1::jsonb, 1, 1, now() AT TIME ZONE 'UTC') RETURNING id::text`, settings).Scan(&id); err != nil {
			t.Fatalf("seed memory toolkit: %v", err)
		}
		journal := &memoryJournal{}
		if err := repo.DeleteToolkitRowFirst(ctx, "1", id, journal); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if want := id; len(journal.toolkits) != 1 || fmt.Sprint(journal.toolkits[0]) != want {
			t.Fatalf("settings %s: the journal got %v, want %s", settings, journal.toolkits, want)
		}
	}
	// Another type is never offered the memory enqueue.
	other := seedOneToolkit(ctx, t, repo)
	journal := &memoryJournal{}
	if err := repo.DeleteToolkitRowFirst(ctx, "1", other, journal); err != nil {
		t.Fatal(err)
	}
	if len(journal.toolkits) != 0 {
		t.Fatalf("a non-memory toolkit was offered the memory enqueue: %v", journal.toolkits)
	}
}
