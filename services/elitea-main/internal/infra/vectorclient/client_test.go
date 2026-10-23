package vectorclient_test

// The client against a fake elitea-vector behind REAL mutual TLS on a loopback
// listener: the material is loaded from PEM files as in production, the server
// requires and verifies the client certificate, and the fake answers what a
// test tells it to, so every classification the package promises is exercised
// against a gRPC status that travelled over the wire.

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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	vectorv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	indexingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/vectorclient"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const indexID = "0d7a4a6e-7b4e-4c1a-9d2c-2f0f0a9f5e11"

// pki is a throwaway CA that issues a server leaf for 127.0.0.1 / "vector" and
// a client leaf with one DNS SAN, and writes the client's material to files.
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
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-runtime-ca"},
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

func (p *pki) leaf(t *testing.T, name string, usage x509.ExtKeyUsage, serial int64) (tls.Certificate, []byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		DNSNames: []string{name}}
	if usage == x509.ExtKeyUsageServerAuth {
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, der, key
}

// clientFiles writes a client leaf and returns the cert, key and CA paths.
func (p *pki) clientFiles(t *testing.T, name string) (string, string, string) {
	t.Helper()
	_, der, key := p.leaf(t, name, x509.ExtKeyUsageClientAuth, 3)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(p.dir, name+".crt")
	keyPath := filepath.Join(p.dir, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, filepath.Join(p.dir, "ca.crt")
}

// fakeVector is the VectorService under test control: it records the requests
// and the DNS identity of the caller's certificate, and answers what the test
// set.
type fakeVector struct {
	vectorv1.UnimplementedVectorServiceServer
	mu         sync.Mutex
	deletes    []*vectorv1.DeleteRequest
	drops      []int64
	identities []string
	delete     func(*vectorv1.DeleteRequest) (*vectorv1.DeleteResponse, error)
	drop       func(int64) (*vectorv1.DropProjectResponse, error)
}

func (f *fakeVector) record(ctx context.Context) {
	if peerInfo, ok := peer.FromContext(ctx); ok {
		if tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo); ok && len(tlsInfo.State.PeerCertificates) > 0 {
			f.identities = append(f.identities, strings.Join(tlsInfo.State.PeerCertificates[0].DNSNames, ","))
		}
	}
}

func (f *fakeVector) Delete(ctx context.Context, request *vectorv1.DeleteRequest) (*vectorv1.DeleteResponse, error) {
	f.mu.Lock()
	f.deletes = append(f.deletes, request)
	f.record(ctx)
	f.mu.Unlock()
	if f.delete == nil {
		return &vectorv1.DeleteResponse{Collections: 2}, nil
	}
	return f.delete(request)
}

func (f *fakeVector) DropProject(ctx context.Context, request *vectorv1.DropProjectRequest) (*vectorv1.DropProjectResponse, error) {
	f.mu.Lock()
	f.drops = append(f.drops, request.GetProjectId())
	f.record(ctx)
	f.mu.Unlock()
	if f.drop == nil {
		return &vectorv1.DropProjectResponse{Collections: 3}, nil
	}
	return f.drop(request.GetProjectId())
}

// serve starts the fake behind mutual TLS and returns its address.
func serve(t *testing.T, p *pki, vector *fakeVector) string {
	t.Helper()
	serverCert, _, _ := p.leaf(t, "vector", x509.ExtKeyUsageServerAuth, 2)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: p.pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	vectorv1.RegisterVectorServiceServer(server, vector)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

func newClient(t *testing.T, p *pki, target string) *vectorclient.Client {
	t.Helper()
	cert, key, ca := p.clientFiles(t, "elitea-main")
	client, err := vectorclient.New(vectorclient.Config{
		Target: target, ClientCertFile: cert, ClientKeyFile: key, CAFile: ca,
		ServerName: "vector", IndexDeleteTimeout: 5 * time.Second, ProjectDropTimeout: 5 * time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestDeleteIndexVectorsIsAWholeNamespaceDeleteOverMutualTLS(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	vector := &fakeVector{}
	client := newClient(t, p, serve(t, p, vector))
	if client.Deferred() {
		t.Fatal("a real client must not report itself deferred: the sweeper would idle for ever")
	}

	err := client.DeleteIndexVectors(context.Background(), indexingapp.IndexVectorNamespace{ProjectID: 7, IndexID: indexID})
	if err != nil {
		t.Fatalf("DeleteIndexVectors: %v", err)
	}
	if len(vector.deletes) != 1 {
		t.Fatalf("the store saw %d deletes, want 1", len(vector.deletes))
	}
	request := vector.deletes[0]
	switch {
	case request.GetProjectId() != 7:
		t.Errorf("project_id = %d, want 7 (an administrator must name the project)", request.GetProjectId())
	case request.GetNamespace().GetSource() != vectorv1.Source_SOURCE_TOOLKIT_INDEX:
		t.Errorf("source = %v, want TOOLKIT_INDEX", request.GetNamespace().GetSource())
	case request.GetNamespace().GetNamespaceId() != indexID:
		t.Errorf("namespace_id = %q, want the index id", request.GetNamespace().GetNamespaceId())
	case request.GetWholeNamespace() == nil:
		t.Errorf("selector = %T, want the whole namespace", request.GetSelector())
	case request.GetSpace() != nil:
		t.Errorf("space = %v, want none (every collection)", request.GetSpace())
	}
	if len(vector.identities) != 1 || vector.identities[0] != "elitea-main" {
		t.Errorf("the store saw the client identities %v, want [elitea-main]", vector.identities)
	}
}

func TestDeleteIndexVectorsRefusesBadInputBeforeTheWire(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	vector := &fakeVector{}
	client := newClient(t, p, serve(t, p, vector))
	for _, namespace := range []indexingapp.IndexVectorNamespace{
		{ProjectID: 0, IndexID: indexID},
		{ProjectID: 7, IndexID: "not-a-uuid"},
		{ProjectID: 7, IndexID: strings.ToUpper(indexID)},
		{ProjectID: 7, IndexID: ""},
	} {
		err := client.DeleteIndexVectors(context.Background(), namespace)
		if !errors.Is(err, vectorclient.ErrConfig) {
			t.Errorf("%+v: err = %v, want ErrConfig", namespace, err)
		}
	}
	if len(vector.deletes) != 0 {
		t.Fatalf("invalid requests reached the store: %v", vector.deletes)
	}
}

func TestDeleteIndexVectorsReportsAFailureAndNamesARefusal(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	var code codes.Code
	vector := &fakeVector{delete: func(*vectorv1.DeleteRequest) (*vectorv1.DeleteResponse, error) {
		return nil, status.Error(code, "as told")
	}}
	client := newClient(t, p, serve(t, p, vector))
	namespace := indexingapp.IndexVectorNamespace{ProjectID: 7, IndexID: indexID}

	code = codes.Unavailable
	err := client.DeleteIndexVectors(context.Background(), namespace)
	if err == nil || errors.Is(err, platformops.ErrNotAllowed) || !strings.Contains(err.Error(), "Unavailable") {
		t.Fatalf("an outage must be an error naming the code, got %v", err)
	}

	// A refused certificate carries both the sweeper's class (it backs off to
	// its longest pause) and platformops's.
	code = codes.PermissionDenied
	err = client.DeleteIndexVectors(context.Background(), namespace)
	if !errors.Is(err, platformops.ErrNotAllowed) || !errors.Is(err, indexingapp.ErrIndexVectorDeletionRefused) {
		t.Fatalf("a refused certificate must be named as refused, got %v", err)
	}

	code = codes.Unauthenticated
	if err := client.DeleteIndexVectors(context.Background(), namespace); !errors.Is(err, indexingapp.ErrIndexVectorDeletionRefused) {
		t.Fatalf("UNAUTHENTICATED must be a refusal, got %v", err)
	}
}

// Nobody listens at the target: a call fails fast (UNAVAILABLE, retry-later),
// well inside the index delete route's budget, instead of waiting out the
// call's own deadline for a listener to appear.
func TestAnUnreachableTargetFailsFastWithinTheRouteBudget(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := listener.Addr().String()
	_ = listener.Close()
	client := newClient(t, p, target) // 5s call timeouts

	started := time.Now()
	err = client.DeleteIndexVectors(context.Background(), indexingapp.IndexVectorNamespace{ProjectID: 7, IndexID: indexID})
	if err == nil || errors.Is(err, platformops.ErrNotAllowed) || !strings.Contains(err.Error(), "Unavailable") {
		t.Fatalf("err = %v, want an UNAVAILABLE failure", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the call took %v against a closed port; it must fail fast", elapsed)
	}
	started = time.Now()
	if _, err := client.DeleteProjectData(context.Background(), 7); !errors.Is(err, platformops.ErrRetryLater) {
		t.Fatalf("drop err = %v, want retry-later", err)
	} else if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the drop took %v against a closed port; it must fail fast", elapsed)
	}
	// The caller's own, shorter bound is what the route passes: honoured too.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started = time.Now()
	_ = client.DeleteIndexVectors(ctx, indexingapp.IndexVectorNamespace{ProjectID: 7, IndexID: indexID})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("a 200ms caller bound took %v", elapsed)
	}
}

// The client material is swapped on disk while the client lives (the chart
// renews the Secret and elitea-vector restarts with its new certificate): the
// server comes back under a new CA, the files follow, and the SAME client
// reaches it again with no restart of its own.
func TestRotatedMaterialIsPickedUpWithoutANewClient(t *testing.T) {
	t.Parallel()
	old, fresh := newPKI(t), newPKI(t)
	// Both PKIs issue the client leaf to the same file names (a mount).
	fresh.dir = old.dir
	vector := &fakeVector{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	start := func(p *pki, listener net.Listener) *grpc.Server {
		serverCert, _, _ := p.leaf(t, "vector", x509.ExtKeyUsageServerAuth, 2)
		server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{serverCert},
			ClientCAs: p.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12})))
		vectorv1.RegisterVectorServiceServer(server, vector)
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(server.Stop)
		return server
	}
	server := start(old, listener)

	client := newClient(t, old, addr)
	if err := client.DeleteIndexVectors(context.Background(), indexingapp.IndexVectorNamespace{ProjectID: 7, IndexID: indexID}); err != nil {
		t.Fatalf("before the rotation: %v", err)
	}

	// elitea-vector restarts under the new CA on the same address. The client's
	// files are still the old ones, so its next connection is refused (a
	// handshake failure surfaces as UNAVAILABLE), and no deletion reaches the
	// store.
	server.Stop()
	deletesBefore := len(vector.deletes)
	relistened, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	start(fresh, relistened)
	err = client.DeleteIndexVectors(context.Background(), indexingapp.IndexVectorNamespace{ProjectID: 7, IndexID: indexID})
	if err == nil || len(vector.deletes) != deletesBefore {
		t.Fatalf("the old material was accepted by a rotated server: err=%v deletes=%d", err, len(vector.deletes)-deletesBefore)
	}

	// The Secret is rewritten with the new CA's material: the client reloads on
	// its next connection. gRPC reconnects with a backoff of about a second, so
	// the success is polled for a few seconds.
	cert, key, ca := fresh.clientFiles(t, "elitea-main")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fresh.ca.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	for _, path := range []string{cert, key, ca} {
		if err := os.Chtimes(path, future, future); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := client.DeleteIndexVectors(context.Background(), indexingapp.IndexVectorNamespace{ProjectID: 7, IndexID: indexID})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rotated material was never picked up: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(vector.deletes) != deletesBefore+1 {
		t.Fatalf("the store saw %d deletes after the rotation, want 1", len(vector.deletes)-deletesBefore)
	}
}

func TestDeleteIndexVectorsReturnsTheCallersCancellation(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	vector := &fakeVector{delete: func(*vectorv1.DeleteRequest) (*vectorv1.DeleteResponse, error) {
		time.Sleep(200 * time.Millisecond)
		return &vectorv1.DeleteResponse{}, nil
	}}
	client := newClient(t, p, serve(t, p, vector))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := client.DeleteIndexVectors(ctx, indexingapp.IndexVectorNamespace{ProjectID: 7, IndexID: indexID})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
}

func TestDeleteProjectDataIsDropProjectOverMutualTLS(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	vector := &fakeVector{}
	client := newClient(t, p, serve(t, p, vector))
	if vectorclient.App != "vector" {
		t.Fatalf("App = %q, want the provisioner's %q", vectorclient.App, projectprovisioning.AppVector)
	}

	summary, err := client.DeleteProjectData(context.Background(), 7)
	if err != nil {
		t.Fatalf("DeleteProjectData: %v", err)
	}
	if summary != "dropped the project's vectors in 3 collection(s)" {
		t.Errorf("summary = %q", summary)
	}
	if len(vector.drops) != 1 || vector.drops[0] != 7 {
		t.Errorf("the store saw drops %v, want [7]", vector.drops)
	}
}

func TestDeleteProjectDataClassifiesTheStatusForTheJournal(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	var code codes.Code
	vector := &fakeVector{drop: func(int64) (*vectorv1.DropProjectResponse, error) {
		return nil, status.Error(code, "as told")
	}}
	client := newClient(t, p, serve(t, p, vector))

	cases := []struct {
		code codes.Code
		want error
	}{
		{codes.Unavailable, platformops.ErrRetryLater},
		{codes.DeadlineExceeded, platformops.ErrRetryLater},
		{codes.FailedPrecondition, platformops.ErrRetryLater},
		{codes.Aborted, platformops.ErrRetryLater},
		{codes.ResourceExhausted, platformops.ErrRetryLater},
		{codes.Unauthenticated, platformops.ErrNotAllowed},
		{codes.PermissionDenied, platformops.ErrNotAllowed},
		{codes.InvalidArgument, platformops.ErrConfig},
	}
	for _, tc := range cases {
		code = tc.code
		_, err := client.DeleteProjectData(context.Background(), 7)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.code, err, tc.want)
		}
		if !strings.Contains(err.Error(), tc.code.String()) || !strings.Contains(err.Error(), "as told") {
			t.Errorf("%s: the message must carry the code and the store's message, got %q", tc.code, err)
		}
	}
	// INTERNAL and the rest are plain failures: not retry-later, not refused,
	// not unsupported. The journal retries them.
	code = codes.Internal
	_, err := client.DeleteProjectData(context.Background(), 7)
	if err == nil || errors.Is(err, platformops.ErrRetryLater) || errors.Is(err, platformops.ErrNotAllowed) ||
		errors.Is(err, platformops.ErrNotSupported) || errors.Is(err, platformops.ErrConfig) {
		t.Fatalf("INTERNAL must be a plain failure, got %v", err)
	}
	// UNIMPLEMENTED is a plain failure too: DropProject is in the contract, an
	// elitea-vector that does not serve it is broken, not "keeps nothing".
	code = codes.Unimplemented
	if _, err := client.DeleteProjectData(context.Background(), 7); errors.Is(err, platformops.ErrNotSupported) {
		t.Fatalf("UNIMPLEMENTED must not close the step as unsupported, got %v", err)
	}
	// Invalid ids never reach the wire.
	for _, projectID := range []int64{0, -1, 1 << 31} {
		if _, err := client.DeleteProjectData(context.Background(), projectID); !errors.Is(err, platformops.ErrConfig) {
			t.Errorf("project %d: err = %v, want ErrConfig", projectID, err)
		}
	}
	if len(vector.drops) != len(cases)+2 {
		t.Fatalf("the store saw %d drops, want %d", len(vector.drops), len(cases)+2)
	}
}

func TestAServerThatRejectsTheClientCertificateIsNotAllowed(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	// A server that trusts ANOTHER CA refuses the handshake outright. gRPC
	// reports that as UNAVAILABLE on the call, which is a retry-later: the
	// client cannot tell a wrong CA from an outage at this layer, and the
	// message names the handshake.
	other := newPKI(t)
	serverCert, _, _ := other.leaf(t, "vector", x509.ExtKeyUsageServerAuth, 2)
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: other.pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	vectorv1.RegisterVectorServiceServer(server, &fakeVector{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	client := newClient(t, p, listener.Addr().String())
	_, err = client.DeleteProjectData(context.Background(), 7)
	if !errors.Is(err, platformops.ErrRetryLater) {
		t.Fatalf("a failed handshake must be retry-later, got %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	cert, key, ca := p.clientFiles(t, "elitea-main")
	good := vectorclient.Config{Target: "elitea-vector:9470", ClientCertFile: cert, ClientKeyFile: key, CAFile: ca}
	if err := good.Validate(); err != nil {
		t.Fatalf("a complete config must validate: %v", err)
	}
	for name, mutate := range map[string]func(*vectorclient.Config){
		"empty target":     func(c *vectorclient.Config) { c.Target = "" },
		"url target":       func(c *vectorclient.Config) { c.Target = "https://elitea-vector:9470" },
		"no port":          func(c *vectorclient.Config) { c.Target = "elitea-vector" },
		"no cert":          func(c *vectorclient.Config) { c.ClientCertFile = "" },
		"no key":           func(c *vectorclient.Config) { c.ClientKeyFile = "" },
		"no ca":            func(c *vectorclient.Config) { c.CAFile = "" },
		"negative timeout": func(c *vectorclient.Config) { c.IndexDeleteTimeout = -time.Second },
	} {
		cfg := good
		mutate(&cfg)
		if err := cfg.Validate(); !errors.Is(err, vectorclient.ErrConfig) {
			t.Errorf("%s: err = %v, want ErrConfig", name, err)
		}
		if _, err := vectorclient.New(cfg, nil); !errors.Is(err, vectorclient.ErrConfig) {
			t.Errorf("%s: New err = %v, want ErrConfig", name, err)
		}
	}
	// Material that does not exist is a construction error, not a per-call one.
	missing := good
	missing.ClientCertFile = filepath.Join(p.dir, "absent.crt")
	if _, err := vectorclient.New(missing, nil); !errors.Is(err, vectorclient.ErrConfig) {
		t.Errorf("missing material: New err = %v, want ErrConfig", err)
	}
	if err := (vectorclient.Config{}).Validate(); err == nil {
		t.Error("the zero config validated")
	}
}
