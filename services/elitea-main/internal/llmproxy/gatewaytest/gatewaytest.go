// Package gatewaytest gives a test an llmproxy.GatewayClient connected over
// real mutual TLS to a test gateway that runs an http.Handler
// (llmproxytest). It is separate from llmproxytest because llmproxy's own
// tests use that package, and this one imports llmproxy.
package gatewaytest

import (
	"net/http"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy/llmproxytest"
)

// Client starts a test gateway that serves h and returns a client of it
// that signs the identity under secret. Both stop when the test ends.
func Client(t testing.TB, h http.Handler, secret string) *llmproxy.GatewayClient {
	t.Helper()
	client, _ := ClientAndServer(t, h, secret)
	return client
}

// ClientAndServer is Client that also returns the server, for a test that
// stops it.
func ClientAndServer(t testing.TB, h http.Handler, secret string) (*llmproxy.GatewayClient, *llmproxytest.Server) {
	t.Helper()
	server := llmproxytest.Start(t, h)
	client, err := llmproxy.NewGatewayClient(llmproxy.GatewayClientConfig{
		Target:         server.Target,
		IdentitySecret: secret,
		TLS:            server.ClientTLS(t),
	})
	if err != nil {
		t.Fatalf("gatewaytest: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, server
}
