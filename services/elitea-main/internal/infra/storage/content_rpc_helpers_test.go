package storage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth/grpcauthtest"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// contentPeerContext is ctx as a RuntimeContentService handler sees a caller
// whose runtime-CA client certificate verified at the handshake.
func contentPeerContext(ctx context.Context, certificate *x509.Certificate) context.Context {
	if certificate == nil {
		certificate = &x509.Certificate{}
	}
	state := tls.ConnectionState{
		VerifiedChains:   [][]*x509.Certificate{{certificate}},
		PeerCertificates: []*x509.Certificate{certificate},
	}
	return peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{State: state}})
}

// contentClaimContext adds the execution claim and fence metadata.
func contentClaimContext(ctx context.Context, certificate *x509.Certificate, claimID string, fence []byte) context.Context {
	ctx = contentPeerContext(ctx, certificate)
	return metadata.NewIncomingContext(ctx, metadata.Pairs(grpcauth.MetadataClaimID, claimID, grpcauth.MetadataClaimFence, string(fence)))
}

func contentScope(executionID string, generation uint64) *runtimev1.RuntimeContentScopeV1 {
	return &runtimev1.RuntimeContentScopeV1{ExecutionId: executionID, Generation: generation}
}

// requireContentCode asserts err is a gRPC status with code.
func requireContentCode(t testing.TB, err error, code codes.Code) {
	t.Helper()
	require.Error(t, err)
	status, ok := grpcstatus.FromError(err)
	require.True(t, ok, "not a gRPC status: %v", err)
	require.Equal(t, code, status.Code(), status.Message())
}

// recordingServerStream is a server-streaming handler's view of the call.
type recordingServerStream[T any] struct {
	grpc.ServerStream
	ctx     context.Context
	sent    []*T
	sendErr func(int) error
}

func newRecordingServerStream[T any](ctx context.Context) *recordingServerStream[T] {
	return &recordingServerStream[T]{ctx: ctx}
}

func (s *recordingServerStream[T]) Context() context.Context { return s.ctx }

func (s *recordingServerStream[T]) Send(message *T) error {
	if s.sendErr != nil {
		if err := s.sendErr(len(s.sent)); err != nil {
			return err
		}
	}
	// A real stream marshals at Send, and the handler may then clear its
	// buffers; keep what was on the wire, not the live message.
	if wire, ok := any(message).(proto.Message); ok {
		message = proto.Clone(wire).(any).(*T)
	}
	s.sent = append(s.sent, message)
	return nil
}

// scriptedClientStream is a client-streaming handler's view of the call.
type scriptedClientStream[Req any, Resp any] struct {
	grpc.ServerStream
	ctx      context.Context
	messages []*Req
	closed   *Resp
}

func newScriptedClientStream[Req any, Resp any](ctx context.Context, messages ...*Req) *scriptedClientStream[Req, Resp] {
	return &scriptedClientStream[Req, Resp]{ctx: ctx, messages: messages}
}

func (s *scriptedClientStream[Req, Resp]) Context() context.Context { return s.ctx }

func (s *scriptedClientStream[Req, Resp]) Recv() (*Req, error) {
	if len(s.messages) == 0 {
		return nil, io.EOF
	}
	message := s.messages[0]
	s.messages = s.messages[1:]
	return message, nil
}

func (s *scriptedClientStream[Req, Resp]) SendAndClose(response *Resp) error {
	s.closed = response
	return nil
}

// splitContentChunks splits data the way a client streams it.
func splitContentChunks(data []byte) [][]byte {
	var chunks [][]byte
	for len(data) > 0 {
		size := min(len(data), contentChunkBytes)
		chunks = append(chunks, data[:size])
		data = data[size:]
	}
	return chunks
}

// startContentGRPC serves content on a real TLS 1.3 listener that requires a
// runtime-CA client certificate, and returns the PKI and a client dialer.
func startContentGRPC(t *testing.T, content *ContentServer) (*grpcauthtest.PKI, func(*tls.Config) runtimev1.RuntimeContentServiceClient) {
	t.Helper()
	pki := grpcauthtest.NewPKI(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(pki.ServerTLS(t))))
	content.Register(server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return pki, func(config *tls.Config) runtimev1.RuntimeContentServiceClient {
		conn, err := grpc.NewClient("passthrough:///"+listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(config)))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return runtimev1.NewRuntimeContentServiceClient(conn)
	}
}
