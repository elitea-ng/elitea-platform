package configurations_test

// The platform model's GRANT, end to end against a real PostgreSQL.
//
// ## What only a database can state here
//
// The scope is stored in the row's `data` column and the update replaces that
// column WHOLE. Three of the four facts below are therefore about the round
// trip and not about the branch:
//
//   - the create stores the scope the operator chose, in the shape every reader
//     expects (a string and a list of numbers);
//   - the listing reports it back, which is what the edit form opens on. A row
//     with no scope must report `all`, not an empty string — the panel renders
//     this value, and a blank there would read as "granted to nobody" for every
//     model on a deployment that has not touched one;
//   - an edit that changes something else keeps the scope, because the merge
//     the dialog makes is over the object this listing reported;
//   - and switching away from "selected projects" CLEARS the list, so a later
//     switch back cannot restore a grant nobody re-chose.
//
// The harness (pool, router, provider) is the platform-link file's next door.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
)

// grantOfListedModel is the listing's report of one row's grant.
type grantOfListedModel struct {
	ID         int    `json:"id"`
	Name       string `json:"elitea_title"`
	ShareScope string `json:"share_scope"`
	SharedWith []int  `json:"shared_with"`
}

// readModelGrant reads the listing and returns the grant of the row with this
// title.
func readModelGrant(t *testing.T, router chi.Router, title string) grantOfListedModel {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/gateway/platform_models", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /gateway/platform_models = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Items []grantOfListedModel `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the listing %q: %v", recorder.Body.String(), err)
	}
	for _, item := range body.Items {
		if item.Name == title {
			return item
		}
	}
	t.Fatalf("the listing carries no model called %q: %s", title, recorder.Body.String())
	return grantOfListedModel{}
}

// TestAPlatformModelsGrantIsStoredAndReadBack is the round trip the edit dialog
// depends on.
func TestAPlatformModelsGrantIsStoredAndReadBack(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := platformLinkRouter(pool)
	provider := platformLinkProvider(t, router, "autotest_grant_provider")

	const scoped = "autotest_scoped_model"
	created := platformLinkSend(t, router, http.MethodPost, "/gateway/platform_models",
		map[string]any{
			"elitea_title": scoped,
			"type":         "llm_model",
			"data": map[string]any{
				"name":           "autotest-scoped",
				"ai_credentials": map[string]any{"elitea_title": provider},
				"share_scope":    "projects",
				// Sent as a string, which is what a client that stringifies its
				// ids sends. It has to be STORED as a number, because that is
				// the shape the gateway's own reader and every seed expect.
				"shared_with": []any{"90500", 90500, 42},
			},
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("creating a scoped platform model = %d; body = %s",
			created.Code, created.Body.String())
	}

	stored := storedModelData(t, pool, scoped)
	if stored["share_scope"] != "projects" {
		t.Errorf("stored share_scope = %v, want projects", stored["share_scope"])
	}
	encoded, err := json.Marshal(stored["shared_with"])
	if err != nil {
		t.Fatalf("marshal the stored list: %v", err)
	}
	// De-duplicated and numeric, in the order the operator authored.
	if string(encoded) != "[90500,42]" {
		t.Errorf("stored shared_with = %s, want [90500,42]", encoded)
	}

	listed := readModelGrant(t, router, scoped)
	if listed.ShareScope != "projects" || len(listed.SharedWith) != 2 ||
		listed.SharedWith[0] != 90500 || listed.SharedWith[1] != 42 {
		t.Errorf("the listing reports %#v, want projects [90500 42]", listed)
	}
}

// TestAModelWithNoGrantIsListedAsAvailableToAll.
//
// Every row written before this feature carries no scope, and every one of them
// was offered to every project. The listing has to SAY so: the panel renders
// this field, and an empty string there reads as "granted to nobody".
func TestAModelWithNoGrantIsListedAsAvailableToAll(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := platformLinkRouter(pool)
	provider := platformLinkProvider(t, router, "autotest_ungranted_provider")

	const legacy = "autotest_legacy_model"
	created := platformLinkSend(t, router, http.MethodPost, "/gateway/platform_models",
		map[string]any{
			"elitea_title": legacy,
			"type":         "llm_model",
			"data": map[string]any{
				"name":           "autotest-legacy",
				"ai_credentials": map[string]any{"elitea_title": provider},
			},
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("creating a model with no grant = %d; body = %s",
			created.Code, created.Body.String())
	}
	if listed := readModelGrant(t, router, legacy); listed.ShareScope != "all" {
		t.Errorf("a model with no stored scope is listed as %q, want all", listed.ShareScope)
	}
	// The row itself carries no scope: the listing INTERPRETS an absent field,
	// it does not write one. A create that invented `share_scope: "all"` would
	// make every row look deliberately granted and hide which ones an operator
	// actually chose.
	if _, present := storedModelData(t, pool, legacy)["share_scope"]; present {
		t.Error("the create invented a scope the operator did not choose")
	}
}

// TestWithdrawingAGrantClearsTheProjectList.
//
// The list survives the scope change otherwise, and the next switch back to
// "selected projects" would restore a grant nobody re-chose.
func TestWithdrawingAGrantClearsTheProjectList(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := platformLinkRouter(pool)
	provider := platformLinkProvider(t, router, "autotest_withdrawn_provider")

	const model = "autotest_withdrawn_model"
	created := platformLinkSend(t, router, http.MethodPost, "/gateway/platform_models",
		map[string]any{
			"elitea_title": model,
			"type":         "llm_model",
			"data": map[string]any{
				"name":           "autotest-withdrawn",
				"ai_credentials": map[string]any{"elitea_title": provider},
				"share_scope":    "projects",
				"shared_with":    []any{90500},
			},
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("creating the model = %d; body = %s", created.Code, created.Body.String())
	}
	listed := readModelGrant(t, router, model)

	// The edit the dialog makes: the stored object, with the changed fields
	// over it — so the list is sent back exactly as it was read.
	updated := platformLinkSend(t, router, http.MethodPut,
		"/gateway/platform_models/"+strconv.Itoa(listed.ID), map[string]any{
			"elitea_title": model,
			"data": map[string]any{
				"name":           "autotest-withdrawn",
				"ai_credentials": map[string]any{"elitea_title": provider},
				"share_scope":    "none",
				"shared_with":    []any{90500},
			},
		})
	if updated.Code != http.StatusOK {
		t.Fatalf("withdrawing the grant = %d; body = %s", updated.Code, updated.Body.String())
	}

	after := readModelGrant(t, router, model)
	if after.ShareScope != "none" || len(after.SharedWith) != 0 {
		t.Errorf("after the withdrawal the listing reports %#v, want none with no projects", after)
	}
	encoded, err := json.Marshal(storedModelData(t, pool, model)["shared_with"])
	if err != nil {
		t.Fatalf("marshal the stored list: %v", err)
	}
	if string(encoded) != "[]" {
		t.Errorf("the withdrawn row still names %s", encoded)
	}
}

// TestAnUnstorableGrantIsRefusedAndNothingIsWritten.
//
// The read side is lenient, so an unknown scope WOULD be stored and read back
// as "all" — the model reaching every project while the screen that wrote it
// said otherwise. It is refused here, where the operator can still change it.
func TestAnUnstorableGrantIsRefusedAndNothingIsWritten(t *testing.T) {
	pool := newCreateEchoPool(t)
	router := platformLinkRouter(pool)
	provider := platformLinkProvider(t, router, "autotest_badgrant_provider")

	for name, grant := range map[string]map[string]any{
		"autotest_badscope_model": {"share_scope": "team"},
		"autotest_emptylist_model": {
			"share_scope": "projects", "shared_with": []any{},
		},
	} {
		data := map[string]any{
			"name":           "autotest-bad",
			"ai_credentials": map[string]any{"elitea_title": provider},
		}
		for key, value := range grant {
			data[key] = value
		}
		refusal := platformLinkSend(t, router, http.MethodPost, "/gateway/platform_models",
			map[string]any{"elitea_title": name, "type": "llm_model", "data": data})
		if refusal.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400; body = %s", name, refusal.Code, refusal.Body.String())
		}
		assertNoConfigurationRow(t, pool, name)
	}
}
