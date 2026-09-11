package toolkitdiscovery

import (
	"bytes"
	"crypto/sha256"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestPreparedDiscoveryTypeRequiresExactAdmittedIdentityAndIntegrity(t *testing.T) {
	bundleDigest := bytes.Repeat([]byte{3}, 32)
	expected := PreparedIdentity{CommandID: "cmd", ExecutionID: "execution", Generation: 1, TenantID: "tenant", ResourceProjectID: "7", ProjectionProjectID: "8", InputBundleID: "bundle", InputBundleDigest: bundleDigest, SettingsEntryID: "settings"}
	command := &runtimev1.WorkerCommandV1{CommandId: expected.CommandID, ExecutionId: expected.ExecutionID, Generation: expected.Generation, TenantId: expected.TenantID,
		ResourceProjectId: expected.ResourceProjectID, ProjectionProjectId: expected.ProjectionProjectID, CapabilityId: "toolkit.available_tools.v1",
		InputBundleRef:    &runtimev1.ExecutionInputBundleReferenceV1{InputBundleId: expected.InputBundleID, Digest: &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: bundleDigest}},
		CapabilityCommand: &runtimev1.WorkerCommandV1_ToolkitAvailableTools{ToolkitAvailableTools: &runtimev1.ToolkitAvailableToolsCommandV1{ToolkitType: "mcp_records", SettingsEntryId: "settings"}}}
	commandBytes, err := proto.Marshal(command)
	require.NoError(t, err)
	commandDigest := sha256.Sum256(commandBytes)
	envelope := &runtimev1.SignedWorkerCommandEnvelopeV1{WorkerCommandBytes: commandBytes, WorkerCommandDigest: &runtimev1.DigestV1{Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256, Value: commandDigest[:]}}
	encoded, err := proto.Marshal(envelope)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	got, err := PreparedToolkitType(encoded, digest[:], expected)
	require.NoError(t, err)
	require.Equal(t, "mcp_records", got)
	for name, mutate := range map[string]func(*PreparedIdentity){
		"project": func(e *PreparedIdentity) { e.ResourceProjectID = "9" }, "tenant": func(e *PreparedIdentity) { e.TenantID = "other" },
		"generation": func(e *PreparedIdentity) { e.Generation++ }, "execution": func(e *PreparedIdentity) { e.ExecutionID = "other" },
		"command": func(e *PreparedIdentity) { e.CommandID = "other" }, "settings": func(e *PreparedIdentity) { e.SettingsEntryID = "other" },
		"bundle": func(e *PreparedIdentity) { e.InputBundleID = "other" }, "digest": func(e *PreparedIdentity) { e.InputBundleDigest = bytes.Repeat([]byte{4}, 32) },
	} {
		t.Run(name, func(t *testing.T) {
			foreign := expected
			mutate(&foreign)
			_, err := PreparedToolkitType(encoded, digest[:], foreign)
			require.Error(t, err)
		})
	}
	_, err = PreparedToolkitType(encoded, bytes.Repeat([]byte{9}, 32), expected)
	require.Error(t, err)
	envelope.WorkerCommandDigest.Value = bytes.Repeat([]byte{9}, 32)
	encoded, err = proto.Marshal(envelope)
	require.NoError(t, err)
	digest = sha256.Sum256(encoded)
	_, err = PreparedToolkitType(encoded, digest[:], expected)
	require.Error(t, err)
}
