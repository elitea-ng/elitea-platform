package credentialreferences

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

// rotationStore is an active Jira reference that records each stage.
type rotationStore struct {
	rec      repos.CredentialReferenceRecord
	pending  *repos.PendingRotation
	stages   []repos.RotationStage
	stageErr error
}

func newRotationStore(backend, locator string) *rotationStore {
	return &rotationStore{rec: repos.CredentialReferenceRecord{CredentialReference: credentialgrant.CredentialReference{
		ID: refID, TenantProjectID: 7, Class: credentialgrant.ClassStaticSecret, Backend: backend, Locator: locator,
		ProviderType: "jira", Generation: 3, State: credentialgrant.ReferenceActive,
	}}}
}

func (s *rotationStore) Get(_ context.Context, id string) (repos.CredentialReferenceRecord, error) {
	if id != refID {
		return repos.CredentialReferenceRecord{}, credentialgrant.ErrReferenceNotFound
	}
	return s.rec, nil
}

func (s *rotationStore) StageRotation(_ context.Context, stage repos.RotationStage) (repos.CredentialReferenceRecord, *repos.PendingRotation, error) {
	s.stages = append(s.stages, stage)
	if s.stageErr != nil {
		return repos.CredentialReferenceRecord{}, nil, s.stageErr
	}
	replaced := s.pending
	s.pending = &repos.PendingRotation{Locator: stage.Locator, Generation: stage.Generation, StagedBy: stage.StagedBy,
		StagedAt: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}
	rec := s.rec
	rec.Rotation = s.pending
	return rec, replaced, nil
}

// rotationBackend is a secret store that keeps the last written value and
// records every metadata read, write and retire.
type rotationBackend struct {
	secretstore.SecretStore
	current  int64
	written  []byte
	writes   []secretstore.SecretRef
	contexts []secretstore.StoreContext
	retired  []secretstore.SecretRef
	writeErr error
}

func (b *rotationBackend) Metadata(context.Context, secretstore.StoreContext, secretstore.SecretRef) (secretstore.Metadata, error) {
	return secretstore.Metadata{CurrentVersion: b.current}, nil
}

func (b *rotationBackend) Retire(_ context.Context, _ secretstore.StoreContext, ref secretstore.SecretRef, version int64) error {
	ref.Path += "@" + string(rune('0'+version))
	b.retired = append(b.retired, ref)
	return nil
}

func (b *rotationBackend) KVMount() string { return "kv" }

// passLedger writes straight through, as the ledger does once its row is
// staged; the CAS answer is the version after casVersion.
type passLedger struct{ cas []int64 }

func (l *passLedger) StagedWrite(_ context.Context, store secretstore.SecretStore, sc secretstore.StoreContext, ref secretstore.SecretRef,
	value secretstore.RuntimeSecret, casVersion int64) (int64, error) {
	b := store.(*rotationBackend)
	l.cas = append(l.cas, casVersion)
	b.contexts, b.writes = append(b.contexts, sc), append(b.writes, ref)
	if b.writeErr != nil {
		return 0, b.writeErr
	}
	b.written = append([]byte(nil), value.UnsafeBytes()...)
	return casVersion + 1, nil
}

func newRotator(t *testing.T, store *rotationStore, backend *rotationBackend, ledger *passLedger) *SecretRotator {
	t.Helper()
	registry, err := credentialadapters.NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	rotator, err := NewSecretRotator(store, registry, map[string]secretstore.SecretStore{store.rec.Backend: backend}, ledger, "rev-1")
	if err != nil {
		t.Fatal(err)
	}
	return rotator
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// pgvault keeps one version per name: the new value goes to a new governed_
// name at version 1, and the reference records it as pending. The value is
// the key map the broker shapes.
func TestRotatePGVaultWritesANewName(t *testing.T) {
	store := newRotationStore(secretstore.BackendPGVault, "governed_old")
	backend, ledger := &rotationBackend{}, &passLedger{}
	rec, err := newRotator(t, store, backend, ledger).Rotate(context.Background(), RotateRequest{
		ReferenceID: refID, Actor: "100", Material: map[string]string{"token": "new-token", "username": "svc@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(backend.writes) != 1 || len(store.stages) != 1 || ledger.cas[0] != 0 {
		t.Fatalf("writes %+v, stages %+v, cas %v", backend.writes, store.stages, ledger.cas)
	}
	path := backend.writes[0].Path
	if !strings.HasPrefix(path, "governed_"+strings.ReplaceAll(refID, "-", "")+"_") || path == "governed_old" {
		t.Fatalf("new name %q", path)
	}
	if !centrysecrets.ValidSecretName(path) {
		t.Fatalf("name %q is not a valid pgvault name", path)
	}
	var value map[string]string
	if err := json.Unmarshal(backend.written, &value); err != nil || value["token"] != "new-token" || value["username"] != "svc@example.com" {
		t.Fatalf("written value %q", backend.written)
	}
	sc, stage := backend.contexts[0], store.stages[0]
	if sc.WorkloadScope != repos.CredentialReferenceStagingScope || sc.PolicyRevisionID != "rev-1" || sc.CredentialRefID != refID ||
		sc.TenantProjectID != 7 || !uuidPattern.MatchString(sc.DecisionID) {
		t.Fatalf("store context %+v", sc)
	}
	if stage.Locator != path || stage.Generation != 1 || stage.DecisionID != sc.DecisionID || stage.StagedBy != "100" {
		t.Fatalf("stage %+v", stage)
	}
	if rec.Rotation == nil || rec.Locator != "governed_old" || rec.Generation != 3 {
		t.Fatalf("record %+v", rec)
	}
}

// Vault takes a new version at the same path, check-and-set on the current
// version.
func TestRotateVaultWritesANewVersion(t *testing.T) {
	store := newRotationStore(secretstore.BackendVault, vaultLocator)
	backend, ledger := &rotationBackend{current: 3}, &passLedger{}
	if _, err := newRotator(t, store, backend, ledger).Rotate(context.Background(), RotateRequest{
		ReferenceID: refID, Actor: "100", Material: map[string]string{"token": "pat"},
	}); err != nil {
		t.Fatal(err)
	}
	if backend.writes[0].Path != vaultLocator || backend.writes[0].Mount != "kv" || ledger.cas[0] != 3 || store.stages[0].Generation != 4 {
		t.Fatalf("writes %+v, cas %v, stages %+v", backend.writes, ledger.cas, store.stages)
	}
}

// A second rotation replaces the pending one and retires its version; a
// stage that fails retires the version it wrote.
func TestRotateRetiresVersionsNobodyCanActivate(t *testing.T) {
	store := newRotationStore(secretstore.BackendVault, vaultLocator)
	backend, ledger := &rotationBackend{current: 3}, &passLedger{}
	rotator := newRotator(t, store, backend, ledger)
	store.pending = &repos.PendingRotation{Locator: vaultLocator, Generation: 2, StagedBy: "101"}
	if _, err := rotator.Rotate(context.Background(), RotateRequest{ReferenceID: refID, Actor: "100", Material: map[string]string{"token": "a"}}); err != nil {
		t.Fatal(err)
	}
	if len(backend.retired) != 1 || backend.retired[0].Path != vaultLocator+"@2" {
		t.Fatalf("retired %+v", backend.retired)
	}
	store.stageErr = repos.ErrCredentialReferenceNotRotatable
	_, err := rotator.Rotate(context.Background(), RotateRequest{ReferenceID: refID, Actor: "100", Material: map[string]string{"token": "b"}})
	if !errors.Is(err, repos.ErrCredentialReferenceNotRotatable) || len(backend.retired) != 2 || backend.retired[1].Path != vaultLocator+"@4" {
		t.Fatalf("err %v, retired %+v", err, backend.retired)
	}
}

// Nothing is written for material the provider does not accept, for a
// reference that is not an active static secret, or after a store refusal.
func TestRotateRefusesBeforeWriting(t *testing.T) {
	for name, material := range map[string]map[string]string{
		"no first key": {"username": "svc"},
		"empty value":  {"token": ""},
		"unknown key":  {"token": "t", "password": "p"},
		"no material":  {},
	} {
		store := newRotationStore(secretstore.BackendPGVault, "governed_old")
		backend := &rotationBackend{}
		_, err := newRotator(t, store, backend, &passLedger{}).Rotate(context.Background(), RotateRequest{ReferenceID: refID, Actor: "100", Material: material})
		if !errors.Is(err, ErrRotationMaterial) || len(backend.writes) != 0 {
			t.Fatalf("%s: err %v, writes %d", name, err, len(backend.writes))
		}
	}
	for name, mutate := range map[string]func(*repos.CredentialReferenceRecord){
		"staged":         func(r *repos.CredentialReferenceRecord) { r.State = credentialgrant.ReferenceStaged },
		"oauth":          func(r *repos.CredentialReferenceRecord) { r.Class = credentialgrant.ClassOAuthDelegated },
		"retired":        func(r *repos.CredentialReferenceRecord) { r.State = credentialgrant.ReferenceRetired },
		"other provider": func(r *repos.CredentialReferenceRecord) { r.ProviderType = "nope" },
	} {
		store := newRotationStore(secretstore.BackendPGVault, "governed_old")
		mutate(&store.rec)
		backend := &rotationBackend{}
		_, err := newRotator(t, store, backend, &passLedger{}).Rotate(context.Background(), RotateRequest{ReferenceID: refID, Actor: "100", Material: map[string]string{"token": "t"}})
		if err == nil || len(backend.writes) != 0 {
			t.Fatalf("%s: err %v, writes %d", name, err, len(backend.writes))
		}
	}
	store := newRotationStore(secretstore.BackendPGVault, "governed_old")
	backend := &rotationBackend{writeErr: secretstore.Errorf(secretstore.ErrDenied, secretstore.OpWrite, "no")}
	_, err := newRotator(t, store, backend, &passLedger{}).Rotate(context.Background(), RotateRequest{ReferenceID: refID, Actor: "100", Material: map[string]string{"token": "t"}})
	if !errors.Is(err, ErrRotationWrite) || len(store.stages) != 0 {
		t.Fatalf("err %v, stages %d", err, len(store.stages))
	}
}

// fakeRotator answers rec or err and records the request.
type fakeRotator struct {
	got RotateRequest
	err error
}

func (f *fakeRotator) Rotate(_ context.Context, r RotateRequest) (repos.CredentialReferenceRecord, error) {
	f.got = r
	if f.err != nil {
		return repos.CredentialReferenceRecord{}, f.err
	}
	store := newRotationStore(secretstore.BackendPGVault, "governed_old")
	rec, _, _ := store.StageRotation(context.Background(), repos.RotationStage{Locator: "governed_new", Generation: 1, StagedBy: r.Actor})
	return rec, nil
}

func rotateRouter(t *testing.T, rotator Rotator) http.Handler {
	t.Helper()
	h, err := New(&fakeStore{})
	if err != nil {
		t.Fatal(err)
	}
	if rotator != nil {
		h.WithRotate(rotator)
	}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "token-1", UserID: "100"})))
		})
	})
	r.Post("/credential_references/{id}:rotate", h.Rotate)
	return r
}

// The route passes the caller as actor, shows the pending rotation, and
// never echoes the value.
func TestRotateRouteShowsThePendingRotation(t *testing.T) {
	rotator := &fakeRotator{}
	rec := serve(t, rotateRouter(t, rotator), http.MethodPost, "/credential_references/"+refID+":rotate",
		`{"material":{"token":"s3cr3t-value"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d %s", rec.Code, rec.Body)
	}
	if rotator.got.ReferenceID != refID || rotator.got.Actor != "100" {
		t.Fatalf("request %+v", rotator.got)
	}
	var got referenceView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PendingRotation == nil || got.PendingRotation.Locator != "governed_new" || got.PendingRotation.StagedBy != "100" ||
		got.Locator != "governed_old" {
		t.Fatalf("view %+v", got)
	}
	if strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Fatalf("the response echoes the value: %s", rec.Body)
	}
}

func TestRotateRouteRefusals(t *testing.T) {
	path := "/credential_references/" + refID + ":rotate"
	for _, tc := range []struct {
		name    string
		rotator Rotator
		body    string
		want    int
	}{
		{"not configured", nil, `{"material":{"token":"t"}}`, http.StatusServiceUnavailable},
		{"no material", &fakeRotator{}, `{"material":{}}`, http.StatusBadRequest},
		{"unknown field", &fakeRotator{}, `{"material":{"token":"t"},"actor":"1"}`, http.StatusBadRequest},
		{"bad material", &fakeRotator{err: ErrRotationMaterial}, `{"material":{"x":"t"}}`, http.StatusBadRequest},
		{"not rotatable", &fakeRotator{err: repos.ErrCredentialReferenceNotRotatable}, `{"material":{"token":"t"}}`, http.StatusConflict},
		{"no backend", &fakeRotator{err: ErrBackendNotConfigured}, `{"material":{"token":"t"}}`, http.StatusConflict},
		{"write failed", &fakeRotator{err: ErrRotationWrite}, `{"material":{"token":"t"}}`, http.StatusServiceUnavailable},
		{"not found", &fakeRotator{err: credentialgrant.ErrReferenceNotFound}, `{"material":{"token":"t"}}`, http.StatusNotFound},
	} {
		rec := serve(t, rotateRouter(t, tc.rotator), http.MethodPost, path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, rec.Code, rec.Body, tc.want)
		}
	}
}
