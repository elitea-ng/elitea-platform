package migrations_test

// Issue #533. `owner_id` holds a PROJECT in one tenant table and a USER in the
// next, and no foreign key catches either. The meanings are recorded on the
// columns by tenant/0128_owner_id_column_meanings.sql. A record rots the moment
// a new column arrives without one, and nothing else in this repository can see
// that happen: a column with no constraint and no reader is invisible to the
// type checker, to the linter and to every integration test.
//
// So this test reads the SQL corpus as data. It collects every tenant column
// NAMED `owner_id` or `author_id`, from the bootstrap's tenant template and
// from the ledgered tenant files, and it fails when one of them has no entry in
// 0128's table of meanings.
//
// It needs no PostgreSQL: it parses the same bytes the migration binary
// embeds.

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	migrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// bootstrapSchemaPath is the dev bootstrap, whose tenant template declares most
// of these columns. Only its `%I.` tables are tenant tables; `centry.project`
// also has an `owner_id` and a tenant migration cannot comment it.
const bootstrapSchemaPath = "../internal/infra/db/migrations/001_initial.sql"

// ownerColumnNames are the two names issue #533 is about. `shared_owner_id` is
// deliberately absent: it carries its own qualifier, and no writer disagrees
// about it.
var ownerColumnNames = map[string]bool{"owner_id": true, "author_id": true}

type column struct{ table, name string }

func TestEveryTenantOwnerAndAuthorColumnDeclaresItsMeaning(t *testing.T) {
	t.Parallel()

	declared := map[column]string{}

	bootstrap, err := os.ReadFile(bootstrapSchemaPath)
	if err != nil {
		t.Fatalf("read %s: %v", bootstrapSchemaPath, err)
	}
	for _, c := range tenantOwnerColumns(string(bootstrap), true) {
		declared[c] = bootstrapSchemaPath
	}

	entries, err := migrations.Files.ReadDir("tenant")
	if err != nil {
		t.Fatalf("read the tenant corpus: %v", err)
	}
	documented := map[column]bool{}
	for _, entry := range entries {
		path := "tenant/" + entry.Name()
		body, readErr := migrations.Files.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		text := string(body)
		for _, c := range tenantOwnerColumns(text, false) {
			declared[c] = path
		}
		for _, c := range commentedColumns(text) {
			documented[c] = true
		}
	}

	// A parser that stops finding columns turns this test green while proving
	// nothing, which is the failure mode the whole file exists to avoid. So the
	// columns that were measured when it was written are a FLOOR: the parser
	// must still find each of them. Add to this list when a new one appears;
	// remove from it only when the column itself is gone.
	for _, known := range []column{
		{"applications", "owner_id"},
		{"application_versions", "author_id"},
		{"skills", "owner_id"},
		{"skills", "author_id"},
		{"skill_versions", "author_id"},
		{"elitea_tools", "owner_id"},
		{"elitea_tools", "author_id"},
		{"chat_conversation_folders", "owner_id"},
		{"chat_conversations", "author_id"},
		{"configuration", "author_id"},
	} {
		if _, found := declared[known]; !found {
			t.Errorf("the corpus parser no longer finds %s.%s, so it cannot see a new column either",
				known.table, known.name)
		}
	}

	var missing []string
	for c, source := range declared {
		if !documented[c] {
			missing = append(missing, c.table+"."+c.name+" (declared in "+source+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these tenant columns have no meaning recorded in migrations/tenant/0128_owner_id_column_meanings.sql:\n  %s\n"+
			"Read the writers, decide whether the column holds a project or a user, and add the row. "+
			"The name alone is not evidence — that is the defect #533 records.",
			strings.Join(missing, "\n  "))
	}
}

// commentedColumns reads the (table, column) pairs out of a COMMENT ON COLUMN
// table of meanings. It matches the VALUES list 0128 builds, so a file that
// states a meaning some other way does not count here — which is the point: one
// place holds the table.
var commentEntry = regexp.MustCompile(`\(\s*'([a-z_][a-z0-9_]*)'\s*,\s*'([a-z_][a-z0-9_]*)'\s*,`)

func commentedColumns(sql string) []column {
	if !strings.Contains(sql, "COMMENT ON COLUMN") {
		return nil
	}
	var found []column
	for _, match := range commentEntry.FindAllStringSubmatch(sql, -1) {
		if ownerColumnNames[match[2]] {
			found = append(found, column{table: match[1], name: match[2]})
		}
	}
	return found
}

var (
	// The bootstrap builds each tenant table through `format('CREATE TABLE IF
	// NOT EXISTS %I.<name> (…)', schema_name)`. The `%I.` is what separates a
	// tenant table from a shared one in that file.
	tenantCreate    = regexp.MustCompile(`CREATE TABLE IF NOT EXISTS %I\.([a-z_][a-z0-9_]*)\s*\(`)
	corpusCreate    = regexp.MustCompile(`CREATE TABLE IF NOT EXISTS ([a-z_][a-z0-9_]*)\s*\(`)
	corpusAddColumn = regexp.MustCompile(`ALTER TABLE ([a-z_][a-z0-9_]*)\s+ADD COLUMN(?: IF NOT EXISTS)?\s+([a-z_][a-z0-9_]*)`)
)

// tenantOwnerColumns returns every owner_id/author_id column the SQL declares
// on a tenant table.
//
// bootstrap selects the `%I.` form, which is how 001_initial.sql writes a
// tenant table. A ledgered tenant file names its tables unqualified, because
// the runner pins the transaction's search_path to the tenant schema.
func tenantOwnerColumns(sql string, bootstrap bool) []column {
	// Strip line comments first. Every file in this corpus carries a long
	// header, and a header that discusses `ALTER TABLE` in prose is not a DDL
	// statement. No string literal in this corpus contains `--`.
	sql = stripLineComments(sql)

	pattern := corpusCreate
	if bootstrap {
		pattern = tenantCreate
	}

	var found []column
	for _, location := range pattern.FindAllStringSubmatchIndex(sql, -1) {
		table := sql[location[2]:location[3]]
		body, ok := parenthesisedBody(sql, location[1]-1)
		if !ok {
			continue
		}
		for _, name := range firstTokenOfEachTopLevelPart(body) {
			if ownerColumnNames[name] {
				found = append(found, column{table: table, name: name})
			}
		}
	}
	for _, match := range corpusAddColumn.FindAllStringSubmatch(sql, -1) {
		if ownerColumnNames[match[2]] {
			found = append(found, column{table: match[1], name: match[2]})
		}
	}
	return found
}

func stripLineComments(sql string) string {
	lines := strings.Split(sql, "\n")
	for i, line := range lines {
		if at := strings.Index(line, "--"); at >= 0 {
			lines[i] = line[:at]
		}
	}
	return strings.Join(lines, "\n")
}

// parenthesisedBody returns the text between the parenthesis at open and the
// one that matches it.
func parenthesisedBody(sql string, open int) (string, bool) {
	if open < 0 || open >= len(sql) || sql[open] != '(' {
		return "", false
	}
	depth := 0
	for i := open; i < len(sql); i++ {
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return sql[open+1 : i], true
			}
		}
	}
	return "", false
}

// firstTokenOfEachTopLevelPart splits a column list on the commas that are not
// inside parentheses, and returns the first word of each part. A part that
// starts with CONSTRAINT declares no column, and its first word is simply not a
// column name.
func firstTokenOfEachTopLevelPart(body string) []string {
	var names []string
	depth := 0
	start := 0
	parts := []string{}
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, body[start:])
	for _, part := range parts {
		if token := strings.Fields(strings.TrimSpace(part)); len(token) > 0 {
			names = append(names, token[0])
		}
	}
	return names
}
