package execution

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"google.golang.org/protobuf/proto"
)

func TestValidationInputBundleFactoryBindsExactManifestAndContent(t *testing.T) {
	sequence := 0
	factory := NewConformanceValidationInputBundleFactory(func() (string, error) {
		sequence++
		return fmt.Sprintf("generated-%d", sequence), nil
	})
	settings := []byte(`{"auth_type":"Digest"}`)
	bundle, err := factory.BuildValidationInput(context.Background(), "revision-1", "settings", "revision-1", settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Entries) != 1 || bundle.Digest != runtimedomain.SHA256(bundle.Manifest) || bundle.Entries[0].ContentDigest != runtimedomain.SHA256(settings) {
		t.Fatal("bundle did not bind exact admitted bytes")
	}
	settings[0] = '['
	if string(bundle.Entries[0].Content) != `{"auth_type":"Digest"}` {
		t.Fatal("bundle content aliases caller memory")
	}

	var manifest runtimev1.ExecutionInputBundleV1
	if err := proto.Unmarshal(bundle.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.GetInputBundleId() != bundle.ID || manifest.GetImmutableVersion() != bundle.Version || len(manifest.GetEntries()) != 1 {
		t.Fatalf("unexpected manifest: %v", &manifest)
	}
	entry := manifest.GetEntries()[0]
	if entry.GetEntryId() != bundle.Entries[0].ID || entry.GetContent().GetContentId() != bundle.Entries[0].ContentID || entry.GetContent().GetByteLength() != uint64(bundle.Entries[0].ContentLength) {
		t.Fatalf("manifest does not match durable entry: %v", entry)
	}
	if got := entry.GetContent().GetDigest().GetValue(); string(got) != string(bundle.Entries[0].ContentDigest[:]) {
		t.Fatal("manifest content digest mismatch")
	}
}

func TestValidationInputBundleFactoryMatchesCheckedCrossLanguageCorpusBytes(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	corpusRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "../../../../../testdata/proto/runtime/v1/configuration-validation"))
	for _, name := range []string{"valid", "invalid", "unsupported"} {
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join(corpusRoot, name)
			manifestBytes := mustReadFile(t, filepath.Join(directory, "input-bundle.pb"))
			settings := mustReadFile(t, filepath.Join(directory, "settings.json"))
			envelopeBytes := mustReadFile(t, filepath.Join(directory, "envelope.pb"))

			manifest := &runtimev1.ExecutionInputBundleV1{}
			if err := proto.Unmarshal(manifestBytes, manifest); err != nil {
				t.Fatal(err)
			}
			if len(manifest.GetEntries()) != 1 || manifest.GetEntries()[0].GetContent() == nil {
				t.Fatalf("unexpected corpus manifest: %v", manifest)
			}
			entry := manifest.GetEntries()[0]
			ids := []string{manifest.GetInputBundleId(), entry.GetContent().GetContentId()}
			factory := NewConformanceValidationInputBundleFactory(func() (string, error) {
				if len(ids) == 0 {
					return "", fmt.Errorf("unexpected extra ID request")
				}
				id := ids[0]
				ids = ids[1:]
				return id, nil
			})
			bundle, err := factory.BuildValidationInput(context.Background(), manifest.GetImmutableVersion(), entry.GetEntryId(), entry.GetImmutableVersion(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bundle.Manifest, manifestBytes) {
				t.Fatal("Go factory bytes differ from checked Python/offline contract bytes")
			}

			envelope := &runtimev1.WorkerExecutionEnvelopeV1{}
			if err := proto.Unmarshal(envelopeBytes, envelope); err != nil {
				t.Fatal(err)
			}
			command := &runtimev1.WorkerCommandV1{}
			if err := proto.Unmarshal(envelope.GetSignedCommand().GetWorkerCommandBytes(), command); err != nil {
				t.Fatal(err)
			}
			ref := command.GetInputBundleRef()
			if ref.GetInputBundleId() != bundle.ID || ref.GetImmutableVersion() != bundle.Version || ref.GetMediaType() != bundle.MediaType || ref.GetByteLength() != uint64(len(bundle.Manifest)) || !bytes.Equal(ref.GetDigest().GetValue(), bundle.Digest[:]) {
				t.Fatalf("signed corpus command does not bind Go bundle: %v", ref)
			}
		})
	}
}

func TestValidationInputBundleFactoryRequiresExplicitNonSyntheticProductionPolicy(t *testing.T) {
	if _, err := NewValidationInputBundleFactory(ValidationInputProfile{
		SemanticRole:          "configuration.settings",
		Classification:        "synthetic",
		RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, nil); err == nil {
		t.Fatal("production constructor accepted the public conformance classification")
	}

	factory, err := NewValidationInputBundleFactory(ValidationInputProfile{
		SemanticRole:          "configuration.settings",
		Classification:        "project-confidential",
		RequiredGrantAudience: "elitea.runtime.input.read.v1",
	}, func() (string, error) { return "id", nil })
	if err != nil || factory == nil {
		t.Fatalf("explicit production policy was rejected: factory=%v err=%v", factory, err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
