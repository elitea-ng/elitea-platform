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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

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

	cases := []struct {
		name       string
		configType string
		data       map[string]any
	}{
		{
			// GitLab authenticates this probe with the personal access token
			// header; a user pair is a shape the platform stores for other
			// families and cannot present here. The credential may be fine.
			name:       "a gitlab credential carrying a user pair",
			configType: "gitlab",
			data:       map[string]any{"url": stub.server.URL, "username": "someone", "password": "a-password"},
		},
		{
			// A row with an endpoint and no authentication material at all.
			name:       "a github credential carrying no authentication material",
			configType: "github",
			data:       map[string]any{"base_url": stub.server.URL},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stub.lastPath = ""

			outcome := checker.CheckToolkit(context.Background(), testCase.configType, testCase.data)

			if outcome.Reason != handler.ToolkitCheckReasonUnsupportedType {
				t.Fatalf("an unprobeable auth method must be unsupported_type, got %+v", outcome)
			}
			if stub.lastPath != "" {
				t.Fatalf("nothing must be dialled for a credential this build cannot authorise, asked %q", stub.lastPath)
			}
		})
	}
}

/* ── the GitHub App shape (#857) ──────────────────────────────────────────────
 *
 * Before #857 an app_id + app_private_key credential answered unsupported_type
 * — the same answer a type with no probe at all gets — so the one GitHub
 * authentication shape a platform team is most likely to deploy could never be
 * verified, right or wrong. These cases drive the real checker against an
 * httptest provider, one per outcome.
 *
 * The key is GENERATED here, per run. No private key, real or sample, is
 * committed: a PEM in a repository is a PEM someone will paste into a form.
 */

// testAppKey generates an RSA key and returns it with its PKCS#1 PEM, the form
// GitHub hands out when an App key is created.
func testAppKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	// 2048 is the smallest size GitHub issues, and the slowest part of this
	// file; anything larger buys the assertions nothing.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a test key: %v", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return key, string(encoded)
}

// pkcs8PEM re-encodes the same key in the form other tooling round-trips it to.
func pkcs8PEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("re-encoding the test key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestToolkitCheckProbesAGitHubAppWithAJWTSignedByTheStoredKey(t *testing.T) {
	key, pkcs1 := testAppKey(t)

	for _, encoding := range []struct {
		name string
		pem  string
	}{
		{name: "PKCS#1, the form GitHub issues", pem: pkcs1},
		{name: "PKCS#8, the form other tooling writes", pem: pkcs8PEM(t, key)},
	} {
		t.Run(encoding.name, func(t *testing.T) {
			stub := newProviderStub(t, http.StatusOK)
			checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)
			before := time.Now()

			outcome := checker.CheckToolkit(context.Background(), "github", map[string]any{
				"base_url":        stub.server.URL,
				"app_id":          "424242",
				"app_private_key": encoding.pem,
			})

			if outcome.Reason != handler.ToolkitCheckReasonOK || !outcome.Success() {
				t.Fatalf("a 200 from the provider must be ok, got %+v", outcome)
			}
			// An App JWT is not a user. GitHub answers 403 to /user for one, so
			// a probe that kept the user endpoint would report auth_failed for
			// a credential that is in fact good.
			if stub.lastPath != "/app" {
				t.Fatalf("the App probe must read the App identity endpoint, asked %q", stub.lastPath)
			}

			presented := strings.TrimPrefix(stub.lastAuth, "Bearer ")
			if presented == stub.lastAuth || presented == "" {
				t.Fatalf("the App probe must present a bearer token, sent %q", stub.lastAuth)
			}
			if strings.Contains(stub.lastAuth, "PRIVATE KEY") {
				t.Fatalf("the probe presented key material rather than a JWT: %q", stub.lastAuth)
			}

			claims := &jwt.RegisteredClaims{}
			parsed, err := jwt.ParseWithClaims(presented, claims, func(*jwt.Token) (any, error) {
				return &key.PublicKey, nil
			}, jwt.WithValidMethods([]string{"RS256"}))
			if err != nil || !parsed.Valid {
				t.Fatalf("the token must be an RS256 JWT signed by the stored key: %v", err)
			}
			if claims.Issuer != "424242" {
				t.Fatalf("iss must be the App id, got %q", claims.Issuer)
			}
			if claims.IssuedAt == nil || claims.ExpiresAt == nil {
				t.Fatal("the JWT must carry both iat and exp")
			}
			if !claims.IssuedAt.Before(before.Add(time.Second)) {
				t.Fatalf("iat must not be in the provider's future, got %v", claims.IssuedAt.Time)
			}
			// GitHub refuses a JWT whose exp is more than ten minutes after its
			// iat, and one that has already expired.
			if window := claims.ExpiresAt.Sub(claims.IssuedAt.Time); window <= 0 || window > 10*time.Minute {
				t.Fatalf("exp - iat must be a positive window of at most ten minutes, got %v", window)
			}
			if !claims.ExpiresAt.After(before) {
				t.Fatalf("the JWT must still be valid when it is presented, exp %v", claims.ExpiresAt.Time)
			}
		})
	}
}

func TestToolkitCheckReportsAuthFailedForAGitHubAppTheProviderRefuses(t *testing.T) {
	_, keyPEM := testAppKey(t)
	stub := newProviderStub(t, http.StatusUnauthorized)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)

	outcome := checker.CheckToolkit(context.Background(), "github", map[string]any{
		"base_url":        stub.server.URL,
		"app_id":          "424242",
		"app_private_key": keyPEM,
	})

	if outcome.Reason != handler.ToolkitCheckReasonAuthFailed {
		t.Fatalf("a 401 on the App endpoint must be auth_failed, got %+v", outcome)
	}
	if stub.lastPath != "/app" {
		t.Fatalf("the refusal must have come from a real round trip, asked %q", stub.lastPath)
	}
}

func TestToolkitCheckReportsUnreachableWhenTheGitHubAppEndpointDoesNotAnswer(t *testing.T) {
	_, keyPEM := testAppKey(t)
	stub := newProviderStub(t, http.StatusOK)
	address := stub.server.URL
	stub.server.Close() // nothing listens now: the dial itself fails.
	checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)

	outcome := checker.CheckToolkit(context.Background(), "github", map[string]any{
		"base_url":        address,
		"app_id":          "424242",
		"app_private_key": keyPEM,
	})

	if outcome.Reason != handler.ToolkitCheckReasonUnreachable {
		t.Fatalf("a dial that fails must be unreachable, got %+v", outcome)
	}
	if strings.Contains(outcome.Message, address) {
		t.Fatalf("the message must not carry the tenant-authored URL: %q", outcome.Message)
	}
}

func TestToolkitCheckReportsAuthFailedForAGitHubAppKeyItCannotRead(t *testing.T) {
	_, goodPEM := testAppKey(t)
	// The header line alone, which is what a truncated paste leaves behind.
	const truncated = "-----BEGIN RSA PRIVATE KEY-----"
	const notAKey = "hunter2-this-is-not-a-pem-block"

	cases := []struct {
		name string
		data map[string]any
	}{
		{name: "a truncated PEM", data: map[string]any{"app_id": "424242", "app_private_key": truncated}},
		{name: "material that is not a PEM block at all", data: map[string]any{"app_id": "424242", "app_private_key": notAKey}},
		{name: "a readable key with no App id", data: map[string]any{"app_private_key": goodPEM}},
		{name: "an App id with no key", data: map[string]any{"app_id": "424242"}},
	}

	var messages []string
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stub := newProviderStub(t, http.StatusOK)
			checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)
			data := map[string]any{"base_url": stub.server.URL}
			for key, value := range testCase.data {
				data[key] = value
			}

			outcome := checker.CheckToolkit(context.Background(), "github", data)

			// The METHOD is supported now, so an App credential this build
			// cannot sign with is a verdict on the credential — not the
			// "this platform cannot check this" answer #857 was filed about.
			if outcome.Reason != handler.ToolkitCheckReasonAuthFailed {
				t.Fatalf("an unusable App credential must be auth_failed, got %+v", outcome)
			}
			if stub.lastPath != "" {
				t.Fatalf("there is nothing to ask the provider, but it was asked %q", stub.lastPath)
			}
			for _, secret := range []string{truncated, notAKey, goodPEM, "PRIVATE KEY"} {
				if strings.Contains(outcome.Message, secret) {
					t.Fatalf("the message echoed the key material: %q", outcome.Message)
				}
			}
			if outcome.Message == "" {
				t.Fatal("a refusal with no message leaves the card nothing true to say")
			}
			messages = append(messages, outcome.Message)
		})
	}

	// One fixed message for every way the signing can fail: a message that
	// varied with the cause would report on the key material by implication.
	for _, message := range messages {
		if message != messages[0] {
			t.Fatalf("the App refusal message must be fixed, got %q and %q", messages[0], message)
		}
	}
}

func TestToolkitCheckStillReadsTheUserEndpointForANonAppGitHubCredential(t *testing.T) {
	_, keyPEM := testAppKey(t)
	stub := newProviderStub(t, http.StatusOK)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost(), nil)

	// A row that carries BOTH a token and App fields: the token wins, and the
	// endpoint has to follow the credential that was actually presented.
	outcome := checker.CheckToolkit(context.Background(), "github", map[string]any{
		"base_url":        stub.server.URL,
		"access_token":    "a-real-looking-token",
		"app_id":          "424242",
		"app_private_key": keyPEM,
	})

	if outcome.Reason != handler.ToolkitCheckReasonOK {
		t.Fatalf("a token credential must still be probed as before, got %+v", outcome)
	}
	if stub.lastPath != "/user" {
		t.Fatalf("a token authenticates a user, so the probe must read /user, asked %q", stub.lastPath)
	}
	if stub.lastAuth != "Bearer a-real-looking-token" {
		t.Fatalf("the stored token must win over the App fields, sent %q", stub.lastAuth)
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
