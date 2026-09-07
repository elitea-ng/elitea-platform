package providerhub_test

// AdmittedManifests against a real database.
//
// The reader exists so the toolkit catalogue can project a provider's declared
// toolkits. It must agree with `LatestAdmission` about WHICH revision governs a
// provider, because a catalogue that offered a revision the request-path gate
// refuses would create a toolkit that can never run. The tests below drive the
// same states `TestLatestAdmissionIsWhatTheRequestPathObeys` drives, and assert
// the two agree.

import (
	"context"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhub"
)

func manifestsByProvider(
	t *testing.T, manifests []providerhub.AdmittedManifest,
) map[string]providerhub.AdmittedManifest {
	t.Helper()
	byProvider := make(map[string]providerhub.AdmittedManifest, len(manifests))
	for _, manifest := range manifests {
		if _, duplicate := byProvider[manifest.ProviderID]; duplicate {
			t.Fatalf("provider %q appeared twice; the reader must answer one governing revision per provider",
				manifest.ProviderID)
		}
		byProvider[manifest.ProviderID] = manifest
	}
	return byProvider
}

func TestAdmittedManifestsReturnsTheBytesThatWereRegistered(t *testing.T) {
	pool := admissionPool(t)
	ctx := context.Background()
	manifest := []byte(`{"name": "deepwiki", "provided_toolkits": [{"name": "Wikis"}]}`)

	if _, err := providerhub.Register(ctx, pool, providerhub.Registration{
		ProjectID: 1, ProviderID: "deepwiki", Origin: "https://elitea-deepwiki:8443",
		Manifest: manifest, Actor: "facade:deepwiki",
	}); err != nil {
		t.Fatal(err)
	}

	read, err := providerhub.AdmittedManifests(ctx, pool, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != 1 {
		t.Fatalf("want one governing revision, got %d", len(read))
	}
	got := read[0]
	if string(got.Manifest) != string(manifest) {
		t.Fatalf("the stored bytes came back changed: %s", got.Manifest)
	}
	if got.Digest != providerhub.Digest(manifest) {
		t.Fatalf("digest = %q", got.Digest)
	}
	if got.Status != "inactive" || got.Reason != providerhub.InactiveReason {
		t.Fatalf("a fresh registration is recorded, not in force: %+v", got)
	}
	if got.Origin != "https://elitea-deepwiki:8443" {
		t.Fatalf("origin = %q; the catalogue reports provenance without a second query", got.Origin)
	}
}

func TestAdmittedManifestsIsEmptyForAProjectNobodyRegisteredUnder(t *testing.T) {
	pool := admissionPool(t)
	ctx := context.Background()
	if _, err := providerhub.Register(ctx, pool, providerhub.Registration{
		ProjectID: 1, ProviderID: "deepwiki", Origin: "https://elitea-deepwiki:8443",
		Manifest: []byte(`{"name": "deepwiki"}`), Actor: "facade:deepwiki",
	}); err != nil {
		t.Fatal(err)
	}
	read, err := providerhub.AdmittedManifests(ctx, pool, 7)
	if err != nil {
		t.Fatalf("an empty answer is not a failure: %v", err)
	}
	if len(read) != 0 {
		t.Fatalf("a registration leaked across projects: %+v", read)
	}
}

// The DWIKI-013c ordering, from the catalogue's side: a facade that re-files an
// inactive revision after an operator activated one must not make the
// catalogue disagree with the gate.
func TestAdmittedManifestsAgreesWithLatestAdmission(t *testing.T) {
	pool := overlayPool(t)
	ctx := context.Background()

	activated := `{"name": "wikis", "provided_toolkits": [{"name": "Wikis"}]}`
	admitted := registerForActivation(t, pool, activated)
	if _, err := providerhub.Activate(ctx, pool,
		activateRequest(admitted, `{"rate_limit": 1}`)); err != nil {
		t.Fatal(err)
	}
	// The facade boots again and re-files a changed descriptor, which lands as
	// a second, inactive revision with a LATER admitted_at.
	refiled := `{"name": "wikis", "provided_toolkits": [{"name": "Wikis"}, {"name": "Query"}]}`
	if _, err := providerhub.Register(ctx, pool, providerhub.Registration{
		ProjectID: overlayProject, ProviderID: overlayProvider,
		Origin: "https://elitea-deepwiki:8443", Manifest: []byte(refiled), Actor: "facade:wikis",
	}); err != nil {
		t.Fatal(err)
	}

	latest, found, err := providerhub.LatestAdmission(ctx, pool, overlayProject, overlayProvider)
	if err != nil || !found {
		t.Fatalf("latest admission: %v found=%v", err, found)
	}
	read, err := providerhub.AdmittedManifests(ctx, pool, overlayProject)
	if err != nil {
		t.Fatal(err)
	}
	byProvider := manifestsByProvider(t, read)
	governing, present := byProvider[overlayProvider]
	if !present {
		t.Fatalf("the provider is missing from the catalogue read: %+v", read)
	}
	if governing.RevisionID != latest.RevisionID || governing.Status != latest.Status {
		t.Fatalf("the catalogue and the gate disagree: catalogue=%+v gate=%+v", governing, latest)
	}
	if governing.Status != "active" {
		t.Fatalf("an activated revision must keep governing: %+v", governing)
	}
	if string(governing.Manifest) != activated {
		t.Fatalf("the catalogue served the re-filed bytes, not the reviewed ones: %s", governing.Manifest)
	}
}

func TestAdmittedManifestsAnswersOneRowPerProvider(t *testing.T) {
	pool := admissionPool(t)
	ctx := context.Background()
	for _, provider := range []string{"deepwiki", "inventory"} {
		for _, suffix := range []string{"one", "two"} {
			if _, err := providerhub.Register(ctx, pool, providerhub.Registration{
				ProjectID: 1, ProviderID: provider, Origin: "https://" + provider + ".example",
				Manifest: []byte(`{"name": "` + provider + `", "revision": "` + suffix + `"}`),
				Actor:    "facade:" + provider,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	read, err := providerhub.AdmittedManifests(ctx, pool, 1)
	if err != nil {
		t.Fatal(err)
	}
	byProvider := manifestsByProvider(t, read)
	if len(byProvider) != 2 {
		t.Fatalf("want one row per provider, got %d rows: %+v", len(read), read)
	}
}

func TestAdmittedManifestsReportsAMissingPool(t *testing.T) {
	if _, err := providerhub.AdmittedManifests(context.Background(), nil, 1); err == nil {
		t.Fatal("a missing pool must be reported, not read as an empty catalogue")
	}
}
