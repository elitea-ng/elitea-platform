package repos

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Issue 1968: tenant migration 0172 truncates stored descriptions to 2000
// characters and names to 128 (characters, not bytes), leaves configuration
// titles (the key credentials are referenced by) alone, adds the CHECK constraints, and is idempotent. The test
// builds a small schema in the shape of 001_initial.sql, fills it with
// over-limit rows and applies the migration file as the runner would (the
// search_path pinned to the schema). CI-only: skipped without the integration
// database.
func TestEntityTextLimitsMigration_TruncatesAndConstrains(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()

	sqlText, err := os.ReadFile("../../../../migrations/tenant/0172_entity_name_description_limits.sql")
	if err != nil {
		t.Fatal(err)
	}

	exec := func(tx pgx.Tx, query string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	exec(tx, `CREATE SCHEMA lim1968`)
	exec(tx, `SET LOCAL search_path TO lim1968`)
	exec(tx, `
CREATE TABLE applications (id serial PRIMARY KEY, name varchar(128) NOT NULL, description varchar(2304));
CREATE TABLE skills (id serial PRIMARY KEY, name varchar(128) NOT NULL, description varchar(2304) NOT NULL);
CREATE TABLE elitea_tools (id serial PRIMARY KEY, name varchar(128) NOT NULL, description text);
CREATE TABLE chat_conversation_folders (id serial PRIMARY KEY, name varchar NOT NULL);
CREATE TABLE chat_conversations (id serial PRIMARY KEY, name varchar NOT NULL);
CREATE TABLE configuration (id serial PRIMARY KEY, label varchar, elitea_title varchar NOT NULL UNIQUE)`)

	// 2100 and 5000 multi-byte characters: the cut counts characters.
	longDesc := strings.Repeat("字", 2100)
	longName := strings.Repeat("я", 300)
	exec(tx, `INSERT INTO applications (name, description) VALUES ('a', $1), ('b', $2)`, longDesc, strings.Repeat("d", 2000))
	exec(tx, `INSERT INTO skills (name, description) VALUES ('s', $1)`, longDesc)
	exec(tx, `INSERT INTO elitea_tools (name, description) VALUES ('t', $1), ('u', NULL)`, strings.Repeat("x", 5000))
	exec(tx, `INSERT INTO chat_conversation_folders (name) VALUES ($1)`, longName)
	exec(tx, `INSERT INTO chat_conversations (name) VALUES ($1)`, longName)
	// The title is the key other rows reference a credential by: it must survive untouched.
	exec(tx, `INSERT INTO configuration (label, elitea_title) VALUES ($1, $2), ($1, $3)`,
		longName, strings.Repeat("t", 200)+"1", strings.Repeat("t", 200)+"2")

	// Applied twice: the second run is a no-op.
	for run := 0; run < 2; run++ {
		exec(tx, string(sqlText))
	}

	count := func(query string) int {
		t.Helper()
		var n int
		if err := tx.QueryRow(ctx, query).Scan(&n); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
		return n
	}
	if n := count(`SELECT char_length(description) FROM applications WHERE name = 'a'`); n != 2000 {
		t.Fatalf("application description = %d characters, want 2000", n)
	}
	if n := count(`SELECT char_length(description) FROM applications WHERE name = 'b'`); n != 2000 {
		t.Fatalf("a description at the limit must be untouched, got %d", n)
	}
	if n := count(`SELECT char_length(description) FROM skills`); n != 2000 {
		t.Fatalf("skill description = %d", n)
	}
	if n := count(`SELECT char_length(description) FROM elitea_tools WHERE name = 't'`); n != 2000 {
		t.Fatalf("toolkit description = %d", n)
	}
	if n := count(`SELECT count(*) FROM elitea_tools WHERE description IS NULL`); n != 1 {
		t.Fatal("a NULL description must stay NULL")
	}
	if n := count(`SELECT char_length(name) FROM chat_conversation_folders`); n != 128 {
		t.Fatalf("folder name = %d", n)
	}
	if n := count(`SELECT char_length(name) FROM chat_conversations`); n != 128 {
		t.Fatalf("conversation name = %d", n)
	}
	if n := count(`SELECT max(char_length(label)) FROM configuration`); n != 128 {
		t.Fatalf("configuration label = %d", n)
	}
	if n := count(`SELECT count(*) FROM configuration WHERE char_length(elitea_title) = 201`); n != 2 {
		t.Fatalf("the configuration titles were rewritten (%d left at 201 characters, want 2): links that name a credential by title would break", n)
	}

	// The constraints now refuse an over-limit write.
	for _, query := range []string{
		`INSERT INTO applications (name, description) VALUES ('c', repeat('d', 2001))`,
		`INSERT INTO skills (name, description) VALUES ('c', repeat('d', 2001))`,
		`INSERT INTO elitea_tools (name, description) VALUES ('c', repeat('d', 2001))`,
		`INSERT INTO chat_conversation_folders (name) VALUES (repeat('n', 129))`,
	} {
		exec(tx, `SAVEPOINT refused`)
		if _, err := tx.Exec(ctx, query); err == nil {
			t.Fatalf("the CHECK constraint accepted: %s", query)
		}
		exec(tx, `ROLLBACK TO SAVEPOINT refused`)
	}
	exec(tx, `INSERT INTO applications (name, description) VALUES ('ok', repeat('字', 2000))`)
}
