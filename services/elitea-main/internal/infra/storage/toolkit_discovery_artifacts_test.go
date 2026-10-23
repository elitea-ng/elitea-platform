package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type discoveryArtifactStoreFunc func(context.Context, ContentClaim, []byte) (*runtimev1.ToolkitAvailableToolsArtifactReferenceV1, error)

func (f discoveryArtifactStoreFunc) PutToolkitDiscoveryArtifact(ctx context.Context, claim ContentClaim, body []byte) (*runtimev1.ToolkitAvailableToolsArtifactReferenceV1, error) {
	return f(ctx, claim, body)
}
func (f discoveryArtifactStoreFunc) GetToolkitDiscoveryArtifact(context.Context, ContentClaim) ([]byte, error) {
	return nil, ErrContentNotFound
}

func TestToolkitDiscoveryUploadEnforcesAuthorityAndBounds(t *testing.T) {
	content := []byte(`{"tools":[],"args_schemas":{}}`)
	for _, test := range []struct {
		name     string
		ctx      context.Context
		body     []byte
		authErr  error
		authRole string
		writeErr error
		code     codes.Code
	}{
		{name: "valid", code: codes.OK},
		{name: "missing TLS", ctx: metadata.NewIncomingContext(context.Background(), metadata.Pairs(grpcauth.MetadataClaimID, "claim-1", grpcauth.MetadataClaimFence, string(bytes.Repeat([]byte{1}, sha256.Size)))), code: codes.Unauthenticated},
		{name: "missing claim", ctx: contentPeerContext(context.Background(), nil), code: codes.Unauthenticated},
		{name: "stale claim", authErr: ErrContentUnauthorized, code: codes.PermissionDenied},
		{name: "another capability", authRole: "toolkit.other", code: codes.PermissionDenied},
		{name: "oversized", body: bytes.Repeat([]byte{' '}, MaxToolkitDiscoveryArtifactBytes+1), code: codes.ResourceExhausted},
		{name: "empty", body: []byte{}, code: codes.ResourceExhausted},
		{name: "not JSON", body: []byte(`{"tools":`), code: codes.InvalidArgument},
		{name: "not UTF-8", body: []byte("\"\xff\""), code: codes.InvalidArgument},
		{name: "conflict", writeErr: ErrContentRejected, code: codes.Aborted},
		{name: "reserved bucket name held", writeErr: ErrSystemBucketNameTaken, code: codes.Aborted},
		{name: "claim lost during upload", writeErr: ErrContentUnauthorized, code: codes.PermissionDenied},
		{name: "canceled", writeErr: context.Canceled, code: codes.Unavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			writes := 0
			role := "toolkit.available_tools.settings"
			if test.authRole != "" {
				role = test.authRole
			}
			server, err := NewContentServer(contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
				return ContentAuthorization{CapabilityID: "toolkit.available_tools.v1", SemanticRole: role}, test.authErr
			}), contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
				return nil, errors.New("unexpected input read")
			}), 0)
			require.NoError(t, err)
			body := content
			if test.body != nil {
				body = test.body
			}
			expected := &runtimev1.ToolkitAvailableToolsArtifactReferenceV1{ArtifactId: "result", ImmutableVersion: "v1"}
			server.WithToolkitDiscoveryArtifacts(discoveryArtifactStoreFunc(func(_ context.Context, claim ContentClaim, got []byte) (*runtimev1.ToolkitAvailableToolsArtifactReferenceV1, error) {
				writes++
				require.Equal(t, "execution-1", claim.ExecutionID)
				require.Equal(t, "settings", claim.ContentID)
				require.Equal(t, "v1", claim.ImmutableVersion)
				require.Equal(t, body, got)
				return expected, test.writeErr
			}))
			ctx := test.ctx
			if ctx == nil {
				ctx = validContentContext()
			}
			response, err := server.PutToolkitDiscoveryResult(ctx, &runtimev1.PutToolkitDiscoveryResultRequestV1{
				Scope:            contentScope("execution-1", 1),
				ContentId:        "settings",
				ImmutableVersion: "v1",
				ResultJson:       body,
			})
			if test.code == codes.OK {
				require.NoError(t, err)
				require.True(t, proto.Equal(expected, response.GetReference()))
				return
			}
			requireContentCode(t, err, test.code)
			if test.writeErr == nil {
				require.Zero(t, writes)
			}
		})
	}
}

func TestToolkitDiscoveryReadMapsAMissingResult(t *testing.T) {
	server, err := NewContentServer(contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
		return ContentAuthorization{}, nil
	}), contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
		return nil, errors.New("unexpected input read")
	}), 0)
	require.NoError(t, err)
	request := &runtimev1.GetToolkitDiscoveryResultRequestV1{Scope: contentScope("execution-1", 1), ContentId: "settings", ImmutableVersion: "v1"}
	_, err = server.GetToolkitDiscoveryResult(validContentContext(), request)
	requireContentCode(t, err, codes.Unimplemented)

	server.WithToolkitDiscoveryArtifacts(discoveryArtifactStoreFunc(nil))
	_, err = server.GetToolkitDiscoveryResult(validContentContext(), request)
	requireContentCode(t, err, codes.NotFound)
	_, err = server.GetToolkitDiscoveryResult(contentPeerContext(context.Background(), nil), request)
	requireContentCode(t, err, codes.Unauthenticated)
}
