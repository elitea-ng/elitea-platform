package storage

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/stretchr/testify/require"
)

// workspaceAwareSource answers IsWorkspaceBucket by TYPE as production does
// (repos.CurrentRuntimeArtifactRepository): here the project's hidden bucket
// is the one named hidden, whatever its name.
type workspaceAwareSource struct {
	*artifactSourceStub
	hidden string
}

func (source workspaceAwareSource) IsWorkspaceBucket(_ context.Context, _ int64, bucket string) (bool, error) {
	return bucket == source.hidden, nil
}

// The worker's runtime artifact plane refuses the hidden workspace bucket
// (ADR-0034 decision 3) with a 404 on all four operations, before the
// artifact source is read. The stub source would answer any bucket, so the
// 404 can only come from the gate. The gate is by type: a user bucket called
// elitea-workspaces (created before the name was reserved) still reaches the
// source when the hidden bucket carries a suffixed name.
func TestRuntimeArtifactPlaneRefusesTheWorkspaceBucket(t *testing.T) {
	t.Parallel()
	const suffixed = WorkspaceBucketName + "-0a1b2c3d"
	for _, call := range []struct{ operation, body string }{
		{"list", `{"bucket":"%s"}`},
		{"read", `{"bucket":"%s","name":"t/0b6f9a52-1c2d-4e5f-8a9b-0c1d2e3f4a5b/seg/x.seg"}`},
		{"write", `{"bucket":"%s","name":"x.txt","content":"x"}`},
		{"delete", `{"bucket":"%s","name":"x.txt"}`},
	} {
		t.Run(call.operation, func(t *testing.T) {
			t.Parallel()
			for _, hidden := range []string{WorkspaceBucketName, suffixed} {
				source := &artifactSourceStub{}
				server := newArtifactTestServer(t, builderAuthorizerForProject(7), workspaceAwareSource{source, hidden})
				response := serve(server, builderRequest(t, "artifacts/"+call.operation, fmt.Sprintf(call.body, hidden)))
				require.Equal(t, codes.NotFound, response.Code, string(response.Body))
				require.Zero(t, source.projectID, "the hidden bucket must not reach the artifact source")
				require.Empty(t, source.bucket)

				// An ordinary bucket reaches the source, so the refusal above
				// is the gate and not the request shape.
				ordinary := serve(server, builderRequest(t, "artifacts/"+call.operation, fmt.Sprintf(call.body, "agent-artifacts")))
				require.Equal(t, "agent-artifacts", source.bucket, string(ordinary.Body))
			}
			// The user bucket of the reserved name keeps working beside a
			// suffixed hidden bucket.
			source := &artifactSourceStub{}
			server := newArtifactTestServer(t, builderAuthorizerForProject(7), workspaceAwareSource{source, suffixed})
			user := serve(server, builderRequest(t, "artifacts/"+call.operation, fmt.Sprintf(call.body, WorkspaceBucketName)))
			require.Equal(t, WorkspaceBucketName, source.bucket, string(user.Body))
		})
	}
}

func TestReservedBucketName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"elitea-workspaces": true, " elitea-workspaces ": true, "ELITEA-WORKSPACES": true,
		"elitea-workspaces-0a1b2c3d": true,
		"elitea-workspace":           false, "reports": false, "": false,
	} {
		require.Equal(t, want, ReservedBucketName(name), name)
	}
}
