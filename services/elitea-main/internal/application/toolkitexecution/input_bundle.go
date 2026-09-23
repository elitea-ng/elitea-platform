package toolkitexecution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

const (
	toolkitReadRequestEntryID = "toolkit-read-request"
	toolkitReadSchemaRevision = "elitea.runtime.toolkit-execute-read-input.v1"
)

var ErrInvalidAuthoritativeToolkitReadInput = errors.New("invalid authoritative direct toolkit input")

type InputProfile struct {
	Classification        string
	RequiredGrantAudience string
}

func (p InputProfile) validate() error {
	if !validIdentity(p.Classification) || p.Classification == "synthetic" ||
		!validIdentity(p.RequiredGrantAudience) {
		return ErrInvalidAuthoritativeToolkitReadInput
	}
	return nil
}

// InputBundleFactory stores one exact direct toolkit invocation as immutable
// protobuf data-plane content. Redis receives only the bundle and entry
// references; the toolkit snapshot, arguments, and protected settings stay off
// the command stream.
type InputBundleFactory struct {
	profile InputProfile
	newID   executionapp.IDGenerator
}

func NewInputBundleFactory(profile InputProfile, newID executionapp.IDGenerator) (*InputBundleFactory, error) {
	if err := profile.validate(); err != nil {
		return nil, err
	}
	if newID == nil {
		return nil, errors.New("direct toolkit input ID generator is required")
	}
	return &InputBundleFactory{profile: profile, newID: newID}, nil
}

func (f *InputBundleFactory) Build(
	ctx context.Context,
	frozen FrozenCurrentReadTool,
) (executiondomain.InputBundle, executiondomain.ToolkitExecuteReadBinding, error) {
	if err := ctx.Err(); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitExecuteReadBinding{}, err
	}
	input := &runtimev1.ToolkitExecuteReadInputV1{
		SchemaRevision:    toolkitReadSchemaRevision,
		Toolkit:           append([]byte(nil), frozen.ToolkitJSON...),
		ToolkitType:       frozen.ToolkitType,
		ToolkitName:       frozen.ToolkitName,
		ToolName:          frozen.ToolName,
		Arguments:         append([]byte(nil), frozen.ArgumentsJSON...),
		ToolkitGuardrails: append([]byte(nil), frozen.GuardrailsJSON...),
	}
	if err := validateAuthoritativeInput(input); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitExecuteReadBinding{}, err
	}
	content, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	if err != nil || len(content) == 0 || len(content) > executiondomain.MaxToolkitExecuteReadInputBytes {
		return executiondomain.InputBundle{}, executiondomain.ToolkitExecuteReadBinding{}, ErrInvalidAuthoritativeToolkitReadInput
	}

	bundleID, err := f.newID()
	if err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitExecuteReadBinding{}, fmt.Errorf("generate direct toolkit input bundle ID: %w", err)
	}
	contentID, err := f.newID()
	if err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitExecuteReadBinding{}, fmt.Errorf("generate direct toolkit input content ID: %w", err)
	}
	if !validIdentity(bundleID) || !validIdentity(contentID) {
		return executiondomain.InputBundle{}, executiondomain.ToolkitExecuteReadBinding{}, errors.New("direct toolkit input ID generator returned an invalid ID")
	}

	contentDigest := runtimedomain.SHA256(content)
	contentVersion := contentDigest.String()
	bundleVersion := "admission:" + bundleID
	manifest, err := proto.MarshalOptions{Deterministic: true}.Marshal(&runtimev1.ExecutionInputBundleV1{
		InputBundleId:    bundleID,
		ImmutableVersion: bundleVersion,
		Entries: []*runtimev1.ExecutionInputEntryV1{{
			EntryId:          toolkitReadRequestEntryID,
			ImmutableVersion: contentVersion,
			SemanticRole:     executiondomain.ToolkitExecuteReadRequestRole,
			Content: &runtimev1.ScopedContentReferenceV1{
				ContentId:             contentID,
				ImmutableVersion:      contentVersion,
				MediaType:             executiondomain.ToolkitExecuteReadInputMediaType,
				ByteLength:            uint64(len(content)),
				Digest:                toolkitReadDigestProto(contentDigest),
				Classification:        f.profile.Classification,
				RequiredGrantAudience: f.profile.RequiredGrantAudience,
			},
		}},
	})
	if err != nil || len(manifest) == 0 || len(manifest) > 64*1024 {
		return executiondomain.InputBundle{}, executiondomain.ToolkitExecuteReadBinding{}, ErrInvalidAuthoritativeToolkitReadInput
	}

	bundle := executiondomain.InputBundle{
		ID:        bundleID,
		Version:   bundleVersion,
		MediaType: executiondomain.InputBundleManifestMediaType,
		Digest:    runtimedomain.SHA256(manifest),
		Manifest:  manifest,
		Entries: []executiondomain.InputEntry{{
			ID:                    toolkitReadRequestEntryID,
			Version:               contentVersion,
			SemanticRole:          executiondomain.ToolkitExecuteReadRequestRole,
			ContentID:             contentID,
			MediaType:             executiondomain.ToolkitExecuteReadInputMediaType,
			Classification:        f.profile.Classification,
			RequiredGrantAudience: f.profile.RequiredGrantAudience,
			ContentDigest:         contentDigest,
			ContentLength:         int64(len(content)),
			Content:               append([]byte(nil), content...),
		}},
	}
	binding := executiondomain.ToolkitExecuteReadBinding{RequestEntryID: toolkitReadRequestEntryID}
	if err := bundle.Validate(); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitExecuteReadBinding{}, err
	}
	if err := binding.Validate(bundle); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitExecuteReadBinding{}, err
	}
	return bundle, binding, nil
}

func validateAuthoritativeInput(input *runtimev1.ToolkitExecuteReadInputV1) error {
	if input == nil || input.GetSchemaRevision() != toolkitReadSchemaRevision ||
		len(input.ProtoReflect().GetUnknown()) != 0 ||
		!validIdentity(input.GetToolkitType()) || !validIdentity(input.GetToolkitName()) ||
		!validIdentity(input.GetToolName()) ||
		len(input.GetToolkit()) > MaxCurrentReadToolSnapshotBytes ||
		len(input.GetArguments()) > MaxCurrentReadToolArgumentsBytes ||
		len(input.GetToolkitGuardrails()) > MaxCurrentReadToolGuardrailsBytes ||
		!validJSONObject(input.GetArguments()) || !validJSONObject(input.GetToolkitGuardrails()) {
		return ErrInvalidAuthoritativeToolkitReadInput
	}

	toolkit, err := decodeJSONObject(input.GetToolkit())
	if err != nil {
		return ErrInvalidAuthoritativeToolkitReadInput
	}
	id, ok := toolkit["id"].(json.Number)
	if !ok || id.String() == "" || id.String()[0] == '-' || id.String() == "0" {
		return ErrInvalidAuthoritativeToolkitReadInput
	}
	if _, err := id.Int64(); err != nil {
		return ErrInvalidAuthoritativeToolkitReadInput
	}
	if toolkit["type"] != input.GetToolkitType() || toolkit["toolkit_name"] != input.GetToolkitName() {
		return ErrInvalidAuthoritativeToolkitReadInput
	}
	if settings, ok := toolkit["settings"].(map[string]any); !ok || settings == nil {
		return ErrInvalidAuthoritativeToolkitReadInput
	}
	return nil
}

func validJSONObject(value []byte) bool {
	_, err := decodeJSONObject(value)
	return err == nil
}

func decodeJSONObject(value []byte) (map[string]any, error) {
	if len(value) == 0 || !json.Valid(value) {
		return nil, ErrInvalidAuthoritativeToolkitReadInput
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil || decoded == nil {
		return nil, ErrInvalidAuthoritativeToolkitReadInput
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidAuthoritativeToolkitReadInput
	}
	return decoded, nil
}

func toolkitReadDigestProto(digest runtimedomain.Digest) *runtimev1.DigestV1 {
	return &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     append([]byte(nil), digest[:]...),
	}
}
