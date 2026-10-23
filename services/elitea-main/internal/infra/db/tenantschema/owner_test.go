package tenantschema

import (
	"errors"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// A PROJECT-kind owner_id has one correct value per tenant schema: the project
// the schema is named after. OwnerID answers with that number, and it answers
// anything else with a refusal rather than with a substitute value.
func TestOwnerIDIsTheSchemasOwnProject(t *testing.T) {
	t.Parallel()

	for _, projectID := range []string{"1", "7", "4210"} {
		owner, err := OwnerID(projectID)
		if err != nil {
			t.Fatalf("OwnerID(%q): %v", projectID, err)
		}
		quoted, quoteErr := Quote(projectID)
		if quoteErr != nil {
			t.Fatalf("Quote(%q): %v", projectID, quoteErr)
		}
		// The owner and the schema must name the SAME project. A row whose
		// owner_id names a project other than the schema it lives in is the
		// defect #533 records.
		if want := `"` + Prefix + projectID + `"`; quoted != want {
			t.Fatalf("Quote(%q) = %s, want %s", projectID, quoted, want)
		}
		if owner <= 0 {
			t.Errorf("OwnerID(%q) = %d, want a positive project id", projectID, owner)
		}
	}
}

// Everything that is not a project id gets an error. A tenant schema is
// p_<project id>, so a value that cannot name a schema cannot own a row in one
// either.
func TestOwnerIDRefusesWhatIsNotAProjectID(t *testing.T) {
	t.Parallel()

	for _, projectID := range []string{
		"",
		"0",
		"-3",
		"07",
		"prompt_lib",
		"1; DROP SCHEMA p_1",
		`1".configuration, centry.project x --`,
		"99999999999999999999999",
	} {
		owner, err := OwnerID(projectID)
		if err == nil {
			t.Errorf("OwnerID(%q) = %d, want a refusal", projectID, owner)
			continue
		}
		if owner != 0 {
			t.Errorf("OwnerID(%q) refused and still returned %d; a refusal must carry no value", projectID, owner)
		}
		// The refusal has to reach the client as a 400 and disclose nothing
		// about the database.
		var apiErr *apierr.APIError
		if !errors.As(err, &apiErr) {
			t.Errorf("OwnerID(%q) returned %T, want an *apierr.APIError so the caller answers 400", projectID, err)
		}
	}
}
