package configurations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
)

// keyless_credentials_test.go — the write-side rules for data.auth_mode.
//
// Every handler here has no pool. A body that passes every rule reaches the
// store and answers 503; that 503 is the control which proves a 400 came from
// the rule and not from an unrelated check.

func keylessVertexBody(data map[string]any) map[string]any {
	return map[string]any{
		"elitea_title": "platform_vertex", "label": "Platform Vertex", "type": "vertex_ai", "data": data,
	}
}

func keylessRequest(t *testing.T, method string, body map[string]any, platform bool, params map[string]string) *http.Request {
	t.Helper()
	raw, _ := json.Marshal(body)
	request := httptest.NewRequest(method, "/", strings.NewReader(string(raw)))
	routeContext := chi.NewRouteContext()
	for key, value := range params {
		routeContext.URLParams.Add(key, value)
	}
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	if platform {
		ctx = withPlatformCredentialWrite(ctx)
	}
	return request.WithContext(ctx)
}

func keylessHandler(t *testing.T) *Handler {
	t.Helper()
	catalog, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatalf("LoadPinnedCurrentAvailableCatalog() error = %v", err)
	}
	return &Handler{catalog: catalog}
}

func keylessCreate(t *testing.T, body map[string]any, platform bool) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	keylessHandler(t).Create(recorder, keylessRequest(t, http.MethodPost, body, platform, map[string]string{"projectID": "1"}))
	return recorder
}

// TestCreateKeylessCredentialMatrix: the project route refuses the mode, the
// platform surface accepts it for the two supported types only, and a keyless
// row never stores a new key.
func TestCreateKeylessCredentialMatrix(t *testing.T) {
	keyless := map[string]any{"vertex_project": "p", "vertex_location": "us-central1", "auth_mode": "workload_identity"}
	withKey := map[string]any{
		"vertex_project": "p", "vertex_location": "us-central1", "auth_mode": "workload_identity",
		"vertex_credentials": `{"type":"service_account"}`,
	}
	dial := map[string]any{
		"elitea_title": "platform_dial", "label": "Platform DIAL", "type": "ai_dial",
		"data": map[string]any{"api_base": "https://dial.example.com", "auth_mode": "workload_identity"},
	}
	openAI := map[string]any{
		"elitea_title": "platform_openai", "label": "Platform OpenAI", "type": "open_ai",
		"data": map[string]any{"api_base": "https://api.openai.com/v1", "auth_mode": "workload_identity"},
	}
	cases := []struct {
		name     string
		body     map[string]any
		platform bool
		status   int
		contains string
	}{
		{"project route refuses vertex", keylessVertexBody(keyless), false, http.StatusBadRequest, "platform provider"},
		{"project route refuses dial", dial, false, http.StatusBadRequest, "platform provider"},
		{"platform accepts vertex without a document", keylessVertexBody(keyless), true, http.StatusServiceUnavailable, ""},
		{"platform accepts dial without a key", dial, true, http.StatusServiceUnavailable, ""},
		{"platform refuses a new key", keylessVertexBody(withKey), true, http.StatusBadRequest, "data.vertex_credentials"},
		{"platform refuses another type", openAI, true, http.StatusBadRequest, "vertex_ai and ai_dial"},
		{"unknown mode", keylessVertexBody(map[string]any{
			"vertex_project": "p", "vertex_location": "l", "vertex_credentials": "x", "auth_mode": "ambient",
		}), true, http.StatusBadRequest, "data.auth_mode"},
		// The key mode still requires the document (the snapshot's `else`).
		{"key mode still requires the document", keylessVertexBody(map[string]any{
			"vertex_project": "p", "vertex_location": "l",
		}), false, http.StatusBadRequest, "data.vertex_credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := keylessCreate(t, tc.body, tc.platform)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.status, recorder.Body.String())
			}
			if tc.contains != "" && !strings.Contains(recorder.Body.String(), tc.contains) {
				t.Errorf("body %s does not name %q", recorder.Body.String(), tc.contains)
			}
		})
	}
}

// TestUpdateRefusesKeylessModeOnTheProjectRoute: the refusal answers before
// the database is read, so a project PUT cannot arm the mode on a row it owns.
func TestUpdateRefusesKeylessModeOnTheProjectRoute(t *testing.T) {
	recorder := httptest.NewRecorder()
	body := map[string]any{"data": map[string]any{"api_base": "https://dial.example.com", "auth_mode": "workload_identity"}}
	keylessHandler(t).Update(recorder, keylessRequest(t, http.MethodPut, body, false,
		map[string]string{"projectID": "1", "configID": "5"}))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "platform provider") {
		t.Fatalf("status = %d, body = %s, want the 400 platform-only refusal", recorder.Code, recorder.Body.String())
	}
}

// TestSwitchToKeylessClearsTheStoredKey: an edit can send back the stored
// reference of the old key. The switch removes it, so the row the gateway
// reads holds no key and is not ambiguous.
func TestSwitchToKeylessClearsTheStoredKey(t *testing.T) {
	data := map[string]any{
		"api_base": "https://dial.example.com", "auth_mode": "workload_identity",
		"api_key": "{{secret.platform_dial_key}}", "api_token": "",
	}
	if failure := admitKeylessCredentialData("ai_dial", data); failure != nil {
		t.Fatalf("refused: %+v", failure)
	}
	for _, field := range []string{"api_key", "api_token"} {
		if _, present := data[field]; present {
			t.Errorf("%s survived the switch to the keyless mode: %v", field, data)
		}
	}
	// The key mode is untouched.
	keyed := map[string]any{"api_base": "https://dial.example.com", "api_key": "{{secret.k}}"}
	if failure := admitKeylessCredentialData("ai_dial", keyed); failure != nil || keyed["api_key"] != "{{secret.k}}" {
		t.Fatalf("the key mode was changed: %+v, %v", failure, keyed)
	}
}

// TestUnsavedCheckRefusesKeylessPayload: the unsaved check takes api_base from
// the caller, so it must never ask the gateway to mint the platform token.
func TestUnsavedCheckRefusesKeylessPayload(t *testing.T) {
	body := map[string]any{"api_base": "https://attacker.example.com", "auth_mode": "workload_identity"}
	recorder := httptest.NewRecorder()
	h := keylessHandler(t)
	h.connectionChecker = &recordingConnectionChecker{}
	h.CheckConnection(recorder, keylessRequest(t, http.MethodPost, body, false,
		map[string]string{"projectID": "1", "configType": "ai_dial"}))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), unsavedKeylessCheckReason) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if h.connectionChecker.(*recordingConnectionChecker).calls != 0 {
		t.Error("the gateway was asked to probe a keyless payload with a caller-chosen endpoint")
	}
	item := h.checkBatchItem(context.Background(), "1", map[string]any{"id": 1, "type": "ai_dial", "data": body})
	if item["success"] != false || item["reason"] != unsavedKeylessCheckReason {
		t.Fatalf("batch item = %v", item)
	}
}

type recordingConnectionChecker struct{ calls int }

func (c *recordingConnectionChecker) Check(context.Context, string, map[string]any) (ConnectionCheckResult, error) {
	c.calls++
	return ConnectionCheckResult{Success: true}, nil
}

// TestGlobalProviderListingReportsAuthMode: the mode and the audience are
// listed; a keyless row reports no secret, and a key row still hides its key.
func TestGlobalProviderListingReportsAuthMode(t *testing.T) {
	scan := func(data string) GlobalProvider {
		t.Helper()
		item, err := scanGlobalProvider(func(dest ...any) error {
			*dest[5].(*[]byte) = []byte(data)
			return nil
		})
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return item
	}
	keyless := scan(`{"api_base":"https://dial.example.com","auth_mode":"workload_identity","token_audience":"client-id"}`)
	if keyless.AuthMode != "workload_identity" || keyless.TokenAudience != "client-id" || len(keyless.Secrets) != 0 {
		t.Fatalf("keyless row = %+v", keyless)
	}
	keyed := scan(`{"api_base":"https://dial.example.com","api_key":"literal-key"}`)
	if keyed.AuthMode != "key" || len(keyed.Secrets) != 1 || keyed.Secrets[0].Sealed {
		t.Fatalf("key row = %+v", keyed)
	}
	raw, _ := json.Marshal(keyed)
	if strings.Contains(string(raw), "literal-key") {
		t.Fatalf("the listing disclosed a key: %s", raw)
	}
	if got := scan(`{"auth_mode":"ambient"}`).AuthMode; got != "ambient" {
		t.Errorf("an unknown stored mode was reported as %q", got)
	}
}

// TestCatalogueDescribesTheKeylessMode pins the snapshot fields the Admin UI
// renders from.
func TestCatalogueDescribesTheKeylessMode(t *testing.T) {
	h := keylessHandler(t)
	for _, configType := range []string{"vertex_ai", "ai_dial"} {
		schema, ok := h.catalog.DataSchemaByType(configType)
		if !ok {
			t.Fatalf("%s has no data schema", configType)
		}
		properties, _ := schema["properties"].(map[string]any)
		mode, _ := properties["auth_mode"].(map[string]any)
		enum, _ := mode["enum"].([]any)
		if len(enum) != 2 || enum[0] != "key" || enum[1] != "workload_identity" || mode["default"] != "key" {
			t.Errorf("%s auth_mode = %v", configType, mode)
		}
	}
	dial, _ := h.catalog.DataSchemaByType("ai_dial")
	if _, ok := dial["properties"].(map[string]any)["token_audience"]; !ok {
		t.Error("ai_dial has no token_audience")
	}
	vertex, _ := h.catalog.DataSchemaByType("vertex_ai")
	for _, field := range stringsOf(vertex["required"]) {
		if field == "vertex_credentials" {
			t.Error("vertex_credentials is required unconditionally; the keyless mode cannot be saved")
		}
	}
}
