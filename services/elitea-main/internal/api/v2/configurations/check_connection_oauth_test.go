package configurations

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpoauth"
)

// #1998: a delegated credential has no secret to test with, so its check must
// answer the 401 the browser opens its sign-in window on.
func postDelegatedCheck(t *testing.T, handler *Handler, configType, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Post("/check_connection/{projectID}/{configType}", handler.CheckConnection)
	request := httptest.NewRequest(http.MethodPost, "/check_connection/7/"+configType, bytes.NewReader([]byte(body)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// discoveryFetcher serves the documents of a Microsoft-shaped tenant: only the
// "/v2.0" form carries the issuer it is published under.
func discoveryFetcher(served map[string]string, requested *[]string) *mcpoauth.Fetcher {
	return &mcpoauth.Fetcher{Do: func(request *http.Request) (*http.Response, error) {
		*requested = append(*requested, request.URL.String())
		body, ok := served[request.URL.String()]
		if !ok {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	}}
}

func TestCheckConnectionOfADelegatedCredentialRequestsAuthorization(t *testing.T) {
	const tenant = "https://login.example.test/tenant-1"
	var requested []string
	handler := NewHandler(nil)
	handler.oauthDiscovery = discoveryFetcher(map[string]string{
		tenant + "/v2.0/.well-known/openid-configuration": `{
			"issuer": "` + tenant + `/v2.0",
			"authorization_endpoint": "` + tenant + `/oauth2/v2.0/authorize",
			"token_endpoint": "` + tenant + `/oauth2/v2.0/token"
		}`,
	}, &requested)

	recorder := postDelegatedCheck(t, handler, "sharepoint",
		`{"oauth_discovery_endpoint":"`+tenant+`","scopes":["Sites.Read.All"]}`)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", recorder.Code, recorder.Body.String())
	}
	var body struct {
		RequiresAuthorization bool `json:"requires_authorization"`
		AuthMetadata          struct {
			ServerURL        string `json:"server_url"`
			ResourceMetadata struct {
				AuthorizationServers   []string       `json:"authorization_servers"`
				OAuthAuthorizationSrvr map[string]any `json:"oauth_authorization_server"`
				ScopesSupported        []string       `json:"scopes_supported"`
			} `json:"resource_metadata"`
		} `json:"auth_metadata"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	metadata := body.AuthMetadata.ResourceMetadata
	if !body.RequiresAuthorization || body.AuthMetadata.ServerURL != tenant ||
		len(metadata.AuthorizationServers) != 1 || metadata.AuthorizationServers[0] != tenant+"/v2.0" ||
		metadata.OAuthAuthorizationSrvr["authorization_endpoint"] != tenant+"/oauth2/v2.0/authorize" ||
		len(metadata.ScopesSupported) != 1 || metadata.ScopesSupported[0] != "Sites.Read.All" {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestCheckConnectionOfADelegatedCredentialNeverFetchesUnguarded(t *testing.T) {
	handler := NewHandler(nil) // no guard composed
	recorder := postDelegatedCheck(t, handler, "sharepoint",
		`{"oauth_discovery_endpoint":"https://login.example.test/tenant-1"}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "oauth_discovery_failed") {
		t.Fatalf("status = %d body = %s, want a 400 naming oauth_discovery_failed", recorder.Code, recorder.Body.String())
	}
}

func TestCheckConnectionOfADelegatedCredentialSaysWhenTheDocumentIsUnreadable(t *testing.T) {
	var requested []string
	handler := NewHandler(nil)
	handler.oauthDiscovery = discoveryFetcher(map[string]string{}, &requested)
	recorder := postDelegatedCheck(t, handler, "sharepoint",
		`{"oauth_discovery_endpoint":"https://login.example.test/tenant-1"}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "could not be read") {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if len(requested) == 0 {
		t.Fatal("the discovery document was never requested")
	}
}

// An app-only credential (no discovery endpoint) keeps the answer it had.
func TestCheckConnectionOfAnAppOnlyCredentialIsNotAnAuthorizationRequest(t *testing.T) {
	handler := NewHandler(nil)
	recorder := postDelegatedCheck(t, handler, "sharepoint", `{"client_id":"a","site_url":"https://x.sharepoint.com"}`)
	if recorder.Code == http.StatusUnauthorized {
		t.Fatalf("app-only check answered 401: %s", recorder.Body.String())
	}
}

func TestCheckConnectionOfAMicrosoftAliasTenantAcceptsTheTemplatedIssuer(t *testing.T) {
	const alias = "https://login.microsoftonline.com/common"
	var requested []string
	handler := NewHandler(nil)
	handler.oauthDiscovery = discoveryFetcher(map[string]string{
		alias + "/v2.0/.well-known/openid-configuration": `{
			"issuer": "https://login.microsoftonline.com/{tenantid}/v2.0",
			"authorization_endpoint": "` + alias + `/oauth2/v2.0/authorize",
			"token_endpoint": "` + alias + `/oauth2/v2.0/token"
		}`,
	}, &requested)
	handler.oauthDiscovery.IssuerMatch = microsoftTemplatedIssuer
	recorder := postDelegatedCheck(t, handler, "sharepoint", `{"oauth_discovery_endpoint":"`+alias+`"}`)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"authorization_servers":["`+alias+`/v2.0"]`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestMicrosoftTemplatedIssuerIsNarrow(t *testing.T) {
	const tmpl = "https://login.microsoftonline.com/{tenantid}/v2.0"
	for requested, want := range map[string]bool{
		"https://login.microsoftonline.com/common/v2.0":      true,
		"https://login.microsoftonline.com/organizations":    true,
		"https://login.microsoftonline.com/some-tenant/v2.0": false,
		"https://evil.example/common/v2.0":                   false,
		"https://login.microsoftonline.com/common/x/v2.0":    false,
		"http://login.microsoftonline.com/common/v2.0":       false,
	} {
		if got := microsoftTemplatedIssuer(tmpl, requested); got != want {
			t.Errorf("%s: got %v want %v", requested, got, want)
		}
	}
	if microsoftTemplatedIssuer("https://evil.example/{tenantid}/v2.0", "https://login.microsoftonline.com/common/v2.0") {
		t.Error("a templated issuer on another host was accepted")
	}
}

func TestCheckConnectionOfADelegatedCredentialWithoutIssuerIsAFailure(t *testing.T) {
	const tenant = "https://login.example.test/tenant-1"
	var requested []string
	handler := NewHandler(nil)
	handler.oauthDiscovery = discoveryFetcher(map[string]string{}, &requested)
	handler.oauthDiscovery.IssuerMatch = func(string, string) bool { return true }
	handler.oauthDiscovery.Do = func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(
			`{"authorization_endpoint":"` + tenant + `/a","token_endpoint":"` + tenant + `/t"}`))}, nil
	}
	recorder := postDelegatedCheck(t, handler, "sharepoint", `{"oauth_discovery_endpoint":"`+tenant+`"}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "oauth_discovery_failed") ||
		strings.Contains(recorder.Body.String(), `[""]`) {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestCheckConnectionOfADelegatedCredentialIsRateLimited(t *testing.T) {
	var requested []string
	handler := NewHandler(nil)
	handler.oauthDiscovery = discoveryFetcher(map[string]string{}, &requested)
	var last *httptest.ResponseRecorder
	for i := 0; i < modelProbeBurst+1; i++ {
		last = postDelegatedCheck(t, handler, "sharepoint", `{"oauth_discovery_endpoint":"https://login.example.test/t"}`)
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", last.Code, last.Body.String())
	}
}
