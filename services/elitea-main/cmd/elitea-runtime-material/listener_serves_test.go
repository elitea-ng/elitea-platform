package main

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"testing"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/runtimegrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// TestRuntimeListenersServeFromASecretVolume is the end-to-end proof.
//
// It starts from the layout that a Kubernetes Secret volume produces, installs
// the material the way the init container does, loads the three listener
// profiles through the same code path that the composition root uses, and then
// makes a REAL call on each listener.
//
// A refusal is not a pass here. Each listener must answer with a value that
// only its own handler can produce, because a handshake failure and an
// unrouted request both look like "the port is open" from the outside.
func TestRuntimeListenersServeFromASecretVolume(t *testing.T) {
	fixture := newMaterialFixture(t)
	pod := newDeployment(t, fixture, secretVolumeMode)

	addresses := freeLoopbackAddresses(t, 3)
	pod.environment["ELITEA_RUNTIME_CONTROL_ADDRESS"] = addresses[0]
	pod.environment["ELITEA_RUNTIME_OUTPUT_ADDRESS"] = addresses[1]
	pod.environment["ELITEA_RUNTIME_CONTENT_ADDRESS"] = addresses[2]

	if _, err := install(pod.source, pod.lookup); err != nil {
		t.Fatalf("the init container could not install the material: %v", err)
	}

	config, err := runtimecomposition.ConfigFromEnv(pod.lookup)
	if err != nil {
		t.Fatal(err)
	}
	controlTLS, err := runtimegrpc.LoadServerTLSConfig(config.ControlTLS)
	if err != nil {
		t.Fatalf("load control listener TLS: %v", err)
	}
	outputTLS, err := runtimegrpc.LoadServerTLSConfig(config.OutputTLS)
	if err != nil {
		t.Fatalf("load output listener TLS: %v", err)
	}
	contentTLS, err := runtimegrpc.LoadServerTLSConfig(config.ContentTLS)
	if err != nil {
		t.Fatalf("load content listener TLS: %v", err)
	}

	servers, err := runtimegrpc.NewPrivateServerSet(runtimegrpc.PrivateServerConfig{
		ControlAddress:          config.ControlAddress,
		OutputAddress:           config.OutputAddress,
		ContentAddress:          config.ContentAddress,
		ControlTLS:              controlTLS,
		OutputTLS:               outputTLS,
		ContentTLS:              contentTLS,
		ControlMaxRequestBytes:  1 << 20,
		ControlMaxResponseBytes: 1 << 20,
		OutputMaxRequestBytes:   1 << 20,
		OutputMaxResponseBytes:  1 << 20,
		ControlGRPC:             testGRPCPolicy(),
		OutputGRPC:              testGRPCPolicy(),
		ContentMaxConnections:   16,
		ContentMaxStreams:       16,
		ContentReadTimeout:      5 * time.Second,
		ContentWriteTimeout:     5 * time.Second,
		ContentIdleTimeout:      5 * time.Second,
		ContentMaxHeaderBytes:   1 << 16,
		ShutdownTimeout:         5 * time.Second,
	}, runtimegrpc.PrivateServices{
		Control: stubControlService{},
		Output:  stubOutputService{},
		Content: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(writer, "content listener served "+request.URL.Path)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- servers.Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case <-served:
		case <-time.After(15 * time.Second):
			t.Error("the listeners did not stop")
		}
	}()

	callControl(t, config.ControlAddress, fixture.clientTLSFor(t, "control.runtime.test"))
	callOutput(t, config.OutputAddress, fixture.clientTLSFor(t, "output.runtime.test"))
	callContent(t, config.ContentAddress, fixture.clientTLSFor(t, "content.runtime.test"))

	// The same listener, and a client certificate from another authority. The
	// listeners serve, and they serve only what the runtime CA signed.
	stranger := newMaterialFixture(t)
	strangerTLS := stranger.clientTLSFor(t, "control.runtime.test")
	strangerTLS.RootCAs = fixture.rootPool()
	refuseControl(t, config.ControlAddress, strangerTLS)
}

func testGRPCPolicy() runtimegrpc.GRPCServerPolicy {
	return runtimegrpc.GRPCServerPolicy{
		MaxConcurrentStreams:  16,
		MaxConnections:        16,
		MinClientPingInterval: 10 * time.Second,
		KeepaliveTime:         30 * time.Second,
		KeepaliveTimeout:      10 * time.Second,
		MaxConnectionIdle:     60 * time.Second,
		MaxConnectionAge:      300 * time.Second,
		MaxConnectionAgeGrace: 30 * time.Second,
	}
}

func callControl(t *testing.T, address string, clientTLS *tls.Config) {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	response, err := runtimev1.NewRuntimeControlServiceClient(connection).ClaimCommand(ctx,
		&runtimev1.ClaimCommandRequestV1{WorkloadSessionId: "workload-session-under-test"})
	if err != nil {
		t.Fatalf("the control listener did not serve the call: %v", err)
	}
	if response.GetReceipt().GetClaimId() != "claim-for-workload-session-under-test" {
		t.Fatalf("the control response did not come from the handler: %v", response)
	}
}

func callOutput(t *testing.T, address string, clientTLS *tls.Config) {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := runtimev1.NewExecutionOutputServiceClient(connection).Publish(ctx)
	if err != nil {
		t.Fatalf("the output listener did not open the stream: %v", err)
	}
	if err := stream.Send(&runtimev1.ExecutionOutputFrameV1{StreamId: "output-stream-under-test"}); err != nil {
		t.Fatalf("the output listener did not accept a frame: %v", err)
	}
	acknowledgement, err := stream.Recv()
	if err != nil {
		t.Fatalf("the output listener did not answer the frame: %v", err)
	}
	if acknowledgement.GetStreamId() != "output-stream-under-test" {
		t.Fatalf("the output response did not come from the handler: %v", acknowledgement)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
}

func callContent(t *testing.T, address string, clientTLS *tls.Config) {
	t.Helper()
	transport := &http.Transport{TLSClientConfig: clientTLS, Protocols: new(http.Protocols)}
	transport.Protocols.SetHTTP2(true)
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	defer client.CloseIdleConnections()

	response, err := client.Get("https://" + address + "/execution-content")
	if err != nil {
		t.Fatalf("the content listener did not serve the request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the content listener answered %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "content listener served /execution-content" {
		t.Fatalf("the content response did not come from the handler: %q", body)
	}
}

func refuseControl(t *testing.T, address string, clientTLS *tls.Config) {
	t.Helper()
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	response, err := runtimev1.NewRuntimeControlServiceClient(connection).ClaimCommand(ctx,
		&runtimev1.ClaimCommandRequestV1{WorkloadSessionId: "workload-session-under-test"})
	if err == nil {
		t.Fatalf("the control listener served a client that the runtime CA did not sign: %v", response)
	}
}

type stubControlService struct {
	runtimev1.UnimplementedRuntimeControlServiceServer
}

func (stubControlService) ClaimCommand(_ context.Context, request *runtimev1.ClaimCommandRequestV1) (*runtimev1.ClaimCommandResponseV1, error) {
	return &runtimev1.ClaimCommandResponseV1{
		Receipt: &runtimev1.ClaimReceiptV1{ClaimId: "claim-for-" + request.GetWorkloadSessionId()},
	}, nil
}

type stubOutputService struct {
	runtimev1.UnimplementedExecutionOutputServiceServer
}

func (stubOutputService) Publish(stream grpc.BidiStreamingServer[runtimev1.ExecutionOutputFrameV1, runtimev1.ExecutionOutputAckV1]) error {
	for {
		frame, err := stream.Recv()
		if err != nil {
			return nil
		}
		if err := stream.Send(&runtimev1.ExecutionOutputAckV1{StreamId: frame.GetStreamId()}); err != nil {
			return err
		}
	}
}
