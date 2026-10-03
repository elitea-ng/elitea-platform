package api

// #6885 lets a project VIEWER call the MCP OAuth and DCR proxies. Both POST to
// a URL the caller sends. These tests call each proxy as a viewer, through the
// production router, and check that a private, loopback or link-local
// destination is refused before any connection, and that a DNS answer that
// changes between the check and the dial is refused at the dial.

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
	v2analytics "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/analytics"
	v2convs "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	v2folders "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	v2pipelinetriggers "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	v2tags "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/tags"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

// scriptedResolver answers each lookup from a list, one answer per call. The
// last answer repeats.
type scriptedResolver struct {
	calls   atomic.Int32
	answers [][]net.IP
}

func (s *scriptedResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	index := int(s.calls.Add(1)) - 1
	if index >= len(s.answers) {
		index = len(s.answers) - 1
	}
	out := make([]net.IPAddr, 0, len(s.answers[index]))
	for _, ip := range s.answers[index] {
		out = append(out, net.IPAddr{IP: ip})
	}
	return out, nil
}

func newViewerMCPRouter(guard *webhook.DestinationGuard) chi.Router {
	return NewRouter(RouterConfig{
		AuthValidator:               testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:          testPrincipalValidator{},
		AppsRepo:                    struct{ applications.Repository }{},
		SkillsRepo:                  struct{ v2skills.Repository }{},
		FoldersRepo:                 struct{ v2folders.Repository }{},
		TagsRepo:                    struct{ v2tags.Repository }{},
		ConvsRepo:                   struct{ v2convs.Repository }{},
		AnalyticsRepo:               struct{ v2analytics.Repository }{},
		ProjectAccessQuerier:        &memberOfProject{project: "7"},
		ProjectPermissionResolver:   fakePermissionResolver{granted: viewerPermissions, forProject: "7"},
		PipelineTriggers:            v2pipelinetriggers.NewHandler(nil),
		MCPAuthorizationEgressGuard: guard,
	})
}

// countingServer counts every request that reaches it.
func countingServer(t *testing.T, tls bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"internal answer"}`))
	})
	var server *httptest.Server
	if tls {
		server = httptest.NewTLSServer(handler)
	} else {
		server = httptest.NewServer(handler)
	}
	t.Cleanup(server.Close)
	return server, &hits
}

type mcpProxyCase struct {
	path    string
	field   string
	refusal string
	body    map[string]any
}

func mcpProxyCases() []mcpProxyCase {
	return []mcpProxyCase{
		{
			path: "/api/v2/elitea_core/mcp_oauth_proxy/7", field: "token_endpoint",
			refusal: "invalid_token_endpoint",
			body:    map[string]any{"code": "c", "redirect_uri": "https://app.example/cb"},
		},
		{
			path: "/api/v2/elitea_core/mcp_dcr_proxy/7", field: "registration_endpoint",
			refusal: "invalid_registration_endpoint",
			body:    map[string]any{"redirect_uris": []string{"https://app.example/cb"}},
		},
	}
}

func postMCPProxy(t *testing.T, router chi.Router, proxy mcpProxyCase, endpoint string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{proxy.field: endpoint}
	for key, value := range proxy.body {
		body[key] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, proxy.path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, testAuthHeader(request))
	return recorder
}

// The production composition with NO configured guard still refuses: the
// router builds a guard with an empty allowlist.
func TestViewerMCPProxiesRefuseInternalDestinations(t *testing.T) {
	loopback, loopbackHits := countingServer(t, false)
	private := &scriptedResolver{answers: [][]net.IP{{net.ParseIP("10.20.30.40")}}}

	for _, proxy := range mcpProxyCases() {
		for name, tc := range map[string]struct {
			guard    *webhook.DestinationGuard
			endpoint string
		}{
			"loopback http":          {endpoint: loopback.URL + "/token"},
			"localhost name":         {endpoint: strings.Replace(loopback.URL, "127.0.0.1", "localhost", 1) + "/token"},
			"metadata link-local":    {endpoint: "https://169.254.169.254/latest/meta-data"},
			"rfc1918 literal":        {endpoint: "https://10.0.0.1/admin"},
			"ipv6 loopback":          {endpoint: "https://[::1]/token"},
			"name resolving private": {guard: webhook.NewDestinationGuardWithResolver(nil, private), endpoint: "https://idp.internal.example/token"},
		} {
			t.Run(proxy.field+"/"+name, func(t *testing.T) {
				recorder := postMCPProxy(t, newViewerMCPRouter(tc.guard), proxy, tc.endpoint)
				if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), proxy.refusal) {
					t.Fatalf("viewer POST %s to %s = %d %s, want 400 %s",
						proxy.path, tc.endpoint, recorder.Code, recorder.Body.String(), proxy.refusal)
				}
			})
		}
	}
	if hits := loopbackHits.Load(); hits != 0 {
		t.Fatalf("the loopback listener was reached %d time(s); a refused destination must see no connection", hits)
	}
}

// DNS rebinding: the name resolves PUBLIC when the proxy checks it and to
// loopback when the transport dials. The dial-time guard must refuse it, so
// the listener on loopback sees nothing. The listener counts TCP accepts, not
// HTTP requests: an unguarded dial would connect and then fail the TLS
// handshake, which an HTTP-level counter could not tell from a refused dial.
func TestViewerMCPProxiesRefuseARebindAtDialTime(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var hits atomic.Int32
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			hits.Add(1)
			_ = conn.Close()
		}
	}()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split %s: %v", listener.Addr(), err)
	}

	for _, proxy := range mcpProxyCases() {
		t.Run(proxy.field, func(t *testing.T) {
			resolver := &scriptedResolver{answers: [][]net.IP{
				{net.ParseIP("93.184.216.34")},
				{net.ParseIP("127.0.0.1")},
			}}
			router := newViewerMCPRouter(webhook.NewDestinationGuardWithResolver(nil, resolver))
			recorder := postMCPProxy(t, router, proxy, "https://rebind.example:"+port+"/token")
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("rebind POST %s = %d %s, want 502 from a refused dial",
					proxy.path, recorder.Code, recorder.Body.String())
			}
			if resolver.calls.Load() < 2 {
				t.Fatalf("the name was resolved %d time(s); the dial must resolve it again", resolver.calls.Load())
			}
		})
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("the rebound loopback listener was reached %d time(s)", got)
	}
}

// An operator allowlist that names the private network admits it. Without
// this, a guard that refused everything would pass the two tests above.
func TestMCPProxiesReachAnAllowlistedPrivateIdP(t *testing.T) {
	server, hits := countingServer(t, false)
	allowlist, err := egresslib.Parse([]string{"127.0.0.0/8"})
	if err != nil {
		t.Fatalf("parse allowlist: %v", err)
	}
	router := newViewerMCPRouter(webhook.NewDestinationGuard(allowlist))

	recorder := postMCPProxy(t, router, mcpProxyCases()[0], server.URL+"/token")
	// The stub IdP answers an OAuth error, which the proxy relays as 400 with
	// the provider's error code. What matters is that the request arrived.
	if hits.Load() != 1 {
		t.Fatalf("the allowlisted IdP was reached %d time(s), want 1 (status %d %s)",
			hits.Load(), recorder.Code, recorder.Body.String())
	}
}
