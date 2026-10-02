package agentexecution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

const (
	requestEntryID           = "agent-request"
	agentInputSchemaRevision = "elitea.runtime.agent-execution-input.v1"
)

var ErrInvalidAuthoritativeAgentInput = errors.New("invalid authoritative agent input")

type InputProfile struct {
	Classification        string
	RequiredGrantAudience string
}

func (p InputProfile) validate() error {
	if p.Classification == "" || p.Classification == "synthetic" ||
		p.RequiredGrantAudience == "" {
		return ErrInvalidAuthoritativeAgentInput
	}
	return nil
}

// InputBundleFactory stores the complete resolved agent request as immutable
// protobuf data-plane content. Only its entry reference is eligible for the
// worker command/control plane.
type InputBundleFactory struct {
	profile InputProfile
	newID   executionapp.IDGenerator
}

func NewInputBundleFactory(
	profile InputProfile,
	newID executionapp.IDGenerator,
) (*InputBundleFactory, error) {
	if err := profile.validate(); err != nil {
		return nil, err
	}
	if newID == nil {
		return nil, errors.New("agent input ID generator is required")
	}
	return &InputBundleFactory{profile: profile, newID: newID}, nil
}

func (f *InputBundleFactory) Build(
	ctx context.Context,
	input *runtimev1.AgentExecutionInputV1,
	clientStreamID,
	clientMessageID,
	sioEvent string,
) (executiondomain.InputBundle, executiondomain.AgentExecutionBinding, error) {
	if err := ctx.Err(); err != nil {
		return executiondomain.InputBundle{}, executiondomain.AgentExecutionBinding{}, err
	}
	if err := validateAuthoritativeInput(input); err != nil {
		return executiondomain.InputBundle{}, executiondomain.AgentExecutionBinding{}, err
	}

	content, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	if err != nil || len(content) == 0 || len(content) > executiondomain.MaxAgentExecutionInputBytes {
		return executiondomain.InputBundle{}, executiondomain.AgentExecutionBinding{}, ErrInvalidAuthoritativeAgentInput
	}
	bundleID, err := f.newID()
	if err != nil {
		return executiondomain.InputBundle{}, executiondomain.AgentExecutionBinding{}, fmt.Errorf("generate agent input bundle ID: %w", err)
	}
	contentID, err := f.newID()
	if err != nil {
		return executiondomain.InputBundle{}, executiondomain.AgentExecutionBinding{}, fmt.Errorf("generate agent input content ID: %w", err)
	}
	if bundleID == "" || contentID == "" {
		return executiondomain.InputBundle{}, executiondomain.AgentExecutionBinding{}, errors.New("agent input ID generator returned an empty ID")
	}

	contentDigest := runtimedomain.SHA256(content)
	contentVersion := contentDigest.String()
	bundleVersion := "admission:" + bundleID
	manifest, err := proto.MarshalOptions{Deterministic: true}.Marshal(
		&runtimev1.ExecutionInputBundleV1{
			InputBundleId:    bundleID,
			ImmutableVersion: bundleVersion,
			Entries: []*runtimev1.ExecutionInputEntryV1{{
				EntryId:          requestEntryID,
				ImmutableVersion: contentVersion,
				SemanticRole:     executiondomain.AgentExecutionRequestRole,
				Content: &runtimev1.ScopedContentReferenceV1{
					ContentId:             contentID,
					ImmutableVersion:      contentVersion,
					MediaType:             executiondomain.AgentExecutionInputMediaType,
					ByteLength:            uint64(len(content)),
					Digest:                agentDigestProto(contentDigest),
					Classification:        f.profile.Classification,
					RequiredGrantAudience: f.profile.RequiredGrantAudience,
				},
			}},
		},
	)
	if err != nil || len(manifest) == 0 || len(manifest) > 64*1024 {
		return executiondomain.InputBundle{}, executiondomain.AgentExecutionBinding{}, ErrInvalidAuthoritativeAgentInput
	}

	bundle := executiondomain.InputBundle{
		ID:        bundleID,
		Version:   bundleVersion,
		MediaType: executiondomain.InputBundleManifestMediaType,
		Digest:    runtimedomain.SHA256(manifest),
		Manifest:  manifest,
		Entries: []executiondomain.InputEntry{{
			ID:                    requestEntryID,
			Version:               contentVersion,
			SemanticRole:          executiondomain.AgentExecutionRequestRole,
			ContentID:             contentID,
			MediaType:             executiondomain.AgentExecutionInputMediaType,
			Classification:        f.profile.Classification,
			RequiredGrantAudience: f.profile.RequiredGrantAudience,
			ContentDigest:         contentDigest,
			ContentLength:         int64(len(content)),
			Content:               append([]byte(nil), content...),
		}},
	}
	binding := executiondomain.AgentExecutionBinding{
		RequestEntryID:            requestEntryID,
		ClientStreamID:            clientStreamID,
		ClientMessageID:           clientMessageID,
		ClientExecutionGeneration: input.GetExecutionGeneration(),
		SIOEvent:                  sioEvent,
	}
	if err := bundle.Validate(); err != nil {
		return executiondomain.InputBundle{}, executiondomain.AgentExecutionBinding{}, err
	}
	if err := binding.Validate(bundle); err != nil {
		return executiondomain.InputBundle{}, executiondomain.AgentExecutionBinding{}, err
	}
	return bundle, binding, nil
}

func validateAuthoritativeInput(input *runtimev1.AgentExecutionInputV1) error {
	if input == nil || input.GetSchemaRevision() != agentInputSchemaRevision ||
		len(input.ProtoReflect().GetUnknown()) != 0 || !validCurrentProjectContextSnapshot(input.GetProjectContext()) {
		return ErrInvalidAuthoritativeAgentInput
	}
	objectFields := [][]byte{
		input.GetLlm(), input.GetApplication(), input.GetMcpTokens(),
		input.GetMeta(), input.GetContextSettings(),
	}
	for _, field := range objectFields {
		if !validJSONKind(field, '{') {
			return ErrInvalidAuthoritativeAgentInput
		}
	}
	arrayFields := [][]byte{
		input.GetChatHistory(), input.GetTools(), input.GetInternalTools(),
		input.GetIgnoredMcpServers(), input.GetUserDeclinedMcpServers(),
		input.GetHitlDecisions(), input.GetInvokedSkills(), input.GetAppliedSkills(),
		input.GetAttachedSkills(), input.GetInputAttachments(),
		input.GetParallelTerminalErrors(),
	}
	for _, field := range arrayFields {
		if !validJSONKind(field, '[') {
			return ErrInvalidAuthoritativeAgentInput
		}
	}
	if !validAgentUserInput(input.GetUserInput()) ||
		!validOptionalJSONObject(input.GetParallelReconcile()) ||
		input.ExecutionGeneration == nil || input.GetExecutionGeneration() == "" ||
		(input.StepsLimit != nil && input.GetStepsLimit() <= 0) ||
		(input.GetHitlResume() && input.HitlAction == nil && !nonEmptyJSONArray(input.GetHitlDecisions())) {
		return ErrInvalidAuthoritativeAgentInput
	}
	return nil
}

func nonEmptyJSONArray(value []byte) bool {
	var decoded []json.RawMessage
	return json.Unmarshal(value, &decoded) == nil && len(decoded) > 0
}

func validJSONKind(value []byte, prefix byte) bool {
	if len(value) == 0 || !json.Valid(value) {
		return false
	}
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return false
	}
	switch prefix {
	case '{':
		_, ok := decoded.(map[string]any)
		return ok
	case '[':
		_, ok := decoded.([]any)
		return ok
	default:
		return false
	}
}

func validAgentUserInput(value []byte) bool {
	if len(value) == 0 || !json.Valid(value) {
		return false
	}
	var decoded any
	if json.Unmarshal(value, &decoded) != nil {
		return false
	}
	switch decoded.(type) {
	case string, []any:
		return true
	default:
		return false
	}
}

func validOptionalJSONObject(value []byte) bool {
	if string(value) == "null" {
		return true
	}
	return validJSONKind(value, '{')
}

func agentDigestProto(digest runtimedomain.Digest) *runtimev1.DigestV1 {
	return &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     append([]byte(nil), digest[:]...),
	}
}
