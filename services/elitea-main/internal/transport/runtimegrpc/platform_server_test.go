package runtimegrpc

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth/grpcauthtest"
)

// egressStub stands in for a later unit's platform service: any generated
// service works, this one is small.
type egressStub struct {
	runtimev1.UnimplementedEgressAuditServiceServer
}

func (egressStub) ReportRunSummary(ctx context.Context, _ *runtimev1.ReportRunSummaryRequestV1) (*runtimev1.ReportRunSummaryResponseV1, error) {
	if _, ok := grpcauth.PeerIdentity(ctx); !ok {
		return nil, status.Error(codes.Internal, "the interceptor did not run")
	}
	return &runtimev1.ReportRunSummaryResponseV1{}, nil
}

func startPlatformServer(t *testing.T, pki *grpcauthtest.PKI, services ...PlatformService) (string, *PlatformServer) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewPlatformServer(PlatformServerConfig{
		Address: listener.Addr().String(),
		TLS:     pki.ServerTLS(t),
		Policy:  DefaultPlatformGRPCPolicy(),
	}, services...)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("platform listener: %v", err)
		}
	})
	return listener.Addr().String(), server
}

func platformClient(t *testing.T, address string, clientTLS *tls.Config) *grpc.ClientConn {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func TestPlatformServerAuthorizesPerService(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	worker, err := grpcauth.NewAllowlist([]string{"dns:elitea-worker"})
	if err != nil {
		t.Fatal(err)
	}
	address, _ := startPlatformServer(t, pki, PlatformService{
		Name:    runtimev1.EgressAuditService_ServiceDesc.ServiceName,
		Clients: worker,
		Register: func(registrar grpc.ServiceRegistrar) {
			runtimev1.RegisterEgressAuditServiceServer(registrar, egressStub{})
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request := &runtimev1.ReportRunSummaryRequestV1{ReplicaId: "r"}

	allowed := platformClient(t, address, pki.ClientTLS(t, "elitea-worker"))
	if _, err := runtimev1.NewEgressAuditServiceClient(allowed).ReportRunSummary(ctx, request); err != nil {
		t.Fatalf("the allowed identity was refused: %v", err)
	}

	other := platformClient(t, address, pki.ClientTLS(t, "elitea-subapp-host"))
	if _, err := runtimev1.NewEgressAuditServiceClient(other).ReportRunSummary(ctx, request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("another runtime-CA identity = %v, want PermissionDenied", err)
	}

	t.Run("health is served to any verified peer, per service too", func(t *testing.T) {
		for _, service := range []string{"", runtimev1.EgressAuditService_ServiceDesc.ServiceName} {
			response, err := healthv1.NewHealthClient(other).Check(ctx, &healthv1.HealthCheckRequest{Service: service})
			if err != nil || response.GetStatus() != healthv1.HealthCheckResponse_SERVING {
				t.Fatalf("Check(%q) = %v, %v", service, response.GetStatus(), err)
			}
		}
	})

	t.Run("reflection is not served", func(t *testing.T) {
		stream, err := reflectionv1.NewServerReflectionClient(allowed).ServerReflectionInfo(ctx)
		if err == nil {
			_ = stream.Send(&reflectionv1.ServerReflectionRequest{MessageRequest: &reflectionv1.ServerReflectionRequest_ListServices{}})
			_, err = stream.Recv()
		}
		if code := status.Code(err); code != codes.PermissionDenied && code != codes.Unimplemented {
			t.Fatalf("reflection = %v, want it absent", err)
		}
	})

	t.Run("no client certificate fails the handshake", func(t *testing.T) {
		_, err := healthv1.NewHealthClient(platformClient(t, address, pki.AnonymousTLS())).Check(ctx, &healthv1.HealthCheckRequest{})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("Check without a certificate = %v, want Unavailable", err)
		}
	})

	t.Run("a certificate from another CA fails the handshake", func(t *testing.T) {
		foreign := grpcauthtest.NewPKI(t).ClientTLS(t, "elitea-worker")
		foreign.RootCAs = pki.Roots()
		_, err := healthv1.NewHealthClient(platformClient(t, address, foreign)).Check(ctx, &healthv1.HealthCheckRequest{})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("Check with a foreign certificate = %v, want Unavailable", err)
		}
	})

	t.Run("plain HTTP/1.1 is not served", func(t *testing.T) {
		// What deploy/scripts/standalone-stack.sh checks with openssl: an
		// http/1.1 client with the worker's own certificate completes the
		// handshake with NO protocol negotiated (crypto/tls lets an http/1.1
		// offer through without ALPN), and the gRPC server then closes the
		// connection without a byte of HTTP.
		clientTLS := pki.ClientTLS(t, "elitea-worker")
		clientTLS.NextProtos = []string{"http/1.1"}
		connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, clientTLS)
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		defer func() { _ = connection.Close() }()
		if protocol := connection.ConnectionState().NegotiatedProtocol; protocol != "" {
			t.Fatalf("negotiated %q with an http/1.1 client", protocol)
		}
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(connection, "GET /api/v2/probe HTTP/1.1\r\nHost: elitea-main\r\nConnection: close\r\n\r\n"); err != nil {
			return // closed already: nothing was served
		}
		answer, _ := io.ReadAll(connection)
		if len(answer) != 0 {
			t.Fatalf("the platform listener answered HTTP/1.1: %q", answer)
		}
	})
}

func TestPlatformServerShutdownReportsNotServing(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	address, server := startPlatformServer(t, pki)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := healthv1.NewHealthClient(platformClient(t, address, pki.ClientTLS(t, "elitea-worker")))
	watch, err := client.Watch(ctx, &healthv1.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if first, err := watch.Recv(); err != nil || first.GetStatus() != healthv1.HealthCheckResponse_SERVING {
		t.Fatalf("first Watch = %v, %v", first.GetStatus(), err)
	}
	go func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	next, err := watch.Recv()
	if err == nil && next.GetStatus() != healthv1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("after Shutdown Watch = %v, want NOT_SERVING", next.GetStatus())
	}
}

func TestPlatformServerConfiguration(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	base := PlatformServerConfig{Address: "127.0.0.1:0", TLS: pki.ServerTLS(t), Policy: DefaultPlatformGRPCPolicy()}
	worker, _ := grpcauth.NewAllowlist([]string{"dns:elitea-worker"})
	register := func(registrar grpc.ServiceRegistrar) {
		runtimev1.RegisterEgressAuditServiceServer(registrar, egressStub{})
	}
	name := runtimev1.EgressAuditService_ServiceDesc.ServiceName

	httpProfile := base
	httpProfile.TLS = base.TLS.Clone()
	httpProfile.TLS.NextProtos = []string{"h2", "http/1.1"}
	optional := base
	optional.TLS = base.TLS.Clone()
	optional.TLS.ClientAuth = tls.VerifyClientCertIfGiven

	for label, testCase := range map[string]struct {
		config   PlatformServerConfig
		services []PlatformService
	}{
		"no address":                   {config: PlatformServerConfig{TLS: base.TLS, Policy: base.Policy}},
		"http/1.1 ALPN":                {config: httpProfile},
		"optional client certificates": {config: optional},
		"no allowlist":                 {config: base, services: []PlatformService{{Name: name, Register: register}}},
		"registered twice":             {config: base, services: []PlatformService{{Name: name, Clients: worker, Register: register}, {Name: name, Clients: worker, Register: register}}},
		"registration registers another service": {config: base, services: []PlatformService{{
			Name: "elitea.llm.v1.ModelService", Clients: worker, Register: register,
		}}},
		"health named by hand": {config: base, services: []PlatformService{{
			Name: healthv1.Health_ServiceDesc.ServiceName, Clients: worker, Register: func(grpc.ServiceRegistrar) {},
		}}},
	} {
		if _, err := NewPlatformServer(testCase.config, testCase.services...); err == nil {
			t.Errorf("%s: the platform listener was built", label)
		}
	}
}
