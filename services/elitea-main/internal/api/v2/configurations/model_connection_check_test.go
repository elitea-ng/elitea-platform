package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
)

// gatewayStub stands in for elitea-llm-gateway's POST /llm/v1/check_connection.
// It records the decoded request and answers with a fixed body.
type gatewayStub struct {
	*httptest.Server
	hits     atomic.Int64
	requests []checkConnectionRequestBody
	status   int
	answer   string
}

func newGatewayStub(t *testing.T, status int, answer string) *gatewayStub {
	t.Helper()
	stub := &gatewayStub{status: status, answer: answer}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.hits.Add(1)
		if r.URL.Path != "/llm/v1/check_connection" {
			http.NotFound(w, r)
			return
		}
		var body checkConnectionRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		stub.requests = append(stub.requests, body)
		w.WriteHeader(stub.status)
		_, _ = w.Write([]byte(stub.answer))
	}))
	t.Cleanup(stub.Close)
	return stub
}

func resolvedOpenAICredential() map[string]any {
	return map[string]any{
		"ai_credentials": map[string]any{
			"elitea_title":             "openai_creds",
			"private":                  false,
			"api_base":                 "https://api.openai.com/v1",
			"api_key":                  "sk-resolved-on-the-server",
			"configuration_type":       "open_ai",
			"configuration_uuid":       "c0ffee",
			"configuration_project_id": int32(7),
		},
	}
}

func modelCheckHandler(stub *gatewayStub, resolver StoredConfigurationResolver) *Handler {
	options := []Option{WithStoredConfigurationResolver(resolver)}
	if stub != nil {
		options = append(options, WithConnectionChecker(NewGatewayConnectionChecker(stub.URL, http.DefaultTransport, "secret")))
	}
	return NewHandler(nil, options...)
}

func postModelCheck(t *testing.T, handler *Handler, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/check_connection/7/llm_model", strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "7")
	routeContext.URLParams.Add("configType", "llm_model")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	recorder := httptest.NewRecorder()
	handler.CheckConnection(recorder, request)
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder, decoded
}

const modelCheckForm = `{
	"name": " gpt-5 ",
	"api_protocol": "OpenAI",
	"ai_credentials": {"elitea_title": "openai_creds", "private": false, "api_base": "https://attacker.example/v1"},
	"description": "Fast"
}`

func TestLLMModelCheck_SuccessTestsTheUnsavedModelWithTheResolvedCredential(t *testing.T) {
	stub := newGatewayStub(t, http.StatusOK, `{"success":true,"reason":"ok","probe":"completion","latency_ms":1234}`)
	resolver := &recordingStoredResolver{resolved: resolvedOpenAICredential()}

	recorder, body := postModelCheck(t, modelCheckHandler(stub, resolver), modelCheckForm)
	if recorder.Code != http.StatusOK || body["success"] != true || body["message"] != "Connected" {
		t.Fatalf("status %d body %v, want a 200 Connected", recorder.Code, body)
	}
	if body["latency_ms"] != float64(1234) {
		t.Fatalf("latency_ms = %v", body["latency_ms"])
	}

	// The resolver received the REBUILT reference only: the posted api_base
	// beside the title must not reach the expansion.
	if len(resolver.requests) != 1 {
		t.Fatalf("resolver calls = %d", len(resolver.requests))
	}
	reference, _ := resolver.requests[0].Data["ai_credentials"].(map[string]any)
	if len(reference) != 2 || reference["elitea_title"] != "openai_creds" || reference["private"] != false {
		t.Fatalf("resolver reference = %v, want title and private only", reference)
	}
	if resolver.requests[0].ProjectID != 7 {
		t.Fatalf("resolved in project %d, want the path project", resolver.requests[0].ProjectID)
	}

	if len(stub.requests) != 1 {
		t.Fatalf("gateway calls = %d", len(stub.requests))
	}
	sent := stub.requests[0]
	if sent.Model != "gpt-5" || sent.APIProtocol != "openai" || sent.Type != "open_ai" {
		t.Fatalf("gateway request = %+v, want the trimmed model, the protocol and the credential type", sent)
	}
	if sent.APIBase != "https://api.openai.com/v1" || sent.APIKey != "sk-resolved-on-the-server" {
		t.Fatalf("gateway request = %+v, want the RESOLVED credential", sent)
	}
}

func TestLLMModelCheck_FailuresAreCategorised(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		want   string
	}{
		{"wrong model", `{"success":false,"reason":"model_not_found","detail":"The API deployment for this resource does not exist.","probe":"completion"}`,
			"Model not found: The API deployment for this resource does not exist."},
		{"wrong key", `{"success":false,"reason":"unauthorized","probe":"completion"}`, "Authentication failed"},
		{"timeout", `{"success":false,"reason":"timeout","probe":"completion","latency_ms":30000}`,
			"Timed out: The model did not answer within 30 seconds."},
		{"protocol", `{"success":false,"reason":"protocol_error","detail":"the endpoint answered, but not with a model completion","probe":"completion"}`,
			"Wrong API protocol or route: the endpoint answered, but not with a model completion"},
		{"rate limit", `{"success":false,"reason":"rate_limited","probe":"completion"}`, "Rate limited"},
		{"unreachable", `{"success":false,"reason":"unreachable","probe":"completion"}`, "Connection failed"},
		{"egress", `{"success":false,"reason":"egress_not_allowed","probe":"completion"}`,
			"Connection failed: This endpoint is not permitted by the platform's configuration."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newGatewayStub(t, http.StatusOK, tc.answer)
			recorder, body := postModelCheck(t,
				modelCheckHandler(stub, &recordingStoredResolver{resolved: resolvedOpenAICredential()}), modelCheckForm)
			if recorder.Code != http.StatusBadRequest || body["success"] != false || body["message"] != tc.want {
				t.Fatalf("status %d body %v, want 400 %q", recorder.Code, body, tc.want)
			}
		})
	}
}

// A gateway that predates the model probe ignores `model` and answers the
// credential listing. That answer must never read as "Connected".
func TestLLMModelCheck_GatewayWithoutTheModelProbeIsNotSuccess(t *testing.T) {
	stub := newGatewayStub(t, http.StatusOK, `{"success":true,"reason":"ok"}`)
	recorder, body := postModelCheck(t,
		modelCheckHandler(stub, &recordingStoredResolver{resolved: resolvedOpenAICredential()}), modelCheckForm)
	if recorder.Code != http.StatusBadRequest || body["success"] != false {
		t.Fatalf("status %d body %v, want a refusal", recorder.Code, body)
	}
}

func TestLLMModelCheck_RefusesBeforeTheGateway(t *testing.T) {
	cases := []struct {
		name     string
		form     string
		resolver *recordingStoredResolver
		want     string
	}{
		{"no model name", `{"name":"  ","ai_credentials":{"elitea_title":"c","private":false}}`,
			&recordingStoredResolver{resolved: resolvedOpenAICredential()}, "Enter the model name"},
		{"no credentials", `{"name":"m"}`,
			&recordingStoredResolver{resolved: resolvedOpenAICredential()}, "Select AI credentials"},
		{"credential does not resolve", modelCheckForm,
			&recordingStoredResolver{err: errors.New("not found")}, "could not be resolved"},
		{"toolkit credential", modelCheckForm,
			&recordingStoredResolver{resolved: map[string]any{"ai_credentials": map[string]any{"configuration_type": "github"}}},
			"not supported yet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newGatewayStub(t, http.StatusOK, `{"success":true,"probe":"completion"}`)
			recorder, body := postModelCheck(t, modelCheckHandler(stub, tc.resolver), tc.form)
			if recorder.Code != http.StatusBadRequest || body["success"] != false {
				t.Fatalf("status %d body %v, want a 400 refusal", recorder.Code, body)
			}
			message, _ := body["message"].(string)
			if !strings.Contains(message, tc.want) {
				t.Fatalf("message %q, want it to contain %q", message, tc.want)
			}
			if stub.hits.Load() != 0 {
				t.Fatalf("gateway was called %d times for a refused test", stub.hits.Load())
			}
		})
	}
}

func TestLLMModelCheck_NotComposedIsNotAvailable(t *testing.T) {
	recorder, body := postModelCheck(t, modelCheckHandler(nil, &recordingStoredResolver{}), modelCheckForm)
	if recorder.Code != http.StatusBadRequest || body["message"] != storedConnectionCheckUnavailableMessage {
		t.Fatalf("status %d body %v", recorder.Code, body)
	}
}

func TestModelConnectionMessageFor_CredentialReasonsKeepTheirWording(t *testing.T) {
	if got := modelConnectionMessageFor("missing_api_base", ""); got != connectionCheckMessages["missing_api_base"] {
		t.Fatalf("missing_api_base message = %q", got)
	}
	if got := modelConnectionMessageFor("missing_credential_fields", "this credential is missing: aws_region_name"); !strings.Contains(got, "aws_region_name") {
		t.Fatalf("field detail lost: %q", got)
	}
}
