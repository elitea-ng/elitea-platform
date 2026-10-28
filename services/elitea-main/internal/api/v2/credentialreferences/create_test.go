package credentialreferences

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

// fakeLocators answers like a store holding versions of the secrets it
// lists, and records every check.
type fakeLocators struct {
	versions map[string][]int64 // backend + ":" + locator
	err      error
	checks   []LocatorCheck
}

func (l *fakeLocators) Versions(_ context.Context, check LocatorCheck) ([]int64, int64, error) {
	l.checks = append(l.checks, check)
	if l.err != nil {
		return nil, 0, l.err
	}
	versions, ok := l.versions[check.Backend+":"+check.Locator]
	if !ok {
		return nil, 0, ErrLocatorNotFound
	}
	return versions, versions[len(versions)-1], nil
}

const vaultLocator = "elitea/projects/7/credentials/jira-prod"

func createRequest(fields map[string]any) string {
	body := map[string]any{
		"project_id": 7, "provider_type": "jira", "target_identity": "https://ACME.atlassian.net/",
		"class": "static_secret", "backend": "vault", "locator": vaultLocator,
	}
	for key, value := range fields {
		if value == nil {
			delete(body, key)
			continue
		}
		body[key] = value
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// A static secret is staged with the caller as stager, at the current
// version the store reports, after a metadata check that names the new
// reference's id; the target is canonicalised by the adapter.
func TestCreateStagesWithTheCallerAsStager(t *testing.T) {
	store := &fakeStore{}
	locators := &fakeLocators{versions: map[string][]int64{"vault:" + vaultLocator: {1, 2, 3}}}
	rec := serve(t, routerWith(t, store, "100", locators), http.MethodPost, "/credential_references", createRequest(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var got referenceView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "staged" || got.StagedBy == nil || *got.StagedBy != "100" || got.Generation != 3 ||
		got.TargetIdentity != "https://acme.atlassian.net" || got.MigratedFrom != nil {
		t.Fatalf("view = %+v", got)
	}
	if len(store.staged) != 1 || len(locators.checks) != 1 {
		t.Fatalf("staged %d, checks %d", len(store.staged), len(locators.checks))
	}
	staged, check := store.staged[0], locators.checks[0]
	if staged.ID == "" || check.ReferenceID != staged.ID || check.TenantProjectID != 7 || staged.MigratedFrom != "" {
		t.Fatalf("staged %+v, check %+v", staged, check)
	}
	// The response never carries anything but the reference's fields.
	if strings.Contains(rec.Body.String(), "value") {
		t.Fatalf("response = %s", rec.Body)
	}
}

func TestCreatePinsANamedGenerationOnlyWhenReadable(t *testing.T) {
	locators := &fakeLocators{versions: map[string][]int64{"vault:" + vaultLocator: {2, 3}}}
	store := &fakeStore{}
	h := routerWith(t, store, "100", locators)
	if rec := serve(t, h, http.MethodPost, "/credential_references", createRequest(map[string]any{"generation": 2})); rec.Code != http.StatusCreated {
		t.Fatalf("readable generation = %d %s", rec.Code, rec.Body)
	}
	if store.staged[0].Generation != 2 {
		t.Fatalf("generation = %d", store.staged[0].Generation)
	}
	if rec := serve(t, h, http.MethodPost, "/credential_references", createRequest(map[string]any{"generation": 1})); rec.Code != http.StatusConflict {
		t.Fatalf("destroyed generation = %d, want 409", rec.Code)
	}
}

func TestCreateRefusesALocatorTheStoreDoesNotHold(t *testing.T) {
	for name, tc := range map[string]struct {
		locators *fakeLocators
		want     int
	}{
		"absent":         {&fakeLocators{}, http.StatusConflict},
		"no backend":     {&fakeLocators{err: ErrBackendNotConfigured}, http.StatusConflict},
		"store is down":  {&fakeLocators{err: ErrLocatorCheckUnavailable}, http.StatusServiceUnavailable},
		"other failures": {&fakeLocators{err: errors.New("boom")}, http.StatusServiceUnavailable},
	} {
		store := &fakeStore{}
		rec := serve(t, routerWith(t, store, "100", tc.locators), http.MethodPost, "/credential_references", createRequest(nil))
		if rec.Code != tc.want || len(store.staged) != 0 {
			t.Fatalf("%s: = %d (staged %d), want %d", name, rec.Code, len(store.staged), tc.want)
		}
		if strings.Contains(rec.Body.String(), "boom") {
			t.Fatalf("%s: a store error reached the caller: %s", name, rec.Body)
		}
	}
}

func TestCreateValidatesTheBody(t *testing.T) {
	locators := &fakeLocators{versions: map[string][]int64{"vault:" + vaultLocator: {1}, "pgvault:jira_token": {1}}}
	for name, fields := range map[string]map[string]any{
		"no project":              {"project_id": nil},
		"unknown provider":        {"provider_type": "nope"},
		"bad target":              {"target_identity": "ftp://jira"},
		"class not accepted":      {"provider_type": "sharepoint", "target_identity": "https://acme.sharepoint.com/sites/x"},
		"unknown class":           {"class": "magic"},
		"unsupported class":       {"class": "oauth_service"},
		"bad backend":             {"backend": "s3"},
		"no locator":              {"locator": nil},
		"other tenant's path":     {"locator": "elitea/projects/8/credentials/jira-prod"},
		"negative generation":     {"generation": -1},
		"oauth with a locator":    {"class": "oauth_delegated", "locator": "x", "backend": nil},
		"oauth with a generation": {"class": "oauth_delegated", "locator": nil, "backend": nil, "generation": 2},
		"unknown field":           {"staged_by": "200"},
	} {
		store := &fakeStore{}
		rec := serve(t, routerWith(t, store, "100", locators), http.MethodPost, "/credential_references", createRequest(fields))
		if rec.Code != http.StatusBadRequest || len(store.staged) != 0 {
			t.Fatalf("%s: = %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	// pgvault: the locator is a secret name in the project's vault.
	store := &fakeStore{}
	rec := serve(t, routerWith(t, store, "100", locators), http.MethodPost, "/credential_references",
		createRequest(map[string]any{"backend": "pgvault", "locator": "jira_token"}))
	if rec.Code != http.StatusCreated || store.staged[0].Generation != 1 {
		t.Fatalf("pgvault = %d %s", rec.Code, rec.Body)
	}
}

// An oauth_delegated reference has no stored secret: nothing is checked,
// and it is staged at the consent-grant locator.
func TestCreateOAuthDelegatedChecksNoStore(t *testing.T) {
	store := &fakeStore{}
	locators := &fakeLocators{}
	rec := serve(t, routerWith(t, store, "100", locators), http.MethodPost, "/credential_references",
		createRequest(map[string]any{"class": "oauth_delegated", "backend": nil, "locator": nil}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("oauth = %d %s", rec.Code, rec.Body)
	}
	if len(locators.checks) != 0 || store.staged[0].Locator != "oauth_grant" || store.staged[0].Backend != secretstore.BackendPGVault ||
		store.staged[0].Class != credentialgrant.ClassOAuthDelegated {
		t.Fatalf("checks %v, staged %+v", locators.checks, store.staged[0])
	}
}

func TestCreateNeedsAResolvedCallerAndTheDependencies(t *testing.T) {
	store := &fakeStore{}
	if got := serve(t, routerWith(t, store, "", &fakeLocators{}), http.MethodPost, "/credential_references", createRequest(nil)).Code; got != http.StatusForbidden {
		t.Fatalf("no resolved user = %d, want 403", got)
	}
	if got := serve(t, router(t, store, "100"), http.MethodPost, "/credential_references", createRequest(nil)).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("without WithCreate = %d, want 503", got)
	}
}

// The locator check over real SecretStore values: Metadata only, the
// staging workload scope, the reference id and the active revision.
type metadataOnlyStore struct {
	secretstore.SecretStore
	contexts []secretstore.StoreContext
	refs     []secretstore.SecretRef
	meta     secretstore.Metadata
	err      error
}

func (s *metadataOnlyStore) Metadata(_ context.Context, sc secretstore.StoreContext, ref secretstore.SecretRef) (secretstore.Metadata, error) {
	s.contexts, s.refs = append(s.contexts, sc), append(s.refs, ref)
	return s.meta, s.err
}

func (s *metadataOnlyStore) KVMount() string { return "kv" }

func TestSecretStoreLocatorsReadMetadataOnly(t *testing.T) {
	store := &metadataOnlyStore{meta: secretstore.Metadata{CurrentVersion: 4, Versions: []int64{3, 4}}}
	locators, err := NewSecretStoreLocators(map[string]secretstore.SecretStore{secretstore.BackendVault: store}, "2026-10-r1")
	if err != nil {
		t.Fatal(err)
	}
	versions, current, err := locators.Versions(t.Context(), LocatorCheck{TenantProjectID: 7, ReferenceID: refID, Backend: "vault", Locator: vaultLocator})
	if err != nil || current != 4 || len(versions) != 2 {
		t.Fatalf("versions %v current %d err %v", versions, current, err)
	}
	sc, ref := store.contexts[0], store.refs[0]
	if sc.WorkloadScope != "credential-migration" || sc.CredentialRefID != refID || sc.PolicyRevisionID != "2026-10-r1" ||
		sc.TenantProjectID != 7 || sc.DecisionID != "" || ref.Mount != "kv" || ref.Path != vaultLocator {
		t.Fatalf("context %+v ref %+v", sc, ref)
	}
	if _, _, err := locators.Versions(t.Context(), LocatorCheck{TenantProjectID: 7, ReferenceID: refID, Backend: "pgvault", Locator: "x"}); !errors.Is(err, ErrBackendNotConfigured) {
		t.Fatalf("unconfigured backend = %v", err)
	}
	store.err = secretstore.Errorf(secretstore.ErrNotFound, secretstore.OpMetadata, "no such secret")
	if _, _, err := locators.Versions(t.Context(), LocatorCheck{TenantProjectID: 7, ReferenceID: refID, Backend: "vault", Locator: vaultLocator}); !errors.Is(err, ErrLocatorNotFound) {
		t.Fatalf("absent = %v", err)
	}
	store.err = secretstore.Errorf(secretstore.ErrDenied, secretstore.OpMetadata, "denied")
	if _, _, err := locators.Versions(t.Context(), LocatorCheck{TenantProjectID: 7, ReferenceID: refID, Backend: "vault", Locator: vaultLocator}); !errors.Is(err, ErrLocatorCheckUnavailable) {
		t.Fatalf("denied = %v", err)
	}
	if _, err := NewSecretStoreLocators(nil, "r"); err == nil {
		t.Fatal("no stores accepted")
	}
}

var _ Providers = (*credentialadapters.Registry)(nil)
