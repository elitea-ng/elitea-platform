package toolcategory

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The AI disclosure contract (ADR-0043 decision 3, #1500): the text
// parameters of the built-in tools that write human-readable text outside
// the platform (every external_send tool, and the write tools that create a
// pull request, an issue, a case, a ticket or a page). The worker appends the disclosure marker to
// them; Main reads the contract for the toolkit types an administrator may
// let projects opt out of. The shared copy is
// libs/proto/contracts/tool-disclosure-v1.json; a test holds this copy equal.

//go:embed tool-disclosure-v1.json
var disclosureJSON []byte

type disclosureContract struct {
	Contract          string `json:"contract"`
	DefaultMarkerText string `json:"default_marker_text"`
	Toolkits          map[string]map[string]struct {
		Format         string   `json:"format"`
		TextParameters []string `json:"text_parameters"`
	} `json:"toolkits"`
	Exempt map[string]map[string]string `json:"exempt"`
}

// DefaultDisclosureMarkerText is the marker text when no administrator set
// one.
const DefaultDisclosureMarkerText = "Generated with Elitea AI"

// disclosureTypes maps a canonical toolkit type to its contract name.
var disclosureTypes = mustLoadDisclosure(disclosureJSON)

func mustLoadDisclosure(raw []byte) map[string]string {
	types, err := loadDisclosure(raw)
	if err != nil {
		panic(err)
	}
	return types
}

func loadDisclosure(raw []byte) (map[string]string, error) {
	var document disclosureContract
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("toolcategory: the disclosure contract does not decode: %w", err)
	}
	if document.Contract != "elitea.tool-disclosure.v1" || document.DefaultMarkerText != DefaultDisclosureMarkerText {
		return nil, fmt.Errorf("toolcategory: the disclosure contract is %q", document.Contract)
	}
	out := make(map[string]string, len(document.Toolkits))
	for toolkit, tools := range document.Toolkits {
		key := CanonicalKey(toolkit)
		if key == "" || out[key] != "" || len(tools) == 0 {
			return nil, fmt.Errorf("toolcategory: disclosure toolkit %q is empty or listed twice", toolkit)
		}
		for tool, entry := range tools {
			if (entry.Format != "plain" && entry.Format != "markdown" && entry.Format != "html") || len(entry.TextParameters) == 0 {
				return nil, fmt.Errorf("toolcategory: disclosure tool %s.%s is malformed", toolkit, tool)
			}
		}
		out[key] = toolkit
	}
	return out, nil
}

// DisclosureToolkitTypes lists the toolkit types whose tools get the
// disclosure marker, sorted: the types an administrator can let projects
// opt out of.
func DisclosureToolkitTypes() []string {
	out := make([]string, 0, len(disclosureTypes))
	for _, name := range disclosureTypes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DisclosureToolkitType answers the contract name of a toolkit type whose
// tools get the marker, matched on the canonical key.
func DisclosureToolkitType(toolkitType string) (string, bool) {
	key := CanonicalKey(toolkitType)
	if mapped, ok := storedTypes[key]; ok {
		key = mapped
	}
	name, ok := disclosureTypes[strings.TrimSpace(key)]
	return name, ok
}
