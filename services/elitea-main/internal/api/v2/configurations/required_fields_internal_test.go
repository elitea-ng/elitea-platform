package configurations

// The required-field walk, against the PINNED registry snapshot rather than
// against a schema written here.
//
// A hand-written schema in a test proves the walk parses the schema the test
// wrote. What has to hold is that the walk agrees with the snapshot every
// deployment embeds — the same document /configurations/available serves — so
// each case below asks the catalog for the type's own schema and states the
// requirement in the vocabulary the product uses ("an Azure credential with no
// endpoint").

import (
	"encoding/json"
	"testing"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

func pinnedSchemaFor(t *testing.T, configType string) json.RawMessage {
	t.Helper()
	catalog, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatalf("load the pinned catalog: %v", err)
	}
	entry, ok := catalog.EntryByType(configType)
	if !ok {
		t.Fatalf("the pinned catalog carries no %q entry", configType)
	}
	return entry.ConfigSchema
}

func TestMissingRequiredConfigurationFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		configType string
		body       map[string]any
		want       []string
	}{
		{
			name:       "a complete Azure credential is accepted",
			configType: "azure_open_ai",
			body: map[string]any{
				"elitea_title": "autotest_azure",
				"label":        "autotest azure",
				"type":         "azure_open_ai",
				"data":         map[string]any{"api_base": "https://autotest.invalid", "api_key": "k"},
			},
		},
		{
			name:       "an Azure credential with no key is still complete",
			configType: "azure_open_ai",
			body: map[string]any{
				"elitea_title": "autotest_azure",
				"label":        "autotest azure",
				"type":         "azure_open_ai",
				"data":         map[string]any{"api_base": "https://autotest.invalid"},
			},
		},
		{
			name:       "an Azure credential with no endpoint names the endpoint",
			configType: "azure_open_ai",
			body: map[string]any{
				"elitea_title": "autotest_azure",
				"label":        "autotest azure",
				"type":         "azure_open_ai",
				"data":         map[string]any{"api_key": "k"},
			},
			want: []string{"data.api_base"},
		},
		{
			name:       "no label and no data names both",
			configType: "pgvector",
			body:       map[string]any{"elitea_title": "autotest_pgvector", "type": "pgvector"},
			want:       []string{"data", "label"},
		},
		{
			name:       "a vector store with no connection string is complete",
			configType: "pgvector",
			body: map[string]any{
				"elitea_title": "autotest_pgvector",
				"label":        "autotest pgvector",
				"type":         "pgvector",
				"data":         map[string]any{},
			},
		},
		{
			name:       "the legacy `name` alias satisfies elitea_title",
			configType: "pgvector",
			body: map[string]any{
				"name":  "autotest_pgvector",
				"label": "autotest pgvector",
				"type":  "pgvector",
				"data":  map[string]any{},
			},
		},
		{
			name:       "a Bedrock credential carrying only a region is complete",
			configType: "amazon_bedrock",
			body: map[string]any{
				"elitea_title": "autotest_bedrock",
				"label":        "autotest bedrock",
				"type":         "amazon_bedrock",
				"data":         map[string]any{"aws_region_name": "us-east-1"},
			},
		},
		{
			name:       "an embedding model with no model name names it",
			configType: "embedding_model",
			body: map[string]any{
				"elitea_title": "autotest_embedding",
				"label":        "autotest embedding",
				"type":         "embedding_model",
				"data": map[string]any{
					"ai_credentials": map[string]any{"elitea_title": "autotest_cred", "private": false},
				},
			},
			want: []string{"data.name"},
		},
		{
			name:       "an embedding model with no credential link names it",
			configType: "embedding_model",
			body: map[string]any{
				"elitea_title": "autotest_embedding",
				"label":        "autotest embedding",
				"type":         "embedding_model",
				"data":         map[string]any{"name": "autotest-embed"},
			},
			want: []string{"data.ai_credentials"},
		},
		{
			name:       "a credential link written as a bare title is malformed",
			configType: "embedding_model",
			body: map[string]any{
				"elitea_title": "autotest_embedding",
				"label":        "autotest embedding",
				"type":         "embedding_model",
				"data": map[string]any{
					"name":           "autotest-embed",
					"ai_credentials": "autotest_cred",
				},
			},
			want: []string{"data.ai_credentials"},
		},
		{
			// The referenced model declares `private` required and this
			// platform's own writers omit it (src/pages/admin/
			// PlatformModelDialog.tsx), so the walk stops at the KIND of a
			// `$ref` field. Refusing this shape would refuse a reference the
			// expander resolves everywhere else.
			name:       "a credential link with no resolution scope is accepted",
			configType: "embedding_model",
			body: map[string]any{
				"elitea_title": "autotest_embedding",
				"label":        "autotest embedding",
				"type":         "embedding_model",
				"data": map[string]any{
					"name":           "autotest-embed",
					"ai_credentials": map[string]any{"elitea_title": "autotest_cred"},
				},
			},
		},
		{
			// The schema declares `ai_credentials` as an object OR null, so an
			// explicit null is a stated intent and not an omission. Admission
			// refuses the row later, with a reason about the link.
			name:       "an explicitly null credential link is not a missing field",
			configType: "embedding_model",
			body: map[string]any{
				"elitea_title": "autotest_embedding",
				"label":        "autotest embedding",
				"type":         "embedding_model",
				"data":         map[string]any{"name": "autotest-embed", "ai_credentials": nil},
			},
		},
		{
			name:       "a GitHub credential with no label and no data names both",
			configType: "github",
			body:       map[string]any{"elitea_title": "autotest_github", "type": "github"},
			want:       []string{"data", "label"},
		},
		{
			name:       "a data field of the wrong kind is refused",
			configType: "github",
			body: map[string]any{
				"elitea_title": "autotest_github",
				"label":        "autotest github",
				"type":         "github",
				"data":         "https://autotest.invalid",
			},
			want: []string{"data"},
		},
		{
			// An empty label is a value. Pydantic accepts it, the column
			// stores it, and refusing it here would be a new rule rather than
			// the missing half of an existing one.
			name:       "an empty label is present",
			configType: "github",
			body: map[string]any{
				"elitea_title": "autotest_github",
				"label":        "",
				"type":         "github",
				"data":         map[string]any{"base_url": "https://autotest.invalid"},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := missingRequiredConfigurationFields(pinnedSchemaFor(t, testCase.configType), testCase.body, requiredFieldDepthData)
			if len(got) != len(testCase.want) {
				t.Fatalf("missing = %v, want %v", got, testCase.want)
			}
			for index, want := range testCase.want {
				if got[index] != want {
					t.Fatalf("missing = %v, want %v", got, testCase.want)
				}
			}
		})
	}
}

// TestTheRowDepthChecksTheRowAndNotTheProvider pins the compatibility shape:
// the model picker's row is `section: "models"` with a CREDENTIAL type and
// carries `{model, ai_credentials}` rather than that credential's own fields.
// It is a row, and the four row fields still apply; it is not an instance of
// the type, and the type's data contract does not.
func TestTheRowDepthChecksTheRowAndNotTheProvider(t *testing.T) {
	t.Parallel()

	pickerRow := map[string]any{
		"elitea_title": "autotest_model-picker",
		"label":        "autotest model",
		"type":         "open_ai",
		"section":      "models",
		"data": map[string]any{
			"model":          "autotest-model",
			"ai_credentials": map[string]any{"elitea_title": "autotest_cred", "private": false},
		},
	}
	schema := pinnedSchemaFor(t, "open_ai")
	if got := missingRequiredConfigurationFields(schema, pickerRow, requiredFieldDepthRow); len(got) != 0 {
		t.Errorf("the picker row was refused %v", got)
	}
	// The same body at the data depth is refused, which is what makes the
	// choice of depth above the load-bearing part rather than an accident.
	if got := missingRequiredConfigurationFields(schema, pickerRow, requiredFieldDepthData); len(got) != 1 ||
		got[0] != "data.api_base" {
		t.Errorf("at the data depth the picker row is missing %v, want [data.api_base]", got)
	}

	delete(pickerRow, "label")
	if got := missingRequiredConfigurationFields(schema, pickerRow, requiredFieldDepthRow); len(got) != 1 ||
		got[0] != "label" {
		t.Errorf("a picker row with no label is missing %v, want [label]", got)
	}
}

// TestMissingRequiredConfigurationFieldsRefusesNothingWithoutARule is the
// fail-open half. A schema this function cannot read must never turn into a
// refusal: the row would be rejected for a reason nobody could act on.
func TestMissingRequiredConfigurationFieldsRefusesNothingWithoutARule(t *testing.T) {
	t.Parallel()

	body := map[string]any{"type": "anything"}
	for _, schema := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("not json"), json.RawMessage("{}")} {
		if got := missingRequiredConfigurationFields(schema, body, requiredFieldDepthData); len(got) != 0 {
			t.Errorf("schema %q refused %v", string(schema), got)
		}
	}
	if got := missingRequiredConfigurationFields(pinnedSchemaFor(t, "github"), nil, requiredFieldDepthData); len(got) != 0 {
		t.Errorf("a nil body refused %v", got)
	}
}

// TestEveryPinnedTypeStatesTheSameFourTopLevelFields records what the walk is
// reading. If a future snapshot changes the top-level contract, this test says
// so in one place instead of every create refusal changing at once.
func TestEveryPinnedTypeStatesTheSameFourTopLevelFields(t *testing.T) {
	t.Parallel()

	catalog, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatalf("load the pinned catalog: %v", err)
	}
	empty := map[string]any{}
	want := []string{"data", "elitea_title", "label", "type"}
	for _, entry := range catalog.PinnedEntries() {
		got := missingRequiredConfigurationFields(entry.ConfigSchema, empty, requiredFieldDepthData)
		if len(got) != len(want) {
			t.Errorf("%s: empty body missing %v, want %v", entry.Type, got, want)
			continue
		}
		for index := range want {
			if got[index] != want[index] {
				t.Errorf("%s: empty body missing %v, want %v", entry.Type, got, want)
				break
			}
		}
	}
}
