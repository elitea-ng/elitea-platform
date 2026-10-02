package toolkitdiscovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	call "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

const (
	SettingsEntryID = "toolkit-settings"

	inputMediaType            = executiondomain.SettingsJSONMediaType
	maxAdmissionStringBytes   = 256
	maxDiscoveryManifestBytes = 64 * 1024
	maxDiscoveryEntryBytes    = executiondomain.MaxInputEntryContentBytes
)

var ErrInvalidAuthoritativeDiscoveryInput = errors.New("invalid authoritative toolkit discovery input")

type AuthoritativeInputs struct {
	ToolkitType    string
	ToolkitID      int64
	ToolkitVersion string
	Settings       json.RawMessage
	RuntimeContext json.RawMessage
}

func (i AuthoritativeInputs) Clone() AuthoritativeInputs {
	i.Settings = append(json.RawMessage(nil), i.Settings...)
	i.RuntimeContext = append(json.RawMessage(nil), i.RuntimeContext...)
	return i
}

func (i AuthoritativeInputs) validate() error {
	if i.ToolkitType == "" || i.ToolkitID <= 0 ||
		len(i.ToolkitType) > executiondomain.MaxSafeCommandStringBytes ||
		len(i.ToolkitVersion) > executiondomain.MaxSafeCommandStringBytes {
		return ErrInvalidAuthoritativeDiscoveryInput
	}
	if !validBoundedJSONObject(i.Settings) || !validDiscoveryRuntimeContext(i.RuntimeContext) {
		return ErrInvalidAuthoritativeDiscoveryInput
	}
	return nil
}

func validBoundedJSONObject(value []byte) bool {
	if len(value) == 0 || len(value) > maxDiscoveryEntryBytes || !json.Valid(value) {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(value, &object) == nil && object != nil
}

type InputProfile struct {
	Classification        string
	RequiredGrantAudience string
}

func (p InputProfile) validate() error {
	if p.Classification == "" || p.Classification == "synthetic" ||
		p.RequiredGrantAudience == "" ||
		len(p.Classification) > maxAdmissionStringBytes ||
		len(p.RequiredGrantAudience) > maxAdmissionStringBytes {
		return ErrInvalidAuthoritativeDiscoveryInput
	}
	return nil
}

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
		return nil, errors.New("toolkit discovery input ID generator is required")
	}
	return &InputBundleFactory{profile: profile, newID: newID}, nil
}

func (f *InputBundleFactory) Build(
	ctx context.Context,
	inputs AuthoritativeInputs,
) (executiondomain.InputBundle, executiondomain.ToolkitAvailableToolsBinding, error) {
	if err := ctx.Err(); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitAvailableToolsBinding{}, err
	}
	if err := inputs.validate(); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitAvailableToolsBinding{}, err
	}
	bundleID, err := f.newID()
	if err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitAvailableToolsBinding{},
			fmt.Errorf("generate toolkit discovery input bundle ID: %w", err)
	}
	if bundleID == "" {
		return executiondomain.InputBundle{}, executiondomain.ToolkitAvailableToolsBinding{},
			errors.New("toolkit discovery input ID generator returned an empty ID")
	}
	bundleVersion := "admission:" + bundleID

	sources := []struct {
		id      string
		role    string
		content []byte
	}{
		{id: SettingsEntryID, role: executiondomain.ToolkitAvailableToolsSettingsRole, content: inputs.Settings},
		{id: "toolkit-runtime-context", role: "toolkit.available_tools.runtime_context", content: inputs.RuntimeContext},
	}
	entries := make([]executiondomain.InputEntry, 0, len(sources))
	wireEntries := make([]*runtimev1.ExecutionInputEntryV1, 0, len(sources))
	for _, source := range sources {
		contentID, err := f.newID()
		if err != nil {
			return executiondomain.InputBundle{}, executiondomain.ToolkitAvailableToolsBinding{},
				fmt.Errorf("generate toolkit discovery input content ID: %w", err)
		}
		if contentID == "" {
			return executiondomain.InputBundle{}, executiondomain.ToolkitAvailableToolsBinding{},
				errors.New("toolkit discovery input ID generator returned an empty ID")
		}
		content := append([]byte(nil), source.content...)
		digest := runtimedomain.SHA256(content)
		version := digest.String()
		entries = append(entries, executiondomain.InputEntry{
			ID:                    source.id,
			Version:               version,
			SemanticRole:          source.role,
			ContentID:             contentID,
			MediaType:             inputMediaType,
			Classification:        f.profile.Classification,
			RequiredGrantAudience: f.profile.RequiredGrantAudience,
			ContentDigest:         digest,
			ContentLength:         int64(len(content)),
			Content:               content,
		})
		wireEntries = append(wireEntries, &runtimev1.ExecutionInputEntryV1{
			EntryId:          source.id,
			ImmutableVersion: version,
			SemanticRole:     source.role,
			Content: &runtimev1.ScopedContentReferenceV1{
				ContentId:             contentID,
				ImmutableVersion:      version,
				MediaType:             inputMediaType,
				ByteLength:            uint64(len(content)),
				Digest:                digestProto(digest),
				Classification:        f.profile.Classification,
				RequiredGrantAudience: f.profile.RequiredGrantAudience,
			},
		})
	}

	manifest, err := proto.MarshalOptions{Deterministic: true}.Marshal(
		&runtimev1.ExecutionInputBundleV1{
			InputBundleId:    bundleID,
			ImmutableVersion: bundleVersion,
			Entries:          wireEntries,
		},
	)
	if err != nil || len(manifest) == 0 || len(manifest) > maxDiscoveryManifestBytes {
		return executiondomain.InputBundle{}, executiondomain.ToolkitAvailableToolsBinding{},
			ErrInvalidAuthoritativeDiscoveryInput
	}
	bundle := executiondomain.InputBundle{
		ID:        bundleID,
		Version:   bundleVersion,
		MediaType: executiondomain.InputBundleManifestMediaType,
		Digest:    runtimedomain.SHA256(manifest),
		Manifest:  manifest,
		Entries:   entries,
	}
	binding := executiondomain.ToolkitAvailableToolsBinding{
		ToolkitType:     inputs.ToolkitType,
		ToolkitID:       inputs.ToolkitID,
		ToolkitVersion:  inputs.ToolkitVersion,
		SettingsEntryID: SettingsEntryID,
	}
	if err := bundle.Validate(); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitAvailableToolsBinding{}, err
	}
	if err := binding.Validate(bundle); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitAvailableToolsBinding{}, err
	}
	return bundle, binding, nil
}

func digestProto(digest runtimedomain.Digest) *runtimev1.DigestV1 {
	return &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     append([]byte(nil), digest[:]...),
	}
}

func validDiscoveryRuntimeContext(raw []byte) bool {
	if !validBoundedJSONObject(raw) {
		return false
	}
	var value call.RuntimeContext
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || value.ToolkitSecurity == nil || value.LLMModel != "" {
		return false
	}
	return value.ToolkitSecurity.BlockedToolkits != nil && value.ToolkitSecurity.BlockedTools != nil && value.ToolkitSecurity.SensitiveTools != nil
}
