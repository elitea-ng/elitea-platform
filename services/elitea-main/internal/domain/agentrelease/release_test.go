package agentrelease

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	object, err := DecodeObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func encode(t *testing.T, value any) string {
	t.Helper()
	raw, err := encodeCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// ProjectSettings is the SQL blend of the shared projection (agent_chat.sql
// `tools[].settings`); each case is one branch of its CASE.
func TestProjectSettingsMirrorsTheProjection(t *testing.T) {
	cases := []struct {
		name, stored, selected, want string
	}{
		{"no selections", `{"a":1}`, `null`, `{"a":1}`},
		{"agent selection only", `{"a":1}`, `["x"]`, `{"a":1,"selected_tools":["x"]}`},
		{"empty agent selection", `{"a":1}`, `[]`, `{"a":1}`},
		{"stored selection only", `{"selected_tools":["x","y"]}`, `null`,
			`{"available_tools":["x","y"],"selected_tools":["x","y"]}`},
		{"narrowed", `{"selected_tools":["x","y"]}`, `["y","z"]`,
			`{"available_tools":["x","y"],"selected_tools":["y"]}`},
		{"no overlap keeps the agent selection", `{"selected_tools":["x"]}`, `["z"]`,
			`{"available_tools":["x"],"selected_tools":["z"]}`},
		{"a number matches by its text, as jsonb_array_elements_text does", `{"selected_tools":["1"]}`, `[1,"1"]`,
			`{"available_tools":["1"],"selected_tools":["1","1"]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ProjectSettings(decode(t, c.stored), json.RawMessage(c.selected))
			if err != nil {
				t.Fatal(err)
			}
			if encode(t, got) != c.want {
				t.Fatalf("got %s, want %s", encode(t, got), c.want)
			}
		})
	}
}

func TestCanonicalDigestIgnoresKeyOrderAndKeepsNumbers(t *testing.T) {
	left, err := CanonicalSHA256([]byte(`{"b":1.0,"a":[2,{"d":"<x>","c":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	right, err := CanonicalSHA256([]byte(` { "a" : [2, {"c": null, "d": "<x>"}], "b": 1.0 } `))
	if err != nil {
		t.Fatal(err)
	}
	other, err := CanonicalSHA256([]byte(`{"b":1,"a":[2,{"d":"<x>","c":null}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if left != right || left == other {
		t.Fatalf("digests: %s %s %s", left, right, other)
	}
	if _, err := CanonicalSHA256([]byte(`{"a":`)); err == nil {
		t.Fatal("malformed JSON digested")
	}
}

func TestConfigurationReferencesWalksObjectsNotArrays(t *testing.T) {
	settings := decode(t, `{
		"elitea_title": "top-level is not a reference",
		"github": {"elitea_title": "team-gh", "private": false, "credential_reference_id": "cr-1"},
		"nested": {"deeper": {"elitea_title": "mine", "private": true}},
		"list": [{"elitea_title": "in-array"}],
		"blank": {"elitea_title": "  "},
		"a/b": {"elitea_title": "escaped"}
	}`)
	got := ConfigurationReferences(settings)
	want := []ConfigurationReference{
		{Path: "/a~1b", Title: "escaped"},
		{Path: "/github", Title: "team-gh", CredentialReferenceID: "cr-1"},
		{Path: "/nested/deeper", Title: "mine", Private: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSplitDefinitionRemovesSettingsAndPutsThemBack(t *testing.T) {
	definition := []byte(`{"instructions":"x","tools":[{"id":5,"settings":{"token":"secret"}},{"id":6,"settings":null}]}`)
	stripped, settings, err := SplitDefinition(definition)
	if err != nil {
		t.Fatal(err)
	}
	if encode(t, stripped) != `{"instructions":"x","tools":[{"id":5},{"id":6}]}` {
		t.Fatalf("stripped = %s", encode(t, stripped))
	}
	if settings[0]["token"] != "secret" || settings[1] != nil {
		t.Fatalf("settings = %+v", settings)
	}
	if id, ok := ToolkitID(stripped, 1); !ok || id != 6 || ToolCount(stripped) != 2 {
		t.Fatalf("toolkit 1 = %d %v", id, ok)
	}
	if _, ok := ToolkitID(stripped, 2); ok {
		t.Fatal("an index past the tools resolved")
	}
	if !WithToolSettings(stripped, 0, settings[0]) || !WithToolSettings(stripped, 1, nil) {
		t.Fatal("settings not put back")
	}
	if WithToolSettings(stripped, 3, nil) {
		t.Fatal("settings written past the tools")
	}
	if encode(t, stripped) != `{"instructions":"x","tools":[{"id":5,"settings":{"token":"secret"}},{"id":6,"settings":null}]}` {
		t.Fatalf("restored = %s", encode(t, stripped))
	}
	if _, _, err := SplitDefinition([]byte(`{"tools":["not an object"]}`)); !errors.Is(err, ErrDefinitionUnreadable) {
		t.Fatalf("a malformed tool: %v", err)
	}
}

func TestDiff(t *testing.T) {
	before := decode(t, `{"a":1,"b":{"c":"x","d":[1,2]},"gone":true}`)
	after := decode(t, `{"a":1,"b":{"c":"y","d":[1,2,3]},"new":null}`)
	got := Diff(before, after)
	want := []Change{
		{Path: "/b/c", Op: "replace", Before: "x", After: "y"},
		{Path: "/b/d", Op: "replace", Before: []any{json.Number("1"), json.Number("2")},
			After: []any{json.Number("1"), json.Number("2"), json.Number("3")}},
		{Path: "/gone", Op: "remove", Before: true},
		{Path: "/new", Op: "add"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if changes := Diff(before, before); changes == nil || len(changes) != 0 {
		t.Fatalf("equal documents: %#v", changes)
	}
}

func TestHTTPRefusal(t *testing.T) {
	status, body, ok := HTTPRefusal(fmt.Errorf("start: %w", &DriftError{ToolkitID: 9, Reason: DriftConfigurationChanged}))
	if !ok || status != http.StatusConflict || body["code"] != DriftCode || body["reason"] != DriftConfigurationChanged || body["toolkit_id"] != int64(9) {
		t.Fatalf("drift = %d %v %v", status, body, ok)
	}
	status, body, ok = HTTPRefusal(&DriftError{Reason: DriftDefinitionMismatch})
	if _, named := body["toolkit_id"]; !ok || status != http.StatusConflict || named {
		t.Fatalf("drift without a toolkit = %d %v", status, body)
	}
	status, body, ok = HTTPRefusal(fmt.Errorf("wrapped: %w", ErrNotFound))
	if !ok || status != http.StatusNotFound || body["code"] != NotFoundCode {
		t.Fatalf("not found = %d %v %v", status, body, ok)
	}
	if _, _, ok := HTTPRefusal(errors.New("other")); ok {
		t.Fatal("an unrelated error was mapped")
	}
}
