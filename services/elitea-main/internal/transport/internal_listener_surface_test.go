// The internal-listener surface of elitea-main (ADR-0059 decision 7,
// "Internal paths"; story R1.8). Administration data (provider, policy,
// identity and role data) is written through elitea-main's HTTP API only,
// where the gates are. These tests pin that:
//
//   - every gRPC method the composed internal listeners serve, and every
//     route of an internal HTTP mux, is on the reviewed allowlist
//     (internal_listener_allowlist.txt) with what it writes, what decides it
//     and the caller identity it requires; a method that changes
//     administration data must run elitea-main's public route in process,
//     where Main's authentication and permission checks decide;
//   - every place in the module's source that builds a listener or registers
//     a gRPC service is on the allowlist, so a new service or listener fails
//     until it is reviewed;
//   - no internal handler resolves the `administration` permission mode: a
//     handler that builds a synthetic auth.User and resolved that mode would
//     pick up the user's elevation from inside the cluster;
//   - docs/ARCHITECTURE.md lists the internal methods that change
//     administration data and the caller identity each requires, exactly as
//     the allowlist does.
//
// The gateway and the Rust worker sit outside the Go workspace: R1.9 (#2513).
package transport_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/artifactgrpc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/internaltoolgrpc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/modelgrpc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/control"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/egressaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/vectorintrospection"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/workspace"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	vectorv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
)

const (
	modulePath    = "github.com/elitea-ng/elitea-platform/services/elitea-main"
	moduleRoot    = "../.."
	allowlistFile = "internal_listener_allowlist.txt"
	architecture  = "../../../../docs/ARCHITECTURE.md"
	serviceName   = "elitea-main"
)

// composedListeners builds elitea-main's internal gRPC listeners through
// their real constructors, with every optional service present, and returns
// what each serves plus the implementations it registered (whose packages
// the administration-mode scan reads). Registration runs no handler, so
// zero-valued implementations are enough.
func composedListeners(t *testing.T) (map[string]map[string]grpc.ServiceInfo, []any) {
	t.Helper()
	tlsConfig := serverTLS(t)
	clients, err := grpcauth.NewAllowlist([]string{"dns:surface-test"})
	if err != nil {
		t.Fatal(err)
	}
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := http.NotFoundHandler()

	models, err := modelgrpc.NewServer(handler, discard)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactgrpc.NewServer(handler, discard)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := internaltoolgrpc.NewServer(handler, discard)
	if err != nil {
		t.Fatal(err)
	}
	introspection := new(vectorintrospection.Server)
	vector := introspection.PlatformService()
	vector.Clients = clients
	// The platform listener as cmd/elitea-main composes it
	// (platformServices + withVectorIntrospection), every service on.
	platform, err := runtimegrpc.NewPlatformServer(runtimegrpc.PlatformServerConfig{
		Address: "127.0.0.1:0",
		TLS:     tlsConfig,
		Policy:  runtimegrpc.DefaultPlatformGRPCPolicy(),
	}, models.PlatformService(clients), artifacts.PlatformService(clients), tools.PlatformService(clients), vector)
	if err != nil {
		t.Fatal(err)
	}

	controlServer := new(control.Server)
	outputServer := new(output.Server)
	workspaceServer := new(workspace.Server)
	egressServer := new(egressaudit.Server)
	contentServer := new(storage.ContentServer)
	policy := runtimegrpc.DefaultPlatformGRPCPolicy()
	// The private listeners as runtimecomposition composes them, every
	// optional service on.
	private, err := runtimegrpc.NewPrivateServerSet(runtimegrpc.PrivateServerConfig{
		ControlAddress:          "127.0.0.1:1",
		OutputAddress:           "127.0.0.1:2",
		ContentAddress:          "127.0.0.1:3",
		ControlTLS:              tlsConfig,
		OutputTLS:               tlsConfig,
		ContentTLS:              tlsConfig,
		ControlMaxRequestBytes:  1 << 20,
		ControlMaxResponseBytes: 1 << 20,
		OutputMaxRequestBytes:   1 << 20,
		OutputMaxResponseBytes:  1 << 20,
		ControlGRPC:             policy,
		OutputGRPC:              policy,
		ContentGRPC:             policy,
		ShutdownTimeout:         time.Second,
	}, runtimegrpc.PrivateServices{
		Control: controlServer,
		Output:  outputServer,
		Content: []runtimegrpc.PlatformService{{
			Name:     runtimev1.RuntimeContentService_ServiceDesc.ServiceName,
			Clients:  grpcauth.AnyVerifiedPeer(),
			Register: contentServer.Register,
		}},
		VectorIntrospection: introspection,
		Workspace:           workspaceServer,
		EgressAudit:         egressServer,
	})
	if err != nil {
		t.Fatal(err)
	}

	served := map[string]map[string]grpc.ServiceInfo{"platform": platform.ServiceInfo()}
	for listener, services := range private.ServiceInfo() {
		served[listener] = services
	}
	if _, ok := served["platform"][vectorv1.TokenIntrospectionService_ServiceDesc.ServiceName]; !ok {
		t.Fatal("the platform listener did not serve the vector token introspection")
	}
	return served, []any{models, artifacts, tools, introspection, controlServer, outputServer, workspaceServer, egressServer, contentServer}
}

func TestInternalListenersServeOnlyReviewedMethods(t *testing.T) {
	list := loadAllowlist(t, allowlistFile)
	served, _ := composedListeners(t)
	for _, problem := range checkServed(served, list) {
		t.Error(problem)
	}
}

func TestInternalListenerAndRegistrationSitesAreReviewed(t *testing.T) {
	list := loadAllowlist(t, allowlistFile)
	scan := scanSource(t, moduleRoot)
	for _, problem := range checkSource(scan, list) {
		t.Error(problem)
	}
}

func TestInternalHandlersNeverResolveTheAdministrationMode(t *testing.T) {
	_, implementations := composedListeners(t)
	dirs := map[string]bool{}
	// Every package under internal/transport, and the package of every
	// registered implementation (the content server lives in infra/storage).
	if err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skippedDir(entry.Name()) && path != "." {
				return filepath.SkipDir
			}
			dirs[path] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, implementation := range implementations {
		pkg := reflect.TypeOf(implementation).Elem().PkgPath()
		relative, ok := strings.CutPrefix(pkg, modulePath+"/internal/transport")
		if ok {
			dirs["."+relative] = true
			continue
		}
		relative, ok = strings.CutPrefix(pkg, modulePath+"/")
		if !ok {
			t.Fatalf("implementation %T is outside the module", implementation)
		}
		dirs[filepath.Join(moduleRoot, relative)] = true
	}
	if !dirs[filepath.Join(moduleRoot, "internal/infra/storage")] {
		t.Fatal("the content server's package is not scanned")
	}
	for dir := range dirs {
		for _, problem := range scanAdministrationMode(t, dir) {
			t.Error(problem)
		}
	}
}

func TestArchitectureListsTheInternalAdministrationWrites(t *testing.T) {
	list := loadAllowlist(t, allowlistFile)
	for _, problem := range checkArchitecture(t, architecture, serviceName, list) {
		t.Error(problem)
	}
}

// The fixtures: each check must fail on what it exists to catch.

func TestTheSurfaceCheckFailsOnAnUnlistedMethod(t *testing.T) {
	list := loadAllowlist(t, allowlistFile)
	served, _ := composedListeners(t)
	server := grpc.NewServer()
	server.RegisterService(&fixtureAdminServiceDesc, fixtureAdminServer{})
	served["platform"]["fixture.admin.v1.AdminService"] = server.GetServiceInfo()["fixture.admin.v1.AdminService"]
	problems := checkServed(served, list)
	if !containsProblem(problems, "/fixture.admin.v1.AdminService/SetProviderPolicy") {
		t.Fatalf("an unlisted fixture method passed: %v", problems)
	}
}

func TestTheSurfaceCheckFailsOnAnAdministrationWriteMainDoesNotDecide(t *testing.T) {
	list := loadAllowlist(t, filepath.Join("testdata", "listenersurface", "admin_write_without_main.txt"))
	served := map[string]map[string]grpc.ServiceInfo{"platform": {
		"fixture.admin.v1.AdminService": {Methods: []grpc.MethodInfo{{Name: "SetProviderPolicy"}}},
	}}
	problems := checkServed(served, list)
	if !containsProblem(problems, "without an authorization decision in elitea-main") {
		t.Fatalf("an administration write decided by the workload identity alone passed: %v", problems)
	}
}

func TestTheSourceCheckFailsOnAnUnlistedRegistration(t *testing.T) {
	list := loadAllowlist(t, allowlistFile)
	scan := scanSource(t, filepath.Join("testdata", "listenersurface", "registration"))
	problems := checkSource(scan, list)
	for _, want := range []string{"RegisterFixtureAdminServiceServer", "grpc.NewServer", "http.Server", "POST /admin/roles"} {
		if !containsProblem(problems, want) {
			t.Errorf("the source check missed %s: %v", want, problems)
		}
	}
}

func TestTheAdministrationModeScanFailsOnAFixtureHandler(t *testing.T) {
	problems := scanAdministrationMode(t, filepath.Join("testdata", "listenersurface", "adminmode"))
	if len(problems) != 2 {
		t.Fatalf("want the constant and the literal flagged, got %v", problems)
	}
}

func TestTheArchitectureCheckFailsOnAMissingRow(t *testing.T) {
	list := loadAllowlist(t, filepath.Join("testdata", "listenersurface", "admin_write_without_main.txt"))
	problems := checkArchitecture(t, architecture, serviceName, list)
	if !containsProblem(problems, "/fixture.admin.v1.AdminService/SetProviderPolicy") {
		t.Fatalf("an administration write missing from the architecture page passed: %v", problems)
	}
}

type fixtureAdminServer struct{}

type fixtureAdminServiceServer interface{}

var fixtureAdminServiceDesc = grpc.ServiceDesc{
	ServiceName: "fixture.admin.v1.AdminService",
	HandlerType: (*fixtureAdminServiceServer)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "SetProviderPolicy",
		Handler: func(any, context.Context, func(any) error, grpc.UnaryServerInterceptor) (any, error) {
			return nil, nil
		},
	}},
}

func containsProblem(problems []string, want string) bool {
	for _, problem := range problems {
		if strings.Contains(problem, want) {
			return true
		}
	}
	return false
}

// serverTLS is a configuration the runtime listeners accept: TLS 1.3, h2,
// a client certificate required against a CA pool, an ephemeral server
// certificate. Nothing connects.
func serverTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "surface-test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"surface-test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		NextProtos:   []string{"h2"},
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
	}
}
