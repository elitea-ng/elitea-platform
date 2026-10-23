package platformops_test

// The client against a fake platform service behind REAL mutual TLS on a
// loopback listener: the material is loaded from PEM files as in production,
// the server requires and verifies the client certificate, and the fake answers
// what a test tells it to, so every classification the package promises is
// exercised against a gRPC status that travelled over the wire.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	subappv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// pki is a throwaway CA that issues a server leaf for 127.0.0.1 / "engine" and
// a client leaf for CN=elitea-main, and writes the client's material to files.
type pki struct {
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
	pool  *x509.CertPool
	dir   string
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &pki{ca: ca, caKey: key, pool: pool, dir: dir}
}

func (p *pki) leaf(t *testing.T, cn string, usage x509.ExtKeyUsage, serial int64) (tls.Certificate, []byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		DNSNames: []string{"engine"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, der, key
}

// clientFiles writes a client leaf and returns the cert, key and CA paths.
func (p *pki) clientFiles(t *testing.T, cn string) (string, string, string) {
	t.Helper()
	_, der, key := p.leaf(t, cn, x509.ExtKeyUsageClientAuth, 3)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(p.dir, cn+".crt")
	keyPath := filepath.Join(p.dir, cn+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, filepath.Join(p.dir, "ca.crt")
}

// fakeEngine is the platform service under test control. It records the
// requests and answers with what the test set.
type fakeEngine struct {
	subappv1.UnimplementedPlatformOperationsServer
	mu       sync.Mutex
	projects []int32
	toolkits [][2]int32
	project  func(int32) (*subappv1.DeleteProjectResponse, error)
	toolkit  func(int32, int32) (*subappv1.DeleteToolkitResponse, error)
}

func (f *fakeEngine) DeleteProject(_ context.Context, request *subappv1.DeleteProjectRequest) (*subappv1.DeleteProjectResponse, error) {
	f.mu.Lock()
	f.projects = append(f.projects, request.GetProjectId())
	f.mu.Unlock()
	if f.project == nil {
		return &subappv1.DeleteProjectResponse{ProjectId: request.GetProjectId()}, nil
	}
	return f.project(request.GetProjectId())
}

func (f *fakeEngine) DeleteToolkit(_ context.Context, request *subappv1.DeleteToolkitRequest) (*subappv1.DeleteToolkitResponse, error) {
	f.mu.Lock()
	f.toolkits = append(f.toolkits, [2]int32{request.GetProjectId(), request.GetToolkitId()})
	f.mu.Unlock()
	if f.toolkit == nil {
		return nil, status.Error(codes.Unimplemented, "nothing per toolkit")
	}
	return f.toolkit(request.GetProjectId(), request.GetToolkitId())
}

// serve starts the fake behind mutual TLS and returns its address.
func serve(t *testing.T, p *pki, engine *fakeEngine) string {
	t.Helper()
	serverCert, _, _ := p.leaf(t, "engine", x509.ExtKeyUsageServerAuth, 2)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: p.pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	subappv1.RegisterPlatformOperationsServer(server, engine)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

func newClient(t *testing.T, p *pki, target string) *platformops.Client {
	t.Helper()
	cert, key, ca := p.clientFiles(t, "elitea-main")
	client, err := platformops.New(platformops.Config{
		App: platformops.AppInventory, Target: target,
		ClientCertFile: cert, ClientKeyFile: key, CAFile: ca,
		ServerName: "engine", CallTimeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestDeleteProjectDataOverMutualTLS(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	engine := &fakeEngine{project: func(id int32) (*subappv1.DeleteProjectResponse, error) {
		return &subappv1.DeleteProjectResponse{ProjectId: id,
			Wikis:           []*subappv1.WikiDeletion{{WikiId: "a"}, {WikiId: "b"}},
			GraphToolkitIds: []int64{70}}, nil
	}}
	client := newClient(t, p, serve(t, p, engine))

	summary, err := client.DeleteProjectData(context.Background(), 7)
	if err != nil {
		t.Fatalf("DeleteProjectData: %v", err)
	}
	if summary != "deleted 2 wiki index(es), 1 graph(s)" {
		t.Errorf("summary = %q", summary)
	}
	if len(engine.projects) != 1 || engine.projects[0] != 7 {
		t.Errorf("the engine saw projects %v, want [7]", engine.projects)
	}
}

func TestAnEmptyProjectIsACleanSuccess(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	client := newClient(t, p, serve(t, p, &fakeEngine{}))
	summary, err := client.DeleteProjectData(context.Background(), 7)
	if err != nil || summary != "nothing to delete" {
		t.Fatalf("summary=%q err=%v", summary, err)
	}
}

func TestAnIncompleteProjectDeletionIsRetried(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	engine := &fakeEngine{project: func(id int32) (*subappv1.DeleteProjectResponse, error) {
		return &subappv1.DeleteProjectResponse{ProjectId: id, GraphToolkitIds: []int64{70},
			Incomplete: true, BusyToolkitIds: []int64{71}}, nil
	}}
	client := newClient(t, p, serve(t, p, engine))
	summary, err := client.DeleteProjectData(context.Background(), 7)
	if !errors.Is(err, platformops.ErrRetryLater) {
		t.Fatalf("err = %v, want ErrRetryLater", err)
	}
	if summary != "deleted 1 graph(s)" {
		t.Errorf("what went was not reported: %q", summary)
	}
	if !strings.Contains(err.Error(), "1 busy toolkit(s)") {
		t.Errorf("the error does not say what is held: %v", err)
	}
}

func TestStatusCodesAreClassified(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code codes.Code
		want error
	}{
		{codes.FailedPrecondition, platformops.ErrRetryLater},
		{codes.Aborted, platformops.ErrRetryLater},
		{codes.Unavailable, platformops.ErrRetryLater},
		{codes.PermissionDenied, platformops.ErrNotAllowed},
		{codes.Unauthenticated, platformops.ErrNotAllowed},
		{codes.Unimplemented, platformops.ErrNotSupported},
	}
	for _, tc := range cases {
		t.Run(tc.code.String(), func(t *testing.T) {
			t.Parallel()
			p := newPKI(t)
			engine := &fakeEngine{
				project: func(int32) (*subappv1.DeleteProjectResponse, error) {
					return nil, status.Error(tc.code, "as told")
				},
				toolkit: func(int32, int32) (*subappv1.DeleteToolkitResponse, error) {
					return nil, status.Error(tc.code, "as told")
				},
			}
			client := newClient(t, p, serve(t, p, engine))
			if _, err := client.DeleteProjectData(context.Background(), 7); !errors.Is(err, tc.want) {
				t.Errorf("DeleteProjectData: %v, want %v", err, tc.want)
			}
			if _, err := client.DeleteToolkitData(context.Background(), 7, 70); !errors.Is(err, tc.want) {
				t.Errorf("DeleteToolkitData: %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAnInternalFailureIsNeitherRetryNorNotSupported(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	engine := &fakeEngine{project: func(int32) (*subappv1.DeleteProjectResponse, error) {
		return nil, status.Error(codes.Internal, "the store failed")
	}}
	client := newClient(t, p, serve(t, p, engine))
	_, err := client.DeleteProjectData(context.Background(), 7)
	switch {
	case err == nil:
		t.Fatal("no error")
	case errors.Is(err, platformops.ErrRetryLater), errors.Is(err, platformops.ErrNotSupported), errors.Is(err, platformops.ErrNotAllowed):
		t.Fatalf("an internal failure was classified: %v", err)
	case !strings.Contains(err.Error(), "the store failed"):
		t.Fatalf("the engine's message was lost: %v", err)
	}
}

func TestDeleteToolkitDataCarriesBothIDs(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	engine := &fakeEngine{toolkit: func(project, toolkit int32) (*subappv1.DeleteToolkitResponse, error) {
		return &subappv1.DeleteToolkitResponse{ProjectId: project, ToolkitId: toolkit, Deleted: true}, nil
	}}
	client := newClient(t, p, serve(t, p, engine))
	deletion, err := client.DeleteToolkitData(context.Background(), 7, 70)
	if err != nil || !deletion.Deleted {
		t.Fatalf("deletion=%+v err=%v", deletion, err)
	}
	if len(engine.toolkits) != 1 || engine.toolkits[0] != [2]int32{7, 70} {
		t.Errorf("the engine saw %v, want [[7 70]]", engine.toolkits)
	}
}

func TestAnUnreachableEngineIsRetryLater(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	client := newClient(t, p, addr)
	if _, err := client.DeleteProjectData(context.Background(), 7); !errors.Is(err, platformops.ErrRetryLater) {
		t.Fatalf("err = %v, want ErrRetryLater", err)
	}
}

func TestAServerCertificateFromAnotherCAIsRefused(t *testing.T) {
	t.Parallel()
	engineCA := newPKI(t)
	clientCA := newPKI(t)
	addr := serve(t, engineCA, &fakeEngine{})
	// The client trusts ITS CA, which did not sign the engine's certificate.
	client := newClient(t, clientCA, addr)
	if _, err := client.DeleteProjectData(context.Background(), 7); !errors.Is(err, platformops.ErrRetryLater) {
		t.Fatalf("err = %v, want a transport failure (ErrRetryLater), never a success", err)
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	cert, key, ca := p.clientFiles(t, "elitea-main")
	good := platformops.Config{App: platformops.AppDeepWiki, Target: "engine:9443",
		ClientCertFile: cert, ClientKeyFile: key, CAFile: ca}
	if err := good.Validate(); err != nil {
		t.Fatalf("a good config is refused: %v", err)
	}
	bad := map[string]func(*platformops.Config){
		"unknown app":   func(c *platformops.Config) { c.App = "echo" },
		"empty target":  func(c *platformops.Config) { c.Target = "" },
		"url target":    func(c *platformops.Config) { c.Target = "https://engine:9443" },
		"no port":       func(c *platformops.Config) { c.Target = "engine" },
		"no cert":       func(c *platformops.Config) { c.ClientCertFile = "" },
		"no key":        func(c *platformops.Config) { c.ClientKeyFile = "" },
		"no ca":         func(c *platformops.Config) { c.CAFile = "" },
		"missing files": func(c *platformops.Config) { c.CAFile = filepath.Join(p.dir, "absent.crt") },
	}
	for name, mutate := range bad {
		cfg := good
		mutate(&cfg)
		if _, err := platformops.New(cfg, nil); !errors.Is(err, platformops.ErrConfig) {
			t.Errorf("%s: err = %v, want ErrConfig", name, err)
		}
	}
}

func TestAppForToolkitType(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		// The types the shipped descriptors project (type_override, or
		// <provider>_<toolkit>), and the historical DeepWiki spellings.
		"inventory":         platformops.AppInventory,
		"inventory_search":  platformops.AppInventory,
		" inventory ":       platformops.AppInventory,
		"wikis_Wikis":       platformops.AppDeepWiki,
		"wikis_query":       platformops.AppDeepWiki,
		"wiki_query":        platformops.AppDeepWiki,
		"deepwiki_Deepwiki": platformops.AppDeepWiki,
		"deepwiki":          platformops.AppDeepWiki,
		"wikis":             platformops.AppDeepWiki,
		// Exact matches only: no prefix, no case folding, no guessing.
		"inventory_inventory": "",
		"Inventory":           "",
		"Inventory_Graph":     "",
		"inventorying_x":      "",
		"_inventory":          "",
		"github":              "",
		"":                    "",
	}
	for in, want := range cases {
		if got := platformops.AppForToolkitType(in); got != want {
			t.Errorf("AppForToolkitType(%q) = %q, want %q", in, got, want)
		}
	}
	if got := platformops.ToolkitTypesOf(platformops.AppInventory); !slices.Equal(got, []string{"inventory", "inventory_search"}) {
		t.Errorf("ToolkitTypesOf(inventory) = %v", got)
	}
	if got := platformops.ToolkitTypesOf("github"); len(got) != 0 {
		t.Errorf("ToolkitTypesOf(github) = %v", got)
	}
}

// The allow-list is pinned to the descriptors this repository ships: every
// toolkit type a descriptor projects (internal/api/v2/toolkits.
// projectProviderToolkits: the toolkit's type_override, or <provider name>_
// <toolkit name>) must resolve to its application, so a toolkit added to an
// engine cannot be deleted without its engine data from the day it ships.
func TestAppForToolkitTypeCoversTheShippedDescriptors(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..", "..", "elitea-subapp-host", "internal", "apps")
	for directory, app := range map[string]string{"inventory": platformops.AppInventory, "deepwiki": platformops.AppDeepWiki} {
		raw, err := os.ReadFile(filepath.Join(root, directory, "descriptor.json"))
		if err != nil {
			t.Skipf("the shipped descriptor is not readable from here: %v", err)
		}
		var descriptor struct {
			Name     string `json:"name"`
			Toolkits []struct {
				Name     string         `json:"name"`
				Metadata map[string]any `json:"toolkit_metadata"`
			} `json:"provided_toolkits"`
		}
		if err := json.Unmarshal(raw, &descriptor); err != nil {
			t.Fatalf("%s descriptor: %v", directory, err)
		}
		if len(descriptor.Toolkits) == 0 {
			t.Fatalf("%s descriptor publishes no toolkit", directory)
		}
		for _, toolkit := range descriptor.Toolkits {
			toolkitType, _ := toolkit.Metadata["type_override"].(string)
			if toolkitType == "" {
				toolkitType = descriptor.Name + "_" + toolkit.Name
			}
			if got := platformops.AppForToolkitType(toolkitType); got != app {
				t.Errorf("%s descriptor projects type %q, which AppForToolkitType maps to %q, want %q: add it to toolkitTypeApps",
					directory, toolkitType, got, app)
			}
		}
	}
}
