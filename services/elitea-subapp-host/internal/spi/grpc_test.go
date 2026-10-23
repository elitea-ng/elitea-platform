package spi_test

// The provider service over its real transport (spitest: a TLS listener on
// loopback with a minted CA). What used to be two HTTP gates — mutual TLS,
// then identity — is now the listener's handshake and allowlist, then the
// signed identity metadata, and each refusal is checked here against a
// caller that should be refused for exactly that reason.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	subappv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/subapp/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/apps/echo"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi/spitest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const sharedSecret = "shared-with-the-provider"

func echoServer(t *testing.T, secret string, step time.Duration, runner spi.Runner) *spi.Server {
	t.Helper()
	pairs := map[string]string{}
	if secret != "" {
		pairs["ELITEA_ECHO_IDENTITY_SECRET"] = secret
	}
	s, err := spi.SettingsFromEnv("ELITEA_ECHO_", env(pairs))
	if err != nil {
		t.Fatal(err)
	}
	app := echo.App(step)
	if runner != nil {
		app.Runner = runner
	}
	server, err := spi.NewServer(s, app, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	return server
}

func signedContext(identity spi.Identity) context.Context {
	return metadata.NewOutgoingContext(context.Background(), spi.IdentityMetadata(identity, []byte(sharedSecret)))
}

func TestTheListenerAdmitsOnlyAnAllowedVerifiedClientCertificate(t *testing.T) {
	l := spitest.Start(t, echoServer(t, sharedSecret, 0, nil))
	ctx, cancel := context.WithTimeout(signedContext(spi.Identity{ProjectID: "1", UserID: "7"}), 10*time.Second)
	defer cancel()
	slots := func(conn *grpc.ClientConn) error {
		_, err := subappv1.NewProviderServiceClient(conn).Slots(ctx, &subappv1.SlotsRequest{})
		return err
	}
	if err := slots(l.Conn); err != nil {
		t.Fatalf("the allowed caller was refused: %v", err)
	}
	// No client certificate: the handshake fails.
	if err := slots(l.Dial(t, l.PKI.ClientTLS())); status.Code(err) != codes.Unavailable {
		t.Fatalf("an anonymous client: %v", err)
	}
	// A certificate the CA signed for someone the allowlist does not name.
	other := l.PKI.Leaf(t, "elitea-worker", x509.ExtKeyUsageClientAuth)
	if err := slots(l.Dial(t, l.PKI.ClientTLS(other))); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a client off the allowlist: %v", err)
	}
	// The allowed NAME from another CA: not verified, no handshake.
	foreign := spitest.NewPKI(t)
	impostor := foreign.Leaf(t, spitest.Caller, x509.ExtKeyUsageClientAuth)
	cfg := l.PKI.ClientTLS(impostor)
	if err := slots(l.Dial(t, cfg)); status.Code(err) != codes.Unavailable {
		t.Fatalf("a foreign CA's certificate: %v", err)
	}
}

func TestEveryCallButHealthNeedsAValidIdentitySignature(t *testing.T) {
	l := spitest.Start(t, echoServer(t, sharedSecret, 0, nil))
	client := l.Provider()
	bare := context.Background()
	if _, err := client.Slots(bare, &subappv1.SlotsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("an unsigned call: %v", err)
	}
	if _, err := client.Describe(bare, &subappv1.DescribeRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("an unsigned descriptor read: %v", err)
	}
	// A forged project id under a signature for another.
	md := spi.IdentityMetadata(spi.Identity{ProjectID: "1", UserID: "7"}, []byte(sharedSecret))
	md.Set("x-elitea-project-id", "999")
	if _, err := client.Slots(metadata.NewOutgoingContext(bare, md), &subappv1.SlotsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("a forged project id: %v", err)
	}
	// A key sent twice: the signature covers one value, so neither is believed.
	twice := spi.IdentityMetadata(spi.Identity{ProjectID: "1", UserID: "7"}, []byte(sharedSecret))
	twice.Append("x-elitea-project-id", "999")
	if _, err := client.Slots(metadata.NewOutgoingContext(bare, twice), &subappv1.SlotsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("a repeated identity key: %v", err)
	}
	// Health is the registrar's probe and needs only the certificate.
	health, err := client.Health(bare, &subappv1.HealthRequest{})
	if err != nil || health.GetStatus() != "UP" || !health.GetReady() || !health.GetChecks()["invocations"] {
		t.Fatalf("health: %+v %v", health, err)
	}
	var document map[string]any
	if err := json.Unmarshal(health.GetHealthJson(), &document); err != nil || document["plugin"] != echo.Name {
		t.Fatalf("health document: %s %v", health.GetHealthJson(), err)
	}
	if _, err := client.Slots(signedContext(spi.Identity{ProjectID: "1", UserID: "7"}), &subappv1.SlotsRequest{}); err != nil {
		t.Fatalf("a signed call: %v", err)
	}
}

type identityRunner struct{ got chan spi.Identity }

func (identityRunner) Name() string { return "identity" }
func (r identityRunner) Invoke(_ context.Context, call spi.Invoke, tc *spi.Context) (map[string]any, error) {
	r.got <- call.Identity
	return spi.Completed(tc.InvocationID()), nil
}

func invokeOnce(t *testing.T, client subappv1.ProviderServiceClient, ctx context.Context) *subappv1.InvokeEvent {
	t.Helper()
	stream, err := client.Invoke(ctx, &subappv1.InvokeRequest{Toolkit: "Echo", Tool: "echo", RequestJson: []byte(`{"parameters":{"query":"hi"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("an unfollowed invoke kept streaming: %v", err)
	}
	return accepted
}

// The runner receives the identity the signature vouches for — and with no
// shared secret, NOTHING: metadata nobody can verify is not believed.
func TestTheRunnerReceivesOnlyAVerifiedIdentity(t *testing.T) {
	runner := identityRunner{got: make(chan spi.Identity, 1)}
	l := spitest.Start(t, echoServer(t, sharedSecret, 0, runner))
	accepted := invokeOnce(t, l.Provider(), signedContext(spi.Identity{ProjectID: "42", UserID: "7", TenantID: "acme", ExecutionID: "e"}))
	if accepted.GetStatus() != "Started" || !strings.HasPrefix(accepted.GetInvocationId(), "invocation_") {
		t.Fatalf("accepted %+v", accepted)
	}
	var body map[string]any
	if err := json.Unmarshal(accepted.GetBodyJson(), &body); err != nil || body["invocation_id"] != accepted.GetInvocationId() || body["status"] != "Started" {
		t.Fatalf("the acceptance body %s", accepted.GetBodyJson())
	}
	select {
	case got := <-runner.got:
		if got != (spi.Identity{ProjectID: "42", UserID: "7", TenantID: "acme", ExecutionID: "e"}) {
			t.Fatalf("runner saw %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner was never invoked")
	}

	unverified := identityRunner{got: make(chan spi.Identity, 1)}
	dev := spitest.Start(t, echoServer(t, "", 0, unverified))
	md := metadata.Pairs("x-elitea-project-id", "999", "x-elitea-user-id", "1")
	invokeOnce(t, dev.Provider(), metadata.NewOutgoingContext(context.Background(), md))
	select {
	case got := <-unverified.got:
		if got != (spi.Identity{}) {
			t.Fatalf("an unverifiable identity reached the runner as %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the runner was never invoked")
	}
}

func TestAnInvokeThatIsNotAJSONObjectIsInvalid(t *testing.T) {
	l := spitest.Start(t, echoServer(t, "", 0, nil))
	for _, raw := range []string{``, `[]`, `null`, `{`} {
		stream, err := l.Provider().Invoke(context.Background(), &subappv1.InvokeRequest{Toolkit: "Echo", Tool: "echo", RequestJson: []byte(raw)})
		if err == nil {
			_, err = stream.Recv()
		}
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q: %v", raw, err)
		}
	}
}

// A followed invoke streams the progress and ends with the terminal body; a
// Cancel ends a followed run with the cancellation the poll used to report;
// and a caller dropping an unfollowed stream does NOT stop the invocation.
func TestAFollowedInvokeStreamsToItsEndAndCancelStopsIt(t *testing.T) {
	l := spitest.Start(t, echoServer(t, "", 20*time.Millisecond, nil))
	client := l.Provider()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	stream, err := client.Invoke(ctx, &subappv1.InvokeRequest{Toolkit: "Echo", Tool: "echo", RequestJson: []byte(`{"parameters":{"query":"hi"}}`), Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	var last *subappv1.InvokeEvent
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		last = event
		var body map[string]any
		_ = json.Unmarshal(event.GetBodyJson(), &body)
		events, _ := body["custom_events"].([]any)
		for _, e := range events {
			messages = append(messages, e.(map[string]any)["data"].(map[string]any)["message"].(string))
		}
	}
	if last == nil || !last.GetTerminal() || last.GetStatus() != "Completed" {
		t.Fatalf("the stream did not end with the terminal body: %+v", last)
	}
	if strings.Join(messages, "|") != "Received echo|Working|Done" {
		t.Fatalf("progress %v", messages)
	}

	// Cancel a followed run mid-way.
	slow := spitest.Start(t, echoServer(t, "", 200*time.Millisecond, spi.EchoRunner{Step: 200 * time.Millisecond, Steps: []string{"a", "b", "c", "d", "e", "f"}}))
	stream, err = slow.Provider().Invoke(ctx, &subappv1.InvokeRequest{Toolkit: "Echo", Tool: "echo", RequestJson: []byte(`{}`), Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slow.Provider().Cancel(ctx, &subappv1.CancelRequest{Toolkit: "Echo", Tool: "echo", InvocationId: accepted.GetInvocationId()}); err != nil {
		t.Fatal(err)
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("the cancelled stream ended without a terminal body: %v", err)
		}
		if event.GetTerminal() {
			if event.GetStatus() != "Error" || !strings.Contains(string(event.GetBodyJson()), "Invocation cancelled") {
				t.Fatalf("a cancelled run ended as %s", event.GetBodyJson())
			}
			break
		}
	}
	if _, err := slow.Provider().Cancel(ctx, &subappv1.CancelRequest{Toolkit: "Echo", Tool: "echo", InvocationId: "invocation_nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("cancelling an unknown invocation: %v", err)
	}
	if _, err := slow.Provider().GetInvocation(ctx, &subappv1.GetInvocationRequest{Toolkit: "Echo", Tool: "echo", InvocationId: "invocation_nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("reading an unknown invocation: %v", err)
	}

	// Drop a FOLLOWED stream mid-run: the invocation goes on to complete.
	dropCtx, drop := context.WithCancel(ctx)
	stream, err = client.Invoke(dropCtx, &subappv1.InvokeRequest{Toolkit: "Echo", Tool: "echo", RequestJson: []byte(`{}`), Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err = stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	drop()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		read, err := client.GetInvocation(ctx, &subappv1.GetInvocationRequest{Toolkit: "Echo", Tool: "echo", InvocationId: accepted.GetInvocationId()})
		if err != nil {
			t.Fatal(err)
		}
		if read.GetTerminal() {
			if !strings.Contains(string(read.GetBodyJson()), `"Completed"`) {
				t.Fatalf("a dropped stream stopped its invocation: %s", read.GetBodyJson())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the invocation never ended")
}

func TestTheListenerRefusesToBuildWithoutAllowlistOrMutualTLS(t *testing.T) {
	server := echoServer(t, "", 0, nil)
	p := spitest.NewPKI(t)
	if _, err := spi.NewGRPCServer(server, nil, p.ServerTLS(), nil); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("no clients: %v", err)
	}
	for _, cfg := range []*tls.Config{nil, {Certificates: p.ServerTLS().Certificates}, {Certificates: p.ServerTLS().Certificates, ClientAuth: tls.RequireAnyClientCert, ClientCAs: p.Roots}} {
		if _, err := spi.NewGRPCServer(server, []string{"elitea-main"}, cfg, nil); !errors.Is(err, spi.ErrConfig) {
			t.Fatalf("a configuration that does not verify client certificates: %v", err)
		}
	}
	if _, err := spi.NewGRPCServer(nil, []string{"elitea-main"}, p.ServerTLS(), nil); !errors.Is(err, spi.ErrConfig) {
		t.Fatalf("no host: %v", err)
	}
}

// claimLostStore refuses every new invocation the way PostgresStore does
// while it does not hold its owner claim.
type claimLostStore struct{ *spi.MemoryStore }

func (claimLostStore) Create(context.Context, *spi.Invocation) error { return spi.ErrClaimLost }

// A replica without its owner claim accepted nothing, so the refusal is
// Unavailable — a code the caller may retry on another replica — and not
// Internal.
func TestAnInvokeRefusedForALostOwnerClaimIsUnavailable(t *testing.T) {
	s, err := spi.SettingsFromEnv("ELITEA_ECHO_", env(map[string]string{"ELITEA_ECHO_IDENTITY_SECRET": sharedSecret}))
	if err != nil {
		t.Fatal(err)
	}
	server, err := spi.NewServer(s, echo.App(0), nil, spi.WithStore(claimLostStore{spi.NewMemoryStore()}))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	l := spitest.Start(t, server)
	ctx, cancel := context.WithTimeout(signedContext(spi.Identity{ProjectID: "1", UserID: "7"}), 10*time.Second)
	defer cancel()
	stream, err := l.Provider().Invoke(ctx, &subappv1.InvokeRequest{Toolkit: "Echo", Tool: "echo", RequestJson: []byte(`{"parameters":{"query":"hi"}}`)})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("an invoke on a replica without its claim: %v, want Unavailable", err)
	}
}
