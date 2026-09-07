package providerhub_test

// The descriptor reader.
//
// The one rule worth a suite of its own is the dual spelling. The published
// JSON Schema names `_description`, `_type` and `_required`; every descriptor
// on the wire spells them without the underscore, because pylon generates its
// model with `remove_special_field_name_prefix=True`. A reader that honoured
// only one spelling would silently drop a parameter's TYPE, and the create form
// would render every field of such a provider as a plain object.

import (
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhub"
)

func TestParseDescriptorReadsTheWireSpelling(t *testing.T) {
	descriptor, err := providerhub.ParseDescriptor([]byte(`{
	  "name": "inventory",
	  "service_location_url": "https://inventory.example",
	  "provided_toolkits": [{
	    "name": "Inventory",
	    "description": "a graph",
	    "toolkit_config": {
	      "type": "Config",
	      "parameters": {"bucket": {"type": "String", "required": true, "description": "the bucket"}}
	    },
	    "provided_tools": [{"name": "search", "description": "find", "args_schema": {}}],
	    "toolkit_metadata": {"type_override": "inventory"}
	  }]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if descriptor.Name != "inventory" {
		t.Fatalf("name = %q", descriptor.Name)
	}
	toolkit := descriptor.ProvidedToolkits[0]
	if toolkit.Describe() != "a graph" {
		t.Errorf("toolkit description = %q", toolkit.Describe())
	}
	if toolkit.TypeOverride() != "inventory" {
		t.Errorf("type override = %q", toolkit.TypeOverride())
	}
	parameter := toolkit.ToolkitConfig.Parameters["bucket"]
	if parameter.DeclaredType() != "String" || !parameter.IsRequired() || parameter.Describe() != "the bucket" {
		t.Errorf("parameter read back wrong: %+v", parameter)
	}
	if toolkit.ProvidedTools[0].Describe() != "find" {
		t.Errorf("tool description = %q", toolkit.ProvidedTools[0].Describe())
	}
}

func TestParseDescriptorReadsTheSchemasOwnUnderscoreSpelling(t *testing.T) {
	descriptor, err := providerhub.ParseDescriptor([]byte(`{
	  "name": "literal",
	  "provided_toolkits": [{
	    "name": "Literal",
	    "_description": "declared the schema's way",
	    "toolkit_config": {
	      "_type": "Config",
	      "parameters": {"token": {"_type": "Secret", "_required": true, "_description": "a secret"}}
	    },
	    "provided_tools": [{"name": "call", "_description": "invoke", "args_schema": {}}]
	  }]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	toolkit := descriptor.ProvidedToolkits[0]
	if toolkit.Describe() != "declared the schema's way" {
		t.Errorf("toolkit description = %q", toolkit.Describe())
	}
	parameter := toolkit.ToolkitConfig.Parameters["token"]
	if parameter.DeclaredType() != "Secret" {
		t.Errorf("the underscore type was dropped: %+v", parameter)
	}
	if !parameter.IsRequired() {
		t.Error("the underscore required flag was dropped")
	}
	if parameter.Describe() != "a secret" {
		t.Errorf("the underscore description was dropped: %q", parameter.Describe())
	}
	if toolkit.ProvidedTools[0].Describe() != "invoke" {
		t.Errorf("the tool's underscore description was dropped")
	}
}

func TestParseDescriptorPrefersTheWireSpellingWhenBothArePresent(t *testing.T) {
	descriptor, err := providerhub.ParseDescriptor([]byte(`{
	  "name": "both",
	  "provided_toolkits": [{
	    "name": "Both", "description": "wire", "_description": "schema",
	    "toolkit_config": {"parameters": {}}, "provided_tools": []
	  }]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := descriptor.ProvidedToolkits[0].Describe(); got != "wire" {
		t.Fatalf("description = %q, want the wire spelling", got)
	}
}

func TestParseDescriptorRefusesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name     string
		manifest []byte
		wantIn   string
	}{
		{"empty", nil, "empty"},
		{"not json", []byte("{not json"), "parse"},
		{"no name", []byte(`{"provided_toolkits": []}`), "no name"},
		{"blank name", []byte(`{"name": "   "}`), "no name"},
		{"over the limit", append([]byte(`{"name":"x","pad":"`), append(make([]byte, 1<<20), '"', '}')...), "limit"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := providerhub.ParseDescriptor(testCase.manifest)
			if err == nil {
				t.Fatal("want an error, got a descriptor")
			}
			if !strings.Contains(err.Error(), testCase.wantIn) {
				t.Fatalf("error %q does not name the cause %q", err, testCase.wantIn)
			}
		})
	}
}

func TestTypeOverrideIsAbsentWhenTheMetadataDoesNotNameOne(t *testing.T) {
	descriptor, err := providerhub.ParseDescriptor([]byte(`{
	  "name": "plain",
	  "provided_toolkits": [
	    {"name": "A", "toolkit_config": {"parameters": {}}, "provided_tools": []},
	    {"name": "B", "toolkit_metadata": {"application": true},
	     "toolkit_config": {"parameters": {}}, "provided_tools": []},
	    {"name": "C", "toolkit_metadata": {"type_override": "  "},
	     "toolkit_config": {"parameters": {}}, "provided_tools": []}
	  ]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, toolkit := range descriptor.ProvidedToolkits {
		if got := toolkit.TypeOverride(); got != "" {
			t.Errorf("toolkit %q reported an override %q", toolkit.Name, got)
		}
	}
}
