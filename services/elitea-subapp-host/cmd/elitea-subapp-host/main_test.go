package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

func lookup(pairs map[string]string) spi.Lookup {
	return func(key string) (string, bool) { v, ok := pairs[key]; return v, ok }
}

func TestComposeSelectsTheApplicationAndRefusesWhatItCannotServe(t *testing.T) {
	app, _, err := compose(lookup(nil))
	if err != nil || app.Name != "elitea-deepwiki" || app.Runner.Name() != "unavailable" {
		t.Fatalf("default: %v %+v", err, app)
	}
	app, _, err = compose(lookup(map[string]string{"ELITEA_SUBAPP": "echo", "ELITEA_ECHO_RUNNER": "echo", "ELITEA_ECHO_FIXTURE_STEP_SECONDS": "0"}))
	if err != nil || app.Name != "elitea-echo" || app.Runner.Name() != "echo" {
		t.Fatalf("echo: %v %+v", err, app)
	}
	app, _, err = compose(lookup(map[string]string{"ELITEA_DEEPWIKI_RUNNER": "fixture", "ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS": "0"}))
	if err != nil || app.Name != "elitea-deepwiki" || app.Runner.Name() != "fixture" {
		t.Fatalf("fixture: %v %+v", err, app)
	}
	app, _, err = compose(lookup(map[string]string{"ELITEA_DEEPWIKI_RUNNER": "legacy", "ELITEA_DEEPWIKI_ENGINE_SOCKET": "/run/deepwiki/engine.sock"}))
	if err != nil || app.Name != "elitea-deepwiki" || app.Runner.Name() != "legacy" {
		t.Fatalf("legacy: %v %+v", err, app)
	}
	app, _, err = compose(lookup(map[string]string{"ELITEA_SUBAPP": "inventory"}))
	if err != nil || app.Name != "elitea-inventory" || app.Runner.Name() != "unavailable" {
		t.Fatalf("inventory: %v %+v", err, app)
	}
	// Inventory has a fixture runner of its own now. It is composed under
	// Inventory's OWN settings prefix, which is the half that matters here: a
	// host that read the other application's prefix would pace itself from a
	// variable no operator set for it.
	app, _, err = compose(lookup(map[string]string{
		"ELITEA_SUBAPP": "inventory", "ELITEA_INVENTORY_RUNNER": "fixture",
		"ELITEA_INVENTORY_FIXTURE_STEP_SECONDS": "0",
	}))
	if err != nil || app.Name != "elitea-inventory" || app.Runner.Name() != "fixture" {
		t.Fatalf("inventory fixture: %v %+v", err, app)
	}
	for name, pairs := range map[string]map[string]string{
		"an unknown application":       {"ELITEA_SUBAPP": "nope"},
		"the legacy Python runner":     {"ELITEA_DEEPWIKI_RUNNER": "legacy"},
		"the fixture runner elsewhere": {"ELITEA_SUBAPP": "echo", "ELITEA_ECHO_RUNNER": "fixture"},
		"a non-numeric step":           {"ELITEA_DEEPWIKI_FIXTURE_STEP_SECONDS": "soon"},
		"a bad setting":                {"ELITEA_DEEPWIKI_MAX_PARALLEL_WORKERS": "0"},
	} {
		if _, _, err := compose(lookup(pairs)); !errors.Is(err, spi.ErrConfig) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
}

// The container probe: a TCP connect to the listen port, which is all a
// distroless image behind a client-certificate handshake can do from inside.
func TestTheHealthcheckDialsTheListenPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	if err := healthcheck(lookup(map[string]string{"ELITEA_DEEPWIKI_LISTEN_ADDR": ":" + port})); err != nil {
		t.Fatalf("a listening port failed the probe: %v", err)
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	_, deadPort, _ := net.SplitHostPort(closed.Addr().String())
	_ = closed.Close()
	if err := healthcheck(lookup(map[string]string{"ELITEA_DEEPWIKI_LISTEN_ADDR": ":" + deadPort})); err == nil {
		t.Fatal("a closed port passed the probe")
	}
	if err := healthcheck(lookup(map[string]string{"ELITEA_SUBAPP": "nope"})); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("a misconfigured host passed the probe: %v", err)
	}
}

func TestOnlyTheProbeHandshakeAbortIsDropped(t *testing.T) {
	for line, noise := range map[string]bool{
		"http: TLS handshake error from 127.0.0.1:51066: EOF":                                true,
		"http: TLS handshake error from 10.0.0.7:51066: EOF":                                 false,
		"http: TLS handshake error from 127.0.0.1:51066: remote error: tls: bad certificate": false,
		"http: Accept error: accept tcp: too many open files":                                false,
	} {
		if isProbeNoise(line) != noise {
			t.Errorf("%q: noise=%v", line, !noise)
		}
	}
}

// pki mints a CA, a server certificate for 127.0.0.1 and a client
// certificate, all signed by the CA, and writes them under dir.
type pki struct{ ca, serverCert, serverKey, clientCert, clientKey string }

func mintPKI(t *testing.T) pki {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	write := func(name, kind string, der []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	leaf := func(name string, usage x509.ExtKeyUsage, serial int64) (string, string) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
			IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, _ := x509.MarshalECPrivateKey(key)
		return write(name+".crt", "CERTIFICATE", der), write(name+".key", "EC PRIVATE KEY", keyDER)
	}
	p := pki{ca: write("ca.crt", "CERTIFICATE", caDER)}
	p.serverCert, p.serverKey = leaf("server", x509.ExtKeyUsageServerAuth, 2)
	p.clientCert, p.clientKey = leaf("client", x509.ExtKeyUsageClientAuth, 3)
	return p
}

// The listener is the mutual-TLS terminus: a client with no certificate
// cannot complete the handshake, and one with the CA's certificate reaches
// the SPI — which then trusts its own handshake rather than looking for a
// verified-chain marker the request never carries through a proxy.
func TestTheListenerRequiresAndVerifiesTheClientCertificate(t *testing.T) {
	p := mintPKI(t)
	pairs := map[string]string{
		"ELITEA_SUBAPP": "echo", "ELITEA_ECHO_RUNNER": "echo",
		"ELITEA_ECHO_TLS_CERTFILE": p.serverCert, "ELITEA_ECHO_TLS_KEYFILE": p.serverKey, "ELITEA_ECHO_TLS_CA_FILE": p.ca,
	}
	app, settings, err := compose(lookup(pairs))
	if err != nil {
		t.Fatal(err)
	}
	server, err := spi.NewServer(settings, app, nil)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := listenerTLS(settings)
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("client auth %v", tlsConfig.ClientAuth)
	}
	listener := httptest.NewUnstartedServer(server)
	listener.TLS = tlsConfig
	listener.StartTLS()
	defer listener.Close()

	caPEM, _ := os.ReadFile(p.ca)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	clientPair, _ := tls.LoadX509KeyPair(p.clientCert, p.clientKey)

	// No client certificate: the handshake itself fails.
	anonymous := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	if _, err := anonymous.Get(listener.URL + "/slots"); err == nil {
		t.Fatal("a client with no certificate completed the handshake")
	}
	// The CA's client certificate: through to the SPI, which does not
	// answer 496 for its own handshake.
	authenticated := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientPair}, MinVersion: tls.VersionTLS12}}}
	response, err := authenticated.Get(listener.URL + "/slots")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/slots over mTLS: %d", response.StatusCode)
	}

	// A CA file with no certificate in it is a configuration error, not a
	// listener that silently verifies nothing.
	empty := filepath.Join(t.TempDir(), "empty.crt")
	_ = os.WriteFile(empty, []byte("not a certificate"), 0o600)
	settings.TLSCAFile = empty
	if _, err := listenerTLS(settings); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("an empty CA was accepted: %v", err)
	}
}
