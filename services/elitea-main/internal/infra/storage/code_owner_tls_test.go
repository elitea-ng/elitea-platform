package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth/grpcauthtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcstatus "google.golang.org/grpc/status"
)

func codeOwnerClientTLSFixture(t *testing.T, identity string) *tls.Config {
	t.Helper()
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Unix(1, 0), NotAfter: time.Unix(9999999, 0), URIs: []*url.URL{uri}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(bytes.NewReader(bytes.Repeat([]byte{3}, 1024)), template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}
func TestCodeOwnerClientRequiresConfiguredExactMTLSIdentitiesAndFixedOrigin(t *testing.T) {
	requester := "spiffe://elitea/main"
	audience := "spiffe://elitea/supervisor/one"
	for _, name := range []string{"configured", "redirecting origin path", "plain HTTP", "URL credentials", "URL query", "URL fragment", "insecure TLS", "missing CA", "different Main certificate", "certificate callback", "duplicate audience", "missing port"} {
		t.Run(name, func(t *testing.T) {
			cfg := CodeOwnerEndpoint{Audience: audience, Origin: "https://owner.invalid:9443", TLS: codeOwnerClientTLSFixture(t, requester)}
			switch name {
			case "redirecting origin path":
				cfg.Origin += "/other"
			case "plain HTTP":
				cfg.Origin = "http://owner.invalid:9443"
			case "URL credentials":
				cfg.Origin = "https://user:secret@owner.invalid:9443"
			case "missing port":
				cfg.Origin = "https://owner.invalid"
			case "URL query":
				cfg.Origin += "?target=other"
			case "URL fragment":
				cfg.Origin += "#other"
			case "insecure TLS":
				cfg.TLS.InsecureSkipVerify = true
			case "missing CA":
				cfg.TLS.RootCAs = nil
			case "different Main certificate":
				cfg.TLS = codeOwnerClientTLSFixture(t, "spiffe://elitea/worker/one")
			case "certificate callback":
				cfg.TLS.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cfg.TLS.Certificates[0], nil }
			}
			endpoints := []CodeOwnerEndpoint{cfg}
			if name == "duplicate audience" {
				endpoints = append(endpoints, cfg)
			}
			client, err := NewCodeOwnerClient(requester, endpoints)
			if name != "configured" {
				if err == nil || client != nil {
					t.Fatal("unsafe owner client configuration admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			config := client.endpoints[audience].tls
			if config.MinVersion < tls.VersionTLS13 || config.InsecureSkipVerify {
				t.Fatal("configured client broadened TLS bounds")
			}
			leaf := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "elitea", Path: "/supervisor/one"}}}
			state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
			if config.VerifyConnection(state) != nil {
				t.Fatal("configured exact owner refused")
			}
			state.VerifiedChains = nil
			if config.VerifyConnection(state) == nil {
				t.Fatal("unverified Supervisor accepted")
			}
			state.VerifiedChains = [][]*x509.Certificate{{leaf}}
			other := *leaf
			other.URIs = []*url.URL{{Scheme: "spiffe", Host: "elitea", Path: "/supervisor/two"}}
			state.PeerCertificates = []*x509.Certificate{&other}
			if config.VerifyConnection(state) == nil {
				t.Fatal("another verified Supervisor accepted")
			}
			_, err = client.ReadRetainedRuntime(t.Context(), code.PlatformGrantClaims{}, code.SignedGrant{})
			if err == nil {
				t.Fatal("unsigned caller fields selected owner operation")
			}
		})
	}
}

// ownerSupervisorFake records the owner RPCs a real mTLS call delivers.
type ownerSupervisorFake struct {
	runtimev1.UnimplementedSandboxSupervisorServiceServer
	jobKey   string
	request  []byte
	response []byte
	refuse   bool
	block    chan struct{}
}

func (f *ownerSupervisorFake) CodeOwnerRecoveryRead(ctx context.Context, request *runtimev1.CodeOwnerRecoveryReadRequestV1) (*runtimev1.CodeOwnerRecoveryReadResponseV1, error) {
	f.jobKey, f.request = request.GetJobKey(), request.GetRequestJson()
	if f.block != nil {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.refuse {
		return nil, grpcstatus.Error(codes.PermissionDenied, "owner grant refused")
	}
	return &runtimev1.CodeOwnerRecoveryReadResponseV1{ResponseJson: f.response}, nil
}

func startOwnerSupervisor(t *testing.T, pki *grpcauthtest.PKI, fake *ownerSupervisorFake) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(pki.ServerTLS(t))))
	runtimev1.RegisterSandboxSupervisorServiceServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return "https://" + listener.Addr().String()
}

func ownerCall(client *CodeOwnerClient, audience string, ctx context.Context) ([]byte, error) {
	endpoint := client.endpoints[audience]
	return boundedCodeOwnerCall(ctx, 64, time.Second, func(ctx context.Context) ([]byte, error) {
		response, err := endpoint.client.CodeOwnerRecoveryRead(ctx, &runtimev1.CodeOwnerRecoveryReadRequestV1{JobKey: "job", RequestJson: []byte(`{"grant":1}`)})
		return response.GetResponseJson(), err
	})
}

func TestCodeOwnerClientCallsSupervisorOverMTLS(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	fake := &ownerSupervisorFake{response: []byte(`{"state":"completed"}`)}
	origin := startOwnerSupervisor(t, pki, fake)
	requester := "dns:main.test"
	// The supervisor's certificate names grpcauthtest.ServerName.
	audience := "dns:" + grpcauthtest.ServerName
	client, err := NewCodeOwnerClient(requester, []CodeOwnerEndpoint{{Audience: audience, Origin: origin, TLS: pki.ClientTLS(t, "main.test")}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	raw, err := ownerCall(client, audience, t.Context())
	if err != nil || string(raw) != `{"state":"completed"}` || fake.jobKey != "job" || string(fake.request) != `{"grant":1}` {
		t.Fatalf("raw=%q err=%v job=%q request=%q", raw, err, fake.jobKey, fake.request)
	}

	// Any refusal from the supervisor is a refusal, never a transport detail.
	fake.refuse = true
	if _, err := ownerCall(client, audience, t.Context()); !errors.Is(err, code.ErrRejected) {
		t.Fatalf("refusal surfaced as %v", err)
	}
	fake.refuse = false

	// An oversized answer is refused.
	fake.response = bytes.Repeat([]byte{'x'}, 65)
	if _, err := ownerCall(client, audience, t.Context()); !errors.Is(err, code.ErrRejected) {
		t.Fatalf("oversized answer accepted: %v", err)
	}

	// The caller's cancellation is the caller's error.
	fake.block = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := ownerCall(client, audience, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation surfaced as %v", err)
	}
}

func TestCodeOwnerClientRefusesSupervisorWithAnotherIdentity(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	fake := &ownerSupervisorFake{response: []byte(`{}`)}
	origin := startOwnerSupervisor(t, pki, fake)
	client, err := NewCodeOwnerClient("dns:main.test", []CodeOwnerEndpoint{{Audience: "dns:supervisor-two.test", Origin: origin, TLS: pki.ClientTLS(t, "main.test")}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := ownerCall(client, "dns:supervisor-two.test", t.Context()); !errors.Is(err, code.ErrRejected) {
		t.Fatalf("another supervisor identity accepted: %v", err)
	}
	if fake.jobKey != "" {
		t.Fatal("request reached a supervisor with another identity")
	}
}

func TestCodeOwnerSupervisorRefusesMainWithoutCertificate(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	fake := &ownerSupervisorFake{response: []byte(`{}`)}
	origin := startOwnerSupervisor(t, pki, fake)
	conn, err := grpc.NewClient("passthrough:///"+strings.TrimPrefix(origin, "https://"), grpc.WithTransportCredentials(credentials.NewTLS(pki.AnonymousTLS())))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, err = runtimev1.NewSandboxSupervisorServiceClient(conn).CodeOwnerRecoveryRead(t.Context(), &runtimev1.CodeOwnerRecoveryReadRequestV1{JobKey: "job"})
	if grpcstatus.Code(err) != codes.Unavailable || fake.jobKey != "" {
		t.Fatalf("certificate-less caller reached the supervisor: %v", err)
	}
}
