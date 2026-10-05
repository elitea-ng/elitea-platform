package storage

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net/http"
	"net/url"
	"testing"
	"time"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

func codeOwnerClientTLSFixture(t *testing.T, identity string) *tls.Config {
	t.Helper()
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Unix(1, 0), NotAfter: time.Unix(9999999, 0), URIs: []*url.URL{uri}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(bytes.NewReader(bytes.Repeat([]byte{3}, 1024)), template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}
func TestCodeOwnerClientRequiresConfiguredExactMTLSIdentitiesAndFixedOrigin(t *testing.T) {
	requester := "spiffe://elitea/main"
	audience := "spiffe://elitea/supervisor/one"
	for _, name := range []string{"configured", "redirecting origin path", "plain HTTP", "URL credentials", "URL query", "URL fragment", "insecure TLS", "missing CA", "different Main certificate", "certificate callback", "duplicate audience"} {
		t.Run(name, func(t *testing.T) {
			cfg := CodeOwnerEndpoint{Audience: audience, Origin: "https://owner.invalid", TLS: codeOwnerClientTLSFixture(t, requester)}
			switch name {
			case "redirecting origin path":
				cfg.Origin += "/other"
			case "plain HTTP":
				cfg.Origin = "http://owner.invalid"
			case "URL credentials":
				cfg.Origin = "https://user:secret@owner.invalid"
			case "URL query":
				cfg.Origin += "?target=other"
			case "URL fragment":
				cfg.Origin += "#other"
			case "insecure TLS":
				cfg.TLS.InsecureSkipVerify = true
			case "missing CA":
				cfg.TLS.RootCAs = nil
			case "different Main certificate":
				cfg.TLS = codeOwnerClientTLSFixture(t, "spiffe://elitea/worker/one")
			case "certificate callback":
				cfg.TLS.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cfg.TLS.Certificates[0], nil }
			}
			endpoints := []CodeOwnerEndpoint{cfg}
			if name == "duplicate audience" {
				endpoints = append(endpoints, cfg)
			}
			client, err := NewCodeOwnerClient(requester, endpoints)
			if name != "configured" {
				if err == nil || client != nil {
					t.Fatal("unsafe owner client configuration admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			transport := client.endpoints[audience].client.Transport.(*http.Transport)
			if transport.Proxy != nil || !transport.DisableCompression || transport.MaxConnsPerHost != 2 || transport.TLSClientConfig.MinVersion < tls.VersionTLS13 {
				t.Fatal("configured client broadened network/TLS bounds")
			}
			leaf := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "elitea", Path: "/supervisor/one"}}}
			state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
			if transport.TLSClientConfig.VerifyConnection(state) != nil {
				t.Fatal("configured exact owner refused")
			}
			state.VerifiedChains = nil
			if transport.TLSClientConfig.VerifyConnection(state) == nil {
				t.Fatal("unverified Supervisor accepted")
			}
			state.VerifiedChains = [][]*x509.Certificate{{leaf}}
			other := *leaf
			other.URIs = []*url.URL{{Scheme: "spiffe", Host: "elitea", Path: "/supervisor/two"}}
			state.PeerCertificates = []*x509.Certificate{&other}
			if transport.TLSClientConfig.VerifyConnection(state) == nil {
				t.Fatal("another verified Supervisor accepted")
			}
			if client.endpoints[audience].client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
				t.Fatal("owner credential redirect permitted")
			}
			_, err = client.ReadRetainedRuntime(t.Context(), code.PlatformGrantClaims{}, code.SignedGrant{})
			if err == nil {
				t.Fatal("unsigned caller fields selected owner operation")
			}
		})
	}
}
