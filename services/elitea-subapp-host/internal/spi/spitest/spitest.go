// Package spitest serves a host's gRPC listener (spi.NewGRPCServer) over real
// mutual TLS on loopback, for tests.
//
// The provider service replaced the SPI's HTTP surface in #1359, and the
// conformance suites were written against that surface: frozen fixtures that
// pin the bodies, request by request. Handler keeps them reading as they
// were — an http.Handler whose routes are the retired SPI paths, each
// translated into the gRPC call that replaced it — so the fixtures are still
// compared byte for byte, now THROUGH the gRPC wire: TLS handshake, the
// caller allowlist, the identity metadata and the protobuf encoding all sit
// between the test and the host. Nothing outside tests serves these routes.
package spitest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	subappv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Caller is the client identity the listener's allowlist names.
const Caller = "elitea-main"

// PKI is a throwaway certificate authority with a loopback server leaf.
type PKI struct {
	CA     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	Roots  *x509.CertPool
	Server tls.Certificate
}

// NewPKI mints a CA and a server certificate for 127.0.0.1.
func NewPKI(t testing.TB) *PKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "spitest ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	p := &PKI{CA: ca, caKey: key, Roots: x509.NewCertPool()}
	p.Roots.AddCert(ca)
	p.Server = p.Leaf(t, "subapp-host", x509.ExtKeyUsageServerAuth)
	return p
}

// Leaf is a certificate the CA signed for commonName, valid for 127.0.0.1.
func (p *PKI) Leaf(t testing.TB, commonName string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.CA, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// ServerTLS is the listener's configuration: the server leaf, and client
// certificates required and verified against the CA.
func (p *PKI) ServerTLS() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{p.Server}, ClientCAs: p.Roots,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12,
	}
}

// ClientTLS presents certificates (none for an anonymous client).
func (p *PKI) ClientTLS(certificates ...tls.Certificate) *tls.Config {
	return &tls.Config{RootCAs: p.Roots, Certificates: certificates, MinVersion: tls.VersionTLS12}
}

// Listener is a host's gRPC listener running for a test.
type Listener struct {
	PKI  *PKI
	Addr string
	// Conn is a connection as the allowed caller.
	Conn *grpc.ClientConn
}

// Start serves server's gRPC listener with Caller on the allowlist, and dials
// it as Caller. Both are stopped when the test ends.
func Start(t testing.TB, server *spi.Server) *Listener {
	t.Helper()
	p := NewPKI(t)
	grpcServer, err := spi.NewGRPCServer(server, []string{Caller}, p.ServerTLS(), nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	l := &Listener{PKI: p, Addr: listener.Addr().String()}
	l.Conn = l.Dial(t, p.ClientTLS(p.Leaf(t, Caller, x509.ExtKeyUsageClientAuth)))
	return l
}

// Dial connects with the given client TLS configuration.
func (l *Listener) Dial(t testing.TB, cfg *tls.Config) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(l.Addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// Provider is the provider service client over Conn.
func (l *Listener) Provider() subappv1.ProviderServiceClient {
	return subappv1.NewProviderServiceClient(l.Conn)
}

// Handler serves server's listener and answers the retired HTTP SPI routes
// through it (see the package comment).
func Handler(t testing.TB, server *spi.Server) http.Handler {
	t.Helper()
	return Start(t, server).Handler()
}

// Handler is the HTTP-shaped view of the listener. The X-Elitea-* identity
// headers of a request travel as the call's metadata, verbatim: a test that
// signs, forges or omits them reaches the host's verification unchanged.
func (l *Listener) Handler() http.Handler {
	client := l.Provider()
	mux := http.NewServeMux()
	call := func(r *http.Request) context.Context {
		return metadata.NewOutgoingContext(r.Context(), spi.MetadataFromHeaders(r.Header))
	}
	mux.HandleFunc("GET /descriptor", func(w http.ResponseWriter, r *http.Request) {
		response, err := client.Describe(call(r), &subappv1.DescribeRequest{})
		if err != nil {
			writeStatus(w, err)
			return
		}
		writeBody(w, http.StatusOK, response.GetDescriptorJson())
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		response, err := client.Health(call(r), &subappv1.HealthRequest{})
		if err != nil {
			writeStatus(w, err)
			return
		}
		writeBody(w, http.StatusOK, response.GetHealthJson())
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		response, err := client.Health(call(r), &subappv1.HealthRequest{})
		if err != nil {
			writeStatus(w, err)
			return
		}
		state, code := "READY", http.StatusOK
		if !response.GetReady() {
			state, code = "NOT_READY", http.StatusServiceUnavailable
		}
		body, _ := json.Marshal(map[string]any{"status": state, "checks": response.GetChecks()})
		writeBody(w, code, body)
	})
	mux.HandleFunc("GET /slots", func(w http.ResponseWriter, r *http.Request) {
		response, err := client.Slots(call(r), &subappv1.SlotsRequest{})
		if err != nil {
			writeStatus(w, err)
			return
		}
		writeBody(w, http.StatusOK, response.GetSlotsJson())
	})
	mux.HandleFunc("POST /tools/{toolkit}/{tool}/invoke", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeStatus(w, status.Error(codes.InvalidArgument, err.Error()))
			return
		}
		ctx, cancel := context.WithCancel(call(r))
		defer cancel()
		stream, err := client.Invoke(ctx, &subappv1.InvokeRequest{
			Toolkit: r.PathValue("toolkit"), Tool: r.PathValue("tool"), RequestJson: raw,
		})
		if err != nil {
			writeStatus(w, err)
			return
		}
		accepted, err := stream.Recv()
		if err != nil {
			writeStatus(w, err)
			return
		}
		writeBody(w, http.StatusOK, accepted.GetBodyJson())
	})
	mux.HandleFunc("GET /tools/{toolkit}/{tool}/invocations/{invocation}", func(w http.ResponseWriter, r *http.Request) {
		response, err := client.GetInvocation(call(r), &subappv1.GetInvocationRequest{
			Toolkit: r.PathValue("toolkit"), Tool: r.PathValue("tool"), InvocationId: r.PathValue("invocation"),
		})
		if err != nil {
			writeStatus(w, err)
			return
		}
		writeBody(w, http.StatusOK, response.GetBodyJson())
	})
	mux.HandleFunc("DELETE /tools/{toolkit}/{tool}/invocations/{invocation}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := client.Cancel(call(r), &subappv1.CancelRequest{
			Toolkit: r.PathValue("toolkit"), Tool: r.PathValue("tool"), InvocationId: r.PathValue("invocation"),
		}); err != nil {
			writeStatus(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func writeBody(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// HTTPStatus is the status the HTTP surface answered for what the provider
// service now answers with err, and the transport-error envelope it wrote.
func HTTPStatus(err error) (int, string) {
	switch status.Code(err) {
	case codes.InvalidArgument:
		return http.StatusBadRequest, "Bad Request"
	case codes.NotFound:
		return http.StatusNotFound, "Resource Not Found"
	case codes.Unauthenticated:
		return http.StatusUnauthorized, "Unauthorized"
	case codes.PermissionDenied:
		return http.StatusForbidden, "Forbidden"
	}
	return http.StatusInternalServerError, "Internal Server Error"
}

func writeStatus(w http.ResponseWriter, err error) {
	code, message := HTTPStatus(err)
	body, _ := json.Marshal(spi.TransportError(code, message))
	writeBody(w, code, body)
}
