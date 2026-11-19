package configurations_test

// The ten credential types #2143 reported as answering "Checking connection is
// not supported yet" (toolkit_check_vendors.go, toolkit_check_mint.go). Every
// probe is driven through the REAL checker against an httptest provider, and
// each is measured on the two answers that matter to the credential card: ok
// for a provider that accepts the stored credential and auth_failed for one
// that refuses it, plus the request each one actually put on the wire.

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
)

// recordedRequest is what a provider saw.
type recordedRequest struct {
	method, uri, auth, contentType, body string
}

// vendorStub answers every request with one status and body, recording the
// requests in order.
type vendorStub struct {
	server   *httptest.Server
	requests []recordedRequest
}

func newVendorStub(t *testing.T, status int, body string) *vendorStub {
	t.Helper()
	stub := &vendorStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		stub.requests = append(stub.requests, recordedRequest{
			method: r.Method, uri: r.URL.RequestURI(), auth: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), body: string(raw),
		})
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *vendorStub) only(t *testing.T) recordedRequest {
	t.Helper()
	if len(s.requests) != 1 {
		t.Fatalf("expected exactly one request, the provider saw %d: %+v", len(s.requests), s.requests)
	}
	return s.requests[0]
}

func basic(user, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+secret))
}

// A probe that sends one authenticated GET: what it asked for, and how it
// authenticated.
type getProbeCase struct {
	configType string
	data       func(base string) map[string]any
	wantURI    string
	wantAuth   string
}

func getProbeCases() []getProbeCase {
	return []getProbeCase{
		{
			configType: "ado",
			data:       func(base string) map[string]any { return map[string]any{"organization_url": base, "token": "pat-1"} },
			wantURI:    "/_apis/projects?api-version=7.1&$top=1",
			wantAuth:   basic("", "pat-1"),
		},
		{
			configType: "qtest",
			// A pasted /api/v3 suffix must not be doubled.
			data: func(base string) map[string]any {
				return map[string]any{"base_url": base + "/api/v3/", "qtest_api_token": "qt-1"}
			},
			wantURI:  "/api/v3/projects?pageSize=1&page=1",
			wantAuth: "Bearer qt-1",
		},
		{
			configType: "testrail",
			data: func(base string) map[string]any {
				return map[string]any{"url": base, "email": "me+qa@example.com", "password": "tr-1"}
			},
			wantURI:  "/index.php?/api/v2/get_user_by_email&email=me%2Bqa%40example.com",
			wantAuth: basic("me+qa@example.com", "tr-1"),
		},
		{
			configType: "zephyr_enterprise",
			data:       func(base string) map[string]any { return map[string]any{"base_url": base, "token": "ze-1"} },
			wantURI:    "/flex/services/rest/latest/user/current",
			wantAuth:   "Bearer ze-1",
		},
		{
			configType: "zephyr_essential",
			data:       func(base string) map[string]any { return map[string]any{"base_url": base + "/v2", "token": "zs-1"} },
			wantURI:    "/v2/projects?maxResults=1",
			wantAuth:   "Bearer zs-1",
		},
		{
			configType: "report_portal",
			data: func(base string) map[string]any {
				return map[string]any{"endpoint": base, "project": "my project", "api_key": "rp-1"}
			},
			wantURI:  "/api/v1/project/my%20project",
			wantAuth: "Bearer rp-1",
		},
		{
			configType: "testio",
			data:       func(base string) map[string]any { return map[string]any{"endpoint": base, "api_key": "tio-1"} },
			wantURI:    "/customer/v2/products",
			// TestIO's own scheme; a bearer would be refused.
			wantAuth: "Token tio-1",
		},
	}
}

func TestVendorGetProbesAnswerOkForAProviderThatAcceptsTheCredential(t *testing.T) {
	for _, tc := range getProbeCases() {
		t.Run(tc.configType, func(t *testing.T) {
			stub := newVendorStub(t, http.StatusOK, `{"id":1}`)
			checker := handler.NewToolkitConnectionChecker(allowAnyHost())

			outcome := checker.CheckToolkit(context.Background(), tc.configType, tc.data(stub.server.URL))

			if outcome.Reason != handler.ToolkitCheckReasonOK || !outcome.Success() {
				t.Fatalf("a 200 must be ok, got %+v", outcome)
			}
			request := stub.only(t)
			if request.method != http.MethodGet || request.uri != tc.wantURI {
				t.Fatalf("asked %s %q, want GET %q", request.method, request.uri, tc.wantURI)
			}
			if request.auth != tc.wantAuth {
				t.Fatalf("authenticated with %q, want %q", request.auth, tc.wantAuth)
			}
		})
	}
}

func TestVendorGetProbesAnswerAuthFailedForARefusedCredential(t *testing.T) {
	for _, tc := range getProbeCases() {
		t.Run(tc.configType, func(t *testing.T) {
			for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
				stub := newVendorStub(t, status, `{"error":"provider-secret-detail"}`)
				checker := handler.NewToolkitConnectionChecker(allowAnyHost())

				outcome := checker.CheckToolkit(context.Background(), tc.configType, tc.data(stub.server.URL))

				if outcome.Reason != handler.ToolkitCheckReasonAuthFailed || outcome.Success() {
					t.Fatalf("status %d must be auth_failed, got %+v", status, outcome)
				}
				if strings.Contains(outcome.Message, "provider-secret-detail") {
					t.Fatalf("the provider body leaked into the message: %q", outcome.Message)
				}
			}
		})
	}
}

func TestVendorGetProbesAnswerUnreachableWhenNothingAnswers(t *testing.T) {
	for _, tc := range getProbeCases() {
		t.Run(tc.configType, func(t *testing.T) {
			stub := newVendorStub(t, http.StatusOK, "{}")
			address := stub.server.URL
			stub.server.Close()
			checker := handler.NewToolkitConnectionChecker(allowAnyHost())

			outcome := checker.CheckToolkit(context.Background(), tc.configType, tc.data(address))

			if outcome.Reason != handler.ToolkitCheckReasonUnreachable {
				t.Fatalf("a dead provider must be unreachable, got %+v", outcome)
			}
		})
	}
}

// The 203 rule belongs to ado alone: another provider answering 203 is not
// refusing anything.
func TestAChangeOfStatusRuleForAdoDoesNotLeakToOtherProviders(t *testing.T) {
	stub := newVendorStub(t, http.StatusNonAuthoritativeInfo, "{}")
	checker := handler.NewToolkitConnectionChecker(allowAnyHost())

	outcome := checker.CheckToolkit(context.Background(), "testio", map[string]any{"endpoint": stub.server.URL, "api_key": "k"})

	if outcome.Reason == handler.ToolkitCheckReasonAuthFailed {
		t.Fatalf("a 203 from a non-ADO provider must not be auth_failed: %+v", outcome)
	}
}

func TestAdoTreatsASignInRedirectAnswerAsARefusedToken(t *testing.T) {
	stub := newVendorStub(t, http.StatusNonAuthoritativeInfo, "<html>sign in</html>")
	checker := handler.NewToolkitConnectionChecker(allowAnyHost())

	outcome := checker.CheckToolkit(context.Background(), "ado", map[string]any{"organization_url": stub.server.URL, "token": "stale"})

	if outcome.Reason != handler.ToolkitCheckReasonAuthFailed {
		t.Fatalf("a 203 is not a recognised credential, got %+v", outcome)
	}
}

// A credential with no secret has nothing to prove, so the check does not
// certify it: it dials nothing and answers unsupported_type.
func TestVendorProbesDoNotCertifyACredentialThatCarriesNoSecret(t *testing.T) {
	for configType, data := range map[string]func(base string) map[string]any{
		"ado":               func(base string) map[string]any { return map[string]any{"organization_url": base} },
		"zephyr_enterprise": func(base string) map[string]any { return map[string]any{"base_url": base} },
		"report_portal":     func(base string) map[string]any { return map[string]any{"endpoint": base, "api_key": "k"} },
		"testrail":          func(base string) map[string]any { return map[string]any{"url": base, "email": "a@b.c"} },
		"xray":              func(base string) map[string]any { return map[string]any{"base_url": base, "client_id": "id"} },
	} {
		t.Run(configType, func(t *testing.T) {
			stub := newVendorStub(t, http.StatusOK, "{}")
			checker := handler.NewToolkitConnectionChecker(allowAnyHost())

			outcome := checker.CheckToolkit(context.Background(), configType, data(stub.server.URL))

			if outcome.Reason != handler.ToolkitCheckReasonUnsupportedType {
				t.Fatalf("expected unsupported_type, got %+v", outcome)
			}
			if len(stub.requests) != 0 {
				t.Fatalf("nothing may be dialled for a credential with no secret, saw %+v", stub.requests)
			}
		})
	}
}

func TestXrayMintsATokenWithTheStoredClientPairAndAnswersOk(t *testing.T) {
	stub := newVendorStub(t, http.StatusOK, `"the-minted-token"`)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost())

	outcome := checker.CheckToolkit(context.Background(), "xray", map[string]any{
		"base_url": stub.server.URL + "/", "client_id": "cid", "client_secret": "csecret",
	})

	if outcome.Reason != handler.ToolkitCheckReasonOK {
		t.Fatalf("a minted token must be ok, got %+v", outcome)
	}
	request := stub.only(t)
	if request.method != http.MethodPost || request.uri != "/api/v2/authenticate" {
		t.Fatalf("asked %s %q", request.method, request.uri)
	}
	if request.contentType != "application/json" || !strings.Contains(request.body, `"client_id":"cid"`) || !strings.Contains(request.body, `"client_secret":"csecret"`) {
		t.Fatalf("the mint did not carry the stored pair: %q (%s)", request.body, request.contentType)
	}
	if strings.Contains(outcome.Message, "the-minted-token") {
		t.Fatalf("the minted token leaked into the message: %q", outcome.Message)
	}
}

func TestXrayAnswersAuthFailedForAPairTheProviderRefuses(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		stub := newVendorStub(t, status, `{"error":"provider-secret-detail"}`)
		checker := handler.NewToolkitConnectionChecker(allowAnyHost())

		outcome := checker.CheckToolkit(context.Background(), "xray", map[string]any{
			"base_url": stub.server.URL, "client_id": "cid", "client_secret": "wrong",
		})

		if outcome.Reason != handler.ToolkitCheckReasonAuthFailed {
			t.Fatalf("status %d must be auth_failed, got %+v", status, outcome)
		}
	}
}

func TestXrayAnAnswerWithNoTokenIsNotOk(t *testing.T) {
	stub := newVendorStub(t, http.StatusOK, `""`)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost())

	outcome := checker.CheckToolkit(context.Background(), "xray", map[string]any{
		"base_url": stub.server.URL, "client_id": "cid", "client_secret": "s",
	})

	if outcome.Reason != handler.ToolkitCheckReasonUnreachable {
		t.Fatalf("an unreadable token answer must not certify the credential, got %+v", outcome)
	}
}

// sharepointStubs points the probe at a token endpoint and a Graph host.
func sharepointStubs(t *testing.T, loginStatus int, loginBody string, graphStatus int) (login, graph *vendorStub) {
	t.Helper()
	login = newVendorStub(t, loginStatus, loginBody)
	graph = newVendorStub(t, graphStatus, `{"id":"site-1"}`)
	t.Cleanup(handler.SetSharepointEndpointsForTest(login.server.URL, graph.server.URL))
	return login, graph
}

var sharepointCredential = map[string]any{
	"client_id": "app-id", "client_secret": "app-secret", "site_url": "https://contoso.sharepoint.com/sites/team/",
}

func TestSharepointMintsAnEntraTokenSpendsItOnOneGraphReadAndAnswersOk(t *testing.T) {
	login, graph := sharepointStubs(t, http.StatusOK, `{"access_token":"graph-token","expires_in":3599}`, http.StatusOK)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost())

	outcome := checker.CheckToolkit(context.Background(), "sharepoint", sharepointCredential)

	if outcome.Reason != handler.ToolkitCheckReasonOK {
		t.Fatalf("expected ok, got %+v", outcome)
	}
	mint := login.only(t)
	if mint.method != http.MethodPost || mint.uri != "/contoso.onmicrosoft.com/oauth2/v2.0/token" {
		t.Fatalf("minted with %s %q", mint.method, mint.uri)
	}
	form, err := url.ParseQuery(mint.body)
	if err != nil {
		t.Fatalf("the mint body is not a form: %v", err)
	}
	if form.Get("grant_type") != "client_credentials" || form.Get("client_id") != "app-id" || form.Get("client_secret") != "app-secret" || form.Get("scope") != "https://graph.microsoft.com/.default" {
		t.Fatalf("the mint did not carry the client-credentials grant: %v", form)
	}
	spend := graph.only(t)
	if spend.method != http.MethodGet || spend.uri != "/v1.0/sites/contoso.sharepoint.com:/sites/team" {
		t.Fatalf("spent the token on %s %q", spend.method, spend.uri)
	}
	if spend.auth != "Bearer graph-token" {
		t.Fatalf("the Graph read did not carry the minted token: %q", spend.auth)
	}
	if strings.Contains(outcome.Message, "graph-token") || strings.Contains(outcome.Message, "app-secret") {
		t.Fatalf("credential material leaked into the message: %q", outcome.Message)
	}
}

// A 400 from the token endpoint is a verdict on the secret only when its OAuth
// error code says so; a wrong tenant is a 400 too.
func TestMintedProbesReadTheOAuthErrorCodeOfA400(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":"invalid_client","error_description":"AADSTS7000215"}`:                 handler.ToolkitCheckReasonAuthFailed,
		`{"error":"invalid_request","error_description":"AADSTS90002 tenant not found"}`: handler.ToolkitCheckReasonUnreachable,
		`not json`: handler.ToolkitCheckReasonUnreachable,
	} {
		_, _ = sharepointStubs(t, http.StatusBadRequest, body, http.StatusOK)
		checker := handler.NewToolkitConnectionChecker(allowAnyHost())

		outcome := checker.CheckToolkit(context.Background(), "sharepoint", sharepointCredential)

		if outcome.Reason != want {
			t.Fatalf("400 body %q must be %s, got %+v", body, want, outcome)
		}
	}
}

// The token was minted, so the secret is good: a Graph 403 or 404 is the app's
// permission or the site's address, not a refused credential.
func TestSharepointDoesNotBlameTheCredentialForAGraphPermissionOrSiteError(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		_, _ = sharepointStubs(t, http.StatusOK, `{"access_token":"graph-token"}`, status)
		checker := handler.NewToolkitConnectionChecker(allowAnyHost())

		outcome := checker.CheckToolkit(context.Background(), "sharepoint", sharepointCredential)

		if outcome.Reason != handler.ToolkitCheckReasonUnreachable {
			t.Fatalf("graph %d must not warn about the credential, got %+v", status, outcome)
		}
	}
}

func TestSharepointReadsTheRootSiteByHostAlone(t *testing.T) {
	_, graph := sharepointStubs(t, http.StatusOK, `{"access_token":"graph-token"}`, http.StatusOK)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost())

	outcome := checker.CheckToolkit(context.Background(), "sharepoint", map[string]any{
		"client_id": "a", "client_secret": "b", "site_url": "https://contoso.sharepoint.com",
	})

	if outcome.Reason != handler.ToolkitCheckReasonOK {
		t.Fatalf("expected ok, got %+v", outcome)
	}
	if uri := graph.only(t).uri; uri != "/v1.0/sites/contoso.sharepoint.com" {
		t.Fatalf("the root site must be read by host alone, asked %q", uri)
	}
}

func TestSharepointAnswersAuthFailedForAClientSecretEntraRefuses(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		login, graph := sharepointStubs(t, status, `{"error":"invalid_client"}`, http.StatusOK)
		checker := handler.NewToolkitConnectionChecker(allowAnyHost())

		outcome := checker.CheckToolkit(context.Background(), "sharepoint", sharepointCredential)

		if outcome.Reason != handler.ToolkitCheckReasonAuthFailed {
			t.Fatalf("mint status %d must be auth_failed, got %+v", status, outcome)
		}
		login.only(t)
		if len(graph.requests) != 0 {
			t.Fatalf("a token that was never minted must not be spent: %+v", graph.requests)
		}
	}
}

func TestSharepointAnswersAuthFailedWhenGraphDoesNotRecogniseTheMintedToken(t *testing.T) {
	_, _ = sharepointStubs(t, http.StatusOK, `{"access_token":"graph-token"}`, http.StatusUnauthorized)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost())

	outcome := checker.CheckToolkit(context.Background(), "sharepoint", sharepointCredential)

	if outcome.Reason != handler.ToolkitCheckReasonAuthFailed {
		t.Fatalf("expected auth_failed, got %+v", outcome)
	}
}

func TestSharepointStaysUnsupportedWhereThereIsNoAppSecretOrNoSiteToProve(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"no site named":         {"client_id": "a", "client_secret": "b"},
		"not a SharePoint host": {"client_id": "a", "client_secret": "b", "site_url": "https://example.com/sites/x"},
		"delegated":             {"client_id": "a", "client_secret": "b", "site_url": "https://contoso.sharepoint.com", "oauth_discovery_endpoint": "https://login.microsoftonline.com/t"},
		"app-mode (no secret)":  {"oauth_app_id": "11", "site_url": "https://contoso.sharepoint.com"},
	} {
		t.Run(name, func(t *testing.T) {
			login, graph := sharepointStubs(t, http.StatusOK, `{"access_token":"x"}`, http.StatusOK)
			checker := handler.NewToolkitConnectionChecker(allowAnyHost())

			outcome := checker.CheckToolkit(context.Background(), "sharepoint", data)

			if outcome.Reason != handler.ToolkitCheckReasonUnsupportedType {
				t.Fatalf("expected unsupported_type, got %+v", outcome)
			}
			if len(login.requests)+len(graph.requests) != 0 {
				t.Fatal("nothing may be dialled for a credential this probe cannot prove")
			}
		})
	}
}

// #2251: a credential that names its own Entra tenant is checked without a
// site; the minted token is the whole proof and no Graph read is made.
func TestSharepointWithAnExplicitTenantAndNoSiteMintsAndAnswersOk(t *testing.T) {
	for _, tenant := range []string{"contoso.onmicrosoft.com", "0f6d2c3e-1b4a-4c8e-9a7d-5e2f3a4b6c7d"} {
		login, graph := sharepointStubs(t, http.StatusOK, `{"access_token":"graph-token"}`, http.StatusOK)
		checker := handler.NewToolkitConnectionChecker(allowAnyHost())

		outcome := checker.CheckToolkit(context.Background(), "sharepoint", map[string]any{
			"client_id": "a", "client_secret": "b", "tenant_id": tenant,
		})

		if outcome.Reason != handler.ToolkitCheckReasonOK {
			t.Fatalf("tenant %q: expected ok, got %+v", tenant, outcome)
		}
		if uri := login.only(t).uri; uri != "/"+tenant+"/oauth2/v2.0/token" {
			t.Fatalf("minted at %q", uri)
		}
		if len(graph.requests) != 0 {
			t.Fatalf("there is no site to read: %+v", graph.requests)
		}
	}
}

func TestSharepointExplicitTenantWinsOverTheSiteHostAndStillRefusesABadSecret(t *testing.T) {
	login, _ := sharepointStubs(t, http.StatusBadRequest, `{"error":"invalid_client"}`, http.StatusOK)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost())

	outcome := checker.CheckToolkit(context.Background(), "sharepoint", map[string]any{
		"client_id": "a", "client_secret": "b", "tenant": "fabrikam.com", "site_url": "https://contoso.sharepoint.com",
	})

	if outcome.Reason != handler.ToolkitCheckReasonAuthFailed {
		t.Fatalf("expected auth_failed, got %+v", outcome)
	}
	if uri := login.only(t).uri; uri != "/fabrikam.com/oauth2/v2.0/token" {
		t.Fatalf("the explicit tenant must win, minted at %q", uri)
	}
}

func TestSharepointRefusesATenantThatIsNotOneBeforeDialling(t *testing.T) {
	for _, tenant := range []string{"a/b", "../x", "x..y", "https://evil.example", "a b", "-lead"} {
		login, graph := sharepointStubs(t, http.StatusOK, `{"access_token":"x"}`, http.StatusOK)
		checker := handler.NewToolkitConnectionChecker(allowAnyHost())

		outcome := checker.CheckToolkit(context.Background(), "sharepoint", map[string]any{
			"client_id": "a", "client_secret": "b", "tenant_id": tenant,
		})

		if outcome.Reason != handler.ToolkitCheckReasonUnsupportedType {
			t.Fatalf("tenant %q: expected unsupported_type, got %+v", tenant, outcome)
		}
		if len(login.requests)+len(graph.requests) != 0 {
			t.Fatalf("tenant %q: nothing may be dialled", tenant)
		}
	}
}

func TestOpenAPIKeepsTheUnsupportedAnswerWithoutClientCredentials(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"api key":        {"api_key": "k"},
		"anonymous":      {},
		"no token url":   {"client_id": "a", "client_secret": "b"},
		"http token url": {"client_id": "a", "client_secret": "b", "token_url": "http://idp.example.test/token"},
		"delegated":      {"client_id": "a", "client_secret": "b", "token_url": "https://idp.example.test/token", "oauth_discovery_endpoint": "https://idp.example.test/t"},
	} {
		t.Run(name, func(t *testing.T) {
			checker := handler.NewToolkitConnectionChecker(allowAnyHost())

			outcome := checker.CheckToolkit(context.Background(), "openapi", data)

			if outcome.Reason != handler.ToolkitCheckReasonUnsupportedType {
				t.Fatalf("expected unsupported_type, got %+v", outcome)
			}
		})
	}
}

// #2252: the client-credentials mint is the verdict.
func TestOpenAPIClientCredentialsMintsAndAnswersOkWithTheClientInTheBody(t *testing.T) {
	t.Cleanup(handler.SetOpenAPITokenSchemeForTest("http"))
	stub := newVendorStub(t, http.StatusOK, `{"access_token":"t","token_type":"Bearer"}`)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost())

	outcome := checker.CheckToolkit(context.Background(), "openapi", map[string]any{
		"client_id": "cid", "client_secret": "sec", "token_url": stub.server.URL + "/tenant/token", "scope": "api://x/.default",
	})

	if outcome.Reason != handler.ToolkitCheckReasonOK {
		t.Fatalf("expected ok, got %+v", outcome)
	}
	request := stub.only(t)
	if request.method != http.MethodPost || request.uri != "/tenant/token" || request.auth != "" {
		t.Fatalf("default method: %s %q auth=%q", request.method, request.uri, request.auth)
	}
	form, err := url.ParseQuery(request.body)
	if err != nil || form.Get("grant_type") != "client_credentials" || form.Get("client_id") != "cid" ||
		form.Get("client_secret") != "sec" || form.Get("scope") != "api://x/.default" {
		t.Fatalf("the grant is wrong: %v %v", form, err)
	}
	if strings.Contains(outcome.Message, "sec") && strings.Contains(outcome.Message, "cid:") {
		t.Fatalf("credential material leaked into the message: %q", outcome.Message)
	}
}

func TestOpenAPIBasicMethodKeepsTheSecretOutOfTheBody(t *testing.T) {
	t.Cleanup(handler.SetOpenAPITokenSchemeForTest("http"))
	stub := newVendorStub(t, http.StatusOK, `{"access_token":"t"}`)
	checker := handler.NewToolkitConnectionChecker(allowAnyHost())

	outcome := checker.CheckToolkit(context.Background(), "openapi", map[string]any{
		"client_id": "cid", "client_secret": "sec", "token_url": stub.server.URL + "/token", "method": "Basic",
	})

	if outcome.Reason != handler.ToolkitCheckReasonOK {
		t.Fatalf("expected ok, got %+v", outcome)
	}
	request := stub.only(t)
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:sec")); request.auth != want {
		t.Fatalf("Basic header = %q", request.auth)
	}
	if strings.Contains(request.body, "sec") || strings.Contains(request.body, "client_id") {
		t.Fatalf("the secret rode in the body too: %q", request.body)
	}
}

func TestOpenAPIClientCredentialsRefusedByTheProviderIsAuthFailed(t *testing.T) {
	t.Cleanup(handler.SetOpenAPITokenSchemeForTest("http"))
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		stub := newVendorStub(t, status, `{"error":"invalid_client"}`)
		checker := handler.NewToolkitConnectionChecker(allowAnyHost())

		outcome := checker.CheckToolkit(context.Background(), "openapi", map[string]any{
			"client_id": "cid", "client_secret": "bad", "token_url": stub.server.URL + "/token",
		})

		if outcome.Reason != handler.ToolkitCheckReasonAuthFailed {
			t.Fatalf("status %d must be auth_failed, got %+v", status, outcome)
		}
	}
}

func TestEveryReportedTypeButOpenAPIIsCheckable(t *testing.T) {
	for _, configType := range []string{"ado", "qtest", "testrail", "xray", "zephyr_enterprise", "zephyr_essential", "sharepoint", "report_portal", "testio"} {
		if !handler.IsToolkitCheckableType(configType) {
			t.Errorf("%s must carry a probe", configType)
		}
	}
}

// The vendors' public hosts are allowed with no operator setting.
func TestVendorPublicHostsAreAllowedByDefault(t *testing.T) {
	t.Setenv(handler.ToolkitCheckAllowlistEnv, "")
	checker := handler.NewToolkitConnectionCheckerFromEnv()
	// A cancelled context: the allowlist is applied before the dial, and the
	// dial then fails at once, so the test never reaches the internet.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for configType, data := range map[string]map[string]any{
		"ado":              {"organization_url": "https://dev.azure.com/acme", "token": "t"},
		"qtest":            {"base_url": "https://acme.qtestnet.com", "qtest_api_token": "t"},
		"testrail":         {"url": "https://acme.testrail.io", "email": "a@b.c", "password": "p"},
		"xray":             {"client_id": "i", "client_secret": "s"},
		"zephyr_essential": {"token": "t"},
		"sharepoint":       sharepointCredential,
	} {
		t.Run(configType, func(t *testing.T) {
			outcome := checker.CheckToolkit(ctx, configType, data)
			if strings.Contains(outcome.Message, handler.ToolkitCheckAllowlistEnv) {
				t.Fatalf("a vendor's own host was refused by the egress policy: %+v", outcome)
			}
		})
	}
}
