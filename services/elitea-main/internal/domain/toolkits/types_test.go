package toolkits_test

// The toolkits domain package, which had no test file at all.
//
// READ THIS BEFORE ADDING TO IT. This package is not on any live path: no file
// outside internal/domain/toolkits imports it, in this service or anywhere else
// in the monorepo (`grep -r "domain/toolkits"` finds only its own three files).
// `Service` is an empty struct with a TODO, and the types below are DTOs whose
// only behaviour is their JSON encoding.
//
// So these tests pin the one thing that can break silently and be believed: the
// wire shape. Every field name in a `json:` tag here is a name some client would
// read, and a struct field renamed during a refactor changes the encoded name
// without changing any Go call site — the compiler stays quiet and the tag is
// the only record of the contract. `omitempty` is asserted for the same reason:
// dropping it turns an absent optional into an explicit zero, which several
// clients treat differently from absence.
//
// What these tests DO NOT do is give the package meaning it does not have. If
// this package is still unimported when somebody next reads it, the right change
// is to delete it and this file with it, not to grow either.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/toolkits"
)

func TestNewServiceIsConstructible(t *testing.T) {
	t.Parallel()

	if toolkits.New() == nil {
		t.Fatal("New() returned nil")
	}
}

// TestDomainTypesEncodeTheNamesTheirTagsDeclare walks every DTO this package
// publishes and compares the encoded object with the exact key set expected.
//
// The table is written as "the fields a fully-populated value encodes to" so a
// field ADDED without a tag (which encodes under its Go name) fails here too,
// not only a field renamed.
func TestDomainTypesEncodeTheNamesTheirTagsDeclare(t *testing.T) {
	t.Parallel()

	moment := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	for name, testCase := range map[string]struct {
		value any
		want  []string
	}{
		"Toolkit": {
			value: toolkits.Toolkit{
				ID: "1", ProjectID: "2", Name: "n", Type: "github", Description: "d",
				Config: map[string]any{"k": "v"}, Status: "ok",
				CreatedAt: moment, UpdatedAt: moment,
			},
			want: []string{
				"id", "project_id", "name", "type", "description", "config",
				"status", "created_at", "updated_at",
			},
		},
		"Tool": {
			value: toolkits.Tool{
				ID: "1", ToolkitID: "2", Name: "n", Description: "d",
				Schema: map[string]any{"type": "object"}, Enabled: true,
			},
			want: []string{"id", "toolkit_id", "name", "description", "schema", "enabled"},
		},
		"ToolkitType": {
			value: toolkits.ToolkitType{Type: "github", Name: "GitHub", Description: "d", Category: "c"},
			want:  []string{"type", "name", "description", "category"},
		},
		"ListResponse": {
			value: toolkits.ListResponse{
				Items: []toolkits.Toolkit{}, Total: 1, Page: 1, PageSize: 20, TotalPages: 1,
			},
			want: []string{"items", "total", "page", "page_size", "total_pages"},
		},
		"DiscoverResponse": {
			value: toolkits.DiscoverResponse{Tools: []toolkits.Tool{}},
			want:  []string{"tools"},
		},
		"CallToolResponse": {
			value: toolkits.CallToolResponse{
				Output: "o", Error: "e", Meta: map[string]any{"k": "v"},
			},
			want: []string{"output", "error", "meta"},
		},
		"TestToolRequest": {
			value: toolkits.TestToolRequest{
				ProjectID: "1", ToolkitID: "2", ToolID: "3", ToolName: "n",
				ToolParams: map[string]any{"k": "v"}, UserID: "4",
			},
			want: []string{
				"project_id", "toolkit_id", "tool_id", "tool_name", "tool_params", "user_id",
			},
		},
		"TestToolResponse": {
			value: toolkits.TestToolResponse{OK: true, Result: "r", Error: "e"},
			want:  []string{"ok", "result", "error"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertEncodesExactly(t, testCase.value, testCase.want)
		})
	}
}

// TestTheRequestTypesHideTheFieldsTheRouterOwns pins the `json:"-"` tags.
//
// ProjectID, ToolkitID and ToolkitType are taken from the URL, never from the
// body. A tag change that let one of them be decoded from the body would make
// the path parameter overridable by the caller — an authorisation boundary, not
// a serialisation detail — and nothing else in this repository would notice.
func TestTheRequestTypesHideTheFieldsTheRouterOwns(t *testing.T) {
	t.Parallel()

	name := "renamed"
	for label, testCase := range map[string]struct {
		value  any
		hidden []string
	}{
		"ListRequest": {
			value:  toolkits.ListRequest{ProjectID: "1", Page: 1, PageSize: 20},
			hidden: []string{"project_id", "ProjectID"},
		},
		"CreateRequest": {
			value: toolkits.CreateRequest{
				ProjectID: "1", Name: "n", Type: "github", Description: "d",
			},
			hidden: []string{"project_id", "ProjectID"},
		},
		"UpdateRequest": {
			value:  toolkits.UpdateRequest{ProjectID: "1", ToolkitID: "2", Name: &name},
			hidden: []string{"project_id", "ProjectID", "toolkit_id", "ToolkitID"},
		},
		"DiscoverRequest": {
			value:  toolkits.DiscoverRequest{ProjectID: "1", ToolkitType: "github"},
			hidden: []string{"project_id", "ProjectID", "toolkit_type", "ToolkitType"},
		},
		"CallToolRequest": {
			value:  toolkits.CallToolRequest{ProjectID: "1", ToolkitID: "2", ToolName: "n"},
			hidden: []string{"project_id", "ProjectID"},
		},
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			encoded := encodeToMap(t, testCase.value)
			for _, key := range testCase.hidden {
				if _, present := encoded[key]; present {
					t.Errorf("%s encodes %q; the router owns that value and the body must not carry it",
						label, key)
				}
			}
		})
	}
}

// TestOptionalDomainFieldsAreOmittedWhenEmpty asserts the `omitempty` half.
func TestOptionalDomainFieldsAreOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		value    any
		required []string
		omitted  []string
	}{
		"an empty Toolkit keeps its identity fields and drops its optional ones": {
			value:    toolkits.Toolkit{},
			required: []string{"id", "project_id", "name", "type", "status", "created_at", "updated_at"},
			omitted:  []string{"description", "config"},
		},
		"an empty Tool keeps id, name and enabled": {
			value:    toolkits.Tool{},
			required: []string{"id", "toolkit_id", "name", "enabled"},
			omitted:  []string{"description", "schema"},
		},
		"a successful TestToolResponse carries no error key": {
			value:    toolkits.TestToolResponse{OK: true},
			required: []string{"ok"},
			omitted:  []string{"error", "result"},
		},
		"an UpdateRequest sends only the fields it changes": {
			value:    toolkits.UpdateRequest{},
			required: nil,
			omitted:  []string{"name", "description", "config"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			encoded := encodeToMap(t, testCase.value)
			for _, key := range testCase.required {
				if _, present := encoded[key]; !present {
					t.Errorf("the empty value dropped %q, which carries no omitempty", key)
				}
			}
			for _, key := range testCase.omitted {
				if _, present := encoded[key]; present {
					t.Errorf("the empty value encodes %q; it is declared omitempty", key)
				}
			}
		})
	}
}

// TestToolkitRoundTripsThroughJSON proves the tags decode as well as encode.
// A tag readable in one direction only is the shape a hand-written tag typo
// takes when the encoder is the only thing exercised.
func TestToolkitRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()

	moment := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	sent := toolkits.Toolkit{
		ID: "7", ProjectID: "1", Name: "fixture", Type: "github",
		Description: "round trip", Config: map[string]any{"repository": "o/r"},
		Status: "active", CreatedAt: moment, UpdatedAt: moment,
	}
	encoded, err := json.Marshal(sent)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var received toolkits.Toolkit
	if err := json.Unmarshal(encoded, &received); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if received.ID != sent.ID || received.ProjectID != sent.ProjectID ||
		received.Name != sent.Name || received.Type != sent.Type ||
		received.Description != sent.Description || received.Status != sent.Status ||
		!received.CreatedAt.Equal(sent.CreatedAt) || !received.UpdatedAt.Equal(sent.UpdatedAt) {
		t.Errorf("round trip changed the value:\n sent %#v\n got  %#v", sent, received)
	}
	if repository, _ := received.Config["repository"].(string); repository != "o/r" {
		t.Errorf("config did not survive: %#v", received.Config)
	}
}

func encodeToMap(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %T: %v", value, err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode %T: %v (%s)", value, err, encoded)
	}
	return decoded
}

func assertEncodesExactly(t *testing.T, value any, want []string) {
	t.Helper()
	encoded := encodeToMap(t, value)
	wanted := make(map[string]struct{}, len(want))
	for _, key := range want {
		wanted[key] = struct{}{}
		if _, present := encoded[key]; !present {
			t.Errorf("%T does not encode %q", value, key)
		}
	}
	for key := range encoded {
		if _, expected := wanted[key]; !expected {
			t.Errorf("%T encodes an undeclared field %q; add it to this table"+
				" once its wire name is deliberate", value, key)
		}
	}
}
