package migrations_test

// Issue #533, the constraint half.
//
// owner_column_meanings_test.go proves the corpus STATES a meaning.
// owner_column_comments_postgres_integration_test.go proves the statement
// REACHES the database. Neither proves that the database now REFUSES a value of
// the wrong kind, because until tenant/0131 no column refused anything: both
// meanings were plain INTEGER with no constraint, so a user id in a project
// column was stored without complaint.
//
// The three tests here read the constraint, not the file:
//
//   - the foreign key exists on each PROJECT-kind owner_id, and it cascades;
//   - a number that names no project is refused with SQLSTATE 23503;
//   - a schema that already holds the wrong kind of number is REPAIRED by the
//     migration and keeps its rows.
//
// The third one is the one a file parser cannot answer. It builds a second
// tenant schema, fills it with rows shaped the way this service wrote them
// before #533 — the caller's user id in owner_id — and then runs the ledgered
// tenant history over it.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	migrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// The tables the issue names that this corpus builds. `prompt_collections` is
// absent on purpose: no migration in this repository creates it, and the legacy
// runtime drops it. 0131 probes for it and this test cannot.
var projectKindOwnerColumns = map[string]string{
	"applications": "applications_owner_project_fkey",
	"skills":       "skills_owner_project_fkey",
}

func TestProjectKindOwnerColumnsCarryAForeignKeyToTheProject(t *testing.T) {
	pool := newMigratedPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for table, constraintName := range projectKindOwnerColumns {
		var (
			referenced   string
			column       string
			onDelete     string
			constraintOK bool
		)
		err := pool.QueryRow(ctx, `
			SELECT parent_space.nspname || '.' || parent.relname,
			       attribute.attname,
			       constraint_.confdeltype,
			       true
			  FROM pg_catalog.pg_constraint AS constraint_
			  JOIN pg_catalog.pg_class AS child ON child.oid = constraint_.conrelid
			  JOIN pg_catalog.pg_namespace AS child_space ON child_space.oid = child.relnamespace
			  JOIN pg_catalog.pg_class AS parent ON parent.oid = constraint_.confrelid
			  JOIN pg_catalog.pg_namespace AS parent_space ON parent_space.oid = parent.relnamespace
			  JOIN pg_catalog.pg_attribute AS attribute
			    ON attribute.attrelid = constraint_.conrelid
			   AND attribute.attnum = constraint_.conkey[1]
			 WHERE child_space.nspname = 'p_1'
			   AND child.relname = $1
			   AND constraint_.conname = $2
			   AND constraint_.contype = 'f'`,
			table, constraintName).Scan(&referenced, &column, &onDelete, &constraintOK)
		if err != nil {
			t.Errorf("p_1.%s has no constraint %s: %v; the meaning stayed a comment", table, constraintName, err)
			continue
		}
		if column != "owner_id" {
			t.Errorf("%s constrains %s, want owner_id", constraintName, column)
		}
		if referenced != "centry.project" {
			t.Errorf("%s references %s, want centry.project", constraintName, referenced)
		}
		// 'c' is ON DELETE CASCADE. Project delete removes the project row
		// before it drops the tenant schema (#374), so NO ACTION here would
		// stop every delete of a project that holds one agent.
		if onDelete != "c" {
			t.Errorf("%s has confdeltype %q, want \"c\" (ON DELETE CASCADE); a project delete would fail",
				constraintName, onDelete)
		}
	}
}

func TestAProjectKindOwnerIDRefusesANumberThatNamesNoProject(t *testing.T) {
	pool := newMigratedPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The number is a user id in every deployment shape this repository builds:
	// 001_initial seeds one project and this database has no project 987654.
	// Before 0131 the INSERT below succeeded and left a row that named a
	// project which does not exist.
	var danglingProjects int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM centry.project WHERE id = 987654`).Scan(&danglingProjects); err != nil {
		t.Fatalf("read the project table: %v", err)
	}
	if danglingProjects != 0 {
		t.Fatal("project 987654 exists, so this test proves nothing")
	}

	_, err := pool.Exec(ctx, `
		INSERT INTO p_1.applications (name, description, owner_id)
		VALUES ('wrong-kind', 'an owner_id that names no project', 987654)`)
	if err == nil {
		t.Fatal("p_1.applications accepted an owner_id that names no project")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("insert failed with %v, want SQLSTATE 23503 (foreign key violation)", err)
	}

	// The same statement with the project of the schema is accepted, so the
	// constraint refuses the wrong number and not every number.
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.applications (name, description, owner_id)
		VALUES ('right-kind', 'the project of this schema', 1)`); err != nil {
		t.Fatalf("p_1.applications refused the project of its own schema: %v", err)
	}
}

// The rows that exist. The issue asks for proof that none of them breaks.
//
// This builds schema p_2 the way a deployment has it — the bootstrap function
// creates the tables — writes the rows this service used to write, and then
// applies the ledgered tenant history. 0131 repairs the rows before it adds the
// constraint, so both rows survive with the project of their schema.
func TestTheMigrationRepairsExistingRowsAndKeepsThem(t *testing.T) {
	pool := newMigratedPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
		INSERT INTO centry.project (id, name, owner_id, create_success)
		VALUES (2, 'Second Project', 1, true)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("create the second project: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT create_tenant_schema('p_2')`); err != nil {
		t.Fatalf("create the second tenant schema: %v", err)
	}

	// The shape this service wrote before #533: the caller's user id in
	// owner_id. User 1 is seeded by 001_initial, so the number is a real user
	// and a wrong project.
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_2.applications (id, name, description, owner_id)
		VALUES (1, 'legacy-shaped', 'owner_id holds the caller user id', 1)`); err != nil {
		t.Fatalf("write the legacy-shaped application row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_2.skills (id, name, description, owner_id, author_id)
		VALUES (1, 'legacy-shaped skill', 'owner_id holds the literal 1', 1, 1)`); err != nil {
		t.Fatalf("write the legacy-shaped skill row: %v", err)
	}

	runner := migrate.New(pool, migrations.Files)
	if err := runner.ApplyTenant(ctx, 2); err != nil {
		t.Fatalf("apply the tenant history to p_2: %v", err)
	}

	for _, table := range []string{"applications", "skills"} {
		var rows, owner int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*), COALESCE(MAX(owner_id), 0) FROM p_2.`+table).Scan(&rows, &owner); err != nil {
			t.Fatalf("read p_2.%s back: %v", table, err)
		}
		if rows != 1 {
			t.Errorf("p_2.%s holds %d rows, want the 1 row that existed before the migration", table, rows)
		}
		if owner != 2 {
			t.Errorf("p_2.%s owner_id = %d, want the project of the schema, which is 2", table, owner)
		}
	}
}
