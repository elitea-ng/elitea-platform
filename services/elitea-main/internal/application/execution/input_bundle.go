package execution

import (
	"context"
	"errors"
	"fmt"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

const (
	validationSettingsRole        = "configuration.settings"
	conformanceClassification     = "synthetic"
	conformanceInputGrantAudience = "elitea.runtime.input.read.v1"
)

var ErrInvalidValidationInputProfile = errors.New("invalid validation input profile")

// ValidationInputProfile is an explicit policy input. Classification and
// grant audience are not inferred from a configuration type: settings bytes
// can carry tenant-confidential business data for every provider.
type ValidationInputProfile struct {
	SemanticRole          string
	Classification        string
	RequiredGrantAudience string
}

func (p ValidationInputProfile) validateProduction() error {
	if p.SemanticRole != validationSettingsRole || p.Classification == "" || p.RequiredGrantAudience == "" || p.Classification == conformanceClassification {
		return ErrInvalidValidationInputProfile
	}
	return nil
}

type ValidationInputBundleFactory struct {
	profile ValidationInputProfile
	newID   IDGenerator
}

// NewValidationInputBundleFactory constructs a production factory. Synthetic
// classification is deliberately rejected; it is reserved for public
// conformance fixtures through NewConformanceValidationInputBundleFactory.
func NewValidationInputBundleFactory(profile ValidationInputProfile, newID IDGenerator) (*ValidationInputBundleFactory, error) {
	if err := profile.validateProduction(); err != nil {
		return nil, err
	}
	if newID == nil {
		newID = randomID
	}
	return &ValidationInputBundleFactory{profile: profile, newID: newID}, nil
}

// NewConformanceValidationInputBundleFactory is the only constructor that
// emits the public "synthetic" classification used by checked test vectors.
func NewConformanceValidationInputBundleFactory(newID IDGenerator) *ValidationInputBundleFactory {
	if newID == nil {
		newID = randomID
	}
	return &ValidationInputBundleFactory{
		profile: ValidationInputProfile{
			SemanticRole:          validationSettingsRole,
			Classification:        conformanceClassification,
			RequiredGrantAudience: conformanceInputGrantAudience,
		},
		newID: newID,
	}
}

func (f *ValidationInputBundleFactory) BuildValidationInput(_ context.Context, configurationRevisionID, entryID, entryVersion string, settings []byte) (executiondomain.InputBundle, error) {
	if configurationRevisionID == "" || entryID == "" || entryVersion == "" || len(settings) == 0 {
		return executiondomain.InputBundle{}, executiondomain.ErrInvalidInputBundle
	}
	bundleID, err := f.newID()
	if err != nil {
		return executiondomain.InputBundle{}, fmt.Errorf("generate input bundle ID: %w", err)
	}
	contentID, err := f.newID()
	if err != nil {
		return executiondomain.InputBundle{}, fmt.Errorf("generate input content ID: %w", err)
	}
	if bundleID == "" || contentID == "" {
		return executiondomain.InputBundle{}, errors.New("input bundle ID generator returned an empty ID")
	}

	content := append([]byte(nil), settings...)
	contentDigest := runtimedomain.SHA256(content)
	manifest := &runtimev1.ExecutionInputBundleV1{
		InputBundleId:    bundleID,
		ImmutableVersion: configurationRevisionID,
		Entries: []*runtimev1.ExecutionInputEntryV1{{
			EntryId:          entryID,
			ImmutableVersion: entryVersion,
			SemanticRole:     f.profile.SemanticRole,
			Content: &runtimev1.ScopedContentReferenceV1{
				ContentId:             contentID,
				ImmutableVersion:      entryVersion,
				MediaType:             executiondomain.SettingsJSONMediaType,
				ByteLength:            uint64(len(content)),
				Digest:                digestProto(contentDigest),
				Classification:        f.profile.Classification,
				RequiredGrantAudience: f.profile.RequiredGrantAudience,
			},
		}},
	}
	manifestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(manifest)
	if err != nil {
		return executiondomain.InputBundle{}, fmt.Errorf("encode input bundle manifest: %w", err)
	}

	bundle := executiondomain.InputBundle{
		ID:        bundleID,
		Version:   configurationRevisionID,
		MediaType: executiondomain.InputBundleManifestMediaType,
		Digest:    runtimedomain.SHA256(manifestBytes),
		Manifest:  manifestBytes,
		Entries: []executiondomain.InputEntry{{
			ID:                    entryID,
			Version:               entryVersion,
			SemanticRole:          f.profile.SemanticRole,
			ContentID:             contentID,
			MediaType:             executiondomain.SettingsJSONMediaType,
			Classification:        f.profile.Classification,
			RequiredGrantAudience: f.profile.RequiredGrantAudience,
			ContentDigest:         contentDigest,
			ContentLength:         int64(len(content)),
			Content:               content,
		}},
	}
	if err := bundle.Validate(); err != nil {
		return executiondomain.InputBundle{}, err
	}
	return bundle, nil
}

func digestProto(digest runtimedomain.Digest) *runtimev1.DigestV1 {
	return &runtimev1.DigestV1{
		Algorithm: runtimev1.DigestAlgorithmV1_DIGEST_ALGORITHM_V1_SHA256,
		Value:     append([]byte(nil), digest[:]...),
	}
}
