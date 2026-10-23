package grpcauth_test

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
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth/grpcauthtest"
)

// A hand-written service so the test does not depend on any product proto:
// Who answers the caller's admitted identity; Count streams until cancelled.
const (
	allowedService  = "elitea.test.v1.Allowed"
	unlistedService = "elitea.test.v1.Unlisted"
)

type echo struct{ cancelled chan error }

func serviceDesc(name string) *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: name,
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Who",
			Handler: func(server any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				request := &wrapperspb.StringValue{}
				if err := decode(request); err != nil {
					return nil, err
				}
				handler := func(ctx context.Context, _ any) (any, error) {
					identity, _ := grpcauth.PeerIdentity(ctx)
					return wrapperspb.String(identity), nil
				}
				if interceptor == nil {
					return handler(ctx, request)
				}
				return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: server, FullMethod: "/" + name + "/Who"}, handler)
			},
		}},
		Streams: []grpc.StreamDesc{{
			StreamName:    "Count",
			ServerStreams: true,
			Handler: func(server any, stream grpc.ServerStream) error {
				request := &wrapperspb.StringValue{}
				if err := stream.RecvMsg(request); err != nil {
					return err
				}
				identity, _ := grpcauth.PeerIdentity(stream.Context())
				for n := 0; ; n++ {
					if err := stream.SendMsg(wrapperspb.String(identity)); err != nil {
						return err
					}
					select {
					case <-stream.Context().Done():
						server.(*echo).cancelled <- stream.Context().Err()
						return stream.Context().Err()
					case <-time.After(10 * time.Millisecond):
					}
				}
			},
		}},
	}
}

func startServer(t *testing.T, serverTLS *tls.Config, policy *grpcauth.Policy) (string, *echo) {
	t.Helper()
	implementation := &echo{cancelled: make(chan error, 1)}
	server := grpc.NewServer(append([]grpc.ServerOption{grpc.Creds(credentials.NewTLS(serverTLS))}, policy.ServerOptions()...)...)
	server.RegisterService(serviceDesc(allowedService), implementation)
	server.RegisterService(serviceDesc(unlistedService), implementation)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String(), implementation
}

func dial(t *testing.T, address string, clientTLS *tls.Config) *grpc.ClientConn {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func who(ctx context.Context, connection *grpc.ClientConn, service string) (string, error) {
	response := &wrapperspb.StringValue{}
	err := connection.Invoke(ctx, "/"+service+"/Who", wrapperspb.String("?"), response)
	return response.GetValue(), err
}

func testPolicy(t *testing.T) *grpcauth.Policy {
	t.Helper()
	allowed, err := grpcauth.NewAllowlist([]string{"dns:elitea-worker", " spiffe://elitea.test/ns/elitea/sa/engine "})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := grpcauth.NewPolicy(map[string]grpcauth.Allowlist{allowedService: allowed})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestPolicyOverRealTLS(t *testing.T) {
	pki := grpcauthtest.NewPKI(t)
	address, implementation := startServer(t, pki.ServerTLS(t), testPolicy(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	worker := dial(t, address, pki.ClientTLS(t, "elitea-worker"))
	t.Run("an allowed identity is served and sees its own identity", func(t *testing.T) {
		identity, err := who(ctx, worker, allowedService)
		if err != nil || identity != "dns:elitea-worker" {
			t.Fatalf("Who = %q, %v", identity, err)
		}
	})

	t.Run("a runtime-CA identity not on the list is PermissionDenied", func(t *testing.T) {
		stranger := dial(t, address, pki.ClientTLS(t, "elitea-subapp-host"))
		if _, err := who(ctx, stranger, allowedService); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("Who = %v, want PermissionDenied", err)
		}
		stream, err := stranger.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/"+allowedService+"/Count")
		if err == nil {
			_ = stream.SendMsg(wrapperspb.String("?"))
			_ = stream.CloseSend()
			err = stream.RecvMsg(&wrapperspb.StringValue{})
		}
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("Count = %v, want PermissionDenied", err)
		}
	})

	t.Run("a service the policy does not name is refused even to an allowed identity", func(t *testing.T) {
		if _, err := who(ctx, worker, unlistedService); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("Who on the unlisted service = %v, want PermissionDenied", err)
		}
	})

	t.Run("no client certificate fails the handshake", func(t *testing.T) {
		anonymous := dial(t, address, pki.AnonymousTLS())
		_, err := who(ctx, anonymous, allowedService)
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("Who without a certificate = %v, want a transport failure (Unavailable)", err)
		}
	})

	t.Run("a certificate from another CA fails the handshake", func(t *testing.T) {
		foreign := grpcauthtest.NewPKI(t).ClientTLS(t, "elitea-worker")
		foreign.RootCAs = pki.Roots()
		_, err := who(ctx, dial(t, address, foreign), allowedService)
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("Who with a foreign certificate = %v, want a transport failure (Unavailable)", err)
		}
	})

	t.Run("an allowed stream is served and cancellation reaches the handler", func(t *testing.T) {
		streamCtx, streamCancel := context.WithCancel(ctx)
		stream, err := worker.NewStream(streamCtx, &grpc.StreamDesc{ServerStreams: true}, "/"+allowedService+"/Count")
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.SendMsg(wrapperspb.String("?")); err != nil {
			t.Fatal(err)
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			message := &wrapperspb.StringValue{}
			if err := stream.RecvMsg(message); err != nil || message.GetValue() != "dns:elitea-worker" {
				t.Fatalf("RecvMsg = %q, %v", message.GetValue(), err)
			}
		}
		streamCancel()
		select {
		case err := <-implementation.cancelled:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("handler context ended with %v, want Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the client's cancellation never reached the stream handler")
		}
		if err := stream.RecvMsg(&wrapperspb.StringValue{}); err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("RecvMsg after cancel = %v, want a cancellation", err)
		}
	})
}

func TestAllowlistConfiguration(t *testing.T) {
	for name, clients := range map[string][]string{
		"empty":          nil,
		"only blanks":    {" ", ""},
		"bare name":      {"elitea-worker"},
		"empty dns":      {"dns:"},
		"spiffe no path": {"spiffe://elitea.test"},
		"with a space":   {"dns:elitea worker"},
	} {
		if _, err := grpcauth.NewAllowlist(clients); err == nil {
			t.Errorf("%s: %q was accepted", name, clients)
		}
	}
	if _, err := grpcauth.NewPolicy(map[string]grpcauth.Allowlist{allowedService: {}}); err == nil {
		t.Error("a policy with an empty allowlist was accepted")
	}
	if _, err := grpcauth.NewPolicy(nil); err == nil {
		t.Error("an empty policy was accepted")
	}
	if !grpcauth.AnyVerifiedPeer().Allows("dns:anything") || grpcauth.AnyVerifiedPeer().Allows("") {
		t.Error("AnyVerifiedPeer admits the wrong identities")
	}
	if (grpcauth.Allowlist{}).Allows("dns:elitea-worker") {
		t.Error("the zero allowlist admitted someone")
	}
}

func TestAllowlistAuthorizeWithoutAPeer(t *testing.T) {
	allowed, err := grpcauth.NewAllowlist([]string{"dns:elitea-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allowed.Authorize(context.Background()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Authorize without a peer = %v, want Unauthenticated", err)
	}
}
