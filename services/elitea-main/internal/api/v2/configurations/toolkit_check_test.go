package configurations_test

// The toolkit connection check (#319's toolkit half, toolkit_check.go).
//
// Every case here drives the REAL checker — the one production builds — against
// an httptest provider, so the four outcomes are measured against an actual
// round trip rather than against a double that could be told to answer
// anything. That is the property #319's own "done means" asks for, restated for
// the family the LLM path never covered.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
)

// allowAnyHost is the explicit "no host restriction" form the policy documents.
// Tests dial an httptest server on 127.0.0.1, which no realistic allowlist
// names, so the egress control is exercised by its own case below instead of
// standing between every other case and the provider.
func allowAnyHost() material.GitEgressPolicy {
	return material.ParseGitEgress("*", handler.ToolkitCheckAllowlistEnv)
}

// providerStub is an httptest provider that records what it was asked, so a
// case can assert the probe really carried the credential rather than only that
// a status came back.
type providerStub struct {
	server     *httptest.Server
	lastPath   string
	lastAuth   string
	lastToken  string
	statusCode int
}

func newProviderStub(t *testing.T, status int) *providerStub {
	t.Helper()
	stub := &providerStub{statusCode: status}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.lastPath = r.URL.Path
		stub.lastAuth = r.Header.Get("Authorization")
		stub.lastToken = r.Header.Get("PRIVATE-TOKEN")
		w.WriteHeader(stub.statusCode)
		_, _ = w.Write([]byte(`{"account_id":"real-provider-body"}`))
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func TestToolkitCheckReportsOkOnlyForAProviderThatAcceptedTheCredential(t *testing.T) {
	stub := newProviderStub(t, http.StatusOK)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)

	outcome := checker.CheckToolkit(context.Background(), "github", map[string]any{
		"base_url":     stub.server.URL,
		"access_token": "a-real-looking-token",
	})

	if outcome.Reason != handler.ToolkitCheckReasonOK || !outcome.Success() {
		t.Fatalf("a 200 from the provider must be ok, got %+v", outcome)
	}
	if stub.lastPath != "/user" {
		t.Fatalf("the github probe must read the identity endpoint, asked %q", stub.lastPath)
	}
	if stub.lastAuth != "Bearer a-real-looking-token" {
		t.Fatalf("the probe must carry the stored token, sent %q", stub.lastAuth)
	}
}

func TestToolkitCheckReportsAuthFailedForARefusedCredential(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		stub := newProviderStub(t, status)
		checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)

		outcome := checker.CheckToolkit(context.Background(), "github", map[string]any{
			"base_url":     stub.server.URL,
			"access_token": "wrong",
		})

		if outcome.Reason != handler.ToolkitCheckReasonAuthFailed {
			t.Fatalf("status %d must be auth_failed, got %+v", status, outcome)
		}
		if !strings.Contains(outcome.Message, "Authentication failed") {
			t.Fatalf("the message must say what happened, got %q", outcome.Message)
		}
		if outcome.Success() {
			t.Fatalf("a refusal must never report success: %+v", outcome)
		}
	}
}

func TestToolkitCheckReportsUnreachableWhenNothingAnswers(t *testing.T) {
	// A server that is already closed: the dial fails, which is the shape a
	// wrong host or a firewalled endpoint takes.
	stub := newProviderStub(t, http.StatusOK)
	address := stub.server.URL
	stub.server.Close()

	checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)
	outcome := checker.CheckToolkit(context.Background(), "gitlab", map[string]any{
		"url":           address,
		"private_token": "a-token",
	})

	if outcome.Reason != handler.ToolkitCheckReasonUnreachable {
		t.Fatalf("a failed dial must be unreachable, got %+v", outcome)
	}
	if outcome.Success() {
		t.Fatalf("an unreachable provider must never report success: %+v", outcome)
	}
}

func TestToolkitCheckReportsUnsupportedTypeForAFamilyWithNoProbe(t *testing.T) {
	checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)

	outcome := checker.CheckToolkit(context.Background(), "sharepoint", map[string]any{"base_url": "https://example.invalid"})

	if outcome.Reason != handler.ToolkitCheckReasonUnsupportedType {
		t.Fatalf("a type with no probe must be unsupported_type, got %+v", outcome)
	}
	if handler.IsToolkitCheckableType("sharepoint") {
		t.Fatal("IsToolkitCheckableType must agree with the probe table")
	}
}

func TestToolkitCheckReportsUnsupportedTypeForAnAuthMethodItCannotProbe(t *testing.T) {
	stub := newProviderStub(t, http.StatusOK)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)

	// A GitHub App credential: real, usable by the worker, and not something a
	// single GET can exercise. It must not be reported as a refusal.
	outcome := checker.CheckToolkit(context.Background(), "github", map[string]any{
		"base_url":        stub.server.URL,
		"app_id":          "12345",
		"app_private_key": "-----BEGIN RSA PRIVATE KEY-----",
	})

	if outcome.Reason != handler.ToolkitCheckReasonUnsupportedType {
		t.Fatalf("an unprobeable auth method must be unsupported_type, got %+v", outcome)
	}
	if stub.lastPath != "" {
		t.Fatalf("nothing must be dialled for a credential this build cannot authorise, asked %q", stub.lastPath)
	}
}

func TestToolkitCheckRefusesAHostTheAllowlistDoesNotName(t *testing.T) {
	stub := newProviderStub(t, http.StatusOK)
	policy := material.ParseGitEgress("api.github.com", handler.ToolkitCheckAllowlistEnv)
	checker := handler.NewToolkitConnectionChecker(policy, nil)

	outcome := checker.CheckToolkit(context.Background(), "jira", map[string]any{
		"base_url": stub.server.URL,
		"username": "someone@example.com",
		"api_key":  "a-token",
	})

	if outcome.Reason != handler.ToolkitCheckReasonUnreachable {
		t.Fatalf("a host off the allowlist must not be dialled, got %+v", outcome)
	}
	if stub.lastPath != "" {
		t.Fatalf("the egress check must run BEFORE the dial, but the provider was asked %q", stub.lastPath)
	}
	if strings.Contains(outcome.Message, "a-token") {
		t.Fatalf("the refusal must not carry the credential: %q", outcome.Message)
	}
}

func TestToolkitCheckNeverPutsTheSecretOrTheProviderBodyInTheMessage(t *testing.T) {
	stub := newProviderStub(t, http.StatusUnauthorized)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)

	const secret = "super-secret-token-value"
	outcome := checker.CheckToolkit(context.Background(), "bitbucket", map[string]any{
		"url":      stub.server.URL,
		"username": "someone",
		"password": secret,
	})

	if strings.Contains(outcome.Message, secret) {
		t.Fatalf("the message leaked the credential: %q", outcome.Message)
	}
	if strings.Contains(outcome.Message, "real-provider-body") {
		t.Fatalf("the message echoed the provider's body: %q", outcome.Message)
	}
	// …and the probe really did authenticate: a basic header built from the pair.
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("someone:"+secret))
	if stub.lastAuth != expected {
		t.Fatalf("the bitbucket probe must use basic auth, sent %q", stub.lastAuth)
	}
}

func TestToolkitProbesUseEachFamilysOwnIdentityEndpoint(t *testing.T) {
	cases := []struct {
		configType string
		data       map[string]any
		wantPath   string
		wantHeader func(stub *providerStub) string
	}{
		{
			configType: "gitlab",
			data:       map[string]any{"private_token": "gl-token"},
			wantPath:   "/api/v4/user",
			wantHeader: func(stub *providerStub) string { return stub.lastToken },
		},
		{
			configType: "jira",
			data:       map[string]any{"username": "u", "api_key": "k", "api_version": "3"},
			wantPath:   "/rest/api/3/myself",
			wantHeader: func(stub *providerStub) string { return stub.lastAuth },
		},
		{
			configType: "confluence",
			data:       map[string]any{"username": "u", "api_key": "k"},
			wantPath:   "/rest/api/user/current",
			wantHeader: func(stub *providerStub) string { return stub.lastAuth },
		},
		{
			configType: "bitbucket",
			data:       map[string]any{"username": "u", "password": "p"},
			wantPath:   "/2.0/user",
			wantHeader: func(stub *providerStub) string { return stub.lastAuth },
		},
	}

	for _, testCase := range cases {
		stub := newProviderStub(t, http.StatusOK)
		data := map[string]any{"base_url": stub.server.URL, "url": stub.server.URL}
		for key, value := range testCase.data {
			data[key] = value
		}

		checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)
		outcome := checker.CheckToolkit(context.Background(), testCase.configType, data)

		if outcome.Reason != handler.ToolkitCheckReasonOK {
			t.Fatalf("%s: expected ok, got %+v", testCase.configType, outcome)
		}
		if stub.lastPath != testCase.wantPath {
			t.Fatalf("%s: probe asked %q, want %q", testCase.configType, stub.lastPath, testCase.wantPath)
		}
		if testCase.wantHeader(stub) == "" {
			t.Fatalf("%s: the probe carried no credential at all", testCase.configType)
		}
	}
}

func TestToolkitCheckKeepsABaseUrlsOwnPathPrefix(t *testing.T) {
	// A Confluence Cloud base URL ends in /wiki. Dropping it makes every check
	// answer 404, which this build would report as unreachable — a working
	// credential shown as broken.
	stub := newProviderStub(t, http.StatusOK)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)

	outcome := checker.CheckToolkit(context.Background(), "confluence", map[string]any{
		"base_url": stub.server.URL + "/wiki",
		"username": "u",
		"api_key":  "k",
	})

	if outcome.Reason != handler.ToolkitCheckReasonOK {
		t.Fatalf("expected ok, got %+v", outcome)
	}
	if stub.lastPath != "/wiki/rest/api/user/current" {
		t.Fatalf("the base URL's own path was dropped: asked %q", stub.lastPath)
	}
}

func TestToolkitCheckRefusesANonHttpBaseUrl(t *testing.T) {
	checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)

	outcome := checker.CheckToolkit(context.Background(), "github", map[string]any{
		"base_url":     "file:///etc/passwd",
		"access_token": "t",
	})

	if outcome.Reason != handler.ToolkitCheckReasonUnsupportedType {
		t.Fatalf("a non-http scheme must not be dialled, got %+v", outcome)
	}
}

/* ── the route ─────────────────────────────────────────────────────────────
 *
 * The handler half: POST /check_connection/{projectID}/{configType} answered
 * "Checking connection is not supported yet for configuration type github" for
 * every toolkit type before this change, whatever the credential was.
 */

func setupConfigRouterWithToolkitChecker(checker handler.ToolkitConnectionChecker) *chi.Mux {
	h := handler.NewHandler(nil,
		handler.WithToolkitConnectionChecker(checker),
		handler.WithPermissionResolver(entitledResolver()),
	)
	router := chi.NewRouter()
	router.Use(withTestUser)
	router.Mount("/api/v2", h.Routes())
	return router
}

func postCheckConnection(t *testing.T, router *chi.Mux, configType string, body string) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost,
		"/api/v2/check_connection/1/"+url.PathEscape(configType),
		strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("the response body is not JSON: %q", recorder.Body.String())
	}
	return recorder.Code, decoded
}

func TestCheckConnectionRouteReportsTheToolkitProbesOutcome(t *testing.T) {
	stub := newProviderStub(t, http.StatusOK)
	router := setupConfigRouterWithToolkitChecker(handler.NewToolkitConnectionChecker(allowAnyHost(), nil))

	status, body := postCheckConnection(t, router, "github",
		`{"base_url":"`+stub.server.URL+`","access_token":"t"}`)

	if status != http.StatusOK {
		t.Fatalf("a provider that accepted the credential must answer 200, got %d: %v", status, body)
	}
	if body["success"] != true {
		t.Fatalf("success must be true: %v", body)
	}
	if body["reason"] != handler.ToolkitCheckReasonOK {
		t.Fatalf("the route must publish the machine-readable reason: %v", body)
	}
}

func TestCheckConnectionRouteReportsAuthFailedForAToolkitCredentialTheProviderRefuses(t *testing.T) {
	stub := newProviderStub(t, http.StatusUnauthorized)
	router := setupConfigRouterWithToolkitChecker(handler.NewToolkitConnectionChecker(allowAnyHost(), nil))

	status, body := postCheckConnection(t, router, "jira",
		`{"base_url":"`+stub.server.URL+`","username":"u","api_key":"wrong"}`)

	if status != http.StatusBadRequest {
		t.Fatalf("a refused credential must answer 400, got %d: %v", status, body)
	}
	if body["success"] != false {
		t.Fatalf("success must be false: %v", body)
	}
	if body["reason"] != handler.ToolkitCheckReasonAuthFailed {
		t.Fatalf("the route must report auth_failed, got %v", body)
	}
	message, _ := body["message"].(string)
	if strings.Contains(message, "not supported yet") {
		t.Fatal("a probed toolkit type must no longer answer the not-supported message")
	}
	if strings.Contains(message, "wrong") {
		t.Fatalf("the message leaked the credential: %q", message)
	}
}

func TestCheckConnectionRouteStillRefusesAToolkitTypeWithNoProbe(t *testing.T) {
	router := setupConfigRouterWithToolkitChecker(handler.NewToolkitConnectionChecker(allowAnyHost(), nil))

	// `sharepoint` is a real catalogue type with no probe: it must keep the
	// honest "not supported yet" answer rather than becoming an error.
	status, body := postCheckConnection(t, router, "sharepoint", `{"base_url":"https://example.invalid"}`)

	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %v", status, body)
	}
	message, _ := body["message"].(string)
	if !strings.Contains(message, "not supported yet") {
		t.Fatalf("expected the not-supported message, got %q", message)
	}
	if body["reason"] != handler.ToolkitCheckReasonUnsupportedType {
		t.Fatalf("expected unsupported_type, got %v", body)
	}
}
