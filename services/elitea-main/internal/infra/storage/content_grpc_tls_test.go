package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// blockingReader serves one chunk-sized prefix, then blocks until released.
type blockingReader struct {
	data    []byte
	release chan struct{}
	closed  atomic.Bool
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		<-r.release
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *blockingReader) Close() error { r.closed.Store(true); return nil }

// TestRuntimeContentGetInputOverMTLS drives GetInput through a real TLS 1.3
// listener: the claim and fence travel as metadata, the execution scope in
// the request, and the caller identity is the verified client certificate.
func TestRuntimeContentGetInputOverMTLS(t *testing.T) {
	data := bytes.Repeat([]byte("elitea input "), 20000) // > 3 chunks
	digest := sha256.Sum256(data)
	fence := bytes.Repeat([]byte{9}, grpcauth.ClaimFenceBytes)
	const worker = "dns:worker.runtime.test"

	server, err := NewContentServer(
		contentAuthorizerFunc(func(_ context.Context, claim ContentClaim) (ContentAuthorization, error) {
			// The fence and identity check that Postgres performs.
			identity, err := workloadIdentity(claim.PeerCertificate)
			if err != nil || identity != worker || claim.ClaimID != "claim-1" || !bytes.Equal(claim.FenceToken, fence) || claim.ExecutionID != "execution-1" || claim.Generation != 1 {
				return ContentAuthorization{}, ErrContentUnauthorized
			}
			return ContentAuthorization{
				ResourceProjectID: "42",
				InputBundleID:     "bundle-1",
				CapabilityID:      executiondomain.AgentApplicationCapability,
				SemanticRole:      executiondomain.AgentExecutionRequestRole,
				ExpectedMediaType: executiondomain.AgentExecutionInputMediaType,
				ExpectedDigest:    digest,
				ExpectedLength:    int64(len(data)),
			}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}),
		int64(len(data)),
	)
	require.NoError(t, err)
	pki, dial := startContentGRPC(t, server)
	client := dial(pki.ClientTLS(t, "worker.runtime.test"))
	request := &runtimev1.GetInputRequestV1{Scope: contentScope("execution-1", 1), ContentId: "settings", ImmutableVersion: "v1"}

	read := func(client runtimev1.RuntimeContentServiceClient, ctx context.Context) ([]byte, *runtimev1.InputContentHeaderV1, error) {
		stream, err := client.GetInput(ctx, request)
		if err != nil {
			return nil, nil, err
		}
		var header *runtimev1.InputContentHeaderV1
		var body []byte
		for {
			message, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return body, header, nil
			}
			if err != nil {
				return body, header, err
			}
			if message.GetHeader() != nil {
				require.Nil(t, header, "second header")
				header = message.GetHeader()
				continue
			}
			require.NotNil(t, header, "chunk before header")
			require.LessOrEqual(t, len(message.GetChunk()), contentChunkBytes)
			body = append(body, message.GetChunk()...)
		}
	}

	t.Run("authorized caller", func(t *testing.T) {
		body, header, err := read(client, grpcauth.WithContentClaim(t.Context(), "claim-1", fence))
		require.NoError(t, err)
		require.Equal(t, data, body)
		require.EqualValues(t, len(data), header.GetLength())
		require.Equal(t, digest[:], header.GetSha256())
	})
	t.Run("wrong identity", func(t *testing.T) {
		_, _, err := read(dial(pki.ClientTLS(t, "other.runtime.test")), grpcauth.WithContentClaim(t.Context(), "claim-1", fence))
		requireContentCode(t, err, codes.PermissionDenied)
	})
	t.Run("no client certificate", func(t *testing.T) {
		_, _, err := read(dial(pki.AnonymousTLS()), grpcauth.WithContentClaim(t.Context(), "claim-1", fence))
		requireContentCode(t, err, codes.Unavailable)
	})
	t.Run("no claim metadata", func(t *testing.T) {
		_, _, err := read(client, t.Context())
		requireContentCode(t, err, codes.Unauthenticated)
	})
	t.Run("stale fence", func(t *testing.T) {
		_, _, err := read(client, grpcauth.WithContentClaim(t.Context(), "claim-1", bytes.Repeat([]byte{8}, grpcauth.ClaimFenceBytes)))
		requireContentCode(t, err, codes.PermissionDenied)
	})
	t.Run("short fence", func(t *testing.T) {
		_, _, err := read(client, grpcauth.WithContentClaim(t.Context(), "claim-1", fence[:16]))
		requireContentCode(t, err, codes.Unauthenticated)
	})
	t.Run("wrong claim", func(t *testing.T) {
		_, _, err := read(client, grpcauth.WithContentClaim(t.Context(), "claim-2", fence))
		requireContentCode(t, err, codes.PermissionDenied)
	})
}

// TestRuntimeContentCallerCancellationStopsTheSource proves a cancelled
// caller releases the source read and the request slot.
func TestRuntimeContentCallerCancellationStopsTheSource(t *testing.T) {
	fence := bytes.Repeat([]byte{9}, grpcauth.ClaimFenceBytes)
	source := &blockingReader{data: bytes.Repeat([]byte{1}, contentChunkBytes), release: make(chan struct{})}
	opened := make(chan struct{})
	server, err := NewContentServerWithLimits(
		contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
			return ContentAuthorization{
				ResourceProjectID: "42",
				InputBundleID:     "bundle-1",
				CapabilityID:      executiondomain.AgentApplicationCapability,
				SemanticRole:      executiondomain.AgentExecutionRequestRole,
				ExpectedMediaType: executiondomain.AgentExecutionInputMediaType,
				ExpectedDigest:    sha256.Sum256(nil),
				ExpectedLength:    2 * contentChunkBytes,
			}, nil
		}),
		contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
			close(opened)
			return source, nil
		}),
		4*contentChunkBytes,
		1,
	)
	require.NoError(t, err)
	pki, dial := startContentGRPC(t, server)
	client := dial(pki.ClientTLS(t, "worker.runtime.test"))
	ctx, cancel := context.WithCancel(grpcauth.WithContentClaim(t.Context(), "claim-1", fence))
	stream, err := client.GetInput(ctx, &runtimev1.GetInputRequestV1{Scope: contentScope("execution-1", 1), ContentId: "settings", ImmutableVersion: "v1"})
	require.NoError(t, err)
	go func() {
		<-opened
		cancel()
	}()
	_, err = stream.Recv()
	requireContentCode(t, err, codes.Canceled)
	// The handler returns once the source is released; then its slot is free.
	close(source.release)
	require.Eventually(t, func() bool { return len(server.requests) == 0 && source.closed.Load() }, 5*time.Second, 10*time.Millisecond)
}
