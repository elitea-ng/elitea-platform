package agentsoul

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func refusalCode(t *testing.T, err error) string {
	t.Helper()
	var refused *Error
	if !errors.As(err, &refused) {
		t.Fatalf("want a soul refusal, got %v", err)
	}
	return refused.Code
}

func TestTheContractExampleIsAValidSoul(t *testing.T) {
	encoded, err := normalizeJSON([]byte(`{"schema":"elitea.soul/v1","display_name":"Mira","pronouns":"they/them",
		"role":"Release coordinator","voice":["concise"],"values":["cite sources"],"boundaries":["never merges without a green CI"],
		"relationships":[{"agent":"ci-doctor","stance":"delegate failing builds"}],"proactive_goals":["watch CI"],"body":"Short sentences."}`))
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(encoded, &back); err != nil || back["display_name"] != "Mira" {
		t.Fatalf("round trip lost a field: %s %v", encoded, err)
	}
}

func TestNoSoulIsNotAnError(t *testing.T) {
	for _, raw := range []string{"", "null", "  "} {
		if encoded, err := normalizeJSON([]byte(raw)); err != nil || encoded != nil {
			t.Fatalf("%q: %s %v", raw, encoded, err)
		}
	}
}

func TestACapabilityKeyIsRefusedAtAnyDepth(t *testing.T) {
	for _, raw := range []string{
		`{"schema":"elitea.soul/v1","tools":["github"]}`,
		`{"schema":"elitea.soul/v1","Credentials":{"x":"y"}}`,
		`{"schema":"elitea.soul/v1","permissions":["admin"]}`,
		`{"schema":"elitea.soul/v1","relationships":[{"agent":"a","stance":"b","scopes":["write"]}]}`,
	} {
		_, err := normalizeJSON([]byte(raw))
		if code := refusalCode(t, err); code != CodeCapabilityKey {
			t.Fatalf("%s: code %s", raw, code)
		}
	}
}

func TestTheSizeLimitCountsTextBytes(t *testing.T) {
	// The `schema` value counts too: body fills the rest exactly, in
	// two-byte characters plus the odd byte.
	room := MaxBytes - len(SchemaID)
	body := strings.Repeat("é", room/2) + strings.Repeat("a", room%2)
	if _, err := Normalize(map[string]any{"schema": SchemaID, "body": body}); err != nil {
		t.Fatalf("exactly at the limit: %v", err)
	}
	_, err := Normalize(map[string]any{"schema": SchemaID, "body": body, "display_name": "M"})
	if code := refusalCode(t, err); code != CodeTooLarge {
		t.Fatalf("code %s", code)
	}
}

func TestTheSchemaRefusesUnknownKeysAndBadShapes(t *testing.T) {
	for _, raw := range []string{
		`{"display_name":"no schema"}`,
		`{"schema":"elitea.soul/v2"}`,
		`{"schema":"elitea.soul/v1","mood":"sunny"}`,
		`{"schema":"elitea.soul/v1","voice":"not a list"}`,
		`{"schema":"elitea.soul/v1","relationships":[{"agent":"a"}]}`,
		`["not an object"]`,
	} {
		_, err := normalizeJSON([]byte(raw))
		if code := refusalCode(t, err); code != CodeInvalid {
			t.Fatalf("%s: code %s", raw, code)
		}
	}
}

func TestARefusalNamesTheFieldNotTheValue(t *testing.T) {
	_, err := normalizeJSON([]byte(`{"schema":"elitea.soul/v1","pronouns":"` + strings.Repeat("x", 40) + `"}`))
	if err == nil || !strings.Contains(err.Error(), "pronouns") || strings.Contains(err.Error(), "xxxx") {
		t.Fatalf("message %v", err)
	}
}
