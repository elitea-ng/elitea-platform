package toolkitcalltool

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
	// SettingsEntryID and ArgumentsEntryID are FIXED, not generated. A tool run
	// has exactly three inputs and they never vary, so a stable id lets the
	// worker, the output binding and this producer name the same entry without
	// a row to look it up in.
	SettingsEntryID       = "toolkit-settings"
	ArgumentsEntryID      = "toolkit-arguments"
	RuntimeContextEntryID = "toolkit-runtime-context"

	inputMediaType             = executiondomain.SettingsJSONMediaType
	maxAdmissionStringBytes    = 256
	maxToolRunManifestBytes    = 64 * 1024
	maxToolRunEntryBytes       = executiondomain.MaxInputEntryContentBytes
	toolRunInputSchemaRevision = "elitea.runtime.toolkit-call-tool-input.v1"
)

var ErrInvalidAuthoritativeToolRunInput = errors.New("invalid authoritative tool-run input")

// AuthoritativeInputs are selected by a project-scoped resolver AFTER toolkit
// visibility and permission. Settings arrive as the resolver froze them —
// configuration and secret references, never redeemed plaintext; the worker
// redeems them under a claimed data-plane grant. Arguments are caller content
// and are treated as such everywhere below.
type AuthoritativeInputs struct {
	ToolkitType    string
	ToolkitID      int64
	ToolkitVersion string
	ToolName       string
	Settings       json.RawMessage
	Arguments      json.RawMessage
	RuntimeContext json.RawMessage
}

func (i AuthoritativeInputs) Clone() AuthoritativeInputs {
	i.RuntimeContext = append(json.RawMessage(nil), i.RuntimeContext...)
	i.Settings = append(json.RawMessage(nil), i.Settings...)
	i.Arguments = append(json.RawMessage(nil), i.Arguments...)
	return i
}

func (i AuthoritativeInputs) validate() error {
	if i.ToolkitType == "" || i.ToolName == "" || i.ToolkitID <= 0 ||
		len(i.ToolkitType) > executiondomain.MaxSafeCommandStringBytes ||
		len(i.ToolName) > executiondomain.MaxSafeCommandStringBytes ||
		len(i.ToolkitVersion) > executiondomain.MaxSafeCommandStringBytes {
		return ErrInvalidAuthoritativeToolRunInput
	}
	if !validBoundedJSONObject(i.Settings) || !validBoundedJSONObject(i.Arguments) || !validRuntimeContext(i.RuntimeContext) {
		return ErrInvalidAuthoritativeToolRunInput
	}
	return nil
}

func validBoundedJSONObject(value []byte) bool {
	if len(value) == 0 || len(value) > maxToolRunEntryBytes || !json.Valid(value) {
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
		return ErrInvalidAuthoritativeToolRunInput
	}
	return nil
}

// InputBundleFactory stores the settings and the arguments as immutable
// data-plane content. Only their entry references are eligible for the worker
// command and for Redis.
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
		return nil, errors.New("tool-run input ID generator is required")
	}
	return &InputBundleFactory{profile: profile, newID: newID}, nil
}

func (f *InputBundleFactory) Build(
	ctx context.Context,
	inputs AuthoritativeInputs,
) (executiondomain.InputBundle, executiondomain.ToolkitCallToolBinding, error) {
	if err := ctx.Err(); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitCallToolBinding{}, err
	}
	if err := inputs.validate(); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitCallToolBinding{}, err
	}
	bundleID, err := f.newID()
	if err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitCallToolBinding{},
			fmt.Errorf("generate tool-run input bundle ID: %w", err)
	}
	if bundleID == "" {
		return executiondomain.InputBundle{}, executiondomain.ToolkitCallToolBinding{},
			errors.New("tool-run input ID generator returned an empty ID")
	}
	bundleVersion := "admission:" + bundleID

	sources := []struct {
		id      string
		role    string
		content []byte
	}{
		{id: SettingsEntryID, role: executiondomain.ToolkitCallToolSettingsRole, content: inputs.Settings},
		{id: ArgumentsEntryID, role: executiondomain.ToolkitCallToolArgumentsRole, content: inputs.Arguments},
		{id: RuntimeContextEntryID, role: executiondomain.ToolkitCallToolRuntimeContextRole, content: inputs.RuntimeContext},
	}
	entries := make([]executiondomain.InputEntry, 0, len(sources))
	wireEntries := make([]*runtimev1.ExecutionInputEntryV1, 0, len(sources))
	for _, source := range sources {
		contentID, err := f.newID()
		if err != nil {
			return executiondomain.InputBundle{}, executiondomain.ToolkitCallToolBinding{},
				fmt.Errorf("generate tool-run input content ID: %w", err)
		}
		if contentID == "" {
			return executiondomain.InputBundle{}, executiondomain.ToolkitCallToolBinding{},
				errors.New("tool-run input ID generator returned an empty ID")
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
	if err != nil || len(manifest) == 0 || len(manifest) > maxToolRunManifestBytes {
		return executiondomain.InputBundle{}, executiondomain.ToolkitCallToolBinding{},
			ErrInvalidAuthoritativeToolRunInput
	}
	bundle := executiondomain.InputBundle{
		ID:        bundleID,
		Version:   bundleVersion,
		MediaType: executiondomain.InputBundleManifestMediaType,
		Digest:    runtimedomain.SHA256(manifest),
		Manifest:  manifest,
		Entries:   entries,
	}
	binding := executiondomain.ToolkitCallToolBinding{
		ToolkitType:      inputs.ToolkitType,
		ToolName:         inputs.ToolName,
		ToolkitID:        inputs.ToolkitID,
		ToolkitVersion:   inputs.ToolkitVersion,
		SettingsEntryID:  SettingsEntryID,
		ArgumentsEntryID: ArgumentsEntryID,
	}
	if err := bundle.Validate(); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitCallToolBinding{}, err
	}
	if err := binding.Validate(bundle); err != nil {
		return executiondomain.InputBundle{}, executiondomain.ToolkitCallToolBinding{}, err
	}
	return bundle, binding, nil
}

func digestProto(digest runtimedomain.Digest) *runtimev1.DigestV1 {
	return &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     append([]byte(nil), digest[:]...),
	}
}
