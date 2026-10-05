package configurations

import "testing"

// The UI's create/update body carries the display name under `elitea_title`
// (features/credentials/api/configurations.ts). Reading `name` alone wrote
// every row's elitea_title as "", and that column is UNIQUE, so the second
// configuration in a project failed permanently with 500 (#131).
func TestFirstStrValPrefersEliteaTitleOverLegacyName(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "the body the UI actually sends",
			body: map[string]any{"elitea_title": "my creds", "label": "my creds"},
			want: "my creds",
		},
		{
			name: "elitea_title wins over a legacy name",
			body: map[string]any{"elitea_title": "chosen", "name": "legacy"},
			want: "chosen",
		},
		{
			name: "legacy name still accepted alone",
			body: map[string]any{"name": "legacy"},
			want: "legacy",
		},
		{
			name: "an empty elitea_title falls through rather than winning",
			body: map[string]any{"elitea_title": "", "name": "legacy"},
			want: "legacy",
		},
		{
			name: "neither key present",
			body: map[string]any{"label": "only a label"},
			want: "",
		},
		{
			name: "a non-string value is not coerced",
			body: map[string]any{"elitea_title": 7, "name": "legacy"},
			want: "legacy",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := firstStrVal(testCase.body, "elitea_title", "name"); got != testCase.want {
				t.Errorf("firstStrVal = %q, want %q", got, testCase.want)
			}
		})
	}
}

// The UI never sends `section`, so the column landed empty and the row
// belonged to none of the seven sections the AI-Configuration page queries
// (#131). The registry entry for the type is the authority.
func TestSectionForDerivesFromTheRegistryWhenTheBodyOmitsIt(t *testing.T) {
	handler := NewHandler(nil)
	if handler.catalog == nil {
		t.Fatal("NewHandler must load the pinned catalog; every derivation below depends on it")
	}

	cases := []struct {
		name       string
		configType string
		requested  string
		want       string
	}{
		{name: "credential type derives ai_credentials", configType: "open_ai", want: "ai_credentials"},
		{name: "azure credential type", configType: "azure_open_ai", want: "ai_credentials"},
		{name: "model type derives llm", configType: "llm_model", want: "llm"},
		{name: "toolkit type derives credentials", configType: "github", want: "credentials"},
		{name: "an explicit body value wins", configType: "open_ai", requested: "models", want: "models"},
		{name: "an unknown type stores nothing", configType: "not_a_registered_type", want: ""},
		{name: "an empty type stores nothing", configType: "", want: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := handler.sectionFor(testCase.configType, testCase.requested); got != testCase.want {
				t.Errorf("sectionFor(%q, %q) = %q, want %q",
					testCase.configType, testCase.requested, got, testCase.want)
			}
		})
	}
}

// POST returns both `id` and `uuid`; matching on `id` alone made every
// detail/update/delete call carrying the uuid 404 (#131).
func TestConfigurationIDColumnMatchesNumericIDsAndUUIDs(t *testing.T) {
	cases := []struct {
		configID string
		want     string
	}{
		{configID: "22", want: "id"},
		{configID: "0", want: "id"},
		{configID: "-3", want: "id"},
		{configID: "4df27bfc-d74f-4b29-a334-ae8341aaf895", want: "uuid::text"},
		{configID: "", want: "uuid::text"},
		{configID: "22abc", want: "uuid::text"},
		// Outside the 32-bit id column: no row has it, so the route answers
		// 404 instead of a PostgreSQL range error (500).
		{configID: "2147483647", want: "id"},
		{configID: "2147483648", want: "uuid::text"},
		{configID: "99999999999", want: "uuid::text"},
	}
	for _, testCase := range cases {
		t.Run(testCase.configID, func(t *testing.T) {
			if got := configurationIDColumn(testCase.configID); got != testCase.want {
				t.Errorf("configurationIDColumn(%q) = %q, want %q", testCase.configID, got, testCase.want)
			}
		})
	}
}

// COALESCE($n, column) can only preserve a stored value if the parameter is
// actually NULL; strVal's "" overwrote the column on every update.
func TestNullableStrValPreservesOmittedColumns(t *testing.T) {
	if got := nullableStrVal(""); got != nil {
		t.Errorf("nullableStrVal(\"\") = %v, want nil so COALESCE keeps the stored value", got)
	}
	if got := nullableStrVal("llm"); got != "llm" {
		t.Errorf("nullableStrVal(%q) = %v, want the value itself", "llm", got)
	}
}

// The client fires one list request per section; the handler ignored the
// parameter and returned the whole table each time, so one credential
// rendered under all seven headings (#131).
func TestConfigurationSectionFilterBindsOnlyWhenSectionsAreRequested(t *testing.T) {
	clause, args := configurationSectionFilter(nil, 1)
	if clause != "" || args != nil {
		t.Errorf("no sections must mean no filter; got clause %q args %v", clause, args)
	}

	clause, args = configurationSectionFilter([]string{"ai_credentials"}, 1)
	if clause != " AND section = ANY($1)" {
		t.Errorf("clause = %q", clause)
	}
	if len(args) != 1 {
		t.Fatalf("expected one bound argument, got %d", len(args))
	}
	sections, ok := args[0].([]string)
	if !ok || len(sections) != 1 || sections[0] != "ai_credentials" {
		t.Errorf("bound argument = %v, want []string{\"ai_credentials\"}", args[0])
	}

	// The surrounding list query already binds limit/offset as $1/$2.
	clause, _ = configurationSectionFilter([]string{"llm", "tts"}, 3)
	if clause != " AND section = ANY($3)" {
		t.Errorf("clause at placeholder 3 = %q", clause)
	}
	_, args = configurationSectionFilter([]string{"llm", "tts"}, 3)
	if sections, ok := args[0].([]string); !ok || len(sections) != 2 {
		t.Errorf("repeated ?section= values must all bind; got %v", args[0])
	}
}
