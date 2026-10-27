package credentialadapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

type catalogueFile struct {
	Schema    string              `json:"schema"`
	Providers []catalogueProvider `json:"providers"`
}

type catalogueProvider struct {
	ProviderType   string              `json:"provider_type"`
	AuthClasses    []string            `json:"auth_classes"`
	TargetKind     string              `json:"target_kind"`
	MaterialKeys   map[string][]string `json:"material_keys"`
	Qualification  map[string]string   `json:"qualification"`
	ProbeOperation string              `json:"probe_operation"`
	Operations     []struct {
		ID           string `json:"id"`
		Class        string `json:"class"`
		ResourceKind string `json:"resource_kind"`
	} `json:"operations"`
}

// loadCatalogue reads the shared contract the Rust governed tool mapping is
// tested against too (repo root testdata/proto/runtime/v1).
func loadCatalogue(t *testing.T) catalogueFile {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	path := filepath.Clean(filepath.Join(filepath.Dir(self),
		"../../../../testdata/proto/runtime/v1/governed-operations/catalogue.json"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the shared catalogue: %v", err)
	}
	var out catalogueFile
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode the shared catalogue: %v", err)
	}
	if out.Schema != "elitea.runtime.governed-operations.v1" {
		t.Fatalf("catalogue schema = %q", out.Schema)
	}
	return out
}

// toCatalogue projects a descriptor onto the catalogue's fields.
func toCatalogue(d AdapterDescriptor) catalogueProvider {
	out := catalogueProvider{
		ProviderType:  d.ProviderType,
		TargetKind:    string(d.TargetKind),
		MaterialKeys:  map[string][]string{},
		Qualification: map[string]string{},
	}
	for _, c := range d.AuthClasses {
		out.AuthClasses = append(out.AuthClasses, string(c))
	}
	for c, keys := range d.MaterialKeys {
		out.MaterialKeys[string(c)] = keys
	}
	for c, q := range d.Qualification {
		out.Qualification[string(c)] = q
	}
	if d.Probe != nil {
		out.ProbeOperation = string(d.Probe.Operation)
	}
	for _, op := range d.Operations {
		out.Operations = append(out.Operations, struct {
			ID           string `json:"id"`
			Class        string `json:"class"`
			ResourceKind string `json:"resource_kind"`
		}{string(op.ID), op.Class, op.ResourceKind})
	}
	return out
}

// The built-in descriptors equal the shared catalogue exactly: same
// providers in the same order, same classes, target kinds, material keys,
// qualifications, probes and operations.
func TestBuiltinDescriptorsMatchTheSharedCatalogue(t *testing.T) {
	catalogue := loadCatalogue(t)
	builtin := Builtin()
	if len(builtin) != len(catalogue.Providers) {
		t.Fatalf("%d built-in descriptors, the catalogue lists %d", len(builtin), len(catalogue.Providers))
	}
	for i, want := range catalogue.Providers {
		got := toCatalogue(builtin[i])
		if !reflect.DeepEqual(got, want) {
			t.Errorf("provider %d differs from the catalogue\n got: %+v\nwant: %+v", i, got, want)
		}
	}
}

func TestBuiltinRegistryBuildsAndAnswersTheCatalogue(t *testing.T) {
	r, err := NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range loadCatalogue(t).Providers {
		for _, op := range p.Operations {
			id := credentialgrant.OperationID(op.ID)
			if !r.Known(id) || r.Class(id) != op.Class {
				t.Errorf("%s: known=%v class=%q, want class %q", id, r.Known(id), r.Class(id), op.Class)
			}
			if provider, ok := r.ProviderFor(id); !ok || provider != p.ProviderType {
				t.Errorf("ProviderFor(%s) = %q, %v", id, provider, ok)
			}
		}
		for class, keys := range p.MaterialKeys {
			if got := r.MaterialKeys(p.ProviderType, credentialgrant.CredentialClass(class)); !reflect.DeepEqual(got, keys) {
				t.Errorf("MaterialKeys(%s, %s) = %v, want %v", p.ProviderType, class, got, keys)
			}
		}
	}
}

// Gate D6: no built-in class claims qualification; each is fixture or
// unqualified.
func TestNoBuiltinClassClaimsQualification(t *testing.T) {
	for _, d := range Builtin() {
		for class, q := range d.Qualification {
			if q != QualificationFixture && q != QualificationUnqualified {
				t.Errorf("%s/%s is %q: no support claim before gate D6", d.ProviderType, class, q)
			}
		}
	}
}

// The provider facts of issue 1120 are encoded.
func TestProviderNotes(t *testing.T) {
	sp := SharePoint()
	if sp.Audience != graphAudience || !reflect.DeepEqual(sp.Scopes[oauthDelegated], []string{"Sites.Selected"}) {
		t.Errorf("sharepoint audience=%q scopes=%v", sp.Audience, sp.Scopes)
	}
	if q := QTest(); !reflect.DeepEqual(q.RotationRequired, []credentialgrant.CredentialClass{staticSecret}) {
		t.Errorf("qtest rotation = %v", q.RotationRequired)
	}
	for _, d := range []AdapterDescriptor{Jira(), Confluence()} {
		if d.RateLimitHint != atlassianRateLimit {
			t.Errorf("%s has no Atlassian rate-limit hint", d.ProviderType)
		}
	}
}
