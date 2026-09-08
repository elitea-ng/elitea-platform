package configurations

import (
	"encoding/json"
	"strings"
	"testing"
)

// The compat projection must serve the stored title under `elitea_title` as
// well as `name`.
//
// Every product reader resolves a credential by `elitea_title`: a toolkit
// stores `{"github_configuration": {"elitea_title": …}}`, the model and
// toolkit forms build their credential picker from `row.elitea_title`, and the
// credential editor seeds its stable lookup key from it. While this route
// served the column as `name` alone, all of them read an empty title — the
// picker offered nothing to link, and an edit-save re-derived the stable key
// from the display label.
func TestConfigurationJSONCarriesEliteaTitleAndName(t *testing.T) {
	payload, err := json.Marshal(Configuration{
		ID: 11, ProjectID: 7, Label: "My Prod Key", Name: "internal-key-v1",
		Type: "github", Section: "credentials",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, field := range []string{
		`"elitea_title":"internal-key-v1"`,
		`"name":"internal-key-v1"`,
		`"label":"My Prod Key"`,
		`"project_id":7`,
	} {
		if !strings.Contains(text, field) {
			t.Fatalf("payload %s does not contain %s", text, field)
		}
	}
}

// A list page carries the same projection for every member, and the marshaller
// must not recurse.
func TestConfigurationListItemsCarryEliteaTitle(t *testing.T) {
	payload, err := json.Marshal(ListResponse{
		Items: []Configuration{{ID: 1, Name: "first"}, {ID: 2, Name: "second"}},
		Total: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, field := range []string{`"elitea_title":"first"`, `"elitea_title":"second"`} {
		if !strings.Contains(text, field) {
			t.Fatalf("payload %s does not contain %s", text, field)
		}
	}
}
