package credentialreferences

// POST /api/v2/admin/credential_references (#1482): create a credential
// reference from scratch, for a governed integration with no toolkit
// configuration for migrate-references to start from.
//
//   - The reference is written `staged`, with the caller as `staged_by`,
//     through the same repos.CredentialReferences.StageReference the
//     migration uses (target binding upsert, insert, audit decision, one
//     transaction). Activation stays the two-person `:activate`.
//   - The target is canonicalised by the provider adapter, as the migration
//     does, so one target has one target binding whichever path staged it.
//   - A static_secret locator is checked against the secret store with a
//     Metadata call only: the secret's value is never read, returned or
//     logged. The check pins the generation the broker will read.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

// maxLocatorBytes is the 0184 CHECK on credential_references.locator.
const maxLocatorBytes = 512

// Typed refusals of the locator check. Each message is safe to return.
var (
	// ErrLocatorNotFound is a locator that names no secret, or a generation
	// that is not a readable version of it.
	ErrLocatorNotFound = errors.New("credential reference: the locator names no readable secret version in the secret store")
	// ErrBackendNotConfigured is a backend this deployment has no store for.
	ErrBackendNotConfigured = errors.New("credential reference: the backend is not configured on this deployment")
	// ErrLocatorCheckUnavailable is a store that could not answer.
	ErrLocatorCheckUnavailable = errors.New("credential reference: the secret store could not check the locator")
)

// LocatorCheck names the secret a new reference will point at.
type LocatorCheck struct {
	TenantProjectID int64
	// ReferenceID is the id the reference will be written with: the store
	// call names the reference it serves.
	ReferenceID string
	Backend     string
	Locator     string
}

// Locators answers which versions of a secret exist, without reading one.
type Locators interface {
	// Versions returns the secret's readable versions, ascending, and its
	// current version. It fails with ErrLocatorNotFound,
	// ErrBackendNotConfigured or ErrLocatorCheckUnavailable.
	Versions(ctx context.Context, check LocatorCheck) (versions []int64, current int64, err error)
}

// Providers is the governed provider registry (*credentialadapters.Registry).
type Providers interface {
	Descriptor(providerType string) (credentialadapters.AdapterDescriptor, bool)
}

var _ Providers = (*credentialadapters.Registry)(nil)

// WithCreate enables POST /admin/credential_references. Without it the
// route answers 503.
func (h *Handler) WithCreate(locators Locators, providers Providers) *Handler {
	if h != nil {
		h.locators, h.providers = locators, providers
	}
	return h
}

type createBody struct {
	ProjectID      int64  `json:"project_id"`
	ProviderType   string `json:"provider_type"`
	TargetIdentity string `json:"target_identity"`
	Class          string `json:"class"`
	Backend        string `json:"backend"`
	Locator        string `json:"locator"`
	Generation     int64  `json:"generation"`
}

// Create serves POST /admin/credential_references.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	who, ok := actor(w, r)
	if !ok {
		return
	}
	if h.locators == nil || h.providers == nil {
		writeError(w, http.StatusServiceUnavailable, "credential reference creation is not configured")
		return
	}
	var body createBody
	if !decode(w, r, &body) {
		return
	}
	staged, message := h.validateCreate(body)
	if message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	staged.ID, staged.StagedBy = h.newID(), who
	if staged.Class == credentialgrant.ClassStaticSecret {
		versions, current, err := h.locators.Versions(r.Context(), LocatorCheck{
			TenantProjectID: staged.TenantProjectID, ReferenceID: staged.ID,
			Backend: staged.Backend, Locator: staged.Locator,
		})
		if err != nil {
			writeLocatorError(w, err)
			return
		}
		switch {
		case staged.Generation == 0 && current > 0:
			staged.Generation = current
		case staged.Generation == 0, !containsVersion(versions, staged.Generation):
			writeError(w, http.StatusConflict, ErrLocatorNotFound.Error())
			return
		}
	}
	result, err := h.store.StageReference(r.Context(), staged, h.newID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view(result.Record))
}

// validateCreate checks the body and returns the reference to stage, or a
// message for a 400. The target is canonicalised by the provider adapter.
func (h *Handler) validateCreate(body createBody) (repos.StagedReference, string) {
	if body.ProjectID <= 0 {
		return repos.StagedReference{}, "project_id is required"
	}
	descriptor, ok := h.providers.Descriptor(body.ProviderType)
	if !ok || descriptor.CanonicalizeTarget == nil {
		return repos.StagedReference{}, "provider_type is not a governed provider"
	}
	target, err := descriptor.CanonicalizeTarget(body.TargetIdentity)
	if err != nil || target == "" {
		return repos.StagedReference{}, "target_identity is not a valid target of the provider"
	}
	class := credentialgrant.CredentialClass(body.Class)
	if !class.Valid() || !descriptor.Accepts(class) {
		return repos.StagedReference{}, "class is not a credential class the provider accepts"
	}
	staged := repos.StagedReference{
		TenantProjectID: body.ProjectID, ProviderType: descriptor.ProviderType, TargetIdentity: target,
		Class: class, Backend: body.Backend, Locator: body.Locator, Generation: body.Generation,
	}
	switch class {
	case credentialgrant.ClassStaticSecret:
		switch {
		case body.Backend != secretstore.BackendPGVault && body.Backend != secretstore.BackendVault:
			return repos.StagedReference{}, "backend must be pgvault or vault"
		case body.Locator == "" || len(body.Locator) > maxLocatorBytes:
			return repos.StagedReference{}, "locator is required, at most 512 bytes"
		case body.Generation < 0:
			return repos.StagedReference{}, "generation must not be negative"
		case body.Backend == secretstore.BackendVault && !vaultLocatorUnder(body.Locator, vaultPrefix(body.ProjectID)):
			// The tenant boundary: a Vault path of another project is
			// never referenced from this one, including by climbing out of
			// the prefix with a ".." segment.
			return repos.StagedReference{}, "a vault locator must be a plain path under " + vaultPrefix(body.ProjectID)
		}
	case credentialgrant.ClassOAuthDelegated:
		// The material is the consent grant; nothing is stored to check.
		if (body.Backend != "" && body.Backend != secretstore.BackendPGVault) ||
			(body.Locator != "" && body.Locator != repos.CredentialReferenceOAuthLocator) || body.Generation != 0 {
			return repos.StagedReference{}, "an oauth_delegated reference takes no backend, locator or generation"
		}
		staged.Backend, staged.Locator = secretstore.BackendPGVault, repos.CredentialReferenceOAuthLocator
	default:
		return repos.StagedReference{}, "only static_secret and oauth_delegated references can be created"
	}
	return staged, ""
}

// vaultPrefix is the Vault path tree of one project's credentials, the one
// migrate-references copies into and the documented role policies cover.
func vaultPrefix(projectID int64) string {
	return "elitea/projects/" + strconv.FormatInt(projectID, 10) + "/credentials/"
}

// vaultLocatorUnder reports whether locator is a path below prefix with no
// empty, "." or ".." segment, so a prefix match cannot be climbed out of.
func vaultLocatorUnder(locator, prefix string) bool {
	rest, ok := strings.CutPrefix(locator, prefix)
	if !ok || rest == "" {
		return false
	}
	for _, segment := range strings.Split(rest, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func containsVersion(versions []int64, version int64) bool {
	for _, v := range versions {
		if v == version {
			return true
		}
	}
	return false
}

func writeLocatorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrLocatorNotFound):
		writeError(w, http.StatusConflict, ErrLocatorNotFound.Error())
	case errors.Is(err, ErrBackendNotConfigured):
		writeError(w, http.StatusConflict, ErrBackendNotConfigured.Error())
	default:
		writeError(w, http.StatusServiceUnavailable, ErrLocatorCheckUnavailable.Error())
	}
}

// SecretStoreLocators checks locators against the governed secret stores,
// with Metadata only (secretstore.SecretStore never returns a value from
// it).
type SecretStoreLocators struct {
	stores           map[string]secretstore.SecretStore
	policyRevisionID string
}

// NewSecretStoreLocators builds the check over the stores the broker reads.
func NewSecretStoreLocators(stores map[string]secretstore.SecretStore, policyRevisionID string) (*SecretStoreLocators, error) {
	if len(stores) == 0 || policyRevisionID == "" {
		return nil, errors.New("credential reference locator check: the secret stores and the policy revision are required")
	}
	return &SecretStoreLocators{stores: stores, policyRevisionID: policyRevisionID}, nil
}

type mounted interface{ KVMount() string }

// Versions implements Locators.
func (l *SecretStoreLocators) Versions(ctx context.Context, check LocatorCheck) ([]int64, int64, error) {
	store, ok := l.stores[check.Backend]
	if !ok || store == nil {
		return nil, 0, ErrBackendNotConfigured
	}
	ref := secretstore.SecretRef{Backend: check.Backend, Path: check.Locator}
	if m, ok := store.(mounted); ok {
		ref.Mount = m.KVMount()
	}
	meta, err := store.Metadata(ctx, secretstore.StoreContext{
		TenantProjectID: check.TenantProjectID, CredentialRefID: check.ReferenceID,
		WorkloadScope: repos.CredentialReferenceStagingScope, PolicyRevisionID: l.policyRevisionID,
	}, ref)
	switch {
	case errors.Is(err, secretstore.ErrNotFound), errors.Is(err, secretstore.ErrInvalidRequest):
		return nil, 0, ErrLocatorNotFound
	case err != nil:
		return nil, 0, ErrLocatorCheckUnavailable
	}
	return meta.Versions, meta.CurrentVersion, nil
}

// newID is the id of a new reference and of a new target binding.
func (h *Handler) newID() string {
	if h.ids != nil {
		return h.ids()
	}
	return uuid.NewString()
}
