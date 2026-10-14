package eliteacore

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	toolkitexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpregistry"
)

type bindingResolver struct {
	mu       sync.Mutex
	settings map[string]any
	toolType string
}

func (r *bindingResolver) ResolveDelegatedAuthToolkitSettings(
	context.Context, int32, int32, int32,
) (toolkitexecutionapp.DelegatedAuthToolkitSettings, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := make(map[string]any, len(r.settings))
	for key, value := range r.settings {
		copied[key] = value
	}
	return toolkitexecutionapp.DelegatedAuthToolkitSettings{ToolkitType: r.toolType, Settings: copied}, true, nil
}

func (r *bindingResolver) setURL(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settings["url"] = value
}

type fakeCatalogue struct{ entry mcpregistry.PrebuiltServer }

func (c fakeCatalogue) Lookup(context.Context, string) (mcpregistry.PrebuiltServer, error) {
	return c.entry, nil
}

// bindingGuard refuses private literals and sends every dial to one test
// server, so the guarded client is observable without real DNS.
type bindingGuard struct {
	target  string
	dials   atomic.Int32
	refused atomic.Int32
}

func (g *bindingGuard) Validate(_ context.Context, raw string) error {
	if strings.Contains(raw, "//10.") {
		g.refused.Add(1)
		return errors.New("private address")
	}
	return nil
}

func (g *bindingGuard) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	g.dials.Add(1)
	return (&net.Dialer{}).DialContext(ctx, network, g.target)
}

// idp is a metadata + token server. Hosts are told apart by the Host header.
type idp struct {
	mu       sync.Mutex
	hosts    []string
	paths    []string
	metadata func(host, path string) (int, string)
	token    url.Values
	delay    time.Duration
}

func (p *idp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.hosts = append(p.hosts, r.Host)
	p.paths = append(p.paths, r.URL.Path)
	p.mu.Unlock()
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		p.mu.Lock()
		p.token = r.Form
		p.mu.Unlock()
		_, _ = w.Write([]byte(`{"access_token":"access","token_type":"Bearer"}`))
		return
	}
	status, body := p.metadata(r.Host, r.URL.Path)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (p *idp) sawHost(host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, seen := range p.hosts {
		if seen == host {
			return true
		}
	}
	return false
}

func goodMetadata(host, path string) (int, string) {
	switch path {
	case "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource":
		return 200, `{"resource":"https://` + host + `/mcp","authorization_servers":["https://` + host + `"]}`
	case "/.well-known/oauth-authorization-server":
		return 200, `{"issuer":"https://` + host + `","authorization_endpoint":"https://` + host +
			`/authorize","token_endpoint":"https://` + host + `/token"}`
	}
	return 404, `{}`
}

type bindingFixture struct {
	handler  *Handler
	resolver *bindingResolver
	guard    *bindingGuard
	provider *idp
}

func newBindingFixture(t *testing.T, toolType string, settings map[string]any, catalogue *mcpregistry.PrebuiltServer) *bindingFixture {
	t.Helper()
	provider := &idp{metadata: goodMetadata}
	server := httptest.NewTLSServer(provider)
	t.Cleanup(server.Close)
	// The test certificate names example.com; the hosts below are invented.
	server.Client().Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // test server
	guard := &bindingGuard{target: server.Listener.Addr().String()}
	resolver := &bindingResolver{settings: settings, toolType: toolType}
	handler := NewHandler(nil,
		WithHTTPClient(server.Client()),
		WithMCPAuthorizationEgressGuard(guard),
		WithDelegatedAuthToolkitSettingsResolver(resolver),
	)
	if catalogue != nil {
		handler.prebuiltMCP = fakeCatalogue{entry: *catalogue}
		handler.prebuiltMCPVault = stubSecretReader{value: "catalogue-secret"}
	}
	return &bindingFixture{handler: handler, resolver: resolver, guard: guard, provider: provider}
}

func (f *bindingFixture) exchange(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", "7")
	request = request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: "11"}))
	recorder := httptest.NewRecorder()
	f.handler.MCPOAuthProxy(recorder, request)
	return recorder
}

func exchangeBody(tokenEndpoint string, extra string) string {
	return `{"token_endpoint":"` + tokenEndpoint + `","code":"c","redirect_uri":"https://elitea.example/cb",` +
		`"client_secret":"********","toolkit_id":"19"` + extra + `}`
}

func catalogueEntry(serverURL string) *mcpregistry.PrebuiltServer {
	return &mcpregistry.PrebuiltServer{
		Enabled: true, ServerURL: serverURL, ClientID: "catalogue-client", ClientSecretRef: "ref",
	}
}

// The attack: an editor saves an attacker URL on a prebuilt toolkit and the
// attacker's metadata names an attacker token endpoint. The catalogue's secret
// must not move, and nothing at the attacker's host may even be contacted.
func TestCatalogueSecretIsNeverReleasedToAnAttackerChosenEndpoint(t *testing.T) {
	f := newBindingFixture(t, "mcp_github",
		map[string]any{"url": "https://evil.example/mcp"}, catalogueEntry("https://good.example/mcp"))
	recorder := f.exchange(t, exchangeBody("https://evil.example/token", ""))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "untrusted_token_endpoint")
	require.Zero(t, f.guard.dials.Load(), "no transport call was made")
	require.Empty(t, f.provider.token)
}

// An editor-set token_endpoint on the toolkit also cannot bind the operator's secret.
func TestCatalogueSecretIgnoresAnEditorSetTokenEndpointSetting(t *testing.T) {
	f := newBindingFixture(t, "mcp_github",
		map[string]any{"url": "https://evil.example/mcp", "token_endpoint": "https://evil.example/token"},
		catalogueEntry("https://good.example/mcp"))
	recorder := f.exchange(t, exchangeBody("https://evil.example/token", ""))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Zero(t, f.guard.dials.Load())
}

func TestLegitimatePrebuiltToolkitBindsThroughTheCatalogueServer(t *testing.T) {
	f := newBindingFixture(t, "mcp_github", map[string]any{"url": ""}, catalogueEntry("https://good.example/mcp"))
	recorder := f.exchange(t, exchangeBody("https://good.example/token", ""))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "catalogue-secret", f.provider.token.Get("client_secret"))
	require.True(t, f.provider.sawHost("good.example"))
	require.Positive(t, f.guard.dials.Load(), "discovery and exchange use the authorization guard")
}

func TestOwnSecretDirectToolkitBindsThroughItsSavedURL(t *testing.T) {
	f := newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_id": "me", "client_secret": "own-secret"}, nil)
	recorder := f.exchange(t, exchangeBody("https://own.example/token", ""))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "own-secret", f.provider.token.Get("client_secret"))
}

func TestAPrivateAuthorizationServerIsRefusedByTheGuard(t *testing.T) {
	f := newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
	f.provider.metadata = func(host, path string) (int, string) {
		if strings.HasPrefix(path, "/.well-known/oauth-protected-resource") {
			return 200, `{"resource":"https://` + host + `/mcp","authorization_servers":["https://10.0.0.5"]}`
		}
		return 404, `{}`
	}
	recorder := f.exchange(t, exchangeBody("https://10.0.0.5/token", ""))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Positive(t, f.guard.refused.Load())
	require.Empty(t, f.provider.token)
}

func TestDiscoveryFetchesTimeOutIndividually(t *testing.T) {
	f := newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
	f.handler.mcpDiscoveryFetchTimeout = 50 * time.Millisecond
	f.provider.delay = 400 * time.Millisecond
	started := time.Now()
	recorder := f.exchange(t, exchangeBody("https://own.example/token", ""))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Less(t, time.Since(started), 3*time.Second)
}

func TestExchangeSharesTheOverallBudgetWithDiscovery(t *testing.T) {
	f := newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
	f.handler.mcpProxyBudget = 500 * time.Millisecond
	f.provider.delay = 200 * time.Millisecond
	// Two discovery fetches fit in the budget; the exchange then gets only the
	// remainder, not a fresh allowance, and cannot finish.
	started := time.Now()
	recorder := f.exchange(t, exchangeBody("https://own.example/token", ""))
	require.NotEqual(t, http.StatusOK, recorder.Code)
	require.Less(t, time.Since(started), 2*time.Second)
	require.Empty(t, f.provider.token.Get("client_secret"))
}

func TestAuthorizationServerListIsCapped(t *testing.T) {
	for count, want := range map[int]int{4: http.StatusOK, 5: http.StatusBadRequest} {
		f := newBindingFixture(t, "mcp",
			map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
		f.provider.metadata = func(host, path string) (int, string) {
			if strings.HasPrefix(path, "/.well-known/oauth-protected-resource") {
				servers := make([]string, count)
				servers[0] = `"https://` + host + `"`
				for i := 1; i < count; i++ {
					servers[i] = `"https://other` + string(rune('a'+i)) + `.example"`
				}
				return 200, `{"resource":"https://` + host + `/mcp","authorization_servers":[` + strings.Join(servers, ",") + `]}`
			}
			return goodMetadata(host, path)
		}
		recorder := f.exchange(t, exchangeBody("https://own.example/token", ""))
		require.Equal(t, want, recorder.Code, "servers=%d %s", count, recorder.Body.String())
	}
}

// Both paths run the one walk, so both reject the same bad documents.
func TestSyncAndBindingRejectTheSameBadDiscoveryDocuments(t *testing.T) {
	bad := map[string]func(host, path string) (int, string){
		"issuer with a query": func(host, path string) (int, string) {
			if strings.HasPrefix(path, "/.well-known/oauth-protected-resource") {
				return 200, `{"resource":"https://` + host + `/mcp","authorization_servers":["https://` + host + `?x=1"]}`
			}
			return goodMetadata(host, path)
		},
		"no servers": func(host, path string) (int, string) {
			return 200, `{"resource":"https://` + host + `/mcp","authorization_servers":[]}`
		},
		"resource mismatch": func(host, path string) (int, string) {
			return 200, `{"resource":"https://elsewhere.example/mcp","authorization_servers":["https://` + host + `"]}`
		},
		"issuer mismatch": func(host, path string) (int, string) {
			status, body := goodMetadata(host, path)
			return status, strings.Replace(body, `"issuer":"https://`+host+`"`, `"issuer":"https://other.example"`, 1)
		},
	}
	for name, metadata := range bad {
		t.Run(name, func(t *testing.T) {
			f := newBindingFixture(t, "mcp", map[string]any{"url": "https://own.example/mcp"}, nil)
			f.provider.metadata = metadata
			endpoint, err := url.Parse("https://own.example/mcp")
			require.NoError(t, err)
			_, syncErr := f.handler.syncMetadataSource().discover(context.Background(), endpoint, "", mcpSyncMaxServers, false)
			_, guardedErr := f.handler.authorizationMetadataSource().discover(context.Background(), endpoint, "", mcpBindingMaxServers, true)
			require.Error(t, syncErr)
			require.Error(t, guardedErr)
		})
	}
}

func TestAdvertisedMetadataURLIsHonouredOnlyWhenSameOrigin(t *testing.T) {
	f := newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
	recorder := f.exchange(t, exchangeBody("https://own.example/token",
		`,"resource_metadata_url":"https://evil.example/md"`))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.False(t, f.provider.sawHost("evil.example"), "a cross-origin hint is ignored")

	f = newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
	recorder = f.exchange(t, exchangeBody("https://own.example/token",
		`,"resource_metadata_url":"https://own.example/custom-md"`))
	require.Equal(t, http.StatusBadRequest, recorder.Code, "the hint was used: that path is not served")
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	require.Contains(t, f.provider.paths, "/custom-md")
}

func TestBoundEndpointIsCachedAndInvalidatedWhenTheToolkitURLChanges(t *testing.T) {
	f := newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
	now := time.Now()
	f.handler.mcpBindingClock = func() time.Time { return now }
	require.Equal(t, http.StatusOK, f.exchange(t, exchangeBody("https://own.example/token", "")).Code)

	// The metadata server goes away: the cached binding still serves a refresh.
	f.provider.metadata = func(string, string) (int, string) { return 503, `{}` }
	require.Equal(t, http.StatusOK, f.exchange(t, exchangeBody("https://own.example/token", "")).Code)

	// An edited URL is a different key and needs fresh discovery, which fails.
	f.resolver.setURL("https://moved.example/mcp")
	require.Equal(t, http.StatusBadRequest, f.exchange(t, exchangeBody("https://own.example/token", "")).Code)

	// Past the TTL the entry is not trusted any more.
	f.resolver.setURL("https://own.example/mcp")
	now = now.Add(mcpBindingCacheTTL + time.Minute)
	require.Equal(t, http.StatusBadRequest, f.exchange(t, exchangeBody("https://own.example/token", "")).Code)
}
