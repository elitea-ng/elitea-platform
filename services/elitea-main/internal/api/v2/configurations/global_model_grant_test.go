package configurations

// The WRITE side of the platform-model grant.
//
// The read side is lenient on purpose: an absent or malformed scope is read as
// "every project", because that is what every row written before the field
// existed meant (application/configurations/model_grant.go). That leniency is
// only safe while the write side refuses what it cannot store — a value stored
// and then read back as "all" is the opposite of the operator's choice,
// reported to them as a success.

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// admitGrant runs one `data` object through the grant rule and reports what
// survived.
func admitGrant(t *testing.T, data map[string]any) (map[string]any, int) {
	t.Helper()
	recorder := httptest.NewRecorder()
	body := map[string]any{"elitea_title": "m", "type": "llm_model", "data": data}
	if !admitGlobalModelGrant(recorder, body) {
		return nil, recorder.Code
	}
	stored, _ := body["data"].(map[string]any)
	return stored, recorder.Code
}

// TestABodyWithNoGrantIsAdmittedUnchanged.
//
// A partial update that renames a model touches no scope, and a `data` object
// that carries none is a row with no scope — which is "all", and is what the
// row meant before this feature. Requiring the field would refuse every client
// that predates it.
func TestABodyWithNoGrantIsAdmittedUnchanged(t *testing.T) {
	recorder := httptest.NewRecorder()
	if !admitGlobalModelGrant(recorder, map[string]any{"elitea_title": "m"}) {
		t.Fatalf("a body with no data was refused: %d", recorder.Code)
	}
	stored, code := admitGrant(t, map[string]any{"name": "gpt-4o"})
	if stored == nil {
		t.Fatalf("a data object with no scope was refused: %d", code)
	}
	if _, present := stored[shareScopeKey]; present {
		t.Errorf("the rule invented a scope for a body that sent none: %v", stored)
	}
}

// TestAnUnknownScopeIsRefused. It would be STORED and then read back as "all",
// so the model would reach every project while the screen said otherwise.
func TestAnUnknownScopeIsRefused(t *testing.T) {
	for _, scope := range []any{"team", "ALL", "", 7, true} {
		stored, code := admitGrant(t, map[string]any{"name": "m", shareScopeKey: scope})
		if stored != nil {
			t.Errorf("scope %v was admitted", scope)
		}
		if code != 400 {
			t.Errorf("scope %v answered %d, want 400", scope, code)
		}
	}
}

// TestASelectedProjectsGrantMustNameAProject. An empty list grants the model to
// nobody, which is the `none` choice — and an operator who meant `none` did not
// choose "selected projects".
func TestASelectedProjectsGrantMustNameAProject(t *testing.T) {
	for _, list := range []any{[]any{}, "12", nil, []any{"twelve"}, []any{0}} {
		data := map[string]any{"name": "m", shareScopeKey: "projects"}
		if list != nil {
			data[sharedWithKey] = list
		}
		if stored, code := admitGrant(t, data); stored != nil || code != 400 {
			t.Errorf("shared_with %v answered %d and stored %v, want a 400", list, code, stored)
		}
	}
}

// TestASelectedProjectsGrantIsStoredAsTheIdsThatWereREAD.
//
// What the rule admitted and what is stored must be the same list, or the
// refusal above proves nothing about the row. A stringified id becomes a
// number, and a repeated id is stored once.
func TestASelectedProjectsGrantIsStoredAsTheIdsThatWereRead(t *testing.T) {
	stored, code := admitGrant(t, map[string]any{
		"name": "m", shareScopeKey: "projects",
		sharedWithKey: []any{"12", 12.0, 34.0},
	})
	if stored == nil {
		t.Fatalf("a valid grant was refused: %d", code)
	}
	encoded, err := json.Marshal(stored[sharedWithKey])
	if err != nil {
		t.Fatalf("marshal the stored list: %v", err)
	}
	if string(encoded) != "[12,34]" {
		t.Errorf("stored shared_with = %s, want [12,34]", encoded)
	}
}

// TestLeavingTheSelectedProjectsScopeClearsTheList.
//
// A row granted to three projects and then withdrawn would otherwise keep
// naming them, and the next edit that switched back to "selected projects"
// would silently restore a grant nobody re-chose.
func TestLeavingTheSelectedProjectsScopeClearsTheList(t *testing.T) {
	for _, scope := range []string{"all", "none"} {
		stored, code := admitGrant(t, map[string]any{
			"name": "m", shareScopeKey: scope, sharedWithKey: []any{12.0},
		})
		if stored == nil {
			t.Fatalf("scope %q was refused: %d", scope, code)
		}
		encoded, err := json.Marshal(stored[sharedWithKey])
		if err != nil {
			t.Fatalf("marshal the stored list: %v", err)
		}
		if string(encoded) != "[]" {
			t.Errorf("scope %q kept shared_with = %s, want it cleared", scope, encoded)
		}
	}
}

// The two `data` keys, named once for these tests so a rename of the stored
// field cannot leave them asserting a key nothing writes.
const (
	shareScopeKey = "share_scope"
	sharedWithKey = "shared_with"
)
