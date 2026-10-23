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

	toolkitexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/mcpregistry"
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
	return toolkitexecutionapp.DelegatedAuthToolkitSettings{
		ToolkitType: r.toolType, Settings: copied, ProjectOwnedSettings: copied,
	}, true, nil
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
	slowHost string
	slowPath string
}

func (p *idp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.hosts = append(p.hosts, r.Host)
	p.paths = append(p.paths, r.URL.Path)
	p.mu.Unlock()
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	if p.slowPath != "" && r.URL.Path == p.slowPath {
		time.Sleep(600 * time.Millisecond)
	}
	if p.slowHost != "" && r.Host == p.slowHost {
		time.Sleep(600 * time.Millisecond)
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

func (f *bindingFixture) attackerContacted(t *testing.T) {
	t.Helper()
	require.False(t, f.provider.sawHost("evil.example"), "the attacker's host was contacted")
	require.Empty(t, f.provider.token, "a token request reached the provider")
}

// The attack: an editor saves an attacker URL on a prebuilt toolkit whose
// catalogue URL is an exact one. The operator's secret is judged only by the
// catalogue's server, never by anything the editor wrote.
func TestCatalogueSecretIsNeverReleasedToAnAttackerChosenEndpoint(t *testing.T) {
	f := newBindingFixture(t, "mcp_github",
		map[string]any{"url": "https://evil.example/mcp"}, catalogueEntry("https://good.example/mcp"))
	recorder := f.exchange(t, exchangeBody("https://evil.example/token", ""))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "untrusted_token_endpoint")
	f.attackerContacted(t)
}

// A url TEMPLATE lets the user pick the host, so discovery from it would let
// them pick where the secret goes. Without a pinned token_url it is refused with
// the reason, and nothing is dialled at all.
func TestCatalogueTemplateWithoutAPinnedTokenURLRefusesRelease(t *testing.T) {
	f := newBindingFixture(t, "mcp_github",
		map[string]any{"url": "https://evil.example/mcp", "workspace": "evil"},
		catalogueEntry("https://{workspace}.example/mcp"))
	recorder := f.exchange(t, exchangeBody("https://evil.example/token", ""))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "catalogue_token_endpoint_not_pinned")
	require.Zero(t, f.guard.dials.Load(), "no transport call was made")
	f.attackerContacted(t)
}

func TestCatalogueSecretIgnoresEditorSetTokenSettingsAndHints(t *testing.T) {
	for name, settings := range map[string]map[string]any{
		"token_endpoint":           {"url": "https://evil.example/mcp", "token_endpoint": "https://evil.example/token"},
		"token_url":                {"token_url": "https://evil.example/token"},
		"oauth_discovery_endpoint": {"oauth_discovery_endpoint": "https://evil.example"},
		"nested decoy": {"openapi_configuration": map[string]any{
			"client_secret": "decoy", "token_endpoint": "https://evil.example/token",
			"oauth_discovery_endpoint": "https://evil.example",
		}},
		"whitespace secret": {"client_secret": "   ", "token_url": "https://evil.example/token"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newBindingFixture(t, "mcp_github", settings, catalogueEntry("https://{workspace}.example/mcp"))
			recorder := f.exchange(t, exchangeBody("https://evil.example/token", ""))
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Zero(t, f.guard.dials.Load())
			f.attackerContacted(t)
		})
	}
	t.Run("hint", func(t *testing.T) {
		f := newBindingFixture(t, "mcp_github", map[string]any{"url": ""}, catalogueEntry("https://good.example/mcp"))
		recorder := f.exchange(t, exchangeBody("https://good.example/token",
			`,"resource_metadata_url":"https://good.example/hinted"`))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		f.provider.mu.Lock()
		defer f.provider.mu.Unlock()
		require.NotContains(t, f.provider.paths, "/hinted", "a hint is never consulted for a catalogue secret")
	})
}

func TestLegitimateCatalogueBindsThroughItsExactURLOrItsPinnedTokenURL(t *testing.T) {
	f := newBindingFixture(t, "mcp_github", map[string]any{"url": ""}, catalogueEntry("https://good.example/mcp"))
	recorder := f.exchange(t, exchangeBody("https://good.example/token", ""))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "catalogue-secret", f.provider.token.Get("client_secret"))
	require.Positive(t, f.guard.dials.Load(), "discovery and exchange use the authorization guard")

	entry := catalogueEntry("https://{workspace}.example/mcp")
	entry.TokenURL = "https://good.example/token"
	f = newBindingFixture(t, "mcp_github", map[string]any{"url": "https://acme.example/mcp"}, entry)
	recorder = f.exchange(t, exchangeBody("https://good.example/token", ""))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "catalogue-secret", f.provider.token.Get("client_secret"))
	f.provider.mu.Lock()
	require.Equal(t, []string{"/token"}, f.provider.paths, "a pinned token_url needs no discovery")
	f.provider.mu.Unlock()

	f = newBindingFixture(t, "mcp_github", map[string]any{"url": ""}, entry)
	recorder = f.exchange(t, exchangeBody("https://evil.example/token", ""))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	f.attackerContacted(t)
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
	require.Equal(t, http.StatusGatewayTimeout, recorder.Code)
	require.Less(t, time.Since(started), 3*time.Second)
}

func TestDiscoveryCannotConsumeTheExchangeWindow(t *testing.T) {
	f := newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
	// Discovery (two fetches) takes ~300ms of a 400ms window; the exchange then
	// takes ~150ms of its OWN 400ms window. Under one shared 400ms budget the
	// whole call (~450ms) could not finish.
	f.handler.mcpDiscoveryBudget = 400 * time.Millisecond
	f.handler.mcpExchangeBudget = 400 * time.Millisecond
	f.provider.delay = 150 * time.Millisecond
	recorder := f.exchange(t, exchangeBody("https://own.example/token", ""))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "own-secret", f.provider.token.Get("client_secret"))
}

func TestDiscoveryIsBoundedByItsOwnShare(t *testing.T) {
	f := newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
	f.handler.mcpDiscoveryBudget = 250 * time.Millisecond
	f.handler.mcpExchangeBudget = 5 * time.Second
	f.provider.delay = 200 * time.Millisecond
	started := time.Now()
	recorder := f.exchange(t, exchangeBody("https://own.example/token", ""))
	require.Equal(t, http.StatusGatewayTimeout, recorder.Code, recorder.Body.String())
	require.Less(t, time.Since(started), 2*time.Second)
	require.Empty(t, f.provider.token)
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
			_, syncErr := f.handler.syncMetadataSource().discover(context.Background(), endpoint, "", mcpSyncMaxServers, false, nil)
			_, guardedErr := f.handler.authorizationMetadataSource().discover(context.Background(), endpoint, "", mcpBindingMaxServers, true, nil)
			require.Error(t, syncErr)
			require.Error(t, guardedErr)
		})
	}
}

// A hint never steers a released secret. On a multi-tenant origin another
// tenant's path can publish metadata that names the requested resource and an
// attacker's token endpoint; with a secret in play the hint is not even fetched.
func TestAHintNeverSteersAReleasedSecret(t *testing.T) {
	for name, hint := range map[string]string{
		"same-origin other tenant": "https://saas.example/tenantB/md",
		"cross-origin":             "https://evil.example/md",
	} {
		t.Run(name, func(t *testing.T) {
			f := newBindingFixture(t, "mcp",
				map[string]any{"url": "https://saas.example/tenantA/mcp", "client_secret": "own-secret"}, nil)
			f.provider.metadata = func(host, path string) (int, string) {
				switch path {
				case "/tenantB/md":
					return 200, `{"resource":"https://saas.example/tenantA/mcp","authorization_servers":["https://evil.example"]}`
				case "/.well-known/oauth-protected-resource/tenantA/mcp":
					return 200, `{"resource":"https://saas.example/tenantA/mcp","authorization_servers":["https://saas.example"]}`
				case "/.well-known/oauth-authorization-server":
					if host == "evil.example" {
						return 200, `{"issuer":"https://evil.example","authorization_endpoint":"https://evil.example/a","token_endpoint":"https://evil.example/token"}`
					}
					return 200, `{"issuer":"https://saas.example","authorization_endpoint":"https://saas.example/a","token_endpoint":"https://saas.example/token"}`
				}
				return 404, `{}`
			}
			recorder := f.exchange(t, exchangeBody("https://evil.example/token", `,"resource_metadata_url":"`+hint+`"`))
			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			f.attackerContacted(t)
			f.provider.mu.Lock()
			require.NotContains(t, f.provider.paths, "/tenantB/md")
			f.provider.mu.Unlock()

			// ...and the real endpoint still binds, from the well-known paths.
			recorder = f.exchange(t, exchangeBody("https://saas.example/token", `,"resource_metadata_url":"`+hint+`"`))
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		})
	}
}

// Hints stay usable for a public client (no secret is at stake), and fall back
// to the well-known locations when they fail or do not name the endpoint asked
// about.
func TestAPublicClientsHintFallsBackToWellKnown(t *testing.T) {
	f := newBindingFixture(t, "mcp", map[string]any{"url": "https://own.example/mcp"}, nil)
	endpoint, err := url.Parse("https://own.example/token")
	require.NoError(t, err)
	bind := func(hint string) bindingOutcome {
		return f.handler.mcpDiscoveryBindsTokenEndpoint(context.Background(), mcpBindingRequest{
			projectID: 7, toolkitID: 19, startURL: "https://own.example/mcp", tokenEndpoint: endpoint, advertised: hint,
		})
	}

	// A hint that fails.
	require.Equal(t, bindingBound, bind("https://own.example/missing-md"))

	// A hint that succeeds but advertises a different endpoint.
	f2 := newBindingFixture(t, "mcp", map[string]any{"url": "https://own.example/mcp"}, nil)
	f2.provider.metadata = func(host, path string) (int, string) {
		if path == "/other-md" {
			return 200, `{"resource":"https://` + host + `/mcp","authorization_servers":["https://decoy.example"]}`
		}
		if host == "decoy.example" && path == "/.well-known/oauth-authorization-server" {
			return 200, `{"issuer":"https://decoy.example","authorization_endpoint":"https://decoy.example/a","token_endpoint":"https://decoy.example/token"}`
		}
		return goodMetadata(host, path)
	}
	f = f2
	require.Equal(t, bindingBound, bind("https://own.example/other-md"))
	f.provider.mu.Lock()
	require.Contains(t, f.provider.paths, "/other-md", "the public client's hint was tried first")
	f.provider.mu.Unlock()
}

func TestATimeoutOnThePathSpecificDocumentStillTriesTheRootDocument(t *testing.T) {
	f := newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
	f.provider.slowPath = "/.well-known/oauth-protected-resource/mcp"
	f.handler.mcpDiscoveryFetchTimeout = 100 * time.Millisecond
	recorder := f.exchange(t, exchangeBody("https://own.example/token", ""))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	require.Contains(t, f.provider.paths, "/.well-known/oauth-protected-resource", "the root document was tried")
}

func TestUnpinnedCatalogueRefusalTellsTheUserWhatToDo(t *testing.T) {
	f := newBindingFixture(t, "mcp_github", map[string]any{"url": "https://x.example/acme/mcp"},
		catalogueEntry("https://x.example/{workspace}/mcp"))
	recorder := f.exchange(t, exchangeBody("https://x.example/token", ""))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "catalogue_token_endpoint_not_pinned")
	require.Contains(t, recorder.Body.String(), "operator must pin")
}

func TestCatalogueAndOwnBindingsDoNotEvictEachOther(t *testing.T) {
	f := newBindingFixture(t, "mcp_github", map[string]any{"url": ""}, catalogueEntry("https://good.example/mcp"))
	f.handler.mcpBindings.capacity = 1
	require.Equal(t, http.StatusOK, f.exchange(t, exchangeBody("https://good.example/token", "")).Code)

	// Churn the own-secret cache far past its capacity.
	now := time.Now()
	for toolkit := int32(100); toolkit < 110; toolkit++ {
		f.handler.mcpBindings.store(mcpBindingKey{projectID: 7, toolkitID: toolkit, startURL: "https://t.example/mcp"},
			[]string{"https://i.example/token"}, now)
	}
	// The catalogue binding survived: a metadata outage still releases.
	f.provider.metadata = func(string, string) (int, string) { return 503, `{}` }
	require.Equal(t, http.StatusOK, f.exchange(t, exchangeBody("https://good.example/token", "")).Code)
}

func twoServerMetadata(first, second string) func(host, path string) (int, string) {
	return func(host, path string) (int, string) {
		switch {
		case strings.HasPrefix(path, "/.well-known/oauth-protected-resource"):
			return 200, `{"resource":"https://` + host + `/mcp","authorization_servers":["https://` + first + `","https://` + second + `"]}`
		case path == "/.well-known/oauth-authorization-server":
			return 200, `{"issuer":"https://` + host + `","authorization_endpoint":"https://` + host +
				`/authorize","token_endpoint":"https://` + host + `/token"}`
		}
		return 404, `{}`
	}
}

func TestDiscoveryStopsAtTheMatchingServerAndNeverWaitsOnLaterOnes(t *testing.T) {
	f := newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
	f.provider.metadata = twoServerMetadata("first.example", "slow.example")
	f.provider.slowHost = "slow.example"
	f.handler.mcpDiscoveryFetchTimeout = 100 * time.Millisecond
	recorder := f.exchange(t, exchangeBody("https://first.example/token", ""))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.False(t, f.provider.sawHost("slow.example"), "the later server was never asked")
}

func TestATimeoutIsReportedAsATimeoutNotAsUntrusted(t *testing.T) {
	f := newBindingFixture(t, "mcp",
		map[string]any{"url": "https://own.example/mcp", "client_secret": "own-secret"}, nil)
	f.provider.metadata = twoServerMetadata("slow.example", "slow.example")
	f.provider.slowHost = "slow.example"
	f.handler.mcpDiscoveryFetchTimeout = 100 * time.Millisecond
	recorder := f.exchange(t, exchangeBody("https://elsewhere.example/token", ""))
	require.Equal(t, http.StatusGatewayTimeout, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "oauth_discovery_timeout")
}

func TestBindingCacheIsAnLRUNotAWipeAll(t *testing.T) {
	cache := &mcpBindingCache{capacity: 2}
	now := time.Now()
	endpoint, err := url.Parse("https://idp.example/token")
	require.NoError(t, err)
	key := func(toolkit int32) mcpBindingKey {
		return mcpBindingKey{projectID: 1, toolkitID: toolkit, startURL: "https://t.example/mcp"}
	}
	cache.store(key(1), []string{endpoint.String()}, now)
	cache.store(key(2), []string{endpoint.String()}, now)
	require.True(t, cache.lookup(key(1), endpoint, now), "touching 1 makes 2 the oldest")
	cache.store(key(3), []string{endpoint.String()}, now)
	require.True(t, cache.lookup(key(1), endpoint, now), "a fresh entry survives another tenant's churn")
	require.True(t, cache.lookup(key(3), endpoint, now))
	require.False(t, cache.lookup(key(2), endpoint, now), "only the least recently used entry was evicted")
	require.False(t, cache.lookup(key(1), endpoint, now.Add(mcpBindingCacheTTL+time.Second)), "expired entries go")
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
