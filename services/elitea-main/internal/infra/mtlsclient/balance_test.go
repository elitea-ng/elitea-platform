package mtlsclient_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/mtlsclient"
)

// countingHealth answers every Check and counts it.
type countingHealth struct {
	*health.Server
	calls atomic.Int64
}

func (c *countingHealth) Check(ctx context.Context, in *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	c.calls.Add(1)
	return c.Server.Check(ctx, in)
}

func backend(t *testing.T) (string, *countingHealth) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	counter := &countingHealth{Server: health.NewServer()}
	healthpb.RegisterHealthServer(server, counter)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	return lis.Addr().String(), counter
}

// TestBalancedSpreadsCallsOverEveryResolvedAddress is the property the hops
// rely on: one client connection, a resolver that returns two replicas (what
// DNS returns for a headless Service), and calls that reach BOTH. Without the
// option (the control) pick_first sends every call to one replica.
func TestBalancedSpreadsCallsOverEveryResolvedAddress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		options  []grpc.DialOption
		balanced bool
	}{
		{name: "balanced", options: []grpc.DialOption{mtlsclient.Balanced()}, balanced: true},
		{name: "pick_first control", balanced: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addrA, a := backend(t)
			addrB, b := backend(t)
			r := manual.NewBuilderWithScheme("headless")
			r.InitialState(resolver.State{Addresses: []resolver.Address{{Addr: addrA}, {Addr: addrB}}})
			options := append([]grpc.DialOption{
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithResolvers(r),
			}, tc.options...)
			conn, err := grpc.NewClient("headless:///elitea-inventory-svc:9443", options...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			client := healthpb.NewHealthClient(conn)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for range 20 {
				if _, err := client.Check(ctx, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(true)); err != nil {
					t.Fatal(err)
				}
			}
			gotA, gotB := a.calls.Load(), b.calls.Load()
			if gotA+gotB != 20 {
				t.Fatalf("calls = %d + %d, want 20", gotA, gotB)
			}
			if tc.balanced && (gotA == 0 || gotB == 0) {
				t.Fatalf("round_robin left a replica idle: %d and %d calls", gotA, gotB)
			}
			if !tc.balanced && gotA != 0 && gotB != 0 {
				t.Fatalf("the control spread the calls (%d and %d): the test does not tell the option apart", gotA, gotB)
			}
		})
	}
}
