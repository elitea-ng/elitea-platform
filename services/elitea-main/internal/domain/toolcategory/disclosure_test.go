package toolcategory

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The embedded copy is the shared contract, byte for byte.
func TestDisclosureContractCopyIsCurrent(t *testing.T) {
	t.Parallel()
	shared := repoFile(t, "libs", "proto", "contracts", "tool-disclosure-v1.json")
	if !bytes.Equal(shared, disclosureJSON) {
		t.Fatal("internal/domain/toolcategory/tool-disclosure-v1.json differs from " +
			"libs/proto/contracts/tool-disclosure-v1.json; copy the contract over")
	}
}

// Every listed tool is external_send, and its text parameters are
// arguments of the tool in the SDK catalogue the platform serves.
func TestDisclosureNamesExternalSendToolsAndTheirArguments(t *testing.T) {
	t.Parallel()
	var document disclosureContract
	if err := json.Unmarshal(disclosureJSON, &document); err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Entries []struct {
			Type        string `json:"type"`
			ArgsSchemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"args_schemas"`
		} `json:"entries"`
	}
	raw := repoFile(t, "services", "elitea-main", "internal", "runtimecomposition", "current_toolkit_schema_snapshot.json")
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	for toolkit, tools := range document.Toolkits {
		for tool, entry := range tools {
			if got := Of(toolkit, tool); got != ExternalSend {
				t.Errorf("%s.%s is %q, not external_send", toolkit, tool, got)
			}
			found := false
			for _, catalogue := range snapshot.Entries {
				schema, ok := catalogue.ArgsSchemas[tool]
				if CanonicalKey(catalogue.Type) != CanonicalKey(toolkit) || !ok {
					continue
				}
				found = true
				for _, path := range entry.TextParameters {
					top, _, _ := strings.Cut(path, "[].")
					if _, ok := schema.Properties[top]; !ok {
						t.Errorf("%s.%s has no argument %q", toolkit, tool, top)
					}
				}
			}
			if !found {
				t.Errorf("the catalogue does not serve %s.%s", toolkit, tool)
			}
		}
	}
}

func TestDisclosureToolkitTypes(t *testing.T) {
	t.Parallel()
	types := DisclosureToolkitTypes()
	for _, want := range []string{"jira", "github", "slack", "yagmail"} {
		if !slices.Contains(types, want) {
			t.Errorf("%q is not a disclosure toolkit type: %v", want, types)
		}
	}
	if name, ok := DisclosureToolkitType(" Jira "); !ok || name != "jira" {
		t.Errorf("Jira = %q, %t", name, ok)
	}
	if _, ok := DisclosureToolkitType("sql"); ok {
		t.Error("sql writes no outbound text")
	}
	for name, raw := range map[string]string{
		"not json":       `{`,
		"wrong contract": `{"contract":"x","default_marker_text":"Generated with Elitea AI","toolkits":{}}`,
		"bad format":     `{"contract":"elitea.tool-disclosure.v1","default_marker_text":"Generated with Elitea AI","toolkits":{"a":{"b":{"format":"html","text_parameters":["c"]}}}}`,
	} {
		if _, err := loadDisclosure([]byte(raw)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}
