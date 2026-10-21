package artifacts_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
)

// The callback hop's TLS origin is elitea-main's platform listener (#1359),
// which requires a runtime-CA client certificate at the handshake. The upload
// client must present the configured one; without it the listener refuses.
func TestUploadPresentsTheCallbackClientCertificate(t *testing.T) {
	directory := t.TempDir()
	caKey, caCert := newTestCA(t)
	writePEM(t, filepath.Join(directory, "runtime-ca.crt"), "CERTIFICATE", caCert.Raw)
	serverPair := issueLeaf(t, caKey, caCert, x509.ExtKeyUsageServerAuth, "", "")
	issueLeaf(t, caKey, caCert, x509.ExtKeyUsageClientAuth,
		filepath.Join(directory, "callback-client.crt"), filepath.Join(directory, "callback-client.key"))

	clients := x509.NewCertPool()
	clients.AddCert(caCert)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverPair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clients,
	}
	server.StartTLS()
	defer server.Close()

	settings := artifacts.Settings{BaseURL: server.URL, APIKey: "token", ProjectID: "7"}
	withIdentity, err := artifacts.NewHTTPClient(settings, artifacts.CallbackTLS{
		CAFile:   filepath.Join(directory, "runtime-ca.crt"),
		CertFile: filepath.Join(directory, "callback-client.crt"),
		KeyFile:  filepath.Join(directory, "callback-client.key"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := withIdentity.Upload(context.Background(), "wiki", "page.md", []byte("x")); err != nil {
		t.Fatalf("the upload with the callback identity failed: %v", err)
	}

	anonymous, err := artifacts.NewHTTPClient(settings, artifacts.CallbackTLS{CAFile: filepath.Join(directory, "runtime-ca.crt")})
	if err != nil {
		t.Fatal(err)
	}
	if err := anonymous.Upload(context.Background(), "wiki", "page.md", []byte("x")); err == nil {
		t.Fatal("the platform listener stand-in served an upload with no client certificate")
	}

	if _, err := artifacts.NewHTTPClient(settings, artifacts.CallbackTLS{CertFile: "/only-the-cert"}); err == nil {
		t.Fatal("a certificate without its key was accepted")
	}
}

func newTestCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "runtime test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, certificate
}

// issueLeaf signs a leaf; with paths it also writes the PEM pair there.
func issueLeaf(t *testing.T, caKey *ecdsa.PrivateKey, ca *x509.Certificate, usage x509.ExtKeyUsage, certPath, keyPath string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "leaf"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if certPath != "" {
		writePEM(t, certPath, "CERTIFICATE", der)
		writePEM(t, keyPath, "PRIVATE KEY", pkcs8)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
