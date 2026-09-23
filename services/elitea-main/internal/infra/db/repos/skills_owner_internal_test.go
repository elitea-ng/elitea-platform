package repos

// The integration tests beside this file need a PostgreSQL service. These run
// everywhere and catch the #533 regression at the statement level.

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// skills.owner_id is the OWNING PROJECT. The statement bound it to the literal
// 1, so every skill created through this service claimed project 1 whatever
// schema it landed in. Project 1 is the seeded Default Project, so the value
// broke no constraint and named the wrong project in silence.
//
// owner_id must therefore reach the statement as a PLACEHOLDER the caller
// fills, and it must not share that placeholder with author_id: the two columns
// hold different kinds of number.
func TestCreateSkillSQLBindsTheProjectToOwnerID(t *testing.T) {
	t.Parallel()

	// createSkillSQL takes an ALREADY-QUOTED identifier, so the test builds it
	// the way the repository does rather than restating the quoting.
	schema, err := tenantschema.Quote("7")
	if err != nil {
		t.Fatalf("Quote(7) failed: %v", err)
	}
	statement := createSkillSQL(schema)

	columns := regexp.MustCompile(`INSERT INTO "p_7"\.skills \(([^)]*)\)`).FindStringSubmatch(statement)
	if columns == nil {
		t.Fatalf("createSkillSQL produced an unrecognisable INSERT:\n%s", statement)
	}
	named := strings.Split(strings.ReplaceAll(columns[1], " ", ""), ",")

	values := regexp.MustCompile(`VALUES \((.*)\)`).FindStringSubmatch(statement)
	if values == nil {
		t.Fatalf("createSkillSQL has no VALUES list:\n%s", statement)
	}
	bound := splitTopLevel(values[1])
	if len(bound) != len(named) {
		t.Fatalf("%d columns but %d values: %v vs %v", len(named), len(bound), named, bound)
	}

	placeholderOf := map[string]string{}
	for position, name := range named {
		placeholderOf[name] = strings.TrimSpace(bound[position])
	}
	ownerValue, hasOwner := placeholderOf["owner_id"]
	if !hasOwner {
		t.Fatalf("owner_id is not among the inserted columns %v", named)
	}
	if !strings.HasPrefix(ownerValue, "$") {
		t.Errorf("owner_id is bound to %q rather than to a placeholder, so it cannot carry the project; "+
			"a literal here gives every skill in every schema the same owner", ownerValue)
	}
	if authorValue, hasAuthor := placeholderOf["author_id"]; hasAuthor && authorValue == ownerValue {
		t.Errorf("owner_id and author_id share the value %s; one holds a project and the other holds a user",
			ownerValue)
	}
}

// A project id that names no tenant schema cannot own a row in one either. The
// refusal has to happen before the statement is built, so the test runs against
// a repository with NO pool: reaching the database at all would panic here, and
// that is what makes the assertion real.
func TestCreateSkillRefusesAProjectIDThatNamesNoSchema(t *testing.T) {
	t.Parallel()

	repo := &SkillsRepo{}
	for _, projectID := range []string{"", "0", "prompt_lib", "1; DROP SCHEMA p_1"} {
		if _, err := repo.Create(context.Background(), projectID, skills.Skill{Name: "n"}); err == nil {
			t.Errorf("Create(%q) succeeded, want a refusal", projectID)
		}
	}
}

// splitTopLevel splits a VALUES list on the commas that are not inside
// parentheses, so `gen_random_uuid()` stays one value.
func splitTopLevel(list string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, list[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, list[start:])
}

func TestCreateSkillRefusesMissingAuthorBeforeDatabaseAccess(t *testing.T) {
	repo := &SkillsRepo{}
	if _, err := repo.Create(context.Background(), "1", skills.Skill{Name: "missing-author"}); err == nil {
		t.Fatal("skill creation accepted a missing author")
	}
	if _, err := repo.CreateVersion(context.Background(), "1", "7", skills.VersionCreateInput{Name: "missing-author"}); err == nil {
		t.Fatal("version creation accepted a missing author")
	}
}
