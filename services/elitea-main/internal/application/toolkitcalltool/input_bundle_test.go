package toolkitcalltool

import (
	"context"
	"encoding/json"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

func newTestFactory(t *testing.T) *InputBundleFactory {
	t.Helper()
	counter := 0
	factory, err := NewInputBundleFactory(
		InputProfile{Classification: "tenant-confidential", RequiredGrantAudience: "aud"},
		func() (string, error) {
			counter++
			return "id-" + string(rune('a'+counter)), nil
		},
	)
	if err != nil {
		t.Fatalf("compose bundle factory: %v", err)
	}
	return factory
}

// Settings, arguments, and runtime context have distinct roles. One entry
// serving as both would put caller content where the platform's own redeemed
// settings belong, which is the single thing this bundle's shape prevents.
func TestBuildProducesThreeDistinctlyRoledEntries(t *testing.T) {
	bundle, binding, err := newTestFactory(t).Build(context.Background(), testInputs())
	if err != nil {
		t.Fatalf("build bundle: %v", err)
	}
	if len(bundle.Entries) != 3 {
		t.Fatalf("bundle carries %d entries", len(bundle.Entries))
	}
	roles := map[string]string{}
	for _, entry := range bundle.Entries {
		roles[entry.SemanticRole] = entry.ID
		if entry.MediaType != executiondomain.SettingsJSONMediaType {
			t.Fatalf("entry %q media type %q", entry.ID, entry.MediaType)
		}
		if entry.Classification != "tenant-confidential" || entry.RequiredGrantAudience != "aud" {
			t.Fatalf("entry %q lost its input profile", entry.ID)
		}
	}
	if roles[executiondomain.ToolkitCallToolSettingsRole] != SettingsEntryID ||
		roles[executiondomain.ToolkitCallToolArgumentsRole] != ArgumentsEntryID {
		t.Fatalf("roles are not bound to the fixed entry ids: %+v", roles)
	}
	if binding.SettingsEntryID == binding.ArgumentsEntryID {
		t.Fatal("the binding names one entry for both roles")
	}
	if err := binding.Validate(bundle); err != nil {
		t.Fatalf("the binding does not validate against its own bundle: %v", err)
	}
	if err := bundle.Validate(); err != nil {
		t.Fatalf("bundle does not validate: %v", err)
	}
}

// The manifest is what the worker resolves entries through, so it must describe
// the same three entries the bundle carries, byte for byte.
func TestBuildManifestMatchesTheBundleEntries(t *testing.T) {
	bundle, _, err := newTestFactory(t).Build(context.Background(), testInputs())
	if err != nil {
		t.Fatalf("build bundle: %v", err)
	}
	var manifest runtimev1.ExecutionInputBundleV1
	if err := proto.Unmarshal(bundle.Manifest, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.GetInputBundleId() != bundle.ID || manifest.GetImmutableVersion() != bundle.Version {
		t.Fatalf("manifest identity does not match the bundle")
	}
	if len(manifest.GetEntries()) != len(bundle.Entries) {
		t.Fatalf("manifest carries %d entries, bundle carries %d",
			len(manifest.GetEntries()), len(bundle.Entries))
	}
	for index, wire := range manifest.GetEntries() {
		entry := bundle.Entries[index]
		content := wire.GetContent()
		if wire.GetEntryId() != entry.ID || wire.GetSemanticRole() != entry.SemanticRole ||
			content.GetContentId() != entry.ContentID ||
			content.GetByteLength() != uint64(entry.ContentLength) ||
			string(content.GetDigest().GetValue()) != string(entry.ContentDigest[:]) {
			t.Fatalf("manifest entry %d does not match the bundle entry", index)
		}
	}
}

func TestBuildRefusesInputItCannotBind(t *testing.T) {
	for name, inputs := range map[string]AuthoritativeInputs{
		"no toolkit type": {ToolkitID: 1, ToolName: "t", Settings: json.RawMessage(`{}`), Arguments: json.RawMessage(`{}`)},
		"no tool name":    {ToolkitType: "github", ToolkitID: 1, Settings: json.RawMessage(`{}`), Arguments: json.RawMessage(`{}`)},
		"no toolkit id":   {ToolkitType: "github", ToolName: "t", Settings: json.RawMessage(`{}`), Arguments: json.RawMessage(`{}`)},
		"settings are not an object": {
			ToolkitType: "github", ToolkitID: 1, ToolName: "t",
			Settings: json.RawMessage(`[]`), Arguments: json.RawMessage(`{}`),
		},
		"arguments are not an object": {
			ToolkitType: "github", ToolkitID: 1, ToolName: "t",
			Settings: json.RawMessage(`{}`), Arguments: json.RawMessage(`"x"`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := newTestFactory(t).Build(context.Background(), inputs); err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
}
