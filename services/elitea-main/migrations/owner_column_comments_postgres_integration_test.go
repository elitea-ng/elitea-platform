package migrations_test

// Issue #533, the database half. owner_column_meanings_test.go proves the
// corpus STATES a meaning for every owner_id and author_id column. This proves
// the statement REACHES the database: it builds the real schema — 001_initial
// then every shared and tenant file — and reads pg_description back.
//
// The two halves catch different failures. A row missing from 0128's table is a
// missing statement; a comment that never lands is a guard that skipped, a
// column name that drifted, or a table the migration could not reach. The
// second one is invisible to a file parser, and it is the shape this corpus has
// been bitten by before: a guarded branch that answers "nothing to do" reads
// exactly like success.

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTenantOwnerColumnsCarryTheirMeaningInTheDatabase(t *testing.T) {
	pool := newMigratedPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Read every owner_id/author_id column that p_1 actually has, with the
	// comment 0128 put on it. LEFT JOIN, so a column with NO comment comes back
	// as a row with an empty one rather than not coming back at all.
	rows, err := pool.Query(ctx, `
		SELECT c.relname, a.attname, COALESCE(d.description, '')
		  FROM pg_class AS c
		  JOIN pg_namespace AS n ON n.oid = c.relnamespace
		  JOIN pg_attribute AS a ON a.attrelid = c.oid
		  LEFT JOIN pg_description AS d ON d.objoid = c.oid AND d.objsubid = a.attnum
		 WHERE n.nspname = 'p_1'
		   AND c.relkind = 'r'
		   AND a.attnum > 0
		   AND NOT a.attisdropped
		   AND a.attname IN ('owner_id', 'author_id')
		 ORDER BY c.relname, a.attname`)
	if err != nil {
		t.Fatalf("read the tenant column comments: %v", err)
	}
	defer rows.Close()

	comments := map[string]string{}
	for rows.Next() {
		var table, column, description string
		if scanErr := rows.Scan(&table, &column, &description); scanErr != nil {
			t.Fatalf("scan a column comment: %v", scanErr)
		}
		comments[table+"."+column] = description
	}
	if rows.Err() != nil {
		t.Fatalf("read the tenant column comments: %v", rows.Err())
	}
	if len(comments) == 0 {
		t.Fatal("p_1 has no owner_id or author_id column at all, so this test proves nothing")
	}

	for column, description := range comments {
		if strings.TrimSpace(description) == "" {
			t.Errorf("%s carries no comment; the meaning stayed in the migration file", column)
		}
	}

	// The three columns the issue is about. Each must say which KIND of number
	// it holds, because the name says nothing: one project column and one user
	// column share the name `owner_id`, and a join written on the wrong guess
	// returns rows that look valid.
	for column, kind := range map[string]string{
		"elitea_tools.owner_id":              "PROJECT",
		"skills.owner_id":                    "PROJECT",
		"applications.owner_id":              "PROJECT",
		"chat_conversation_folders.owner_id": "USER",
		"elitea_tools.author_id":             "USER",
	} {
		description, present := comments[column]
		if !present {
			t.Errorf("p_1 has no %s, so the schema this corpus builds is not the one 0128 describes", column)
			continue
		}
		if !strings.Contains(description, kind) {
			t.Errorf("%s says %q, which does not name %s as the kind of number it holds",
				column, description, kind)
		}
	}

	// applications.owner_id WAS the disputed one: the legacy runtime and this
	// service's Fork path read it as a project, and every writer here stored a
	// user. tenant/0131 settled it. The writers now store the project, the
	// existing rows were repaired, and the column carries a foreign key to
	// centry.project.
	//
	// So the comment must no longer say DISPUTED. A stale dispute is worse than
	// no record: it tells the next reader to distrust a column that is now
	// constrained, and it invites a second "correction" of writers that are
	// already correct.
	description := comments["applications.owner_id"]
	if strings.Contains(description, "DISPUTED") {
		t.Errorf("applications.owner_id still says %q; tenant/0131 settled the dispute (#533)", description)
	}
	if !strings.Contains(description, "0131") {
		t.Errorf("applications.owner_id says %q; it must name the migration that settled it (#533)", description)
	}
}
