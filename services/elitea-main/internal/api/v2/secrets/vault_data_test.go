package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// The exact document repos.SetCurrentModelDefault leaves behind: the model id
// is a JSON integer, beside ordinary string secrets (F1 / UI-PD-1).
const intModelVault = `{
  "secrets": {
    "api_key": "sk-live",
    "default_chat_model_name": "gpt-4o",
    "default_chat_model_project_id": 42,
    "flag": true,
    "nothing": null,
    "ratio": 1.5
  },
  "hidden_secrets": {"hidden_id": 7}
}`

func decodeVault(t *testing.T, document string) vaultData {
	t.Helper()
	var v vaultData
	if err := json.Unmarshal([]byte(document), &v); err != nil {
		t.Fatalf("decode %s: %v", document, err)
	}
	return v
}

func storedRaw(t *testing.T, v vaultData) (regular, hidden map[string]json.RawMessage) {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode vault: %v", err)
	}
	var stored storedVault
	if err := json.Unmarshal(encoded, &stored); err != nil {
		t.Fatalf("re-decode %s: %v", encoded, err)
	}
	return stored.Secrets, stored.HiddenSecrets
}

func TestVaultDataExposesNonStringValuesAsText(t *testing.T) {
	t.Parallel()
	v := decodeVault(t, intModelVault)
	for name, want := range map[string]string{
		"api_key":                       "sk-live",
		"default_chat_model_name":       "gpt-4o",
		"default_chat_model_project_id": "42",
		"flag":                          "true",
		"nothing":                       "null",
		"ratio":                         "1.5",
	} {
		if got := v.Secrets[name]; got != want {
			t.Errorf("Secrets[%q] = %q, want %q", name, got, want)
		}
	}
	if got := v.HiddenSecrets["hidden_id"]; got != "7" {
		t.Errorf("HiddenSecrets[hidden_id] = %q, want \"7\"", got)
	}
}

// The risk the plan names: a write-back must keep the integer an integer, or
// LookupRegularProjectID and pylon read a string where they expect an int.
func TestVaultDataWriteBackKeepsTheStoredJSONType(t *testing.T) {
	t.Parallel()
	v := decodeVault(t, intModelVault)
	v.Secrets["added"] = "new" // an unrelated route edit
	regular, hidden := storedRaw(t, v)
	for name, want := range map[string]string{
		"default_chat_model_project_id": `42`,
		"flag":                          `true`,
		"nothing":                       `null`,
		"ratio":                         `1.5`,
		"api_key":                       `"sk-live"`,
		"added":                         `"new"`,
	} {
		if got := string(regular[name]); got != want {
			t.Errorf("stored secrets[%q] = %s, want %s", name, got, want)
		}
	}
	if got := string(hidden["hidden_id"]); got != `7` {
		t.Errorf("stored hidden_secrets[hidden_id] = %s, want 7", got)
	}
}

// A value a route CHANGED is what the user typed: a string.
func TestVaultDataChangedValueIsWrittenAsAString(t *testing.T) {
	t.Parallel()
	v := decodeVault(t, intModelVault)
	v.Secrets["default_chat_model_project_id"] = "43"
	regular, _ := storedRaw(t, v)
	if got := string(regular["default_chat_model_project_id"]); got != `"43"` {
		t.Fatalf("stored changed value = %s, want \"43\"", got)
	}
}

// A string that merely LOOKS like a number stays a string.
func TestVaultDataNumericStringStaysAString(t *testing.T) {
	t.Parallel()
	v := decodeVault(t, `{"secrets":{"pin":"42"},"hidden_secrets":{}}`)
	regular, _ := storedRaw(t, v)
	if got := string(regular["pin"]); got != `"42"` {
		t.Fatalf("stored pin = %s, want \"42\"", got)
	}
}

// Hide moves the value as it is, as pylon does.
func TestVaultDataHideCarriesTheStoredType(t *testing.T) {
	t.Parallel()
	v := decodeVault(t, intModelVault)
	if !v.moveToHidden("default_chat_model_project_id") {
		t.Fatal("moveToHidden reported the secret absent")
	}
	if v.moveToHidden("no_such_secret") {
		t.Fatal("moveToHidden reported an absent secret present")
	}
	regular, hidden := storedRaw(t, v)
	if _, still := regular["default_chat_model_project_id"]; still {
		t.Fatal("the hidden secret is still in secrets")
	}
	if got := string(hidden["default_chat_model_project_id"]); got != `42` {
		t.Fatalf("stored hidden value = %s, want 42", got)
	}
}

// An in-memory vault (the seeders and a freshly created one) encodes both
// collections as objects — the shape centrysecrets.RewriteWrapped requires.
func TestVaultDataEmptyVaultEncodesBothObjects(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(vaultData{})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"secrets":{},"hidden_secrets":{}}` {
		t.Fatalf("empty vault = %s", encoded)
	}
}

func TestVaultDataRefusesADocumentThatIsNotAVault(t *testing.T) {
	t.Parallel()
	for _, document := range []string{`[]`, `{"secrets": 5}`, `not json`} {
		var v vaultData
		if err := json.Unmarshal([]byte(document), &v); err == nil {
			t.Errorf("decode %s: want an error", document)
		}
	}
}

func TestVaultErrorClassNamesEachCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause")
	for want, err := range map[string]error{
		"none":         nil,
		"absent":       ErrVaultAbsent,
		"write":        fmt.Errorf("%w: x", errVaultWrite),
		"rows":         fmt.Errorf("%w: x: %w", errVaultRows, cause),
		"half_written": fmt.Errorf("%w: x", errVaultHalfWritten),
		"key":          fmt.Errorf("%w: x: %w", errVaultKey, cause),
		"ciphertext":   fmt.Errorf("%w: x: %w", errVaultCiphertext, cause),
		"decode":       fmt.Errorf("%w: x: %w", errVaultDecode, cause),
		"other":        cause,
	} {
		if got := vaultErrorClass(err); got != want {
			t.Errorf("vaultErrorClass(%v) = %q, want %q", err, got, want)
		}
	}
}
