package eliteacore

import (
	"reflect"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/ownership"
)

// Issue #533, the writer half for `prompt_collections`.
//
// The statement bound `owner_id` and `author_id` to the SAME placeholder, so
// the caller's user id reached a column that holds a project. The test reads
// the statement text, because the table is not in this repository's migration
// corpus: no migration creates it, so no integration test can insert a row.
func TestCreateCollectionSQLGivesOwnerAndAuthorTheirOwnPlaceholders(t *testing.T) {
	t.Parallel()

	statement := createCollectionInsertSQL("p_7")

	if !strings.Contains(statement, "INSERT INTO p_7.prompt_collections") {
		t.Fatalf("statement addresses the wrong table:\n%s", statement)
	}
	if !strings.Contains(statement, "(name, description, owner_id, author_id, status, meta)") {
		t.Fatalf("statement names other columns than the writer sets:\n%s", statement)
	}
	if strings.Contains(statement, "$3, $3") {
		t.Fatalf("owner_id and author_id share one placeholder, so a user id reaches the project column:\n%s", statement)
	}
	if !strings.Contains(statement, "VALUES ($1, $2, $3, $4,") {
		t.Fatalf("owner_id must take $3 and author_id must take $4:\n%s", statement)
	}
}

// The two ids that the import and the fork carry are different TYPES, so a call
// that swaps them does not compile. This reads the signature, because a
// compile-time guarantee leaves nothing to assert at run time.
func TestImportSkillTakesAProjectAndAUserAndNotTwoInts(t *testing.T) {
	t.Parallel()

	handler := &Handler{}
	signature := reflect.TypeOf(handler.importSkill)

	// The method value drops the receiver, so the parameters are
	// (context, schema, ownerID, authorID).
	ownerParameter := signature.In(2)
	authorParameter := signature.In(3)

	if ownerParameter != reflect.TypeOf(ownership.ProjectID(0)) {
		t.Errorf("importSkill takes %s as the owner, want ownership.ProjectID", ownerParameter)
	}
	if authorParameter != reflect.TypeOf(ownership.UserID(0)) {
		t.Errorf("importSkill takes %s as the author, want ownership.UserID", authorParameter)
	}
	if ownerParameter == authorParameter {
		t.Error("the owner and the author have one type, so a swapped call still compiles")
	}
}

// tenantOwnerID answers with a ProjectID. A path segment that is not a project
// id gets an error and never a substitute value.
func TestTenantOwnerIDAnswersATypedProjectID(t *testing.T) {
	t.Parallel()

	owner, err := tenantOwnerID("42")
	if err != nil {
		t.Fatalf("tenantOwnerID(\"42\"): %v", err)
	}
	if owner != ownership.ProjectID(42) {
		t.Errorf("tenantOwnerID(\"42\") = %d, want 42", owner.Int64())
	}
	if reflect.TypeOf(owner) == reflect.TypeOf(ownership.UserID(0)) {
		t.Error("tenantOwnerID answers a UserID, so a project id can reach an author column")
	}
}
