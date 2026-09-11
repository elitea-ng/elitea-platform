package toolkitdiscovery

import (
	"bytes"
	"crypto/sha256"
	"errors"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/protobuf/proto"
)

// PreparedIdentity is read from the admitted execution and its immutable input.
// The prepared outbox envelope is the existing owner of the toolkit type.
type PreparedIdentity struct {
	CommandID, ExecutionID, TenantID, ResourceProjectID, ProjectionProjectID string
	Generation                                                               uint64
	InputBundleID, SettingsEntryID                                           string
	InputBundleDigest                                                        []byte
}

// PreparedToolkitType validates an envelope already stored by trusted admission.
// It verifies integrity and identity; signature verification belongs to transport.
func PreparedToolkitType(encoded, storedDigest []byte, expected PreparedIdentity) (string, error) {
	invalid := errors.New("invalid prepared toolkit discovery command")
	digest := sha256.Sum256(encoded)
	if len(encoded) == 0 || len(encoded) > 65536 || len(storedDigest) != sha256.Size || !bytes.Equal(digest[:], storedDigest) {
		return "", invalid
	}
	var envelope runtimev1.SignedWorkerCommandEnvelopeV1
	if proto.Unmarshal(encoded, &envelope) != nil {
		return "", invalid
	}
	commandDigest := sha256.Sum256(envelope.GetWorkerCommandBytes())
	if envelope.GetWorkerCommandDigest().GetAlgorithm() != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 || !bytes.Equal(envelope.GetWorkerCommandDigest().GetValue(), commandDigest[:]) {
		return "", invalid
	}
	var command runtimev1.WorkerCommandV1
	if proto.Unmarshal(envelope.GetWorkerCommandBytes(), &command) != nil {
		return "", invalid
	}
	input := command.GetInputBundleRef()
	if command.GetCommandId() != expected.CommandID || command.GetExecutionId() != expected.ExecutionID || command.GetGeneration() != expected.Generation ||
		command.GetTenantId() != expected.TenantID || command.GetResourceProjectId() != expected.ResourceProjectID || command.GetProjectionProjectId() != expected.ProjectionProjectID ||
		command.GetCapabilityId() != "toolkit.available_tools.v1" || input.GetInputBundleId() != expected.InputBundleID || len(expected.InputBundleDigest) != sha256.Size ||
		input.GetDigest().GetAlgorithm() != runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256 || !bytes.Equal(input.GetDigest().GetValue(), expected.InputBundleDigest) ||
		command.GetToolkitAvailableTools().GetSettingsEntryId() != expected.SettingsEntryID || command.GetToolkitAvailableTools().GetToolkitType() == "" {
		return "", invalid
	}
	return command.GetToolkitAvailableTools().GetToolkitType(), nil
}
