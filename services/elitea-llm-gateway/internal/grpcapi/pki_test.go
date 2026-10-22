package grpcapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	gatewayv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/gateway/v1"
)

const testServerName = "elitea-llm-gateway"

// testPKI is a throwaway CA for real-TLS tests.
type testPKI struct {
	ca     *x509.Certificate
	key    ed25519.PrivateKey
	roots  *x509.CertPool
	serial atomic.Int64
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "gateway gRPC test CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	p := &testPKI{ca: ca, key: key, roots: roots}
	p.serial.Store(1)
	return p
}

func (p *testPKI) issue(t *testing.T, dnsName string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(p.serial.Add(1)),
		Subject:      pkix.Name{CommonName: "ignored-" + dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}, p.ca, pub, p.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, p.ca.Raw}, PrivateKey: key}
}

func (p *testPKI) serverTLS(t *testing.T) *tls.Config {
	t.Helper()
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{p.issue(t, testServerName, x509.ExtKeyUsageServerAuth)},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    p.roots,
		NextProtos:   []string{"h2"},
	}
}

func (p *testPKI) clientTLS(t *testing.T, dnsName string) *tls.Config {
	t.Helper()
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{p.issue(t, dnsName, x509.ExtKeyUsageClientAuth)},
		RootCAs:      p.roots,
		ServerName:   testServerName,
	}
}

type tlsConfigT = tls.Config

// withRoots makes cfg trust the server of pki while it keeps its own client
// certificate, so the server is the one that refuses.
func withRoots(cfg *tls.Config, pki *testPKI) *tls.Config {
	cfg.RootCAs = pki.roots
	return cfg
}

func (p *testPKI) anonymousTLS() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: p.roots, ServerName: testServerName}
}

// startTestServer serves GatewayService over real mutual TLS on a loopback
// port, admitting dns:elitea-main.
func startTestServer(t *testing.T, pki *testPKI, llm, status http.Handler) *Server {
	t.Helper()
	if status == nil {
		status = http.NotFoundHandler()
	}
	service, err := NewService(Config{LLM: llm, Status: status})
	if err != nil {
		t.Fatal(err)
	}
	allow, err := ParseAllowlist("dns:elitea-main")
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(lis.Addr().String(), pki.serverTLS(t), allow, service)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	srv.addr = lis.Addr().String()
	return srv
}

func dialTestClient(t *testing.T, srv *Server, clientTLS *tls.Config) gatewayv1.GatewayServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(srv.addr, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return gatewayv1.NewGatewayServiceClient(conn)
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}
