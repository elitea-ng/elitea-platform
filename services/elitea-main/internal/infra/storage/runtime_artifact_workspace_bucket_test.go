package storage

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The worker's runtime artifact plane refuses the hidden workspace bucket
// (ADR-0034 decision 3) with a 404 on all four operations, before the claim's
// actor or the artifact source is consulted: the stub source would answer any
// bucket, so the 404 can only come from the reserved-name gate.
func TestRuntimeArtifactPlaneRefusesTheWorkspaceBucket(t *testing.T) {
	t.Parallel()
	for _, call := range []struct{ operation, body string }{
		{"list", `{"bucket":"elitea-workspaces"}`},
		{"read", `{"bucket":"elitea-workspaces","name":"t/0b6f9a52-1c2d-4e5f-8a9b-0c1d2e3f4a5b/seg/x.seg"}`},
		{"write", `{"bucket":"elitea-workspaces","name":"x.txt","content":"x"}`},
		{"delete", `{"bucket":"elitea-workspaces","name":"x.txt"}`},
	} {
		t.Run(call.operation, func(t *testing.T) {
			t.Parallel()
			source := &artifactSourceStub{}
			server := newArtifactTestServer(t, builderAuthorizerForProject(7), source)
			response := httptest.NewRecorder()
			server.Routes().ServeHTTP(response, builderRequest(t, "artifacts/"+call.operation, call.body))
			require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
			require.Zero(t, source.projectID, "the hidden bucket must not reach the artifact source")
			require.Empty(t, source.bucket)

			// The same call with an ordinary bucket reaches the source, so the
			// refusal above is the gate and not the request shape.
			ordinary := httptest.NewRecorder()
			server.Routes().ServeHTTP(ordinary, builderRequest(t, "artifacts/"+call.operation,
				replaceBucket(call.body)))
			require.Equal(t, "agent-artifacts", source.bucket, ordinary.Body.String())
		})
	}
}

func TestReservedBucketName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"elitea-workspaces": true, " elitea-workspaces ": true, "ELITEA-WORKSPACES": true,
		"elitea-workspace": false, "reports": false, "": false,
	} {
		require.Equal(t, want, ReservedBucketName(name), name)
	}
}

func replaceBucket(body string) string {
	const reserved = `"bucket":"elitea-workspaces"`
	out := []byte(body)
	for i := 0; i+len(reserved) <= len(out); i++ {
		if string(out[i:i+len(reserved)]) == reserved {
			return string(out[:i]) + `"bucket":"agent-artifacts"` + string(out[i+len(reserved):])
		}
	}
	return body
}
