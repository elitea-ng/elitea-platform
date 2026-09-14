package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
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

func TestToolkitDiscoveryUploadEnforcesAuthorityBoundsAndDigest(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func(*http.Request)
		authErr  error
		writeErr error
		status   int
	}{
		{name: "valid", status: 200},
		{name: "missing TLS", mutate: func(r *http.Request) { r.TLS = nil }, status: 401},
		{name: "stale claim", authErr: ErrContentUnauthorized, status: 403},
		{name: "oversized", mutate: func(r *http.Request) { r.ContentLength = MaxToolkitDiscoveryArtifactBytes + 1 }, status: 413},
		{name: "wrong digest", mutate: func(r *http.Request) { r.Header.Set("Content-Digest", "sha-256=:wrong:") }, status: 400},
		{name: "wrong media", mutate: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, status: 400},
		{name: "conflict", writeErr: ErrContentRejected, status: 409},
		{name: "claim lost during upload", writeErr: ErrContentUnauthorized, status: 403},
		{name: "canceled", writeErr: context.Canceled, status: 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := []byte(`{"tools":[],"args_schemas":{}}`)
			writes := 0
			server, err := NewContentServer(contentAuthorizerFunc(func(context.Context, ContentClaim) (ContentAuthorization, error) {
				return ContentAuthorization{CapabilityID: "toolkit.available_tools.v1", SemanticRole: "toolkit.available_tools.settings"}, test.authErr
			}), contentStoreFunc(func(context.Context, string, string, string, string) (io.ReadCloser, error) {
				return nil, errors.New("unexpected input read")
			}), 0)
			require.NoError(t, err)
			expected := &runtimev1.ToolkitAvailableToolsArtifactReferenceV1{ArtifactId: "result", ImmutableVersion: "v1"}
			server.WithToolkitDiscoveryArtifacts(discoveryArtifactStoreFunc(func(_ context.Context, claim ContentClaim, body []byte) (*runtimev1.ToolkitAvailableToolsArtifactReferenceV1, error) {
				writes++
				require.Equal(t, "execution-1", claim.ExecutionID)
				require.Equal(t, "settings", claim.ContentID)
				require.Equal(t, content, body)
				return expected, test.writeErr
			}))
			request := validContentRequest(t)
			request.Method = http.MethodPut
			request.URL.Path += "/toolkit-discovery-result"
			request.Body = io.NopCloser(bytes.NewReader(content))
			request.ContentLength = int64(len(content))
			request.Header.Set("Content-Type", ToolkitDiscoveryArtifactMediaType)
			request.Header.Set("Content-Digest", formatSHA256Digest(sha256.Sum256(content)))
			if test.mutate != nil {
				test.mutate(request)
			}
			recorder := httptest.NewRecorder()
			server.Routes().ServeHTTP(recorder, request)
			require.Equal(t, test.status, recorder.Code)
			if test.status == 200 {
				var actual runtimev1.ToolkitAvailableToolsArtifactReferenceV1
				require.NoError(t, proto.Unmarshal(recorder.Body.Bytes(), &actual))
				require.True(t, proto.Equal(expected, &actual))
				require.Equal(t, "application/protobuf", recorder.Header().Get("Content-Type"))
			} else if test.writeErr == nil {
				require.Zero(t, writes)
			}
		})
	}
}
