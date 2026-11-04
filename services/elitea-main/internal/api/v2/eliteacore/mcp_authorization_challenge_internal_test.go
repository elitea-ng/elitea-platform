package eliteacore

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	toolkitexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// challengeResolver answers one (project, actor, toolkit) and nothing else,
// as the real resolver does for a row the actor cannot see.
type challengeResolver struct {
	mu       sync.Mutex
	project  int32
	actor    int32
	toolkit  int32
	toolType string
	settings map[string]any
	calls    int
}

func (r *challengeResolver) ResolveDelegatedAuthToolkitSettings(
	_ context.Context, project, actor, toolkit int32,
) (toolkitexecutionapp.DelegatedAuthToolkitSettings, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if project != r.project || actor != r.actor || toolkit != r.toolkit {
		return toolkitexecutionapp.DelegatedAuthToolkitSettings{}, false, nil
	}
	return toolkitexecutionapp.DelegatedAuthToolkitSettings{ToolkitType: r.toolType, Settings: r.settings}, true, nil
}

type challengeFixture struct {
	handler  *Handler
	resolver *challengeResolver
	guard    *bindingGuard
	provider *idp
}

func newChallengeFixture(t *testing.T, toolType string, settings map[string]any) *challengeFixture {
	t.Helper()
	provider := &idp{metadata: goodMetadata}
	server := httptest.NewTLSServer(provider)
	t.Cleanup(server.Close)
	server.Client().Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // test server
	guard := &bindingGuard{target: server.Listener.Addr().String()}
	resolver := &challengeResolver{project: 7, actor: 11, toolkit: 19, toolType: toolType, settings: settings}
	handler := NewHandler(nil,
		WithHTTPClient(server.Client()),
		WithMCPAuthorizationEgressGuard(guard),
		WithDelegatedAuthToolkitSettingsResolver(resolver),
	)
	return &challengeFixture{handler: handler, resolver: resolver, guard: guard, provider: provider}
}

func (f *challengeFixture) post(t *testing.T, project, toolkit string, user *auth.User, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", project)
	routeContext.URLParams.Add("toolkitID", toolkit)
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	if user != nil {
		request = request.WithContext(auth.ContextWithUser(request.Context(), *user))
	}
	recorder := httptest.NewRecorder()
	f.handler.MCPAuthorizationChallenge(recorder, request)
	return recorder
}

func (f *challengeFixture) fetched() int {
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	return len(f.provider.paths)
}

var sessionUser = &auth.User{UserID: "11"}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
	return body
}

func TestAuthorizationChallengeAnswersOnlyTheStoredToolkitsPublicMetadata(t *testing.T) {
	f := newChallengeFixture(t, "mcp", map[string]any{
		"url": "https://good.example/mcp", "headers": map[string]any{"Authorization": "Bearer stored-header-secret"},
	})
	recorder := f.post(t, "7", "19", sessionUser, "/?url=https://evil.example/mcp",
		`{"url":"https://evil.example/mcp","server_url":"https://evil.example/mcp","resource_metadata_url":"https://evil.example/x"}`)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	body := decodeBody(t, recorder)
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	require.Equal(t, []string{"resource_metadata", "resource_metadata_url", "server_url"}, keys)
	require.Equal(t, "https://good.example/mcp", body["server_url"])
	require.Equal(t, "https://good.example/.well-known/oauth-protected-resource/mcp", body["resource_metadata_url"])
	metadata := body["resource_metadata"].(map[string]any)
	require.Equal(t, "https://good.example/mcp", metadata["resource"])
	require.Equal(t, []any{"https://good.example"}, metadata["authorization_servers"])
	require.NotEmpty(t, metadata["oauth_authorization_server"])
	for _, forbidden := range []string{"stored-header-secret", "access_token", "client_secret", "evil.example"} {
		require.NotContains(t, recorder.Body.String(), forbidden)
	}
	require.False(t, f.provider.sawHost("evil.example"), "a request URL steered the discovery")
	require.Positive(t, f.guard.dials.Load(), "discovery did not dial through the guarded client")
}

// The §3a matrix at the handler; the router rows (foreign project, a member
// without tool.patch, an entitled member) are in router_elitea_core_project_scope_test.go.
func TestAuthorizationChallengeIdentityMatrix(t *testing.T) {
	cases := []struct {
		name    string
		user    *auth.User
		project string
		toolkit string
		status  int
		code    string
	}{
		{"session owner", sessionUser, "7", "19", http.StatusOK, ""},
		{"token with an owning user", &auth.User{UserID: "11", TokenID: "5", AuthType: "token"}, "7", "19", http.StatusOK, ""},
		{"token without an owning user", &auth.User{TokenID: "5", AuthType: "token"}, "7", "19", http.StatusUnauthorized, "authentication_required"},
		{"unauthenticated", nil, "7", "19", http.StatusUnauthorized, "authentication_required"},
		{"another user", &auth.User{UserID: "12"}, "7", "19", http.StatusNotFound, "toolkit_not_found"},
		{"another project's toolkit", sessionUser, "8", "19", http.StatusNotFound, "toolkit_not_found"},
		{"unknown toolkit", sessionUser, "7", "20", http.StatusNotFound, "toolkit_not_found"},
		{"malformed toolkit id", sessionUser, "7", "x", http.StatusBadRequest, "invalid_toolkit_id"},
		{"malformed project id", sessionUser, "x", "19", http.StatusBadRequest, "invalid_project_id"},
	}
	var refusals = map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newChallengeFixture(t, "mcp", map[string]any{"url": "https://good.example/mcp"})
			recorder := f.post(t, tc.project, tc.toolkit, tc.user, "/", "")
			require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
			if tc.code == "" {
				return
			}
			require.Equal(t, map[string]any{"error": tc.code}, decodeBody(t, recorder))
			require.Zero(t, f.fetched(), "a refused request reached the network")
			if tc.status == http.StatusNotFound {
				refusals[tc.name] = recorder.Body.String()
			}
		})
	}
	// A probe cannot tell another user's, another project's and a missing toolkit apart.
	require.Equal(t, refusals["unknown toolkit"], refusals["another project's toolkit"])
	require.Equal(t, refusals["unknown toolkit"], refusals["another user"])
}

func TestAuthorizationChallengeRefusesANonMCPToolkitWithoutFetching(t *testing.T) {
	f := newChallengeFixture(t, "openapi", map[string]any{"url": "https://good.example/api"})
	recorder := f.post(t, "7", "19", sessionUser, "/", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, map[string]any{"error": "unsupported_toolkit_type"}, decodeBody(t, recorder))
	require.Zero(t, f.fetched())
}

func TestAuthorizationChallengeRefusesAStoredURLThatIsNotHTTPS(t *testing.T) {
	f := newChallengeFixture(t, "mcp", map[string]any{"url": "http://good.example/mcp"})
	recorder := f.post(t, "7", "19", sessionUser, "/", "")
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, map[string]any{"error": "invalid_toolkit_resource"}, decodeBody(t, recorder))
	require.Zero(t, f.fetched())
}

func TestAuthorizationChallengeRefusesAGuardedDestination(t *testing.T) {
	f := newChallengeFixture(t, "mcp", map[string]any{"url": "https://10.0.0.5/mcp"})
	recorder := f.post(t, "7", "19", sessionUser, "/", "")
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.Equal(t, map[string]any{"error": "authorization_discovery_failed"}, decodeBody(t, recorder))
	require.Positive(t, f.guard.refused.Load())
	require.Zero(t, f.guard.dials.Load())
}

// Upstream bodies, internal URLs and causes never reach the browser.
func TestAuthorizationChallengeFailureIsNeutral(t *testing.T) {
	f := newChallengeFixture(t, "mcp", map[string]any{"url": "https://good.example/mcp"})
	f.provider.metadata = func(string, string) (int, string) {
		return http.StatusInternalServerError, `{"trace":"panic at http://internal.svc.cluster.local:8080/x"}`
	}
	recorder := f.post(t, "7", "19", sessionUser, "/", "")
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.Equal(t, map[string]any{"error": "authorization_discovery_failed"}, decodeBody(t, recorder))
}

// A protected-resource document that names another resource is refused typed,
// and nothing from it is answered.
func TestAuthorizationChallengeRefusesMetadataForAnotherResource(t *testing.T) {
	for name, named := range map[string]string{
		"another host":   "https://other.example/mcp",
		"another path":   "https://good.example/other",
		"trailing slash": "https://good.example/mcp/",
	} {
		t.Run(name, func(t *testing.T) {
			f := newChallengeFixture(t, "mcp", map[string]any{"url": "https://good.example/mcp"})
			f.provider.metadata = func(host, path string) (int, string) {
				if strings.HasPrefix(path, "/.well-known/oauth-protected-resource") {
					return 200, `{"resource":"` + named + `","authorization_servers":["https://` + host + `"]}`
				}
				return goodMetadata(host, path)
			}
			recorder := f.post(t, "7", "19", sessionUser, "/", "")
			require.Equal(t, http.StatusBadGateway, recorder.Code, recorder.Body.String())
			code := decodeBody(t, recorder)["error"]
			require.Contains(t, []any{"authorization_discovery_failed", "authorization_resource_mismatch"}, code)
			require.NotContains(t, recorder.Body.String(), "example")
		})
	}
}

func TestAuthorizationChallengeLogsNoMetadataOrStoredSecrets(t *testing.T) {
	var buffer bytes.Buffer
	var mu sync.Mutex
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buffer}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	settings := map[string]any{"url": "https://good.example/mcp", "headers": map[string]any{"Authorization": "Bearer stored-header-secret"}}
	ok := newChallengeFixture(t, "mcp", settings)
	require.Equal(t, http.StatusOK, ok.post(t, "7", "19", sessionUser, "/", "").Code)
	failing := newChallengeFixture(t, "mcp", settings)
	failing.provider.metadata = func(string, string) (int, string) { return http.StatusInternalServerError, `{"trace":"upstream-body"}` }
	require.Equal(t, http.StatusBadGateway, failing.post(t, "7", "19", sessionUser, "/", "").Code)

	mu.Lock()
	logged := buffer.String()
	mu.Unlock()
	require.Contains(t, logged, "authorization_discovery_failed", "the failure was not logged at all")
	for _, forbidden := range []string{"stored-header-secret", "upstream-body", "/authorize", "/token", "oauth-protected-resource", "good.example"} {
		require.NotContains(t, logged, forbidden)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
