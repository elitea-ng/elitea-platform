package output

import (
	"bytes"
	"errors"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
)

func credentialPausedResult() *runtimev1.AgentExecutionResultV1 {
	digest := &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     bytes.Repeat([]byte{1}, 32),
	}
	return &runtimev1.AgentExecutionResultV1{
		InputBundleId: "bundle-1", InputBundleDigest: digest,
		RequestEntryId: "request-1", RequestImmutableVersion: "version-1", RequestContentDigest: digest,
		TerminalState: runtimev1.AgentExecutionTerminalStateV1_AGENT_EXECUTION_TERMINAL_STATE_V1_PAUSED_CREDENTIAL_AUTHORIZATION,
		ResultArtifact: &runtimev1.AgentExecutionArtifactReferenceV1{
			ArtifactId: "artifact-1", ImmutableVersion: "artifact-version-1",
			MediaType: outputapp.AgentResultMediaType, ByteLength: 1, Digest: digest,
			Classification: outputapp.AgentResultClassification,
		},
	}
}

// Flag off (the option not composed): the governed pause state is refused,
// exactly as any unknown state was before it existed.
func TestCredentialAuthorizationPauseIsRefusedWithoutTheOption(t *testing.T) {
	_, err := agentExecutionResultDomain(credentialPausedResult())
	if !errors.Is(err, outputapp.ErrInvalidAgentExecutionOutput) {
		t.Fatalf("err = %v, want the invalid-output refusal", err)
	}
	server := &Server{}
	_, err = agentExecutionResultDomainFor(credentialPausedResult(), server.credentialAuthorization)
	if !errors.Is(err, outputapp.ErrInvalidAgentExecutionOutput) {
		t.Fatalf("err = %v, want the invalid-output refusal", err)
	}
}

func TestCredentialAuthorizationPauseMapsToAPausedAuthorizationWhenGoverned(t *testing.T) {
	server := &Server{}
	if err := WithCredentialAuthorizationPause()(server); err != nil {
		t.Fatal(err)
	}
	result, err := agentExecutionResultDomainFor(credentialPausedResult(), server.credentialAuthorization)
	if err != nil || result.TerminalState != outputapp.AgentExecutionTerminalPausedAuthorization {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
