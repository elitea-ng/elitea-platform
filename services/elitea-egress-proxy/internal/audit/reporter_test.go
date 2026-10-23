package audit

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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-egress-proxy/internal/config"
	"github.com/elitea-ng/elitea-platform/services/elitea-egress-proxy/internal/proxy"
)

type fakeClient struct {
	mu       sync.Mutex
	fail     int
	requests []*runtimev1.ReportRunSummaryRequestV1
}

func (f *fakeClient) ReportRunSummary(_ context.Context, in *runtimev1.ReportRunSummaryRequestV1, _ ...grpc.CallOption) (*runtimev1.ReportRunSummaryResponseV1, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return nil, errors.New("unavailable")
	}
	f.requests = append(f.requests, in)
	return &runtimev1.ReportRunSummaryResponseV1{}, nil
}

func (f *fakeClient) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func summary() proxy.Summary {
	return proxy.Summary{ExecutionID: "exec-1", ProjectID: 7, Final: true, TokenExpiresAt: time.Unix(1_800_000_000, 0),
		Connections: 1, HostsReached: []proxy.HostReached{{Host: "pypi.org", Port: 443, Connections: 1}},
		HostsRefused: []proxy.HostRefused{{Host: "evil.example", Port: 443, Reason: "sni-mismatch", Count: 1}}}
}

func TestReporterDeliversWithRetryAndCloseDrains(t *testing.T) {
	client := &fakeClient{fail: 2}
	r := New(client, "replica-a", time.Second, nil)
	r.Start()
	r.Submit(summary())
	deadline := time.Now().Add(5 * time.Second)
	for client.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if client.count() != 1 || r.Delivered() != 1 {
		t.Fatalf("delivered %d", r.Delivered())
	}
	got := client.requests[0]
	if got.GetReplicaId() != "replica-a" || got.GetExecutionId() != "exec-1" || !got.GetFinal() ||
		got.GetHostsReached()[0].GetHost() != "pypi.org" || got.GetHostsRefused()[0].GetReason() != "sni-mismatch" {
		t.Fatalf("%v", got)
	}
	// Queued at shutdown (the replica_shutdown summaries): Close delivers.
	r.Close(context.Background())
	shutdown := summary()
	shutdown.ReplicaShutdown = true
	r2 := New(client, "replica-a", time.Second, nil)
	r2.Submit(shutdown)
	r2.Close(context.Background())
	if client.count() != 2 {
		t.Fatalf("close did not drain: %d", client.count())
	}
}

func TestReporterNeverBlocksTheProxy(t *testing.T) {
	r := New(&fakeClient{}, "replica-a", time.Second, nil) // not started: nothing drains
	for range queueSize + 5 {
		r.Submit(summary())
	}
	if r.Dropped() != 5 {
		t.Fatalf("dropped %d", r.Dropped())
	}
}

func TestReportFitsTheControlListenerBound(t *testing.T) {
	s := summary()
	for i := range 256 {
		s.HostsReached = append(s.HostsReached, proxy.HostReached{Host: strings.Repeat("a", 240) + string(rune('a'+i%26)), Port: 443})
	}
	request := New(&fakeClient{}, "r", time.Second, nil).request(s)
	if proto.Size(request) > maxReportBytes || request.GetOtherHosts() == 0 {
		t.Fatalf("size %d other %d", proto.Size(request), request.GetOtherHosts())
	}
}

// --- the real transport: gRPC over mTLS, the client identity checked ---

type pki struct {
	caPEM []byte
	ca    *x509.Certificate
	key   *ecdsa.PrivateKey
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "runtime-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return &pki{caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), ca: ca, key: key}
}

func (p *pki) issue(t *testing.T, name string, usage x509.ExtKeyUsage, ip net.IP) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{name},
		ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature}
	if ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

type mainStandIn struct {
	runtimev1.UnimplementedEgressAuditServiceServer
	mu      sync.Mutex
	clients []string
}

func (m *mainStandIn) ReportRunSummary(ctx context.Context, _ *runtimev1.ReportRunSummaryRequestV1) (*runtimev1.ReportRunSummaryResponseV1, error) {
	p, _ := peer.FromContext(ctx)
	info := p.AuthInfo.(credentials.TLSInfo)
	m.mu.Lock()
	m.clients = append(m.clients, info.State.VerifiedChains[0][0].DNSNames[0])
	m.mu.Unlock()
	return &runtimev1.ReportRunSummaryResponseV1{}, nil
}

func TestDialReportsOverMutualTLS(t *testing.T) {
	ca := newPKI(t)
	serverCert, serverKey := ca.issue(t, "elitea-main", x509.ExtKeyUsageServerAuth, net.ParseIP("127.0.0.1"))
	pair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.caPEM)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
	})))
	standIn := &mainStandIn{}
	runtimev1.RegisterEgressAuditServiceServer(server, standIn)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	dir := t.TempDir()
	clientCert, clientKey := ca.issue(t, "elitea-egress-proxy", x509.ExtKeyUsageClientAuth, nil)
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cfg := config.AuditConfig{Target: listener.Addr().String(), ServerName: "elitea-main",
		CAFile: write("ca.crt", ca.caPEM), CertFile: write("tls.crt", clientCert), KeyFile: write("tls.key", clientKey),
		ReplicaID: "replica-a", Timeout: 5 * time.Second}
	reporter, err := Dial(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	reporter.Start()
	reporter.Submit(summary())
	reporter.Close(context.Background())
	if reporter.Delivered() != 1 {
		t.Fatalf("delivered %d dropped %d", reporter.Delivered(), reporter.Dropped())
	}
	if len(standIn.clients) != 1 || standIn.clients[0] != "elitea-egress-proxy" {
		t.Fatalf("clients %v", standIn.clients)
	}

	// No client certificate pair: refused at start.
	bad := cfg
	bad.KeyFile = filepath.Join(dir, "absent.key")
	if _, err := Dial(bad, nil); err == nil {
		t.Fatal("a missing key must fail the start")
	}
}
