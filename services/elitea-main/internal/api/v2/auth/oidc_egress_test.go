package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
)

// mapResolver answers a fixed address per host name.
type mapResolver map[string]string

func (m mapResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if addr, ok := m[host]; ok {
		return []net.IPAddr{{IP: net.ParseIP(addr)}}, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// loopbackIdentityClient is the guarded client with the development loopback
// flag on, for tests whose fake identity provider is an httptest server.
func loopbackIdentityClient(t *testing.T) *http.Client {
	t.Helper()
	client, err := NewIdentityProviderHTTPClient(nil, true)
	if err != nil {
		t.Fatalf("NewIdentityProviderHTTPClient: %v", err)
	}
	return client
}

func guardedClient(t *testing.T, allowlist []string, loopback bool, resolver mapResolver) *http.Client {
	t.Helper()
	client, err := NewIdentityProviderHTTPClient(allowlist, loopback, egress.WithResolver(resolver))
	if err != nil {
		t.Fatalf("NewIdentityProviderHTTPClient: %v", err)
	}
	return client
}

func TestDiscoveryRefusesMetadataAndUnpermittedPrivateIssuers(t *testing.T) {
	resolver := mapResolver{"metadata.idp.test": "169.254.169.254", "private.idp.test": "10.0.0.1"}
	// Even an allowlist that names the whole private and link-local space
	// cannot open the metadata address.
	client := guardedClient(t, []string{"169.254.0.0/16"}, false, resolver)
	for _, issuer := range []string{"http://metadata.idp.test", "http://private.idp.test", "http://169.254.169.254"} {
		_, err := discoverOIDCProvider(context.Background(), client, issuer)
		if !errors.Is(err, egress.ErrDestinationRefused) {
			t.Fatalf("discovery of %s: err = %v, want the egress refusal", issuer, err)
		}
	}
}

func TestDiscoveryAdmitsAPermittedPrivateIssuer(t *testing.T) {
	resolver := mapResolver{"idp.corp.test": "10.0.0.1"}
	client := guardedClient(t, []string{"10.0.0.0/8"}, false, resolver)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// Nothing listens there; the point is that the guard let the dial happen.
	_, err := discoverOIDCProvider(ctx, client, "http://idp.corp.test")
	if err == nil || errors.Is(err, egress.ErrDestinationRefused) {
		t.Fatalf("discovery of a permitted private issuer: err = %v, want a dial error, not the refusal", err)
	}
}

func TestLoopbackNeedsTheDevelopmentFlag(t *testing.T) {
	server := newDiscoveryServer(t, "")
	// A loopback allowlist entry is ignored without the flag.
	refused := guardedClient(t, []string{"127.0.0.1", "localhost"}, false, mapResolver{})
	if _, err := discoverOIDCProvider(context.Background(), refused, server.URL); !errors.Is(err, egress.ErrDestinationRefused) {
		t.Fatalf("loopback without the flag: err = %v, want the egress refusal", err)
	}
	if _, err := discoverOIDCProvider(context.Background(), loopbackIdentityClient(t), server.URL); err != nil {
		t.Fatalf("loopback with the flag: %v", err)
	}
}

func TestTheDiscoveredKeySetUsesTheGuardedClient(t *testing.T) {
	server := newDiscoveryServer(t, "http://keys.idp.test/jwks")
	client := guardedClient(t, nil, true, mapResolver{"keys.idp.test": "169.254.169.254"})
	provider, err := discoverOIDCProvider(context.Background(), client, server.URL)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	// Any compact token makes the verifier fetch the key set first.
	_, err = provider.Verifier(&oidc.Config{SkipClientIDCheck: true, SkipExpiryCheck: true, SkipIssuerCheck: true}).Verify(context.Background(), "eyJhbGciOiJSUzI1NiIsImtpZCI6ImsifQ.e30.c2ln")
	if err == nil || !strings.Contains(err.Error(), egress.ErrDestinationRefused.Error()) {
		t.Fatalf("verify with a metadata JWKS URI: err = %v, want the egress refusal", err)
	}
}

func TestNilClientRefusesDiscovery(t *testing.T) {
	if _, err := discoverOIDCProvider(context.Background(), nil, "https://idp.example"); err == nil {
		t.Fatal("discovery without a guarded client must fail")
	}
}

func TestNamesLoopback(t *testing.T) {
	for entry, want := range map[string]bool{
		"localhost": true, "127.0.0.1": true, "127.0.0.1:9400": true, "::1": true, "[::1]:80": true,
		"127.0.0.0/8": true, "0.0.0.0/0": true, "::/0": true,
		"10.0.0.0/8": false, "10.0.0.5:8443": false, "idp.corp.example": false, "*.corp.example": false,
	} {
		if got := namesLoopback(entry); got != want {
			t.Errorf("namesLoopback(%q) = %v, want %v", entry, got, want)
		}
	}
}

// Every OIDC discovery in elitea-main goes through discoverOIDCProvider.
func TestNoUnguardedOIDCDiscovery(t *testing.T) {
	_, self, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(self), "..", "..", "..")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)
		if strings.Contains(text, "oidc.NewProvider(") && filepath.Base(path) != "oidc_egress.go" {
			t.Errorf("%s calls oidc.NewProvider directly; use discoverOIDCProvider", path)
		}
		if strings.Contains(path, filepath.Join("api", "v2", "auth")) && strings.Count(text, "oidc.NewRemoteKeySet(") != strings.Count(text, "oidc.NewRemoteKeySet(oidc.ClientContext(") {
			t.Errorf("%s builds a key set without the guarded client", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// newDiscoveryServer serves a discovery document whose issuer is itself.
func newDiscoveryServer(t *testing.T, jwksURI string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		keys := jwksURI
		if keys == "" {
			keys = server.URL + "/jwks"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 server.URL,
			"authorization_endpoint": server.URL + "/authorize",
			"token_endpoint":         server.URL + "/token",
			"jwks_uri":               keys,
		})
	}))
	t.Cleanup(server.Close)
	return server
}
