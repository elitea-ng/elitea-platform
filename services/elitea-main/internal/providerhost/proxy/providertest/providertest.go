// Package providertest stands a fake provider up behind the REAL hop: a
// gRPC server with mutual TLS serving elitea.subapp.v1.ProviderService, whose
// every call is answered by an http.Handler written against the frozen SPI
// paths (/slots, /tools/{toolkit}/{tool}/invoke, .../invocations/{id},
// /descriptor, /health).
//
// So a facade test keeps its provider double as it was written — a handler
// that records the request and answers a body — while the bytes between the
// facade and that double travel exactly as they do in production: dialled by
// providerhost/proxy, over TLS 1.2+ with a client certificate the server
// requires, the identity as signed gRPC metadata. The handler sees the
// identity as the X-Elitea-* headers the metadata carries and nothing else:
// the browser's own headers have no way across.
package providertest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	subappv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
)

// Start serves handler as a provider on 127.0.0.1 under serverTLS and
// returns the host:port to dial. The server stops when the test ends.
func Start(t testing.TB, handler http.Handler, serverTLS *tls.Config) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.MaxRecvMsgSize(8<<20))
	subappv1.RegisterProviderServiceServer(server, &adapter{handler: handler})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

// Config starts handler as a provider behind mutual TLS with a fresh test
// PKI, and returns an enabled facade.Config that reaches it: the target, a
// client certificate the server trusts, the CA and the server name.
func Config(t testing.TB, handler http.Handler) facade.Config {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "providertest-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := func(name string, usage x509.ExtKeyUsage) (tls.Certificate, *ecdsa.PrivateKey) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, DNSNames: []string{name},
		}, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, key
	}
	serverCert, _ := leaf("provider.internal", x509.ExtKeyUsageServerAuth)
	clientCert, clientKey := leaf("elitea-main", x509.ExtKeyUsageClientAuth)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	target := Start(t, handler, &tls.Config{
		Certificates: []tls.Certificate{serverCert}, ClientCAs: pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12,
	})
	dir := t.TempDir()
	write := func(name, blockType string, der []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	return facade.Config{
		Enabled:        true,
		Target:         target,
		ClientCertFile: write("tls.crt", "CERTIFICATE", clientCert.Certificate[0]),
		ClientKeyFile:  write("tls.key", "EC PRIVATE KEY", keyDER),
		CAFile:         write("ca.crt", "CERTIFICATE", ca.Raw),
		ServerName:     "provider.internal",
		IdentitySecret: "shared-with-the-provider",
		Timeout:        10 * time.Second,
	}
}

type adapter struct {
	subappv1.UnimplementedProviderServiceServer
	handler http.Handler
}

// serve runs the handler on the request the call stands for.
func (a *adapter) serve(ctx context.Context, method, path string, body []byte) (int, []byte) {
	request := httptest.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
	request.Header = http.Header{}
	md, _ := metadata.FromIncomingContext(ctx)
	for key, values := range md {
		if strings.HasPrefix(key, "x-elitea-") {
			for _, value := range values {
				request.Header.Add(textproto.CanonicalMIMEHeaderKey(key), value)
			}
		}
	}
	if authority := md.Get(":authority"); len(authority) > 0 {
		request.Host = authority[0]
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	a.handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.Bytes()
}

// refusal is the gRPC status the provider service answers for what the
// HTTP double answered.
func refusal(code int, body []byte) error {
	var envelope struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &envelope)
	message := envelope.Message
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	switch code {
	case http.StatusBadRequest:
		return status.Error(codes.InvalidArgument, message)
	case http.StatusNotFound:
		return status.Error(codes.NotFound, message)
	case http.StatusUnauthorized:
		return status.Error(codes.Unauthenticated, message)
	case http.StatusForbidden:
		return status.Error(codes.PermissionDenied, message)
	case http.StatusServiceUnavailable:
		return status.Error(codes.Unavailable, message)
	}
	return status.Error(codes.Internal, message)
}

func (a *adapter) Describe(ctx context.Context, _ *subappv1.DescribeRequest) (*subappv1.DescribeResponse, error) {
	code, body := a.serve(ctx, http.MethodGet, "/descriptor", nil)
	if code != http.StatusOK {
		return nil, refusal(code, body)
	}
	return &subappv1.DescribeResponse{DescriptorJson: body}, nil
}

func (a *adapter) Health(ctx context.Context, _ *subappv1.HealthRequest) (*subappv1.HealthResponse, error) {
	code, body := a.serve(ctx, http.MethodGet, "/health", nil)
	if code != http.StatusOK {
		return nil, refusal(code, body)
	}
	var health struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(body, &health)
	return &subappv1.HealthResponse{Status: health.Status, HealthJson: body, Ready: true}, nil
}

func (a *adapter) Slots(ctx context.Context, _ *subappv1.SlotsRequest) (*subappv1.SlotsResponse, error) {
	code, body := a.serve(ctx, http.MethodGet, "/slots", nil)
	if code != http.StatusOK {
		return nil, refusal(code, body)
	}
	return &subappv1.SlotsResponse{SlotsJson: body}, nil
}

func invokePath(toolkit, tool string) string {
	return "/tools/" + url.PathEscape(toolkit) + "/" + url.PathEscape(tool) + "/invoke"
}

func invocationPath(toolkit, tool, id string) string {
	return "/tools/" + url.PathEscape(toolkit) + "/" + url.PathEscape(tool) + "/invocations/" + url.PathEscape(id)
}

func (a *adapter) Invoke(request *subappv1.InvokeRequest, stream grpc.ServerStreamingServer[subappv1.InvokeEvent]) error {
	code, body := a.serve(stream.Context(), http.MethodPost, invokePath(request.GetToolkit(), request.GetTool()), request.GetRequestJson())
	if code != http.StatusOK {
		return refusal(code, body)
	}
	var accepted struct {
		InvocationID string `json:"invocation_id"`
		Status       string `json:"status"`
	}
	_ = json.Unmarshal(body, &accepted)
	return stream.Send(&subappv1.InvokeEvent{InvocationId: accepted.InvocationID, Status: accepted.Status, BodyJson: body})
}

func (a *adapter) GetInvocation(ctx context.Context, request *subappv1.GetInvocationRequest) (*subappv1.GetInvocationResponse, error) {
	code, body := a.serve(ctx, http.MethodGet, invocationPath(request.GetToolkit(), request.GetTool(), request.GetInvocationId()), nil)
	if code != http.StatusOK {
		return nil, refusal(code, body)
	}
	return &subappv1.GetInvocationResponse{BodyJson: body}, nil
}

func (a *adapter) Cancel(ctx context.Context, request *subappv1.CancelRequest) (*subappv1.CancelResponse, error) {
	code, body := a.serve(ctx, http.MethodDelete, invocationPath(request.GetToolkit(), request.GetTool(), request.GetInvocationId()), nil)
	if code != http.StatusOK && code != http.StatusNoContent {
		return nil, refusal(code, body)
	}
	return &subappv1.CancelResponse{}, nil
}
