package natsconn

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lookupOf(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func TestFromEnvAllOrNothing(t *testing.T) {
	m, err := FromEnv("GATEWAY", lookupOf(nil))
	if err != nil || m.Enabled() || m.Mode() != "none" {
		t.Fatalf("no variables: got %+v, %v; want the plaintext posture", m, err)
	}
	m, err = FromEnv("GATEWAY", lookupOf(map[string]string{
		"GATEWAY_NATS_TLS_CA_FILE":   "/ca",
		"GATEWAY_NATS_TLS_CERT_FILE": "/crt",
		"GATEWAY_NATS_TLS_KEY_FILE":  "/key",
	}))
	if err != nil || !m.Enabled() || m.Mode() != "mtls" || m.CAFile != "/ca" {
		t.Fatalf("all three: got %+v, %v", m, err)
	}
	for _, partial := range []map[string]string{
		{"GATEWAY_NATS_TLS_CERT_FILE": "/crt", "GATEWAY_NATS_TLS_KEY_FILE": "/key"},
		{"GATEWAY_NATS_TLS_CA_FILE": "/ca"},
		{"GATEWAY_NATS_TLS_CA_FILE": "/ca", "GATEWAY_NATS_TLS_CERT_FILE": "/crt"},
	} {
		if _, err := FromEnv("GATEWAY", lookupOf(partial)); err == nil {
			t.Errorf("half-configured %v accepted", partial)
		} else if !strings.Contains(err.Error(), "half-configured") {
			t.Errorf("error does not say what is wrong: %v", err)
		}
	}
	if _, err := FromEnv("GATEWAY", nil); err == nil {
		t.Error("nil lookup accepted")
	}
}

func TestCheckURL(t *testing.T) {
	secured := Material{CAFile: "/ca", CertFile: "/crt", KeyFile: "/key", envPrefix: "GATEWAY"}
	plain := Material{envPrefix: "GATEWAY"}
	cases := []struct {
		name string
		m    Material
		url  string
		ok   bool
	}{
		{"mtls tls url", secured, "tls://elitea-nats.elitea.svc.cluster.local:4222", true},
		{"mtls tls list", secured, "tls://a:4222, tls://b:4222", true},
		{"mtls nats url", secured, "nats://elitea-nats:4222", false},
		{"mtls schemeless", secured, "elitea-nats:4222", false},
		{"mtls one plaintext in list", secured, "tls://a:4222,nats://b:4222", false},
		{"mtls credential", secured, "tls://user:pass@a:4222", false},
		{"mtls token", secured, "tls://token@a:4222", false},
		{"plain nats", plain, "nats://nats:4222", true},
		{"plain credential allowed in dev", plain, "nats://u:p@nats:4222", true},
		{"plain tls refused", plain, "tls://nats:4222", false},
	}
	for _, c := range cases {
		err := c.m.CheckURL(c.url)
		if (err == nil) != c.ok {
			t.Errorf("%s: CheckURL(%q) = %v, want ok=%v", c.name, c.url, err, c.ok)
		}
		if err != nil && strings.Contains(err.Error(), "pass@") {
			t.Errorf("%s: error echoes the credential: %v", c.name, err)
		}
	}
}

func TestIdentityNames(t *testing.T) {
	if got := IdentityURI(DefaultTrustDomain, IdentityGateway); got != "spiffe://elitea.internal/nats/elitea-llm-gateway" {
		t.Errorf("IdentityURI = %q", got)
	}
	if got := InboxPrefix(IdentityMain); got != "_INBOX_elitea-main" {
		t.Errorf("InboxPrefix = %q", got)
	}
	if BaseTLSConfig().MinVersion != tls.VersionTLS13 {
		t.Error("BaseTLSConfig is not TLS 1.3 only")
	}
}

// cert-manager renews a certificate in place. The callbacks must read the
// files again, so the next handshake presents the new certificate.
func TestCallbacksRereadRotatedFiles(t *testing.T) {
	dir := t.TempDir()
	m := Material{
		CAFile:   filepath.Join(dir, "ca.crt"),
		CertFile: filepath.Join(dir, "tls.crt"),
		KeyFile:  filepath.Join(dir, "tls.key"),
	}
	first := writePair(t, m, "first")
	if err := m.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	got, err := m.ClientCertificate()
	if err != nil || !bytes.Equal(got.Certificate[0], first) {
		t.Fatalf("first read: %v", err)
	}
	second := writePair(t, m, "second")
	got, err = m.ClientCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Certificate[0], second) {
		t.Error("ClientCertificate returned the old certificate after the files were replaced")
	}
	pool, err := m.RootCAs()
	if err != nil || pool == nil {
		t.Fatalf("RootCAs: %v", err)
	}
	if err := os.WriteFile(m.CAFile, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RootCAs(); err == nil {
		t.Error("a CA file with no PEM certificate was accepted")
	}
}

func TestCheckNamesAMissingFile(t *testing.T) {
	m := Material{CAFile: "/nonexistent/ca.crt", CertFile: "/nonexistent/tls.crt", KeyFile: "/nonexistent/tls.key"}
	if err := m.Check(); err == nil {
		t.Error("Check passed with missing files")
	}
	if err := (Material{}).Check(); err != nil {
		t.Errorf("Check of the plaintext posture: %v", err)
	}
}

// writePair writes a self-signed certificate as both the client pair and the
// CA, and returns its DER.
func writePair(t *testing.T, m Material, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for path, body := range map[string][]byte{
		m.CertFile: certPEM,
		m.CAFile:   certPEM,
		m.KeyFile:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return der
}
