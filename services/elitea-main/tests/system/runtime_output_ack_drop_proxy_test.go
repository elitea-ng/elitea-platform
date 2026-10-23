package system_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	outputACKDropPassThrough int32 = iota
	outputACKDropArmed
	outputACKDropHolding
	outputACKDropComplete
)

// outputACKDropProxy is a harness-only mTLS proxy for the production output
// stream. It preserves the worker identity certificate and incoming metadata
// while providing one deterministic commit-before-ACK-loss fault point.
//
// Once armed, the first ACK with a positive committed sequence is received
// from Main but not delivered to the worker. The worker-side stream closes with
// Unavailable, and every later stream passes through without fault injection.
type outputACKDropProxy struct {
	runtimev1.UnimplementedExecutionOutputServiceServer

	address    string
	server     *grpc.Server
	upstream   *grpc.ClientConn
	dropState  atomic.Int32
	droppedACK chan struct{}
}

func startOutputACKDropProxy(t *testing.T, upstreamAddress string, pki runtimePKI) *outputACKDropProxy {
	t.Helper()

	serverTLS := loadOutputProxyServerTLS(t, pki)
	clientTLS := loadOutputProxyClientTLS(t, pki)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for output ACK-drop proxy: %v", err)
	}

	upstream, err := grpc.NewClient(
		upstreamAddress,
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)),
	)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("configure output ACK-drop upstream: %v", err)
	}

	proxy := &outputACKDropProxy{
		address:    listener.Addr().String(),
		server:     grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS))),
		upstream:   upstream,
		droppedACK: make(chan struct{}),
	}
	runtimev1.RegisterExecutionOutputServiceServer(proxy.server, proxy)

	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- proxy.server.Serve(listener)
	}()
	t.Cleanup(func() {
		proxy.server.Stop()
		_ = proxy.upstream.Close()
		select {
		case err := <-serveErrors:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("serve output ACK-drop proxy: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("output ACK-drop proxy did not stop")
		}
	})

	return proxy
}

func (p *outputACKDropProxy) armCommittedACKDrop(t *testing.T) <-chan struct{} {
	t.Helper()
	if !p.dropState.CompareAndSwap(outputACKDropPassThrough, outputACKDropArmed) {
		t.Fatalf("output ACK-drop proxy cannot be armed from state %d", p.dropState.Load())
	}
	return p.droppedACK
}

func (p *outputACKDropProxy) releaseCommittedACKDrop(t *testing.T) {
	t.Helper()
	if !p.dropState.CompareAndSwap(outputACKDropHolding, outputACKDropComplete) {
		t.Fatalf("output ACK-drop proxy cannot be released from state %d", p.dropState.Load())
	}
}

func (p *outputACKDropProxy) Publish(stream grpc.BidiStreamingServer[runtimev1.ExecutionOutputFrameV1, runtimev1.ExecutionOutputAckV1]) error {
	if p.dropState.Load() == outputACKDropHolding {
		return status.Error(codes.Unavailable, "committed output ACK-loss window is held by the harness")
	}
	upstreamContext := stream.Context()
	if incoming, ok := metadata.FromIncomingContext(upstreamContext); ok {
		upstreamContext = metadata.NewOutgoingContext(upstreamContext, incoming.Copy())
	}
	upstream, err := runtimev1.NewExecutionOutputServiceClient(p.upstream).Publish(upstreamContext)
	if err != nil {
		return status.Errorf(codes.Unavailable, "open upstream output stream: %v", err)
	}

	initialCredit, err := upstream.Recv()
	if err != nil {
		return status.Errorf(codes.Unavailable, "receive upstream output credit: %v", err)
	}
	if err := stream.Send(initialCredit); err != nil {
		return err
	}

	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			_ = upstream.CloseSend()
			return nil
		}
		if err != nil {
			return err
		}
		if err := upstream.Send(frame); err != nil {
			return status.Errorf(codes.Unavailable, "forward output frame upstream: %v", err)
		}
		ack, err := upstream.Recv()
		if err != nil {
			return status.Errorf(codes.Unavailable, "receive upstream output ACK: %v", err)
		}
		if ack.GetCommittedContiguousSequence() > 0 &&
			p.dropState.CompareAndSwap(outputACKDropArmed, outputACKDropHolding) {
			close(p.droppedACK)
			_ = upstream.CloseSend()
			return status.Error(codes.Unavailable, "injected committed output ACK loss")
		}
		if err := stream.Send(ack); err != nil {
			return err
		}
	}
}

func loadOutputProxyServerTLS(t *testing.T, pki runtimePKI) *tls.Config {
	t.Helper()
	certificate, roots := loadOutputProxyTLSMaterial(t, pki.outputCertPath, pki.outputKeyPath, pki.caPath)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    roots,
	}
}

func loadOutputProxyClientTLS(t *testing.T, pki runtimePKI) *tls.Config {
	t.Helper()
	certificate, roots := loadOutputProxyTLSMaterial(t, pki.workerCertPath, pki.workerKeyPath, pki.caPath)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ServerName:   "localhost",
		Certificates: []tls.Certificate{certificate},
		RootCAs:      roots,
	}
}

func loadOutputProxyTLSMaterial(t *testing.T, certPath, keyPath, caPath string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load output ACK-drop certificate: %v", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read output ACK-drop CA: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("parse output ACK-drop CA")
	}
	return certificate, roots
}

type outputACKDropUpstream struct {
	runtimev1.UnimplementedExecutionOutputServiceServer

	metadata chan metadata.MD
	peers    chan string
}

func (s *outputACKDropUpstream) Publish(stream grpc.BidiStreamingServer[runtimev1.ExecutionOutputFrameV1, runtimev1.ExecutionOutputAckV1]) error {
	incoming, _ := metadata.FromIncomingContext(stream.Context())
	s.metadata <- incoming.Copy()
	if remotePeer, ok := peer.FromContext(stream.Context()); ok {
		if tlsInfo, ok := remotePeer.AuthInfo.(credentials.TLSInfo); ok &&
			len(tlsInfo.State.PeerCertificates) == 1 &&
			len(tlsInfo.State.PeerCertificates[0].URIs) == 1 {
			s.peers <- tlsInfo.State.PeerCertificates[0].URIs[0].String()
		}
	}
	if err := stream.Send(&runtimev1.ExecutionOutputAckV1{
		CreditFrames: 1,
		CreditBytes:  1024,
	}); err != nil {
		return err
	}
	frame, err := stream.Recv()
	if err != nil {
		return err
	}
	return stream.Send(&runtimev1.ExecutionOutputAckV1{
		StreamId:                    frame.GetStreamId(),
		CommittedContiguousSequence: frame.GetSequence(),
		CreditFrames:                1,
		CreditBytes:                 1024,
	})
}

func TestOutputACKDropProxyDropsOnlyFirstArmedCommittedACK(t *testing.T) {
	root := canonicalTempDir(t)
	pki := generateRuntimePKI(t, root)
	upstream := &outputACKDropUpstream{
		metadata: make(chan metadata.MD, 2),
		peers:    make(chan string, 2),
	}
	upstreamAddress := startOutputACKDropUpstream(t, upstream, pki)
	proxy := startOutputACKDropProxy(t, upstreamAddress, pki)
	dropped := proxy.armCommittedACKDrop(t)

	connection, err := grpc.NewClient(
		proxy.address,
		grpc.WithTransportCredentials(credentials.NewTLS(loadOutputProxyClientTLS(t, pki))),
	)
	if err != nil {
		t.Fatalf("configure worker-side output client: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := runtimev1.NewExecutionOutputServiceClient(connection)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outgoing := metadata.NewOutgoingContext(ctx, metadata.Pairs("x-elitea-workload-session", workloadSession))

	first, err := client.Publish(outgoing)
	if err != nil {
		t.Fatalf("open first proxied stream: %v", err)
	}
	assertOutputProxyInitialCredit(t, first)
	if err := first.Send(&runtimev1.ExecutionOutputFrameV1{StreamId: "stream-1", Sequence: 1}); err != nil {
		t.Fatalf("send first proxied frame: %v", err)
	}
	if _, err := first.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("first committed ACK error = %v, want Unavailable", err)
	}
	select {
	case <-dropped:
	case <-ctx.Done():
		t.Fatal("committed ACK drop was not signalled")
	}
	proxy.releaseCommittedACKDrop(t)

	second, err := client.Publish(outgoing)
	if err != nil {
		t.Fatalf("open replay proxied stream: %v", err)
	}
	assertOutputProxyInitialCredit(t, second)
	if err := second.Send(&runtimev1.ExecutionOutputFrameV1{StreamId: "stream-1", Sequence: 1}); err != nil {
		t.Fatalf("send replay proxied frame: %v", err)
	}
	replayACK, err := second.Recv()
	if err != nil {
		t.Fatalf("receive replay committed ACK: %v", err)
	}
	if replayACK.GetCommittedContiguousSequence() != 1 {
		t.Fatalf("replay committed sequence = %d, want 1", replayACK.GetCommittedContiguousSequence())
	}

	for index := 0; index < 2; index++ {
		select {
		case forwarded := <-upstream.metadata:
			if got := forwarded.Get("x-elitea-workload-session"); len(got) != 1 || got[0] != workloadSession {
				t.Fatalf("forwarded workload-session metadata = %v", got)
			}
		case <-ctx.Done():
			t.Fatal("upstream did not receive forwarded metadata")
		}
		select {
		case identity := <-upstream.peers:
			if identity != workloadID {
				t.Fatalf("upstream client workload identity = %q, want %q", identity, workloadID)
			}
		case <-ctx.Done():
			t.Fatal("upstream did not receive the worker identity certificate")
		}
	}
}

func startOutputACKDropUpstream(t *testing.T, service *outputACKDropUpstream, pki runtimePKI) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for output ACK-drop upstream: %v", err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(loadOutputProxyServerTLS(t, pki))))
	runtimev1.RegisterExecutionOutputServiceServer(server, service)
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		select {
		case err := <-serveErrors:
			if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				t.Errorf("serve output ACK-drop upstream: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("output ACK-drop upstream did not stop")
		}
	})
	return listener.Addr().String()
}

func assertOutputProxyInitialCredit(t *testing.T, stream grpc.BidiStreamingClient[runtimev1.ExecutionOutputFrameV1, runtimev1.ExecutionOutputAckV1]) {
	t.Helper()
	credit, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive proxied initial credit: %v", err)
	}
	if credit.GetCommittedContiguousSequence() != 0 || credit.GetCreditFrames() != 1 || credit.GetCreditBytes() != 1024 {
		t.Fatalf("proxied initial credit = %s", fmt.Sprint(credit))
	}
}
