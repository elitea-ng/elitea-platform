package configurations

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

func TestCurrentConfigurationDTOUsesExactCurrentNamesAndNulls(t *testing.T) {
	createdAt := time.Date(2026, time.July, 22, 10, 0, 0, 0, time.UTC)
	dto := newCurrentConfigurationDTO(configurationapp.CurrentConfiguration{
		ID: 9, UUID: "00000000-0000-4000-8000-000000000009", ProjectID: 7,
		EliteaTitle: "elitea-pgvector", Type: "pgvector", Section: "vectorstorage",
		CreatedAt: createdAt, Source: "system",
	})
	payload, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, field := range []string{
		`"id":9`, `"uuid":"00000000-0000-4000-8000-000000000009"`,
		`"project_id":7`, `"elitea_title":"elitea-pgvector"`, `"label":null`,
		`"data":{}`, `"meta":{}`, `"status_logs":null`, `"author_id":null`,
		`"created_at":"2026-07-22T10:00:00"`, `"updated_at":null`,
		`"is_pinned":false`,
	} {
		if !strings.Contains(text, field) {
			t.Fatalf("payload %s does not contain %s", text, field)
		}
	}
	if strings.Contains(text, `"name"`) || strings.Contains(text, `"options"`) {
		t.Fatalf("payload contains prototype or absent optional fields: %s", text)
	}
}

func TestCurrentConfigurationDTOUsesNaivePythonMicrosecondTimestampContract(t *testing.T) {
	createdAt := time.Date(2026, time.July, 22, 10, 0, 0, 123000000, time.UTC)
	updatedAt := time.Date(2026, time.July, 22, 10, 1, 2, 4000, time.UTC)
	payload, err := json.Marshal(newCurrentConfigurationDTO(configurationapp.CurrentConfiguration{
		CreatedAt: createdAt,
		UpdatedAt: &updatedAt,
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, expected := range []string{
		`"created_at":"2026-07-22T10:00:00.123000"`,
		`"updated_at":"2026-07-22T10:01:02.000004"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("payload %s does not contain %s", text, expected)
		}
	}
	if strings.Contains(text, "Z") || strings.Contains(text, "+00:00") {
		t.Fatalf("timestamp gained a synthetic timezone: %s", text)
	}
}

// The LIST route and the SINGLE-ROW route must describe `project_id` the same
// way.
//
// They did not. The reviewed read route serves CurrentConfigurationDTO, whose
// ProjectID is an int32, so it answered `"project_id": 2`. The compatibility
// handler serves the Configuration struct, whose ProjectID was a `string`, so
// it answered `"project_id": "2"` for the same column and the same row.
//
// One client reads both. apps/elitea-web's AI-Configuration screen lists
// configurations through one route and opens a row through the other, and its
// `isConfigurationEditable` compared the row's id with the selected project id
// using `===`. A number never equals a string, so every card on that screen
// reported "No edit permissions" and no configuration could be edited.
//
// The column is an INTEGER. Both projections now emit a JSON number. This test
// marshals both and inspects the decoded type, so a change to either struct
// that reintroduces the disagreement fails here rather than in a browser.
func TestConfigurationProjectIDAgreesBetweenListAndSingleRow(t *testing.T) {
	const projectID = 2

	reviewed, err := json.Marshal(newCurrentConfigurationDTO(configurationapp.CurrentConfiguration{
		ID: 9, ProjectID: projectID, EliteaTitle: "vllm_creds", Type: "vllm", Section: "ai_credentials",
	}))
	if err != nil {
		t.Fatal(err)
	}
	compatibility, err := json.Marshal(Configuration{
		ID: 9, ProjectID: projectID, Name: "vllm_creds", Type: "vllm", Section: "ai_credentials",
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, payload := range map[string][]byte{"reviewed": reviewed, "compatibility": compatibility} {
		var decoded map[string]any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		value, present := decoded["project_id"]
		if !present {
			t.Fatalf("%s payload has no project_id: %s", name, payload)
		}
		// A JSON number decodes into float64 here. A JSON string does not,
		// which is exactly the difference that broke the client.
		number, ok := value.(float64)
		if !ok {
			t.Fatalf("%s project_id is %T (%v), want a JSON number", name, value, value)
		}
		if int(number) != projectID {
			t.Fatalf("%s project_id=%v, want %d", name, value, projectID)
		}
	}
}

func TestCurrentConfigurationDTOCanRepresentPresentEmptyOptions(t *testing.T) {
	options := map[string]any{}
	payload, err := json.Marshal(newCurrentConfigurationDTO(configurationapp.CurrentConfiguration{
		Data: map[string]any{}, Meta: map[string]any{}, Options: &options,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"options":{}`) {
		t.Fatalf("present empty options were omitted: %s", payload)
	}
}

func TestCurrentConfigurationListDTOOmitsUnrequestedSharedAndUsesArrays(t *testing.T) {
	withoutShared, err := json.Marshal(newCurrentConfigurationListDTO(configurationapp.CurrentConfigurationListResult{
		CurrentConfigurationPage: configurationapp.CurrentConfigurationPage{Limit: 20},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(withoutShared), `"shared"`) || !strings.Contains(string(withoutShared), `"items":[]`) {
		t.Fatalf("unrequested shared or null items in %s", withoutShared)
	}

	withShared, err := json.Marshal(newCurrentConfigurationListDTO(configurationapp.CurrentConfigurationListResult{
		CurrentConfigurationPage: configurationapp.CurrentConfigurationPage{Limit: 20},
		Shared:                   &configurationapp.CurrentConfigurationPage{Limit: 10, Offset: 3},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withShared), `"shared":{"total":0,"items":[],"offset":3,"limit":10}`) {
		t.Fatalf("shared page shape changed: %s", withShared)
	}
}
