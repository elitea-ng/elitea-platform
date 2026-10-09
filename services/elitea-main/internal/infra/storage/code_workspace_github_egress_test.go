package storage

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/egress"
)

// TestCodeWorkspaceGitHubDialsOnlyThroughTheEgressGuard proves the GitHub
// reader uses the shared egress guard: reserved, metadata and NAT64-embedded
// addresses are refused before any connect, even when the operator's host
// allowlist names them, and the transport carries no proxy.
func TestCodeWorkspaceGitHubDialsOnlyThroughTheEgressGuard(t *testing.T) {
	allowed, err := egresslib.Parse([]string{"github.example", "10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewCodeWorkspaceGitHub(allowed)
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	transport, ok := adapter.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("GitHub reader has no *http.Transport")
	}
	if transport.Proxy != nil || transport.MaxResponseHeaderBytes != egress.MaxResponseHeaderBytes {
		t.Errorf("Proxy set=%v MaxResponseHeaderBytes=%d, want no proxy and %d",
			transport.Proxy != nil, transport.MaxResponseHeaderBytes, egress.MaxResponseHeaderBytes)
	}
	for _, address := range []string{"0.0.0.1", "192.0.0.8", "240.0.0.1", "100.100.100.200", "64:ff9b::a9fe:a9fe"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := transport.DialContext(ctx, "tcp", net.JoinHostPort(address, "443"))
		cancel()
		if !errors.Is(err, egress.ErrDestinationRefused) {
			t.Errorf("dial %s = %v, want egress.ErrDestinationRefused", address, err)
		}
	}
}
