package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// These tests pin the security-relevant bindings of the llm_model test: which
// identity a private reference resolves against, the self-referential guard
// on the RESOLVED credential, the platform-credential rule and the rate
// bound. The route-level permission is pinned in handler_test.go.

func postModelCheckAs(t *testing.T, handler *Handler, user *auth.User, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/check_connection/7/llm_model", strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "7")
	routeContext.URLParams.Add("configType", "llm_model")
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)
	if user != nil {
		ctx = auth.ContextWithUser(ctx, *user)
	}
	recorder := httptest.NewRecorder()
	handler.CheckConnection(recorder, request.WithContext(ctx))
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder, decoded
}

const privateModelForm = `{"name":"gpt-5","ai_credentials":{"elitea_title":"my_openai","private":true}}`

func okGateway(t *testing.T) *gatewayStub {
	return newGatewayStub(t, http.StatusOK, `{"success":true,"reason":"ok","probe":"completion","latency_ms":5}`)
}

// savedRow is an llm_model row authored by user 42.
func savedRow(model string, private bool) storedConfigurationRow {
	author := 42
	return storedConfigurationRow{
		id: 11, configType: llmModelConfigurationType, authorID: &author,
		data: map[string]any{
			"name":           model,
			"ai_credentials": map[string]any{"elitea_title": "my_openai", "private": private},
		},
	}
}

func withRow(handler *Handler, row storedConfigurationRow, found bool) *Handler {
	handler.llmModelRow = func(_ context.Context, projectID, configID string) (storedConfigurationRow, bool, error) {
		if projectID != "7" || configID != "11" {
			return storedConfigurationRow{}, false, nil
		}
		return row, found, nil
	}
	return handler
}

func TestLLMModelCheck_CreateResolvesAPrivateReferenceForTheCaller(t *testing.T) {
	resolver := &recordingStoredResolver{resolved: resolvedOpenAICredential()}
	user := auth.User{ID: "9"}
	recorder, body := postModelCheckAs(t, modelCheckHandler(okGateway(t), resolver), &user, privateModelForm)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d body %v", recorder.Code, body)
	}
	if got := resolver.requests[0].AuthorID; got == nil || *got != 9 {
		t.Fatalf("AuthorID = %v, want the caller (9): on the create form the caller becomes the author", got)
	}
}

func TestLLMModelCheck_PrincipalWithoutOwningUserResolvesNoPrivateReference(t *testing.T) {
	// A token principal carries no owning user here, so no personal project
	// can be named: the resolver gets no author and refuses the private ref.
	resolver := &recordingStoredResolver{resolved: resolvedOpenAICredential()}
	user := auth.User{ID: "5", TokenID: "5", AuthType: "token"}
	_, _ = postModelCheckAs(t, modelCheckHandler(okGateway(t), resolver), &user, privateModelForm)
	if len(resolver.requests) != 1 || resolver.requests[0].AuthorID != nil {
		t.Fatalf("requests = %+v, want one resolution with no author", resolver.requests)
	}
}

func TestLLMModelCheck_EditResolvesAgainstTheRowAuthor(t *testing.T) {
	// User 9 edits a row that user 42 authored and tests it AS SAVED. The
	// runtime resolves the private reference in 42's personal project, so the
	// test must too, not in the caller's.
	resolver := &recordingStoredResolver{resolved: resolvedOpenAICredential()}
	handler := withRow(modelCheckHandler(okGateway(t), resolver), savedRow("gpt-5", true), true)
	user := auth.User{ID: "9"}
	form := `{"configuration_id":11,"name":"gpt-5","ai_credentials":{"elitea_title":"my_openai","private":true}}`
	recorder, body := postModelCheckAs(t, handler, &user, form)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d body %v", recorder.Code, body)
	}
	if got := resolver.requests[0].AuthorID; got == nil || *got != 42 {
		t.Fatalf("AuthorID = %v, want the row author (42)", got)
	}
}

func TestLLMModelCheck_EditByTheAuthorMayChangeValues(t *testing.T) {
	resolver := &recordingStoredResolver{resolved: resolvedOpenAICredential()}
	handler := withRow(modelCheckHandler(okGateway(t), resolver), savedRow("gpt-4o", true), true)
	user := auth.User{ID: "42"}
	form := `{"configuration_id":"11","name":"gpt-5","ai_credentials":{"elitea_title":"my_openai","private":true}}`
	recorder, body := postModelCheckAs(t, handler, &user, form)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d body %v", recorder.Code, body)
	}
	if got := resolver.requests[0].AuthorID; got == nil || *got != 42 {
		t.Fatalf("AuthorID = %v, want 42", got)
	}
}

func TestLLMModelCheck_NonAuthorCannotSpendAPrivateCredentialOnOtherValues(t *testing.T) {
	resolver := &recordingStoredResolver{resolved: resolvedOpenAICredential()}
	stub := okGateway(t)
	handler := withRow(modelCheckHandler(stub, resolver), savedRow("gpt-4o", true), true)
	user := auth.User{ID: "9"}
	form := `{"configuration_id":11,"name":"o3-pro","ai_credentials":{"elitea_title":"my_openai","private":true}}`
	recorder, body := postModelCheckAs(t, handler, &user, form)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(body["message"].(string), "private AI credentials") {
		t.Fatalf("status %d body %v, want the private-credential refusal", recorder.Code, body)
	}
	if len(resolver.requests) != 0 || stub.hits.Load() != 0 {
		t.Fatal("a refused test resolved the credential or called the gateway")
	}
}

func TestLLMModelCheck_EditOfAnUnknownRowIsNotFound(t *testing.T) {
	resolver := &recordingStoredResolver{resolved: resolvedOpenAICredential()}
	handler := withRow(modelCheckHandler(okGateway(t), resolver), storedConfigurationRow{}, false)
	user := auth.User{ID: "9"}
	recorder, _ := postModelCheckAs(t, handler, &user,
		`{"configuration_id":12,"name":"m","ai_credentials":{"elitea_title":"c","private":true}}`)
	if recorder.Code != http.StatusNotFound || len(resolver.requests) != 0 {
		t.Fatalf("status %d, resolutions %d, want 404 and none", recorder.Code, len(resolver.requests))
	}
}

func TestLLMModelCheck_EditOfAnotherTypeIsNotFound(t *testing.T) {
	row := savedRow("m", true)
	row.configType = "open_ai"
	resolver := &recordingStoredResolver{resolved: resolvedOpenAICredential()}
	handler := withRow(modelCheckHandler(okGateway(t), resolver), row, true)
	user := auth.User{ID: "42"}
	recorder, _ := postModelCheckAs(t, handler, &user,
		`{"configuration_id":11,"name":"m","ai_credentials":{"elitea_title":"my_openai","private":true}}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", recorder.Code)
	}
}

func TestLLMModelCheck_ResolvedSelfReferentialCredentialIsRefused(t *testing.T) {
	selfLLMOrigins()
	previous := selfOriginsCached
	selfOriginsCached = []string{"https://elitea.example/llm/v1"}
	t.Cleanup(func() { selfOriginsCached = previous })

	resolved := resolvedOpenAICredential()
	resolved["ai_credentials"].(map[string]any)["api_base"] = "https://elitea.example/llm/v1"
	stub := okGateway(t)
	recorder, body := postModelCheck(t, modelCheckHandler(stub, &recordingStoredResolver{resolved: resolved}), modelCheckForm)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(body["message"].(string), SelfReferentialCredentialReason) {
		t.Fatalf("status %d body %v, want the self-referential refusal", recorder.Code, body)
	}
	if stub.hits.Load() != 0 {
		t.Fatal("the gateway was called for a credential that points at the platform itself")
	}
}

func platformCredential() map[string]any {
	resolved := resolvedOpenAICredential()
	resolved["ai_credentials"].(map[string]any)["configuration_project_id"] = int32(1)
	return resolved
}

func TestLLMModelCheck_PlatformCredentialTestsOnlyAPublishedModel(t *testing.T) {
	stub := okGateway(t)
	handler := modelCheckHandler(stub, &recordingStoredResolver{resolved: platformCredential()})
	handler.publicProjectID = 1
	var asked []string
	handler.platformModelExposed = func(_ context.Context, model, title string) (bool, error) {
		asked = append(asked, model+"|"+title)
		return false, nil
	}
	recorder, body := postModelCheck(t, handler, modelCheckForm)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(body["message"].(string), "belong to the platform") {
		t.Fatalf("status %d body %v, want the platform-credential refusal", recorder.Code, body)
	}
	if stub.hits.Load() != 0 {
		t.Fatal("an unpublished model was sent with the platform key")
	}
	if len(asked) != 1 || asked[0] != "gpt-5|openai_creds" {
		t.Fatalf("asked = %v", asked)
	}

	handler.platformModelExposed = func(context.Context, string, string) (bool, error) { return true, nil }
	if recorder, body := postModelCheck(t, handler, modelCheckForm); recorder.Code != http.StatusOK {
		t.Fatalf("status %d body %v, want a published model to be tested", recorder.Code, body)
	}

	handler.platformModelExposed = func(context.Context, string, string) (bool, error) { return false, errors.New("db down") }
	if recorder, body := postModelCheck(t, handler, modelCheckForm); recorder.Code != http.StatusBadRequest ||
		body["message"] != storedConnectionCheckUnavailableMessage {
		t.Fatalf("status %d body %v, want a fail-closed refusal", recorder.Code, body)
	}
}

func TestLLMModelCheck_ProjectCredentialIsNotAPlatformCredential(t *testing.T) {
	handler := modelCheckHandler(okGateway(t), &recordingStoredResolver{resolved: resolvedOpenAICredential()})
	handler.publicProjectID = 1
	handler.platformModelExposed = func(context.Context, string, string) (bool, error) {
		t.Fatal("a project credential must not be checked against the platform models")
		return false, nil
	}
	if recorder, body := postModelCheck(t, handler, modelCheckForm); recorder.Code != http.StatusOK {
		t.Fatalf("status %d body %v", recorder.Code, body)
	}
}

func TestLLMModelCheck_RateBoundPerProjectAndUser(t *testing.T) {
	stub := okGateway(t)
	handler := modelCheckHandler(stub, &recordingStoredResolver{resolved: resolvedOpenAICredential()})
	now := time.Unix(1_700_000_000, 0)
	handler.modelProbes.now = func() time.Time { return now }
	user := auth.User{ID: "9"}
	for i := range modelProbeBurst {
		if recorder, body := postModelCheckAs(t, handler, &user, modelCheckForm); recorder.Code != http.StatusOK {
			t.Fatalf("test %d: status %d body %v", i, recorder.Code, body)
		}
	}
	recorder, body := postModelCheckAs(t, handler, &user, modelCheckForm)
	if recorder.Code != http.StatusTooManyRequests || body["reason"] != "rate_limited" {
		t.Fatalf("status %d body %v, want 429 rate_limited", recorder.Code, body)
	}
	if stub.hits.Load() != modelProbeBurst {
		t.Fatalf("gateway hits = %d, want %d", stub.hits.Load(), modelProbeBurst)
	}
	// Another user of the same project has a bound of their own.
	other := auth.User{ID: "10"}
	if recorder, _ := postModelCheckAs(t, handler, &other, modelCheckForm); recorder.Code != http.StatusOK {
		t.Fatalf("status %d for another user", recorder.Code)
	}
	// The bucket refills.
	now = now.Add(modelProbeRefill)
	if recorder, _ := postModelCheckAs(t, handler, &user, modelCheckForm); recorder.Code != http.StatusOK {
		t.Fatalf("status %d after the refill", recorder.Code)
	}
}

func TestModelProbeLimiter_ConcurrencyBound(t *testing.T) {
	limiter := newModelProbeLimiter()
	releases := make([]func(), 0, modelProbeInFlight)
	for range modelProbeInFlight {
		release := limiter.acquire("7/9")
		if release == nil {
			t.Fatal("a test under the bound was refused")
		}
		releases = append(releases, release)
	}
	if limiter.acquire("7/9") != nil {
		t.Fatal("a test over the concurrency bound was admitted")
	}
	releases[0]()
	releases[0]() // a second release must not free a second slot
	if release := limiter.acquire("7/9"); release == nil {
		t.Fatal("a released slot was not reused")
	}
	if limiter.acquire("7/9") != nil {
		t.Fatal("a double release freed two slots")
	}
}

func TestModelProbeLimiter_DropsIdleEntries(t *testing.T) {
	limiter := newModelProbeLimiter()
	now := time.Unix(1_700_000_000, 0)
	limiter.now = func() time.Time { return now }
	for i := range modelProbeMaxKeys {
		if release := limiter.acquire("p/" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)); release != nil {
			release()
		}
	}
	now = now.Add(time.Hour)
	limiter.acquire("new")
	if len(limiter.buckets) > 1 {
		t.Fatalf("buckets = %d, want the idle entries dropped", len(limiter.buckets))
	}
}

func TestLLMModelCheck_ForwardsTheAnthropicEndpointFlag(t *testing.T) {
	resolved := resolvedOpenAICredential()
	credential := resolved["ai_credentials"].(map[string]any)
	credential["configuration_type"] = "vllm"
	credential["api_base"] = "http://vllm.internal:8000"
	credential["use_anthropic_endpoints"] = true
	stub := okGateway(t)
	if recorder, body := postModelCheck(t, modelCheckHandler(stub, &recordingStoredResolver{resolved: resolved}), modelCheckForm); recorder.Code != http.StatusOK {
		t.Fatalf("status %d body %v", recorder.Code, body)
	}
	if !stub.requests[0].UseAnthropicEndpoints {
		t.Fatal("use_anthropic_endpoints did not reach the gateway")
	}
}

// TestLLMModelCheck_ForwardsTheDialProtocol: the gateway probes the route
// the runtime uses for the model's DIAL protocol, so the form's protocol must
// reach it. It is a field of an ai_dial model only.
func TestLLMModelCheck_ForwardsTheDialProtocol(t *testing.T) {
	dialCredential := func(configType string) map[string]any {
		resolved := resolvedOpenAICredential()
		credential := resolved["ai_credentials"].(map[string]any)
		credential["configuration_type"] = configType
		credential["api_base"] = "https://dial.example"
		return resolved
	}
	form := func(protocol string) string {
		return `{"name":"gemini-2.5-pro","dial_protocol":"` + protocol + `",` +
			`"ai_credentials":{"elitea_title":"openai_creds","private":false}}`
	}
	for _, tc := range []struct {
		name, configType, protocol, want string
	}{
		{name: "openai on ai_dial", configType: "ai_dial", protocol: "openai", want: "openai"},
		{name: "anthropic on ai_dial", configType: "ai_dial", protocol: "anthropic", want: "anthropic"},
		{name: "azure on ai_dial", configType: "ai_dial", protocol: "azure", want: "azure"},
		{name: "another credential type", configType: "azure_open_ai", protocol: "openai", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := okGateway(t)
			handler := modelCheckHandler(stub, &recordingStoredResolver{resolved: dialCredential(tc.configType)})
			if recorder, body := postModelCheck(t, handler, form(tc.protocol)); recorder.Code != http.StatusOK {
				t.Fatalf("status %d body %v", recorder.Code, body)
			}
			if got := stub.requests[0].DialProtocol; got != tc.want {
				t.Fatalf("dial_protocol on the wire = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("absent", func(t *testing.T) {
		stub := okGateway(t)
		handler := modelCheckHandler(stub, &recordingStoredResolver{resolved: dialCredential("ai_dial")})
		body := `{"name":"gpt-4o","ai_credentials":{"elitea_title":"openai_creds","private":false}}`
		if recorder, decoded := postModelCheck(t, handler, body); recorder.Code != http.StatusOK {
			t.Fatalf("status %d body %v", recorder.Code, decoded)
		}
		if got := stub.requests[0].DialProtocol; got != "" {
			t.Fatalf("dial_protocol on the wire = %q, want none for the default", got)
		}
	})
}

// TestLLMModelCheck_RefusesAnInvalidDialProtocol: a protocol the save refuses
// is refused before the gateway, with the save's message.
func TestLLMModelCheck_RefusesAnInvalidDialProtocol(t *testing.T) {
	for name, body := range map[string]string{
		"unknown":          `{"name":"gpt-4o","dial_protocol":"bogus","ai_credentials":{"elitea_title":"openai_creds","private":false}}`,
		"openai on claude": `{"name":"claude-sonnet-4-5","dial_protocol":"openai","ai_credentials":{"elitea_title":"openai_creds","private":false}}`,
		"anthropic on gpt": `{"name":"gpt-4o","dial_protocol":"anthropic","ai_credentials":{"elitea_title":"openai_creds","private":false}}`,
	} {
		t.Run(name, func(t *testing.T) {
			stub := okGateway(t)
			handler := modelCheckHandler(stub, &recordingStoredResolver{resolved: resolvedOpenAICredential()})
			recorder, decoded := postModelCheck(t, handler, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status %d body %v, want 400", recorder.Code, decoded)
			}
			if message, _ := decoded["message"].(string); !strings.Contains(message, "dial_protocol") {
				t.Errorf("message = %q, want it to name dial_protocol", message)
			}
			if stub.hits.Load() != 0 {
				t.Fatalf("the gateway was called %d times; want none", stub.hits.Load())
			}
		})
	}
}

func TestLLMModelCheck_SignsTheCallerIntoTheGatewayIdentity(t *testing.T) {
	var userHeader string
	stub := newGatewayStub(t, http.StatusOK, `{"success":true,"probe":"completion"}`)
	stub.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userHeader = r.Header.Get("X-Elitea-User-Id")
		_, _ = w.Write([]byte(`{"success":true,"probe":"completion"}`))
	})
	user := auth.User{ID: "9"}
	if recorder, _ := postModelCheckAs(t, modelCheckHandler(stub, &recordingStoredResolver{resolved: resolvedOpenAICredential()}), &user, modelCheckForm); recorder.Code != http.StatusOK {
		t.Fatalf("status %d", recorder.Code)
	}
	if userHeader != "9" {
		t.Fatalf("user header = %q, want the caller", userHeader)
	}
}
