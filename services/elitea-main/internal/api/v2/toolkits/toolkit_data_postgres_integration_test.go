package toolkits

// The row-first toolkit delete against a real PostgreSQL (#1244): the row
// delete and the journal write are ONE transaction, the type reaches the
// journal from `DELETE ... RETURNING type`, a missing row journals nothing, and
// a journal failure leaves the row in place.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
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

func seedOneToolkit(ctx context.Context, t *testing.T, repo *pgRepo) string {
	t.Helper()
	var id string
	if err := repo.pool.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, owner_id, author_id)
VALUES ('graph', 'inventory_inventory', '', 1, 1) RETURNING id::text`).Scan(&id); err != nil {
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
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
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
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
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
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	repo := &pgRepo{pool: pool}
	id := seedOneToolkit(ctx, t, repo)
	if err := repo.DeleteToolkitRowFirst(ctx, "1", id, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if countToolkits(ctx, t, repo) != 0 {
		t.Fatal("the row survived the delete")
	}
}
