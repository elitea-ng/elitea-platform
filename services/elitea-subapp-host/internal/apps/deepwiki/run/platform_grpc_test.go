package run_test

// The platform service: project deprovisioning over gRPC with mutual TLS
// (elitea.subapp.v1.PlatformOperations), authorised by the verified client
// certificate and nothing else.
//
// These tests are REAL: a TLS listener with RequireAndVerifyClientCert, a
// gRPC client with certificates from a test CA, and the deepwiki runner over a
// fake engine socket. The accept/refuse matrix is the one the HTTP route had.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	subappv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type testPKI struct {
	ca       *x509.Certificate
	caKey    *ecdsa.PrivateKey
	caPool   *x509.CertPool
	serial   int64
	serverTC tls.Certificate
}

func newPKI(t *testing.T, name string) *testPKI {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	p := &testPKI{ca: ca, caKey: key, caPool: pool, serial: 1}
	p.serverTC = p.leaf(t, "host", nil, x509.ExtKeyUsageServerAuth)
	return p
}

// leaf issues a certificate with the given common name and DNS SANs.
func (p *testPKI) leaf(t *testing.T, cn string, dns []string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	p.serial++
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		DNSNames: dns, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (p *testPKI) client(t *testing.T, cn string) tls.Certificate {
	return p.leaf(t, cn, nil, x509.ExtKeyUsageClientAuth)
}

func engineCalls(sidecar *fakeSidecar) int {
	sidecar.mu.Lock()
	defer sidecar.mu.Unlock()
	return len(sidecar.requests)
}

// hostSettings are the deepwiki host's settings over a fake engine.
func hostSettings(t *testing.T, sidecar *fakeSidecar, clients string) spi.Settings {
	t.Helper()
	settings, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", func(key string) (string, bool) {
		switch key {
		case "ELITEA_DEEPWIKI_ENGINE_SOCKET":
			return sidecar.socket, true
		case "ELITEA_DEEPWIKI_IDENTITY_SECRET":
			return identitySecret, true
		case "ELITEA_DEEPWIKI_GIT_ALLOWLIST":
			return "github.com", true
		case "ELITEA_DEEPWIKI_PLATFORM_CLIENTS":
			return clients, clients != ""
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

// platformHost serves the deepwiki platform gRPC service on a loopback port
// behind mutual TLS, exactly as the host's main does, and returns its address
// and the runner. Nothing is listening on it without clients.
func platformHost(t *testing.T, sidecar *fakeSidecar, pki *testPKI, clients string) (string, *run.Runner) {
	t.Helper()
	settings := hostSettings(t, sidecar, clients)
	runner := run.NewEngineRunner(settings)
	server, err := spi.NewServer(settings, deepwiki.App(runner), nil)
	if err != nil {
		t.Fatal(err)
	}
	server.Start(t.Context())
	t.Cleanup(server.Stop)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{pki.serverTC}, ClientCAs: pki.caPool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	ops, _ := server.PlatformOps()
	grpcServer, err := spi.NewPlatformGRPCServer(ops, settings.PlatformClients, tlsConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	return listener.Addr().String(), runner
}

func dial(t *testing.T, addr string, pki *testPKI, cert *tls.Certificate) subappv1.PlatformOperationsClient {
	t.Helper()
	cfg := &tls.Config{RootCAs: pki.caPool, MinVersion: tls.VersionTLS12}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return subappv1.NewPlatformOperationsClient(conn)
}

func deleteProject(client subappv1.PlatformOperationsClient, project int32) (*subappv1.DeleteProjectResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return client.DeleteProject(ctx, &subappv1.DeleteProjectRequest{ProjectId: project})
}

const enginePerWiki = `{"result": {"success": true, "project_id": 17, "wikis": ["acme--a--main", "acme--b--main"],
 "per_wiki": [{"wiki_id": "acme--a--main", "nodes": 5, "edges": 4, "embeddings": 5, "statistics": 9},
              {"wiki_id": "acme--b--main", "nodes": 2, "edges": 1, "embeddings": 2, "statistics": 3}],
 "rows": {"nodes": 7, "edges": 5, "embeddings": 7, "statistics": 12}, "builds": 1, "live_builds": 2}}`

func TestProjectDeprovisioningIsAGRPCServiceAuthorisedByTheMainCertificate(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{strings.ReplaceAll(enginePerWiki, "\n", " ")}, 0)
	pki := newPKI(t, "platform ca")
	addr, _ := platformHost(t, sidecar, pki, "elitea-main")
	main := pki.client(t, "elitea-main")

	got, err := deleteProject(dial(t, addr, pki, &main), 17)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetProjectId() != 17 || got.GetLiveBuilds() != 2 || got.GetStaleBuildsRemoved() != 1 || len(got.GetErrors()) != 0 {
		t.Fatalf("%v", got)
	}
	if len(got.GetWikis()) != 2 {
		t.Fatalf("%v", got.GetWikis())
	}
	first := got.GetWikis()[0]
	if first.GetWikiId() != "acme--a--main" || first.GetNodes() != 5 || first.GetEdges() != 4 ||
		first.GetEmbeddings() != 5 || first.GetStatistics() != 9 {
		t.Fatalf("per-wiki counts: %v", first)
	}
	sidecar.mu.Lock()
	call := sidecar.requests[0]
	sidecar.mu.Unlock()
	arguments, _ := call["arguments"].(map[string]any)
	if call["tool"] != "delete_project_wikis" || arguments[run.ProjectArgument] != "17" {
		t.Fatalf("the engine was called as %v", call)
	}
}

// The allowlist matches a DNS SAN as well as a common name.
func TestTheAllowlistMatchesADNSSANToo(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	pki := newPKI(t, "platform ca")
	addr, _ := platformHost(t, sidecar, pki, "elitea-main.elitea.svc")
	bySAN := pki.leaf(t, "something-else", []string{"elitea-main.elitea.svc"}, x509.ExtKeyUsageClientAuth)
	if _, err := deleteProject(dial(t, addr, pki, &bySAN), 17); err != nil {
		t.Fatal(err)
	}
	byCN := pki.client(t, "elitea-main")
	if _, err := deleteProject(dial(t, addr, pki, &byCN), 17); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a common name not on the list got %v", err)
	}
}

func TestOnlyTheMainCertificateMayDeleteAProject(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	pki := newPKI(t, "platform ca")
	addr, _ := platformHost(t, sidecar, pki, "elitea-main")

	// A certificate from the right CA but another service: PermissionDenied.
	facade := pki.client(t, "elitea-facade")
	if _, err := deleteProject(dial(t, addr, pki, &facade), 17); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("another platform service got %v", err)
	}
	// ... even when the call carries the old inferences: a signed identity
	// with a project and NO user, or a user session, in the metadata.
	signed := map[string]string{}
	header := httptest.NewRequest("POST", "/", nil).Header
	spi.SignHeaders(header, spi.Identity{ProjectID: "17"}, []byte(identitySecret))
	for k := range header {
		signed[strings.ToLower(k)] = header.Get(k)
	}
	pairs := make([]string, 0, 2*len(signed))
	for k, v := range signed {
		pairs = append(pairs, k, v)
	}
	ctx, cancel := context.WithTimeout(metadataContext(pairs...), 10*time.Second)
	defer cancel()
	_, err := dial(t, addr, pki, &facade).DeleteProject(ctx, &subappv1.DeleteProjectRequest{ProjectId: 17})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a signed call with no user id got %v", err)
	}
	// No client certificate: the handshake fails, the call never arrives.
	if _, err := deleteProject(dial(t, addr, pki, nil), 17); err == nil {
		t.Fatal("a call with no client certificate was accepted")
	}
	// The right name from a CA the host does not trust: the handshake fails.
	foreign := newPKI(t, "foreign ca")
	impostor := foreign.client(t, "elitea-main")
	if _, err := deleteProject(dial(t, addr, pki, &impostor), 17); err == nil {
		t.Fatal("a certificate from another CA was accepted")
	}
	// A cleartext client cannot speak to the TLS listener at all.
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := deleteProject(subappv1.NewPlatformOperationsClient(conn), 17); err == nil {
		t.Fatal("a cleartext call was accepted")
	}
	if n := engineCalls(sidecar); n != 0 {
		t.Fatalf("the engine was called %d time(s) for refused callers", n)
	}
}

// With no allowlist the service is not served at all: it cannot be built.
func TestThePlatformServiceIsOffWithoutAnAllowlistAndWithoutClientCertificates(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	pki := newPKI(t, "platform ca")
	settings := hostSettings(t, sidecar, "")
	if len(settings.PlatformClients) != 0 {
		t.Fatalf("%v", settings.PlatformClients)
	}
	runner := run.NewEngineRunner(settings)
	mtls := &tls.Config{Certificates: []tls.Certificate{pki.serverTC}, ClientCAs: pki.caPool, ClientAuth: tls.RequireAndVerifyClientCert}
	if _, err := spi.NewPlatformGRPCServer(runner, nil, mtls, nil); err == nil {
		t.Fatal("a platform service was built with no allowed clients")
	}
	// Clients, but a listener that does not verify client certificates.
	for name, cfg := range map[string]*tls.Config{
		"nil":          nil,
		"no client CA": {Certificates: []tls.Certificate{pki.serverTC}},
		"not required": {Certificates: []tls.Certificate{pki.serverTC}, ClientCAs: pki.caPool, ClientAuth: tls.VerifyClientCertIfGiven},
	} {
		if _, err := spi.NewPlatformGRPCServer(runner, []string{"elitea-main"}, cfg, nil); err == nil {
			t.Errorf("%s: a platform service was built without mutual TLS", name)
		}
	}
	if n := engineCalls(sidecar); n != 0 {
		t.Fatalf("the engine was called %d time(s)", n)
	}
}

func TestTheProjectInTheRequestIsValidated(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	pki := newPKI(t, "platform ca")
	addr, _ := platformHost(t, sidecar, pki, "elitea-main")
	main := pki.client(t, "elitea-main")
	client := dial(t, addr, pki, &main)
	for _, project := range []int32{0, -4} {
		if _, err := deleteProject(client, project); status.Code(err) != codes.InvalidArgument {
			t.Errorf("project %d: %v", project, err)
		}
	}
	if n := engineCalls(sidecar); n != 0 {
		t.Fatalf("the engine was called %d time(s)", n)
	}
}

// The engine's "being published; retry" reaches the caller as ABORTED, an
// engine failure as INTERNAL.
func TestTheEnginesBusyAnswerIsRetryable(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"error": {"message": "Deleting the project's wiki indexes failed: wiki acme--a--main is being published (its lock was not released within 30s); retry the deletion", "error_type": "RuntimeError"}}`}, 0)
	pki := newPKI(t, "platform ca")
	addr, _ := platformHost(t, sidecar, pki, "elitea-main")
	main := pki.client(t, "elitea-main")
	_, err := deleteProject(dial(t, addr, pki, &main), 17)
	if status.Code(err) != codes.Aborted || !strings.Contains(err.Error(), "being published") {
		t.Fatalf("%v", err)
	}
	sidecar.mu.Lock()
	sidecar.byTool = map[string][]string{"delete_project_wikis": {`{"error": {"message": "the index database refused it", "error_type": "RuntimeError"}}`}}
	sidecar.mu.Unlock()
	if _, err := deleteProject(dial(t, addr, pki, &main), 17); status.Code(err) != codes.Internal {
		t.Fatalf("%v", err)
	}
}

// ---------------------------------------------------------------------------
// A deletion stops the generations that would publish into it
// ---------------------------------------------------------------------------

// A generation is a long stream the fake honours a stop on.
func slowGeneration(lines int) []string {
	out := make([]string, 0, lines)
	for i := 0; i < lines; i++ {
		out = append(out, `{"thinking": "working"}`)
	}
	return append(out, `{"result": {"success": true, "result": "done", "wiki_id": "acme--e2e-service--main"}}`)
}

// startGeneration runs generate_wiki for project 90200 (the test transport's
// organization) of acme/e2e-service on main through the manager, and waits
// until the engine has the request.
func startGeneration(t *testing.T, runner *run.Runner, manager *spi.Manager, sidecar *fakeSidecar, request map[string]any) *spi.Invocation {
	t.Helper()
	before := engineCalls(sidecar)
	invocation, err := manager.Submit(t.Context(), "Wikis", "generate_wiki", func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
		return runner.Invoke(ctx, spi.Invoke{Family: spi.Family{Name: "main"}, Toolkit: "Wikis", Tool: "generate_wiki", Request: request}, tc)
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for engineCalls(sidecar) == before {
		if time.Now().After(deadline) {
			t.Fatal("the generation never reached the engine")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return invocation
}

func generationRig(t *testing.T) (*run.Runner, *spi.Manager, *fakeSidecar) {
	t.Helper()
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	sidecar.byTool = map[string][]string{"generate_wiki": slowGeneration(150)}
	settings := hostSettings(t, sidecar, "elitea-main")
	runner := run.NewEngineRunner(settings)
	runner.VerifiedIdentity = false // the project is the test transport's organization
	runner.Artifacts = func(map[string]any) (run.ArtifactClient, error) { return &fakeArtifactClient{}, nil }
	manager := spi.NewManager(nil, time.Hour, nil)
	manager.Start(t.Context())
	t.Cleanup(manager.Stop)
	runner.AttachManager(manager)
	return runner, manager, sidecar
}

// slowEngine pauses between lines, so a generation is still running when a
// deletion arrives.
func slowEngine(sidecar *fakeSidecar) { sidecar.pause = 20 * time.Millisecond }

func TestDeletingAProjectStopsItsRunningGenerationsFirst(t *testing.T) {
	runner, manager, sidecar := generationRig(t)
	slowEngine(sidecar)
	other := fixtureRequest("GO", map[string]any{"api_base": "http://elitea-main:8080/llm/v1", "api_key": "minted", "organization": "555"})
	mine := startGeneration(t, runner, manager, sidecar, fixtureRequest("GO", transport))
	theirs := startGeneration(t, runner, manager, sidecar, other)

	// The delete answers with the engine's counts after the project's
	// generation has been stopped.
	sidecar.mu.Lock()
	sidecar.byTool["delete_project_wikis"] = []string{`{"result": {"success": true, "wikis": ["acme--e2e-service--main"], "builds": 0, "live_builds": 0}}`}
	sidecar.mu.Unlock()
	got, err := runner.DeleteProject(t.Context(), 90200)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Wikis) != 1 {
		t.Fatalf("%v", got)
	}
	if manager.InFlight() != 1 {
		t.Fatalf("%d invocations still run; only the other project's should", manager.InFlight())
	}
	body, _ := manager.Poll(t.Context(), "Wikis", "generate_wiki", mine.ID)
	if body["status"] != "Error" || !strings.Contains(str(body["result"]), "cancelled") {
		t.Fatalf("the project's generation was not cancelled: %v", body)
	}
	// The other project's generation is untouched.
	if body, _ := manager.Poll(t.Context(), "Wikis", "generate_wiki", theirs.ID); body["status"] == "Error" || body["status"] == "Completed" {
		t.Fatalf("another project's generation was stopped: %v", body)
	}
	// And the engine was asked to delete only after its generation ended.
	sidecar.mu.Lock()
	defer sidecar.mu.Unlock()
	last := sidecar.requests[len(sidecar.requests)-1]
	if last["tool"] != "delete_project_wikis" {
		t.Fatalf("the last engine call was %v", last["tool"])
	}
	if len(sidecar.stops) == 0 {
		t.Fatal("the engine was never told to stop the generation")
	}
}

func TestADeletionIsRefusedWhileAGenerationWillNotStop(t *testing.T) {
	runner, manager, sidecar := generationRig(t)
	runner.StopWait = 150 * time.Millisecond
	// The engine ignores the stop: the stream just goes on.
	sidecar.mu.Lock()
	sidecar.byTool["generate_wiki"] = nil
	sidecar.mu.Unlock()
	release := make(chan struct{})
	var stuck atomic.Bool
	stuck.Store(true)
	tool := runner.Tools["generate_wiki"]
	runner.Tools["generate_wiki"] = func(ctx context.Context, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
		<-release // a tool that never looks at its checkpoint
		return tool(ctx, arguments, tc)
	}
	t.Cleanup(func() {
		if stuck.CompareAndSwap(true, false) {
			close(release)
		}
	})
	started := make(chan struct{})
	invocation, err := manager.Submit(t.Context(), "Wikis", "generate_wiki", func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
		close(started)
		return runner.Invoke(ctx, spi.Invoke{Family: spi.Family{Name: "main"}, Toolkit: "Wikis", Tool: "generate_wiki", Request: fixtureRequest("GO", transport)}, tc)
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	time.Sleep(50 * time.Millisecond) // the labels are set at the start of Invoke

	before := engineCalls(sidecar)
	_, err = runner.DeleteProject(t.Context(), 90200)
	if err == nil || !strings.Contains(err.Error(), "a generation is still running; retry") {
		t.Fatalf("%v", err)
	}
	if !errorsIs(err, spi.ErrGenerationRunning) {
		t.Fatalf("the refusal is not the retryable sentinel: %v", err)
	}
	if engineCalls(sidecar) != before {
		t.Fatal("the engine was asked to delete while a generation ran")
	}

	// Once it has ended, the same deletion goes through.
	if stuck.CompareAndSwap(true, false) {
		close(release)
	}
	deadline := time.Now().Add(5 * time.Second)
	for manager.InFlight() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	sidecar.mu.Lock()
	sidecar.byTool["delete_project_wikis"] = []string{`{"result": {"success": true, "wikis": []}}`}
	sidecar.mu.Unlock()
	if _, err := runner.DeleteProject(t.Context(), 90200); err != nil {
		t.Fatalf("after the generation ended: %v", err)
	}
	_ = invocation
}

func TestTheGRPCDeletionIsRefusedWithAPreconditionWhileAGenerationRuns(t *testing.T) {
	sidecar := newFakeSidecar(t, []string{`{"result": {"success": true, "wikis": []}}`}, 0)
	pki := newPKI(t, "platform ca")
	addr, runner := platformHost(t, sidecar, pki, "elitea-main")
	runner.StopWait = 100 * time.Millisecond
	runner.VerifiedIdentity = false // the project is the test transport's organization
	release := make(chan struct{})
	runner.Tools["generate_wiki"] = func(context.Context, map[string]any, *spi.Context) (map[string]any, error) {
		<-release
		return nil, nil
	}
	manager := spi.NewManager(nil, time.Hour, nil)
	manager.Start(t.Context())
	// Cleanups run last-in first-out: the tool is released BEFORE the manager
	// waits for it.
	t.Cleanup(manager.Stop)
	t.Cleanup(func() { close(release) })
	runner.AttachManager(manager)
	_, err := manager.Submit(t.Context(), "Wikis", "generate_wiki", func(ctx context.Context, tc *spi.Context) (map[string]any, error) {
		return runner.Invoke(ctx, spi.Invoke{Family: spi.Family{Name: "main"}, Toolkit: "Wikis", Tool: "generate_wiki", Request: fixtureRequest("GO", transport)}, tc)
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	main := pki.client(t, "elitea-main")
	_, err = deleteProject(dial(t, addr, pki, &main), 90200)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "a generation is still running; retry") {
		t.Fatalf("%v", err)
	}
	if n := engineCalls(sidecar); n != 0 {
		t.Fatalf("the engine was called %d time(s)", n)
	}
}

func errorsIs(err, target error) bool { return errors.Is(err, target) }

func metadataContext(pairs ...string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), pairs...)
}
