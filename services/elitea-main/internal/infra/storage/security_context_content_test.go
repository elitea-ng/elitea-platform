package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// securityContextAuthorizer is a content authorizer that also serves the
// security context namespace; AuthorizeContent must never be reached for it.
type securityContextAuthorizer struct {
	data   []byte
	digest runtimedomain.Digest
	err    error
	claims *[]ContentClaim
}

func (a securityContextAuthorizer) AuthorizeContent(context.Context, ContentClaim) (ContentAuthorization, error) {
	panic("the security context namespace reached the input bundle authorizer")
}

func (a securityContextAuthorizer) OpenSecurityContext(_ context.Context, claim ContentClaim) ([]byte, runtimedomain.Digest, error) {
	*a.claims = append(*a.claims, claim)
	return a.data, a.digest, a.err
}

func noContentStore() ContentStore {
	return contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
		panic("the security context namespace opened the input store")
	})
}

func securityContextRequest(contentID, version string) *runtimev1.GetInputRequestV1 {
	return &runtimev1.GetInputRequestV1{Scope: contentScope("execution-1", 1), ContentId: contentID, ImmutableVersion: version}
}

// GetInput serves a governed execution's context bytes as stored, under the
// claim, without the input bundle path or a materializer (#1115).
func TestContentServerServesTheSecurityContextUnderTheClaim(t *testing.T) {
	t.Parallel()
	data := []byte{0x0a, 0x03, 'r', '-', '1'}
	digest := runtimedomain.Digest(sha256.Sum256(data))
	var claims []ContentClaim
	server, err := NewMaterializingContentServerWithLimits(
		securityContextAuthorizer{data: data, digest: digest, claims: &claims},
		noContentStore(),
		contentMaterializerFunc(func(context.Context, ContentAuthorization, []byte, int64) ([]byte, error) {
			panic("the security context was materialized")
		}),
		0, 1,
	)
	require.NoError(t, err)
	header, body, err := collectInput(server, validContentContext(), securityContextRequest(securitycontext.ContentIDPrefix+"execution-1", digest.String()))
	require.NoError(t, err)
	require.Equal(t, data, body)
	require.Equal(t, securitycontext.MediaType, header.GetMediaType())
	require.Equal(t, digest[:], header.GetSha256())
	require.Len(t, claims, 1)
	require.Equal(t, "claim-1", claims[0].ClaimID)
}

func TestContentServerRefusesAnotherExecutionsSecurityContext(t *testing.T) {
	t.Parallel()
	var claims []ContentClaim
	server, err := NewContentServer(securityContextAuthorizer{claims: &claims}, noContentStore(), 0)
	require.NoError(t, err)
	_, _, err = collectInput(server, validContentContext(), securityContextRequest(securitycontext.ContentIDPrefix+"execution-2", "sha256:00"))
	require.Equal(t, codes.PermissionDenied, grpcstatus.Code(err))
	require.Empty(t, claims, "a foreign execution's context reached the store")
}

func TestContentServerDoesNotReleaseASecurityContextThatFailsItsDigest(t *testing.T) {
	t.Parallel()
	data := []byte{0x0a, 0x00}
	var claims []ContentClaim
	server, err := NewContentServer(securityContextAuthorizer{data: data, digest: runtimedomain.SHA256(bytes.Repeat(data, 2)), claims: &claims}, noContentStore(), 0)
	require.NoError(t, err)
	_, _, err = collectInput(server, validContentContext(), securityContextRequest(securitycontext.ContentIDPrefix+"execution-1", "sha256:00"))
	require.Equal(t, codes.Internal, grpcstatus.Code(err))
}

func TestContentServerWithoutASecurityContextSourceRefusesTheNamespace(t *testing.T) {
	t.Parallel()
	server, err := NewContentServer(contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
		panic("the namespace reached AuthorizeContent")
	}), noContentStore(), 0)
	require.NoError(t, err)
	_, _, err = collectInput(server, validContentContext(), securityContextRequest(securitycontext.ContentIDPrefix+"execution-1", "sha256:00"))
	require.Equal(t, codes.PermissionDenied, grpcstatus.Code(err))
}
