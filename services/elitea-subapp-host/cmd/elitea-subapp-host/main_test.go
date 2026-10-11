package main

import (
	"context"
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
	"strings"
	"testing"
	"time"

	subappv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
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
	// The Python engine is retired: the host refuses to start on it even
	// with a socket, and the error points at the upgrade note.
	_, _, err = compose(lookup(map[string]string{"ELITEA_DEEPWIKI_RUNNER": "legacy", "ELITEA_DEEPWIKI_ENGINE_SOCKET": "/run/deepwiki/engine.sock"}))
	if !errors.Is(err, spi.ErrConfig) || !strings.Contains(err.Error(), "docs/UPGRADING.md") {
		t.Fatalf("legacy was not refused with the upgrade pointer: %v", err)
	}
	app, _, err = compose(lookup(map[string]string{"ELITEA_DEEPWIKI_RUNNER": "native", "ELITEA_DEEPWIKI_ENGINE_SOCKET": "/run/deepwiki/engine.sock"}))
	if err != nil || app.Name != "elitea-deepwiki" || app.Runner.Name() != "native" {
		t.Fatalf("native: %v %+v", err, app)
	}
	app, _, err = compose(lookup(map[string]string{"ELITEA_SUBAPP": "inventory"}))
	if err != nil || app.Name != "elitea-inventory" || app.Runner.Name() != "unavailable" {
		t.Fatalf("inventory: %v %+v", err, app)
	}
	// Inventory's host runner that dials the engine sidecar is `sidecar`
	// (`legacy`, its pre-ADR-0027 name, is an alias).
	for _, name := range []string{"sidecar", "legacy"} {
		app, _, err = compose(lookup(map[string]string{
			"ELITEA_SUBAPP": "inventory", "ELITEA_INVENTORY_RUNNER": name,
			"ELITEA_INVENTORY_ENGINE_SOCKET": "/run/inventory/engine.sock",
		}))
		if err != nil || app.Runner.Name() != "sidecar" {
			t.Fatalf("inventory %s: %v %+v", name, err, app)
		}
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
		"the retired Python runner":    {"ELITEA_DEEPWIKI_RUNNER": "legacy"},
		"the native runner, no socket": {"ELITEA_DEEPWIKI_RUNNER": "native"},
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

// The platform gRPC service is its own listener behind the SPI listener's TLS
// configuration, and it is OFF unless the allowlist names a client.
func TestThePlatformServiceIsOffWithoutClientsAndOnBehindMutualTLS(t *testing.T) {
	p := mintPKI(t)
	base := map[string]string{
		"ELITEA_SUBAPP": "deepwiki", "ELITEA_DEEPWIKI_RUNNER": "native",
		"ELITEA_DEEPWIKI_ENGINE_SOCKET": filepath.Join(t.TempDir(), "none.sock"),
		"ELITEA_DEEPWIKI_GIT_ALLOWLIST": "github.com",
		"ELITEA_DEEPWIKI_TLS_CERTFILE":  p.serverCert, "ELITEA_DEEPWIKI_TLS_KEYFILE": p.serverKey, "ELITEA_DEEPWIKI_TLS_CA_FILE": p.ca,
	}
	build := func(extra map[string]string) (*spi.Server, spi.Settings) {
		t.Helper()
		pairs := map[string]string{}
		for k, v := range base {
			pairs[k] = v
		}
		for k, v := range extra {
			pairs[k] = v
		}
		app, settings, err := compose(lookup(pairs))
		if err != nil {
			t.Fatal(err)
		}
		server, err := spi.NewServer(settings, app, nil)
		if err != nil {
			t.Fatal(err)
		}
		return server, settings
	}
	errs := make(chan error, 2)

	// No clients: nothing is opened, even with an address configured.
	server, settings := build(map[string]string{"ELITEA_DEEPWIKI_PLATFORM_GRPC_ADDR": "127.0.0.1:0"})
	tlsConfig, err := listenerTLS(settings)
	if err != nil {
		t.Fatal(err)
	}
	if platform, err := startPlatformGRPC(server, settings, tlsConfig, nil, errs); err != nil || platform != nil {
		t.Fatalf("with no clients: %v %v", platform, err)
	}

	// Clients, but no mutual TLS to authorise them by: a boot failure.
	server, settings = build(map[string]string{"ELITEA_DEEPWIKI_PLATFORM_CLIENTS": "client", "ELITEA_DEEPWIKI_PLATFORM_GRPC_ADDR": "127.0.0.1:0"})
	if _, err := startPlatformGRPC(server, settings, nil, nil, errs); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("clients without a TLS configuration: %v", err)
	}

	// An application with no platform operations serves none.
	echoApp, echoSettings, err := compose(lookup(map[string]string{"ELITEA_SUBAPP": "echo", "ELITEA_ECHO_RUNNER": "echo", "ELITEA_ECHO_PLATFORM_CLIENTS": "client"}))
	if err != nil {
		t.Fatal(err)
	}
	echoServer, _ := spi.NewServer(echoSettings, echoApp, nil)
	if platform, err := startPlatformGRPC(echoServer, echoSettings, tlsConfig, nil, errs); err != nil || platform != nil {
		t.Fatalf("an application without platform operations: %v %v", platform, err)
	}

	// On: the listener is real, behind the same configuration.
	addr := freeAddr(t)
	server, settings = build(map[string]string{"ELITEA_DEEPWIKI_PLATFORM_CLIENTS": "client", "ELITEA_DEEPWIKI_PLATFORM_GRPC_ADDR": addr})
	tlsConfig, err = listenerTLS(settings)
	if err != nil {
		t.Fatal(err)
	}
	platform, err := startPlatformGRPC(server, settings, tlsConfig, nil, errs)
	if err != nil || platform == nil {
		t.Fatalf("%v %v", platform, err)
	}
	defer platform.Stop()

	caPEM, _ := os.ReadFile(p.ca)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	clientPair, _ := tls.LoadX509KeyPair(p.clientCert, p.clientKey)
	call := func(cfg *tls.Config) error {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err = subappv1.NewPlatformOperationsClient(conn).DeleteProject(ctx, &subappv1.DeleteProjectRequest{ProjectId: 17})
		return err
	}
	// No certificate: no handshake.
	if err := call(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}); err == nil {
		t.Fatal("a client with no certificate was served")
	}
	// The allowed client reaches the operation; the engine is not running, so
	// the operation FAILS (not Unauthenticated, not PermissionDenied).
	err = call(&tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientPair}, MinVersion: tls.VersionTLS12})
	if code := status.Code(err); code != codes.Internal {
		t.Fatalf("the allowed client got %v", err)
	}
	// An allowlist that names another identity refuses the same client.
	other, otherSettings := build(map[string]string{"ELITEA_DEEPWIKI_PLATFORM_CLIENTS": "someone-else", "ELITEA_DEEPWIKI_PLATFORM_GRPC_ADDR": freeAddr(t)})
	otherTLS, _ := listenerTLS(otherSettings)
	otherPlatform, err := startPlatformGRPC(other, otherSettings, otherTLS, nil, errs)
	if err != nil {
		t.Fatal(err)
	}
	defer otherPlatform.Stop()
	addr = otherSettings.PlatformGRPCAddr
	err = call(&tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientPair}, MinVersion: tls.VersionTLS12})
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Fatalf("a client not on the allowlist got %v", err)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String()
}
