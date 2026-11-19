package repos

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type nameLookupTx struct {
	sqlExecutor
	execs    []string
	rowError error
	name     string
}

func (t *nameLookupTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	t.execs = append(t.execs, sql)
	return pgconn.CommandTag{}, nil
}

func (t *nameLookupTx) QueryRow(_ context.Context, _ string, _ ...any) sqlRow {
	return nameRow{name: t.name, err: t.rowError}
}

type nameRow struct {
	name string
	err  error
}

func (r nameRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = r.name
	return nil
}

// A failing name lookup keeps the worker's label (an empty name), rolls back
// to the savepoint so the frame's transaction survives, and never errors.
func TestSavedAgentNameFallsBackWhenTheLookupFails(t *testing.T) {
	t.Parallel()
	tx := &nameLookupTx{rowError: errors.New("relation does not exist")}
	if got := savedAgentName(t.Context(), tx, "p_1", 2); got != "" {
		t.Fatalf("name = %q, want the empty fallback", got)
	}
	if len(tx.execs) != 2 || tx.execs[1] != "ROLLBACK TO SAVEPOINT delegation_notice_name" {
		t.Fatalf("a failed lookup must roll back to its savepoint: %v", tx.execs)
	}
}

func TestSavedAgentNameReadsTheName(t *testing.T) {
	t.Parallel()
	tx := &nameLookupTx{name: "  Real Agent2 "}
	if got := savedAgentName(t.Context(), tx, "p_1", 2); got != "Real Agent2" {
		t.Fatalf("name = %q", got)
	}
	if got := savedAgentName(t.Context(), tx, "p_1", 0); got != "" {
		t.Fatalf("no id must look nothing up, got %q", got)
	}
}
