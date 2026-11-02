package credentialreferences

// POST /api/v2/admin/credential_references/{id}:rotate (#1505): rotate the
// secret of an active static_secret reference without a retire and a
// restage.
//
//   - The new value is written through the secret write ledger: a new
//     version at the same Vault path (check-and-set on the current version),
//     or a new pgvault name, because a pgvault secret has one version only.
//   - The written version is recorded as the reference's PENDING rotation.
//     The broker still releases the current value.
//   - A second operator calls :activate. The reference then moves to the new
//     locator and generation, with its audit event (the same two-person rule
//     as a new reference).
//
// The value is in the request body only. It is never returned, logged or
// stored outside the secret store.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

// maxMaterialValueBytes bounds one material value; maxRotateBodyBytes
// bounds the request body.
const (
	maxMaterialValueBytes = 16 << 10
	maxRotateBodyBytes    = 64 << 10
)

// Rotation refusals. Each message is safe to return.
var (
	// ErrRotationMaterial is a material object the provider's static_secret
	// keys do not accept.
	ErrRotationMaterial = errors.New("credential reference rotation: material must name the provider's static_secret keys, the first one required, each a non-empty string")
	// ErrRotationWrite is a secret store that refused or could not take the
	// new value; nothing was recorded on the reference.
	ErrRotationWrite = errors.New("credential reference rotation: the secret store did not take the new value; nothing changed")
)

// RotateRequest is one rotation. Material maps the provider's static_secret
// material keys to their new values.
type RotateRequest struct {
	ReferenceID string
	Material    map[string]string
	// Actor is the operator's auth_core user id.
	Actor string
}

// Rotator writes the new secret and stages it on the reference.
type Rotator interface {
	Rotate(ctx context.Context, r RotateRequest) (repos.CredentialReferenceRecord, error)
}

// WithRotate enables POST …/{id}:rotate. Without it the route answers 503.
func (h *Handler) WithRotate(rotator Rotator) *Handler {
	if h != nil {
		h.rotator = rotator
	}
	return h
}

type rotateBody struct {
	Material map[string]string `json:"material"`
}

// Rotate serves POST …/{id}:rotate.
func (h *Handler) Rotate(w http.ResponseWriter, r *http.Request) {
	who, ok := actor(w, r)
	if !ok {
		return
	}
	if h.rotator == nil {
		writeError(w, http.StatusServiceUnavailable, "credential reference rotation is not configured")
		return
	}
	var body rotateBody
	if !decodeWithin(w, r, &body, maxRotateBodyBytes) {
		return
	}
	defer clearMaterial(body.Material)
	if len(body.Material) == 0 {
		writeError(w, http.StatusBadRequest, ErrRotationMaterial.Error())
		return
	}
	rec, err := h.rotator.Rotate(r.Context(), RotateRequest{ReferenceID: pathID(r, "rotate"), Material: body.Material, Actor: who})
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, view(rec))
	case errors.Is(err, ErrRotationMaterial):
		writeError(w, http.StatusBadRequest, ErrRotationMaterial.Error())
	case errors.Is(err, ErrBackendNotConfigured):
		writeError(w, http.StatusConflict, ErrBackendNotConfigured.Error())
	case errors.Is(err, ErrRotationWrite):
		writeError(w, http.StatusServiceUnavailable, ErrRotationWrite.Error())
	default:
		writeStoreError(w, err)
	}
}

// clearMaterial drops the request's values. Go strings cannot be zeroed;
// this only ends their reachability early.
func clearMaterial(material map[string]string) {
	for key := range material {
		delete(material, key)
	}
}

// RotationStore is the reference half a rotation needs
// (*repos.CredentialReferences).
type RotationStore interface {
	Get(ctx context.Context, id string) (repos.CredentialReferenceRecord, error)
	StageRotation(ctx context.Context, stage repos.RotationStage) (repos.CredentialReferenceRecord, *repos.PendingRotation, error)
}

var _ RotationStore = (*repos.CredentialReferences)(nil)

// SecretWriter is the secret write ledger (*secretstore.Ledger).
type SecretWriter interface {
	StagedWrite(ctx context.Context, store secretstore.SecretStore, sc secretstore.StoreContext, ref secretstore.SecretRef,
		value secretstore.RuntimeSecret, casVersion int64) (int64, error)
}

var _ SecretWriter = (*secretstore.Ledger)(nil)

// SecretRotator is the production Rotator: the governed secret stores the
// broker reads, written through the ledger under the active policy
// revision.
type SecretRotator struct {
	store            RotationStore
	providers        Providers
	stores           map[string]secretstore.SecretStore
	ledger           SecretWriter
	policyRevisionID string
	random           func([]byte) error
}

// NewSecretRotator builds the rotator.
func NewSecretRotator(store RotationStore, providers Providers, stores map[string]secretstore.SecretStore,
	ledger SecretWriter, policyRevisionID string) (*SecretRotator, error) {
	if store == nil || providers == nil || len(stores) == 0 || ledger == nil || policyRevisionID == "" {
		return nil, errors.New("credential reference rotation: the store, the providers, the secret stores, the ledger and the policy revision are required")
	}
	return &SecretRotator{
		store: store, providers: providers, stores: stores, ledger: ledger, policyRevisionID: policyRevisionID,
		random: func(b []byte) error { _, err := rand.Read(b); return err },
	}, nil
}

// RotationLocatorPrefix starts the pgvault name of a rotated secret. It
// keeps the governed_ prefix of a migrated composite secret.
const RotationLocatorPrefix = "governed_"

// Rotate implements Rotator.
func (s *SecretRotator) Rotate(ctx context.Context, r RotateRequest) (repos.CredentialReferenceRecord, error) {
	current, err := s.store.Get(ctx, r.ReferenceID)
	if err != nil {
		return repos.CredentialReferenceRecord{}, err
	}
	if current.State != credentialgrant.ReferenceActive || current.Class != credentialgrant.ClassStaticSecret {
		return repos.CredentialReferenceRecord{}, repos.ErrCredentialReferenceNotRotatable
	}
	descriptor, ok := s.providers.Descriptor(current.ProviderType)
	if !ok {
		return repos.CredentialReferenceRecord{}, ErrRotationMaterial
	}
	value, err := materialValue(descriptor.MaterialKeys[credentialgrant.ClassStaticSecret], r.Material)
	if err != nil {
		return repos.CredentialReferenceRecord{}, err
	}
	secret := secretstore.NewRuntimeSecret(value)
	clear(value)
	defer secret.Destroy()

	store, ok := s.stores[current.Backend]
	if !ok || store == nil {
		return repos.CredentialReferenceRecord{}, ErrBackendNotConfigured
	}
	decisionID, err := s.token(16)
	if err != nil {
		return repos.CredentialReferenceRecord{}, err
	}
	decisionID = uuidOf(decisionID)
	sc := secretstore.StoreContext{
		TenantProjectID: current.TenantProjectID, CredentialRefID: current.ID, DecisionID: decisionID,
		WorkloadScope: repos.CredentialReferenceStagingScope, PolicyRevisionID: s.policyRevisionID,
	}
	ref := secretstore.SecretRef{Backend: current.Backend, Path: current.Locator}
	if m, ok := store.(mounted); ok {
		ref.Mount = m.KVMount()
	}
	var cas int64
	switch current.Backend {
	case secretstore.BackendVault:
		// A new version at the same path, on top of the current one.
		meta, err := store.Metadata(ctx, sc, ref)
		if err != nil {
			return repos.CredentialReferenceRecord{}, ErrRotationWrite
		}
		cas = meta.CurrentVersion
	case secretstore.BackendPGVault:
		// One version per name: the new value gets a new name.
		suffix, err := s.token(8)
		if err != nil {
			return repos.CredentialReferenceRecord{}, err
		}
		ref.Path = RotationLocatorPrefix + strings.ReplaceAll(current.ID, "-", "") + "_" + suffix
	default:
		return repos.CredentialReferenceRecord{}, ErrBackendNotConfigured
	}
	version, err := s.ledger.StagedWrite(ctx, store, sc, ref, secret, cas)
	if err != nil {
		return repos.CredentialReferenceRecord{}, ErrRotationWrite
	}
	staged, replaced, err := s.store.StageRotation(ctx, repos.RotationStage{
		ReferenceID: current.ID, Locator: ref.Path, Generation: version, DecisionID: decisionID, StagedBy: r.Actor,
	})
	if err != nil {
		// The version stays unreferenced; retire it so the store does not
		// keep a value nobody can activate.
		s.retire(ctx, store, sc, current.Backend, ref.Mount, ref.Path, version)
		return repos.CredentialReferenceRecord{}, err
	}
	if replaced != nil && (replaced.Locator != ref.Path || replaced.Generation != version) {
		// The rotation this one replaces will never be activated.
		s.retire(ctx, store, sc, current.Backend, ref.Mount, replaced.Locator, replaced.Generation)
	}
	return staged, nil
}

// retire is best effort: a version left behind is unreferenced, and the
// broker never reads it.
func (s *SecretRotator) retire(ctx context.Context, store secretstore.SecretStore, sc secretstore.StoreContext,
	backend, mount, path string, version int64) {
	_ = store.Retire(context.WithoutCancel(ctx), sc, secretstore.SecretRef{Backend: backend, Mount: mount, Path: path}, version)
}

func (s *SecretRotator) token(n int) (string, error) {
	b := make([]byte, n)
	if err := s.random(b); err != nil {
		return "", errors.New("credential reference rotation: random source failed")
	}
	return hex.EncodeToString(b), nil
}

// uuidOf formats 32 hex digits as a UUID (version 4, RFC 9562 variant).
func uuidOf(hexDigits string) string {
	b := []byte(hexDigits)
	b[12] = '4'
	b[16] = "89ab"[strings.IndexByte("0123456789abcdef", b[16])%4]
	return string(b[0:8]) + "-" + string(b[8:12]) + "-" + string(b[12:16]) + "-" + string(b[16:20]) + "-" + string(b[20:32])
}

// materialValue is the key map the broker shapes into the catalogue's
// static_secret keys: a JSON object of the provider's keys. The first key
// is required; every key must be one the provider names.
func materialValue(keys []string, material map[string]string) ([]byte, error) {
	if len(keys) == 0 {
		return nil, ErrRotationMaterial
	}
	if value, ok := material[keys[0]]; !ok || value == "" {
		return nil, ErrRotationMaterial
	}
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	for key, value := range material {
		if !allowed[key] || value == "" || len(value) > maxMaterialValueBytes {
			return nil, ErrRotationMaterial
		}
	}
	raw, err := json.Marshal(material)
	if err != nil {
		return nil, ErrRotationMaterial
	}
	return raw, nil
}
