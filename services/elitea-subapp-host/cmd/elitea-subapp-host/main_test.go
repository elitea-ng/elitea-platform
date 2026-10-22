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

// The container probe: GET /readyz on the admin port. A ready host passes;
// one whose readiness fails (a lost owner claim answers 503), a closed port
// and a misconfigured host do not.
func TestTheHealthcheckAsksTheAdminReadyz(t *testing.T) {
	ready := true
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready: owner_claim\n"))
		}
	}))
	defer admin.Close()
	_, port, _ := net.SplitHostPort(admin.Listener.Addr().String())
	env := lookup(map[string]string{"ELITEA_DEEPWIKI_ADMIN_ADDR": ":" + port})
	if err := healthcheck(env); err != nil {
		t.Fatalf("a ready host failed the probe: %v", err)
	}
	ready = false
	if err := healthcheck(env); err == nil || !strings.Contains(err.Error(), "owner_claim") {
		t.Fatalf("a host that is not ready passed the probe: %v", err)
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	_, deadPort, _ := net.SplitHostPort(closed.Addr().String())
	_ = closed.Close()
	if err := healthcheck(lookup(map[string]string{"ELITEA_DEEPWIKI_ADMIN_ADDR": ":" + deadPort})); err == nil {
		t.Fatal("a closed port passed the probe")
	}
	if err := healthcheck(lookup(map[string]string{"ELITEA_SUBAPP": "nope"})); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("a misconfigured host passed the probe: %v", err)
	}
	// The retired HTTP listener's variable fails the probe as it fails the
	// start, rather than probing a port nothing listens on.
	if err := healthcheck(lookup(map[string]string{"ELITEA_DEEPWIKI_LISTEN_ADDR": ":8080"})); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("LISTEN_ADDR passed the probe: %v", err)
	}
}

func TestTheAppKeyIsTheSettingsPrefix(t *testing.T) {
	for prefix, want := range map[string]string{"ELITEA_DEEPWIKI_": "deepwiki", "ELITEA_INVENTORY_": "inventory", "ELITEA_ECHO_": "echo"} {
		if got := appKey(prefix); got != want {
			t.Errorf("%s: %q", prefix, got)
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

func (p pki) clientTLS(t *testing.T, withCertificate bool) *tls.Config {
	t.Helper()
	caPEM, _ := os.ReadFile(p.ca)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	cfg := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	if withCertificate {
		pair, err := tls.LoadX509KeyPair(p.clientCert, p.clientKey)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg
}

// serve composes a host from pairs and serves its one listener the way run
// does (listenerTLS, then listen), returning a connection factory.
func serve(t *testing.T, pairs map[string]string) (spi.Settings, func(*tls.Config) *grpc.ClientConn) {
	t.Helper()
	app, settings, err := compose(lookup(pairs))
	if err != nil {
		t.Fatal(err)
	}
	server, err := spi.NewServer(settings, app, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	tlsConfig, err := listenerTLS(settings)
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("client auth %v", tlsConfig.ClientAuth)
	}
	grpcServer, listener, err := listen(server, settings, tlsConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	addr := listener.Addr().String()
	return settings, func(cfg *tls.Config) *grpc.ClientConn {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
}

func callContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The one listener is the mutual-TLS terminus: a client with no certificate
// cannot complete the handshake, one with the CA's certificate and an allowed
// identity reaches the provider service, and the host will not be built
// without the material or the allowlist.
func TestTheListenerRequiresAndVerifiesTheClientCertificate(t *testing.T) {
	p := mintPKI(t)
	pairs := map[string]string{
		"ELITEA_SUBAPP": "echo", "ELITEA_ECHO_RUNNER": "echo",
		"ELITEA_ECHO_TLS_CERTFILE": p.serverCert, "ELITEA_ECHO_TLS_KEYFILE": p.serverKey, "ELITEA_ECHO_TLS_CA_FILE": p.ca,
		"ELITEA_ECHO_PLATFORM_CLIENTS": "client", "ELITEA_ECHO_PLATFORM_GRPC_ADDR": "127.0.0.1:0",
	}
	settings, dial := serve(t, pairs)
	slots := func(conn *grpc.ClientConn) error {
		_, err := subappv1.NewProviderServiceClient(conn).Slots(callContext(t), &subappv1.SlotsRequest{})
		return err
	}
	if err := slots(dial(p.clientTLS(t, false))); err == nil {
		t.Fatal("a client with no certificate was served")
	}
	if err := slots(dial(p.clientTLS(t, true))); err != nil {
		t.Fatalf("the allowed client was refused: %v", err)
	}
	// The platform operations sit on the same listener; echo has none.
	_, err := subappv1.NewPlatformOperationsClient(dial(p.clientTLS(t, true))).DeleteProject(callContext(t), &subappv1.DeleteProjectRequest{ProjectId: 17})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("echo's platform operations: %v", err)
	}

	// A CA file with no certificate in it is a configuration error, not a
	// listener that silently verifies nothing; no material at all is one too.
	empty := filepath.Join(t.TempDir(), "empty.crt")
	_ = os.WriteFile(empty, []byte("not a certificate"), 0o600)
	broken := settings
	broken.TLSCAFile = empty
	if _, err := listenerTLS(broken); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("an empty CA was accepted: %v", err)
	}
	plain := settings
	plain.TLSCertFile, plain.TLSKeyFile, plain.TLSCAFile = "", "", ""
	if _, err := listenerTLS(plain); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("a host without TLS material was accepted: %v", err)
	}
}

// DeepWiki's listener serves its platform operations to the allowed client
// and refuses a client the allowlist does not name.
func TestThePlatformOperationsShareTheListenerAndItsAllowlist(t *testing.T) {
	p := mintPKI(t)
	base := map[string]string{
		"ELITEA_SUBAPP": "deepwiki", "ELITEA_DEEPWIKI_RUNNER": "native",
		"ELITEA_DEEPWIKI_ENGINE_SOCKET": filepath.Join(t.TempDir(), "none.sock"),
		"ELITEA_DEEPWIKI_GIT_ALLOWLIST": "github.com",
		"ELITEA_DEEPWIKI_TLS_CERTFILE":  p.serverCert, "ELITEA_DEEPWIKI_TLS_KEYFILE": p.serverKey, "ELITEA_DEEPWIKI_TLS_CA_FILE": p.ca,
		"ELITEA_DEEPWIKI_PLATFORM_GRPC_ADDR": "127.0.0.1:0",
	}
	with := func(extra map[string]string) map[string]string {
		pairs := map[string]string{}
		for k, v := range base {
			pairs[k] = v
		}
		for k, v := range extra {
			pairs[k] = v
		}
		return pairs
	}
	_, dial := serve(t, with(map[string]string{"ELITEA_DEEPWIKI_PLATFORM_CLIENTS": "client"}))
	// The allowed client reaches the operation; the engine is not running, so
	// the operation FAILS (not Unauthenticated, not PermissionDenied).
	_, err := subappv1.NewPlatformOperationsClient(dial(p.clientTLS(t, true))).DeleteProject(callContext(t), &subappv1.DeleteProjectRequest{ProjectId: 17})
	if status.Code(err) != codes.Internal {
		t.Fatalf("the allowed client got %v", err)
	}
	_, dial = serve(t, with(map[string]string{"ELITEA_DEEPWIKI_PLATFORM_CLIENTS": "someone-else"}))
	conn := dial(p.clientTLS(t, true))
	if _, err := subappv1.NewPlatformOperationsClient(conn).DeleteProject(callContext(t), &subappv1.DeleteProjectRequest{ProjectId: 17}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a client off the allowlist reached the platform operations: %v", err)
	}
	if _, err := subappv1.NewProviderServiceClient(conn).Slots(callContext(t), &subappv1.SlotsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a client off the allowlist reached the provider service: %v", err)
	}
	// No allowlist: the listener is not built.
	app, settings, err := compose(lookup(base))
	if err != nil {
		t.Fatal(err)
	}
	server, _ := spi.NewServer(settings, app, nil)
	tlsConfig, _ := listenerTLS(settings)
	if _, _, err := listen(server, settings, tlsConfig, nil); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("a listener with no allowlist: %v", err)
	}
}

// Inventory serves the same listener on its own prefix, and its runner adds
// DeleteToolkit (DeepWiki answers it Unimplemented).
func TestInventoryServesTheGraphDeletionsOnItsOwnPrefix(t *testing.T) {
	p := mintPKI(t)
	settings, dial := serve(t, map[string]string{
		"ELITEA_SUBAPP": "inventory", "ELITEA_INVENTORY_RUNNER": "sidecar",
		"ELITEA_INVENTORY_ENGINE_SOCKET": filepath.Join(t.TempDir(), "none.sock"),
		"ELITEA_INVENTORY_TLS_CERTFILE":  p.serverCert, "ELITEA_INVENTORY_TLS_KEYFILE": p.serverKey, "ELITEA_INVENTORY_TLS_CA_FILE": p.ca,
		"ELITEA_INVENTORY_PLATFORM_CLIENTS": "client", "ELITEA_INVENTORY_PLATFORM_GRPC_ADDR": "127.0.0.1:0",
	})
	if got := settings.PlatformClients; len(got) != 1 || got[0] != "client" {
		t.Fatalf("settings: %v", got)
	}
	client := subappv1.NewPlatformOperationsClient(dial(p.clientTLS(t, true)))
	// The allowed client reaches both operations; the engine is not running,
	// so they FAIL (not Unauthenticated, not PermissionDenied, and for
	// DeleteToolkit not Unimplemented).
	if _, err := client.DeleteToolkit(callContext(t), &subappv1.DeleteToolkitRequest{ProjectId: 7, ToolkitId: 70}); status.Code(err) != codes.Internal {
		t.Fatalf("DeleteToolkit: %v", err)
	}
	if _, err := client.DeleteProject(callContext(t), &subappv1.DeleteProjectRequest{ProjectId: 7}); status.Code(err) != codes.Internal {
		t.Fatalf("DeleteProject: %v", err)
	}
}
