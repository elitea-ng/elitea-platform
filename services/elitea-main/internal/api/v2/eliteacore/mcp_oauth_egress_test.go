package eliteacore

import (
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
)

// TestGuardedMCPAuthorizationClientDropsProxyAndCapsHeaders proves the OAuth
// and DCR proxies keep the egress guarantees when they clone a configured
// transport: a proxy on the base transport would make the guard check and
// dial the proxy's address instead of the identity provider's.
func TestGuardedMCPAuthorizationClientDropsProxyAndCapsHeaders(t *testing.T) {
	handler := &Handler{
		httpClient:            &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}},
		mcpAuthorizationGuard: egress.New(nil),
	}
	transport, ok := handler.guardedMCPAuthorizationClient().Transport.(*http.Transport)
	if !ok {
		t.Fatal("guarded MCP authorization client has no *http.Transport")
	}
	if transport.Proxy != nil {
		t.Error("guarded MCP authorization transport keeps a Proxy function")
	}
	if transport.DialContext == nil || transport.DialTLSContext != nil {
		t.Error("guarded MCP authorization transport does not dial only through the guard")
	}
	if transport.MaxResponseHeaderBytes != egress.MaxResponseHeaderBytes {
		t.Errorf("MaxResponseHeaderBytes = %d, want %d", transport.MaxResponseHeaderBytes, egress.MaxResponseHeaderBytes)
	}
}
