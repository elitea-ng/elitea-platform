package artifacts_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
)

// Download names its two refusals with sentinels, and never returns an
// object cut short at the read bound: a cut JSON document is not a shorter
// one, and a caller that parses it reports the wrong fault.
func TestDownloadRefusesAMissingAndAnOversizedObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphs/graph.json"):
			_, _ = w.Write([]byte(`{"nodes": []}`))
		case strings.HasSuffix(r.URL.Path, "/graphs/big.json"):
			_, _ = w.Write([]byte(strings.Repeat("x", artifacts.MaxDownloadBytes+1)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := artifacts.NewHTTPClient(artifacts.Settings{BaseURL: server.URL, APIKey: "token", ProjectID: "7"}, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if data, err := client.Download(ctx, "graphs", "graph.json"); err != nil || string(data) != `{"nodes": []}` {
		t.Fatalf("%q %v", data, err)
	}
	if _, err := client.Download(ctx, "graphs", "absent.json"); !errors.Is(err, artifacts.ErrNotFound) ||
		!strings.HasPrefix(err.Error(), "artifact not found: graphs/absent.json") {
		t.Fatalf("a missing object: %v", err)
	}
	if data, err := client.Download(ctx, "graphs", "big.json"); !errors.Is(err, artifacts.ErrTooLarge) || data != nil {
		t.Fatalf("an oversized object: %d bytes, %v", len(data), err)
	}
}
