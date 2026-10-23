package social_test

// THE BUG, AT THE SURFACE THE USER TOUCHES.
//
// Settings › Memory (apps/elitea-web/src/pages/settings/Memory.tsx) saves
// through `PUT /social/author`, and centry.social_users has held the two jsonb
// columns it writes — `default_context_management` and `default_summarization`
// — since 001_initial.sql. The Go handler read neither and wrote neither: the
// SELECT listed five columns and the upsert five values, so every setting the
// page sent was accepted with a 200 and silently discarded. The classic
// invisible-data defect: the toast says "Settings saved successfully" and
// nothing was saved.
//
// A ROUND TRIP IS THE ONLY HONEST TEST. Asserting the 200 proves nothing —
// today's code returns 200 too. Asserting the UPDATE ran proves nothing —
// there was an UPDATE, it just did not carry these columns. So each test here
// PUTs a value and then GETs it back through the real handler and a real
// database, which is exactly the trip that was broken.
//
// PROOF OF RED. Against the pre-change handler every test in this file fails:
// `TestAuthorMemorySettingsRoundTrip` reads back a response with neither key
// (`default_context_management is absent from the author response`), the
// preservation test fails the same way, and the validation tests fail with
// `status = 200, want 400` because nothing validated a body it did not read.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL). The pool fixture,
// the template and `seedAuthorUser` are shared with
// personal_project_postgres_integration_test.go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

// authorMemoryFixture is one seeded user and the routes that serve them.
type authorMemoryFixture struct {
	t      *testing.T
	routes chi.Router
	email  string
	userID int64
}

func newAuthorMemoryFixture(t *testing.T, email string) authorMemoryFixture {
	t.Helper()
	pool := newPersonalProjectSocialPool(t)
	return authorMemoryFixture{
		t:      t,
		routes: handler.NewHandler(pool).Routes(),
		email:  email,
		userID: seedAuthorUser(t, pool, email, "Memory Tester"),
	}
}

func (f authorMemoryFixture) request(method, target string, body []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{
		ID:     strconv.FormatInt(f.userID, 10),
		UserID: strconv.FormatInt(f.userID, 10),
		Email:  f.email,
	}))
	recorder := httptest.NewRecorder()
	f.routes.ServeHTTP(recorder, request)
	return recorder
}

func (f authorMemoryFixture) put(body map[string]any) *httptest.ResponseRecorder {
	f.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		f.t.Fatalf("encode request body: %v", err)
	}
	return f.request(http.MethodPut, "/author/", encoded)
}

func (f authorMemoryFixture) putOK(body map[string]any) {
	f.t.Helper()
	if recorder := f.put(body); recorder.Code != http.StatusOK {
		f.t.Fatalf("PUT /author/ status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// get returns the author document as a raw map, so a MISSING key and a key
// holding a zero value stay distinguishable — the whole point here.
func (f authorMemoryFixture) get() map[string]any {
	f.t.Helper()
	recorder := f.request(http.MethodGet, "/author/", nil)
	if recorder.Code != http.StatusOK {
		f.t.Fatalf("GET /author/ status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		f.t.Fatalf("decode author response %s: %v", recorder.Body.String(), err)
	}
	return decoded
}

func authorBlock(t *testing.T, document map[string]any, key string) map[string]any {
	t.Helper()
	value, present := document[key]
	if !present {
		t.Fatalf("%s is absent from the author response: %v", key, document)
	}
	block, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want an object", key, value)
	}
	return block
}

// The round trip Settings › Memory depends on.
func TestAuthorMemorySettingsRoundTrip(t *testing.T) {
	fixture := newAuthorMemoryFixture(t, "memory-round-trip@autotest.local")

	fixture.putOK(map[string]any{
		"name":            "Memory Tester",
		"description":     "round trip",
		"personalization": map[string]any{"persona": "developer"},
		"default_context_management": map[string]any{
			"enabled":                  true,
			"max_context_tokens":       32000,
			"preserve_recent_messages": 7,
			"enable_context_editing":   true,
		},
		"default_summarization": map[string]any{
			"enable_summarization":     false,
			"summary_instructions":     "Keep it short",
			"summary_model_name":       "gpt-4o-mini",
			"summary_model_project_id": 3,
			"summary_trigger_ratio":    0.8,
			"target_summary_tokens":    1024,
		},
	})

	document := fixture.get()

	management := authorBlock(t, document, "default_context_management")
	for key, want := range map[string]any{
		"enabled":                  true,
		"max_context_tokens":       float64(32000),
		"preserve_recent_messages": float64(7),
		"enable_context_editing":   true,
	} {
		if management[key] != want {
			t.Errorf("default_context_management.%s = %v, want %v", key, management[key], want)
		}
	}

	summarization := authorBlock(t, document, "default_summarization")
	for key, want := range map[string]any{
		"enable_summarization":     false,
		"summary_instructions":     "Keep it short",
		"summary_model_name":       "gpt-4o-mini",
		"summary_model_project_id": float64(3),
		"summary_trigger_ratio":    0.8,
		"target_summary_tokens":    float64(1024),
	} {
		if summarization[key] != want {
			t.Errorf("default_summarization.%s = %v, want %v", key, summarization[key], want)
		}
	}

	// The blocks are stored in their own columns, not smuggled into
	// `personalization` — that is what makes them readable by the
	// context-strategy resolver, which never looks at the personalization blob.
	if personalization, ok := document["personalization"].(map[string]any); ok {
		if personalization["persona"] != "developer" {
			t.Errorf("personalization.persona = %v, want developer", personalization["persona"])
		}
	}
}

// Settings › AI Personality and Settings › Memory are two pages over ONE
// record. A save from the personality page carries no context settings at all,
// and must not erase the memory page's.
func TestAuthorMemorySettingsSurviveAnUnrelatedSave(t *testing.T) {
	fixture := newAuthorMemoryFixture(t, "memory-preserved@autotest.local")

	fixture.putOK(map[string]any{
		"default_context_management": map[string]any{"max_context_tokens": 21000},
	})
	fixture.putOK(map[string]any{
		"name":            "Memory Tester",
		"description":     "personality page only",
		"personalization": map[string]any{"persona": "analyst"},
	})

	management := authorBlock(t, fixture.get(), "default_context_management")
	if management["max_context_tokens"] != float64(21000) {
		t.Fatalf("max_context_tokens = %v after an unrelated save, want the stored 21000",
			management["max_context_tokens"])
	}
}

// A profile last written by a client that nested the blocks inside
// `personalization` — which is where apps/elitea-web put them precisely
// BECAUSE this handler dropped every other top-level key — is still readable,
// and the next save lifts it into the columns.
func TestAuthorMemorySettingsAcceptTheLegacyNestedPlacement(t *testing.T) {
	fixture := newAuthorMemoryFixture(t, "memory-nested@autotest.local")

	fixture.putOK(map[string]any{
		"personalization": map[string]any{
			"persona":                    "developer",
			"default_context_management": map[string]any{"max_context_tokens": 17000},
			"default_summarization":      map[string]any{"enable_summarization": true},
		},
	})

	document := fixture.get()
	management := authorBlock(t, document, "default_context_management")
	if management["max_context_tokens"] != float64(17000) {
		t.Fatalf("a nested block did not surface at the top level: %v", management)
	}
	if summarization := authorBlock(t, document, "default_summarization"); summarization["enable_summarization"] != true {
		t.Fatalf("nested default_summarization did not surface: %v", summarization)
	}
}

// Out-of-range values are refused, by field, in this API's validation shape.
// Nothing validated these before, because nothing read them.
func TestAuthorMemorySettingsRefuseOutOfRangeValues(t *testing.T) {
	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{
			name:  "below the token floor",
			body:  map[string]any{"default_context_management": map[string]any{"max_context_tokens": 999}},
			field: "default_context_management.max_context_tokens",
		},
		{
			name:  "zero preserved messages",
			body:  map[string]any{"default_context_management": map[string]any{"preserve_recent_messages": 0}},
			field: "default_context_management.preserve_recent_messages",
		},
		{
			name:  "more than 99 preserved messages",
			body:  map[string]any{"default_context_management": map[string]any{"preserve_recent_messages": 100}},
			field: "default_context_management.preserve_recent_messages",
		},
		{
			name:  "summary budget under the floor",
			body:  map[string]any{"default_summarization": map[string]any{"target_summary_tokens": 10}},
			field: "default_summarization.target_summary_tokens",
		},
		{
			name:  "trigger ratio outside 0..1",
			body:  map[string]any{"default_summarization": map[string]any{"summary_trigger_ratio": 1.5}},
			field: "default_summarization.summary_trigger_ratio",
		},
		{
			name:  "wrong type",
			body:  map[string]any{"default_context_management": map[string]any{"max_context_tokens": "lots"}},
			field: "default_context_management.max_context_tokens",
		},
	}

	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAuthorMemoryFixture(t, fmt.Sprintf("memory-invalid-%d@autotest.local", index))
			recorder := fixture.put(testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
			var decoded struct {
				Error string `json:"error"`
				Field string `json:"field"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("decode refusal %s: %v", recorder.Body.String(), err)
			}
			if decoded.Field != testCase.field {
				t.Errorf("field = %q, want %q", decoded.Field, testCase.field)
			}
			if decoded.Error == "" {
				t.Error("a refusal has to say what was wrong")
			}
		})
	}
}

// A refused save must leave the stored settings alone.
func TestAuthorMemorySettingsRefusalDoesNotWrite(t *testing.T) {
	fixture := newAuthorMemoryFixture(t, "memory-refusal-atomic@autotest.local")

	fixture.putOK(map[string]any{
		"default_context_management": map[string]any{"max_context_tokens": 40000},
	})
	if recorder := fixture.put(map[string]any{
		"default_context_management": map[string]any{"max_context_tokens": 12},
	}); recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}

	management := authorBlock(t, fixture.get(), "default_context_management")
	if management["max_context_tokens"] != float64(40000) {
		t.Fatalf("max_context_tokens = %v after a refused save, want the stored 40000",
			management["max_context_tokens"])
	}
}

// An account that has never opened Settings › Memory has no opinion, and the
// response says so by OMITTING the keys rather than by inventing defaults the
// user never chose. The client's own constants are what fill that gap.
func TestAuthorWithoutMemorySettingsOmitsTheBlocks(t *testing.T) {
	fixture := newAuthorMemoryFixture(t, "memory-untouched@autotest.local")
	fixture.putOK(map[string]any{"name": "Memory Tester", "description": "no memory settings"})

	document := fixture.get()
	if _, present := document["default_context_management"]; present {
		t.Errorf("default_context_management is present for an account that never set it: %v", document)
	}
	if _, present := document["default_summarization"]; present {
		t.Errorf("default_summarization is present for an account that never set it: %v", document)
	}
}
