package migrations_test

// tenant/0154: the retired `pgvector_configuration` key in
// `elitea_tools.settings` (#1359).
//
// The cases run the ledgered corpus, write rows the way a database that
// predates 0154 holds them, and then run the migration's own SQL again in the
// tenant schema, for a `jsonb` and for a legacy `json` column. Re-running the
// real file is the point: a copy of the SQL here would measure the copy.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const toolkitPgvectorMigration = "tenant/0154_strip_toolkit_pgvector_configuration.sql"

func applyToolkitPgvectorMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, err := os.ReadFile(toolkitPgvectorMigration)
	if err != nil {
		t.Fatalf("read %s: %v", toolkitPgvectorMigration, err)
	}
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, "SET search_path TO p_1; "+string(body)); err != nil {
		t.Fatalf("apply %s: %v", toolkitPgvectorMigration, err)
	}
}

func TestTheStoredPgvectorConfigurationIsRemovedAndNothingElse(t *testing.T) {
	for _, columnType := range []string{"jsonb", "json"} {
		t.Run(columnType, func(t *testing.T) {
			pool := newMigratedPool(t)
			ctx, cancel := testContext()
			defer cancel()

			if columnType == "json" {
				if _, err := pool.Exec(ctx, `
ALTER TABLE p_1.elitea_tools ALTER COLUMN settings DROP DEFAULT;
ALTER TABLE p_1.elitea_tools ALTER COLUMN settings TYPE json USING settings::json`); err != nil {
					t.Fatalf("make settings json: %v", err)
				}
			}
			seeds := map[string]string{
				"with-reference": `{"url": "https://jira.example", "pgvector_configuration": {"elitea_title": "elitea-pgvector", "private": false}}`,
				"without":        `{"url": "https://jira.example"}`,
				"array":          `["pgvector_configuration"]`,
			}
			for name, settings := range seeds {
				if _, err := pool.Exec(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, owner_id, author_id, meta, settings, created_at)
VALUES ($1, 'jira', '', 1, 1, '{}'::jsonb, $2::text::`+columnType+`, now() AT TIME ZONE 'UTC')`,
					name, settings); err != nil {
					t.Fatalf("seed toolkit %s: %v", name, err)
				}
			}

			// Twice: the migration is idempotent.
			applyToolkitPgvectorMigration(t, pool)
			applyToolkitPgvectorMigration(t, pool)

			want := map[string]string{
				"with-reference": `{"url": "https://jira.example"}`,
				"without":        `{"url": "https://jira.example"}`,
				// Not an object: left alone rather than reinterpreted.
				"array": `["pgvector_configuration"]`,
			}
			for name, settings := range want {
				var got string
				if err := pool.QueryRow(ctx,
					`SELECT settings::jsonb::text FROM p_1.elitea_tools WHERE name = $1`, name).Scan(&got); err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				if got != settings {
					t.Fatalf("%s: settings = %s, want %s", name, got, settings)
				}
			}
		})
	}
}

// A tenant schema without `elitea_tools` (one this repository built by
// itself, 0124) records the migration and changes nothing.
func TestThePgvectorConfigurationStripSkipsASchemaWithoutToolkits(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, `DROP TABLE p_1.elitea_tools CASCADE`); err != nil {
		t.Fatalf("drop elitea_tools: %v", err)
	}
	applyToolkitPgvectorMigration(t, pool)
}
