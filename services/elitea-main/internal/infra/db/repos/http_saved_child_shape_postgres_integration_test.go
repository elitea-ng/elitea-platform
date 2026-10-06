package repos

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Authored real PostgreSQL acceptance of the migrated owning CHECK. The private
// migrated fixture provides the actual constraint definition; its exact CHECK
// is exercised on the four-column projection inside a rolled-back transaction.
// This isolates SQL NULL semantics without dispatching an effect or inventing
// execution/claim authority. It does not claim FK/runtime integration coverage.
func TestPostgresHTTPChildShapeRejectsEveryPartialNullTuple(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	var definition string
	if err = tx.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='elitea_runtime.execution_http_effects'::regclass AND conname='execution_http_effects_saved_child_shape'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(definition, "CHECK (") {
		t.Fatal("owning check missing")
	}
	if _, err = tx.Exec(t.Context(), `CREATE TEMP TABLE http_child_shape_acceptance(saved_child_scope_id text,saved_child_scope_revision bigint,saved_child_scope_digest text,saved_child_graph_thread text) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `ALTER TABLE http_child_shape_acceptance ADD CONSTRAINT execution_http_effects_saved_child_shape `+definition); err != nil {
		t.Fatal(err)
	}
	// All-null root and complete child retain their original valid shapes.
	for _, values := range [][]any{{nil, nil, nil, nil}, {strings.Repeat("a", 64), int64(1), strings.Repeat("b", 64), "exact-child-thread"}} {
		if _, err = tx.Exec(t.Context(), `INSERT INTO http_child_shape_acceptance VALUES($1,$2,$3,$4)`, values...); err != nil {
			t.Fatal(err)
		}
	}
	// Four single-null cases plus every two/three-null partial tuple: 14 total.
	for mask := 1; mask < 15; mask++ {
		t.Run(fmt.Sprintf("present_mask_%04b", mask), func(t *testing.T) {
			values := []any{nil, nil, nil, nil}
			full := []any{strings.Repeat("a", 64), int64(1), strings.Repeat("b", 64), "exact-child-thread"}
			for index := range values {
				if mask&(1<<index) != 0 {
					values[index] = full[index]
				}
			}
			if _, err := tx.Exec(t.Context(), "SAVEPOINT child_shape_case"); err != nil {
				t.Fatal(err)
			}
			_, err := tx.Exec(t.Context(), `INSERT INTO http_child_shape_acceptance VALUES($1,$2,$3,$4)`, values...)
			var postgres *pgconn.PgError
			if !errors.As(err, &postgres) || postgres.Code != "23514" || postgres.ConstraintName != "execution_http_effects_saved_child_shape" {
				t.Fatalf("partial tuple accepted or failed elsewhere: %v", err)
			}
			if _, err := tx.Exec(t.Context(), "ROLLBACK TO SAVEPOINT child_shape_case"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
