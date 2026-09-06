package configurations

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
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

// Every configurations DTO must describe `project_id` as an integer.
//
// The list/single-row disagreement above was not the only copy of it. The
// compatibility model list (`Model`) kept a `string` while its
// ELITEA_CONFIGURATIONS_ENABLED twin (`CurrentModelCatalogItem`) served an
// int32. The OpenAPI contract declared `type: string` for the revalidate
// response, so the generated web client parsed a correct integer answer with
// `zod.string()` and threw.
//
// The two tests below close the class, not one instance:
//
//   - the round trip proves the WIRE value of each named DTO is a JSON number;
//   - the source scan proves that NO struct in either configurations package
//     declares a `json:"project_id"` field with a string type, so a DTO added
//     later cannot reintroduce the disagreement without failing here.
func TestConfigurationsDTOsMarshalProjectIDAsANumber(t *testing.T) {
	const projectID = 4

	dtos := map[string]any{
		"api.Configuration":                         Configuration{ProjectID: projectID},
		"api.Model":                                 Model{ProjectID: projectID},
		"api.CurrentConfigurationDTO":               CurrentConfigurationDTO{ProjectID: projectID},
		"app.CurrentModelCatalogItem":               configurationapp.CurrentModelCatalogItem{ProjectID: projectID},
		"app.CurrentConfigurationOption":            configurationapp.CurrentConfigurationOption{ProjectID: projectID},
		"app.CurrentConfigurationLifecycleSnapshot": configurationapp.CurrentConfigurationLifecycleSnapshot{ProjectID: projectID},
	}

	for name, dto := range dtos {
		payload, err := json.Marshal(dto)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
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

func TestNoConfigurationsStructDeclaresAStringProjectID(t *testing.T) {
	directories := []string{".", "../../../application/configurations"}

	checked := 0
	for _, directory := range directories {
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseDir(fileSet, directory, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", directory, err)
		}
		if len(parsed) == 0 {
			// A checker that finds no subject must fail, not pass (#426).
			t.Fatalf("parsed no package in %s, so this test measured nothing", directory)
		}
		for _, parsedPackage := range parsed {
			ast.Inspect(parsedPackage, func(node ast.Node) bool {
				field, ok := node.(*ast.Field)
				if !ok || field.Tag == nil {
					return true
				}
				tag, err := strconv.Unquote(field.Tag.Value)
				if err != nil {
					return true
				}
				name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
				if name != "project_id" {
					return true
				}
				checked++
				identifier, isIdentifier := field.Type.(*ast.Ident)
				if !isIdentifier {
					t.Errorf("%s: project_id has the composite type %T, want an integer type",
						fileSet.Position(field.Pos()), field.Type)
					return true
				}
				switch identifier.Name {
				case "int", "int8", "int16", "int32", "int64":
				default:
					t.Errorf("%s: project_id is declared %s, want an integer type. "+
						"Both configurations read routes and the OpenAPI contract emit a JSON "+
						"number, and a client that compares the two with === breaks on a string.",
						fileSet.Position(field.Pos()), identifier.Name)
				}
				return true
			})
		}
	}

	if checked == 0 {
		t.Fatal(`found no json:"project_id" struct field, so this test measured nothing`)
	}
	t.Logf(`checked %d json:"project_id" struct fields`, checked)
}
