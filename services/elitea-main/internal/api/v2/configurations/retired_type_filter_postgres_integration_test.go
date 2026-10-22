package configurations

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
)

// The list filter normalises the STORED type in SQL, which a unit test can
// only read as text: whether btrim and lower, with the E-string escapes as
// written, hide "PgVector" and "pgvector " and nothing else is a fact about
// PostgreSQL. The filter runs over literal rows, so the test needs no schema
// and touches no table.
func TestRetiredTypeFilterHidesThePgvectorTypeWhateverItsCaseAndPadding(t *testing.T) {
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	rows, err := conn.Query(ctx, `
SELECT type FROM (VALUES
	('pgvector'), ('PgVector'), ('pgvector '), (E' PGVECTOR\t'), (E'pgvector\n'), (E'pgvector\x0B'),
	('openai'), ('pgvector_x'), ('vector'), ('pgvec tor')
) AS configuration(type) WHERE TRUE`+retiredConfigurationTypeFilter+` ORDER BY type`)
	if err != nil {
		t.Fatalf("run the filter: %v", err)
	}
	kept, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the kept rows: %v", err)
	}
	// Every padded or re-cased pgvector goes; a type that only contains the
	// word, or ends in a v that a wrong escape would strip, stays.
	want := []string{"openai", "pgvec tor", "pgvector_x", "vector"}
	if !slices.Equal(kept, want) {
		t.Fatalf("kept %q, want %q", kept, want)
	}
}
