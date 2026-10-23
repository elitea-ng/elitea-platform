package admin

// The admin surface of the TYPED IDENTITY PROVIDER — the real editor behind the
// Configuration page's dead "Authentication" section.
//
// # Why this is its own surface and not that section made writable
//
// The section's fields address keys inside a legacy plugin's YAML
// (`auth_provider`, `metadata_endpoint`, `client_id`, `client_secret`), and the
// plugin-configuration write path stores a section's fields as plaintext JSONB
// rows in `centry.platform_config`. Two things follow, and either alone decides
// it:
//
//   - `client_secret` is a credential, and `config_values.go:rejectCredentialField`
//     refuses a credential into those rows — correctly, because every holder of
//     `runtime.plugins` can read them. The same is true of a SAML
//     service-provider private key.
//   - A flat list of field values cannot express "this document is an OIDC
//     provider, and these are the invariants it must satisfy". The
//     configuration provenance specification asks for one TYPED provider
//     revision per protocol precisely because the legacy values are scattered,
//     and a settings document reproduces the scattering.
//
// So the definition has its own surface, its own table (shared migration 0095)
// and its own validation (`internal/identityproviders`), and the section says
// where to find it.
//
// # The permission is the one the page it replaces already used
//
// `runtime.plugins`, resolved in administration mode, gates every route here.
// The legacy `plugin_config_values.py` declares the same string, so an operator
// who could edit the Authentication section can author a provider and no new
// grant is needed — which also keeps the grant gate in
// `internal/api/router_permission_grant_gate_test.go` untripped.
//
// # What a caller can and cannot see
//
// A listing returns every definition with its secret rendered as a MASK when one
// is set and omitted when none is. The plaintext is never returned by any route
// here. It is written once, sealed into the GLOBAL vault's hidden bucket
// (`internal/api/v2/secrets/admin_hidden.go`), and read back only by the login
// path inside this service.
//
// # This surface changes how logins work, so it fails closed
//
// Without both the store and the vault, every route answers 503 and writes
// nothing. A definition whose secret vanished is a definition that authenticates
// nothing, and it would fail at the identity provider with a message naming
// neither this service nor the missing credential.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/identityproviders"
)

// IdentityProviderSecretStore is the vault seam this surface seals through.
//
// It is the same interface shape the pre-built MCP catalogue uses, and for the
// same reason: this package does not import the secrets handler, and a test can
// assert that a refused write seals nothing.
type IdentityProviderSecretStore interface {
	StoreAdminHiddenSecret(ctx context.Context, name, value string) error
	DeleteAdminHiddenSecret(ctx context.Context, name string) error
}

// IdentityProviderStore is the store seam.
//
// It is an INTERFACE rather than the concrete `*identityproviders.Store`, which
// differs from the pre-built MCP catalogue beside it. The reason is testability
// of an ORDER: this handler's contract is which refusals happen before the
// database is read and before the vault is written, and a concrete store over a
// nil pool can only prove the refusals that come first. A test that cannot
// reach the later ones would leave the credential-safety rules unheld.
type IdentityProviderStore interface {
	List(ctx context.Context) ([]identityproviders.Provider, error)
	Lookup(ctx context.Context, key string) (identityproviders.Provider, error)
	Upsert(ctx context.Context, provider identityproviders.Provider) (identityproviders.Provider, error)
	Delete(ctx context.Context, key string) (string, error)
}

// WithIdentityProviders supplies the provider store and the vault.
func WithIdentityProviders(store IdentityProviderStore, vault IdentityProviderSecretStore) Option {
	return func(h *Handler) {
		// A nil interface is not stored, so an unwired composition root leaves
		// the field nil rather than boxing one. WithToolkitRegistry states the
		// same rule; a nil POINTER inside a non-nil interface would get past the
		// readiness check and panic on first use.
		if store == nil {
			return
		}
		h.identityProviders = store
		h.identityProviderVault = vault
	}
}

// identityProviderSecretFeature namespaces the vault names this surface
// derives, so a provider can never collide with another feature's credential.
const identityProviderSecretFeature = "identity_provider"

// identityProviderSecretMask is what a set secret renders as. It is the mask the
// global Secrets listing uses, so the admin screens agree about what "a value is
// set and you may not read it" looks like.
const identityProviderSecretMask = "******"

// identityProviderBody is the wire shape of one authored definition.
//
// Secret is a POINTER so three cases stay distinct on a write: absent (leave the
// sealed secret alone), an empty string (clear it), and a value (re-seal it).
// Collapsing absent and empty is how a save from a form that does not echo
// secrets silently erases the credential — and here that would take the
// deployment's federated login down.
type identityProviderBody struct {
	Kind        string  `json:"kind"`
	DisplayName string  `json:"display_name"`
	Enabled     *bool   `json:"enabled"`
	Secret      *string `json:"secret"`

	OIDC *identityproviders.OIDCDocument `json:"oidc"`
	SAML *identityproviders.SAMLDocument `json:"saml"`
}

// identityProviderView is the wire shape a read returns. It never carries a
// plaintext secret.
type identityProviderView struct {
	Key         string `json:"key"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`
	Revision    int    `json:"revision"`
	// Secret is the mask when one is sealed, and absent when none is.
	Secret    string `json:"secret,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`

	OIDC *identityproviders.OIDCDocument `json:"oidc,omitempty"`
	SAML *identityproviders.SAMLDocument `json:"saml,omitempty"`
}

func identityProviderToView(provider identityproviders.Provider) identityProviderView {
	view := identityProviderView{
		Key:         provider.Key,
		Kind:        string(provider.Kind),
		DisplayName: provider.DisplayName,
		Enabled:     provider.Enabled,
		Revision:    provider.Revision,
		OIDC:        provider.OIDC,
		SAML:        provider.SAML,
	}
	if provider.SecretRef != "" {
		view.Secret = identityProviderSecretMask
	}
	if !provider.UpdatedAt.IsZero() {
		view.UpdatedAt = provider.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return view
}

// identityProvidersReady reports whether both dependencies are wired.
func (h *Handler) identityProvidersReady(w http.ResponseWriter) bool {
	if h.identityProviders != nil && h.identityProviderVault != nil {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": "identity provider configuration is not available on this deployment",
	})
	return false
}

// IdentityProviderList answers `GET /admin/identity_providers/administration`.
func (h *Handler) IdentityProviderList(w http.ResponseWriter, r *http.Request) {
	if !h.identityProvidersReady(w) {
		return
	}
	providers, err := h.identityProviders.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the identity provider definitions could not be read"})
		return
	}
	views := make([]identityProviderView, 0, len(providers))
	for _, provider := range providers {
		views = append(views, identityProviderToView(provider))
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": views, "total": len(views)})
}

// IdentityProviderSave answers `PUT /admin/identity_providers/administration/{key}`.
//
// The ORDER of the steps below is the contract, not an implementation detail:
//
//  1. Decode and validate the document. A malformed definition is refused
//     without reading the database and without touching the vault.
//  2. Read the current row, for the fields a save inherits rather than states:
//     whether it is enabled, and which vault entry holds its secret.
//  3. Check what the document needs from the vault, against the secret this
//     write WOULD leave in place.
//  4. Seal, then write.
//
// A refused save must leave the working provider exactly as it was. Sealing
// before the last refusal could pass would overwrite a live credential on its
// way to answering 400.
func (h *Handler) IdentityProviderSave(w http.ResponseWriter, r *http.Request) {
	if !h.identityProvidersReady(w) {
		return
	}

	var body identityProviderBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}

	// The path segment is authoritative over the body, so a caller cannot PUT
	// one key and write another.
	key := identityproviders.NormalizeKey(chi.URLParam(r, "key"))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid provider key"})
		return
	}

	kind, err := identityproviders.ParseKind(body.Kind)
	if err != nil {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": "the provider kind must be oidc or saml", "field": "kind"})
		return
	}

	validated, err := identityproviders.Validate(identityproviders.Provider{
		Key:         key,
		Kind:        kind,
		DisplayName: body.DisplayName,
		OIDC:        body.OIDC,
		SAML:        body.SAML,
	})
	if err != nil {
		writeIdentityProviderValidationError(w, err)
		return
	}

	existing, err := h.identityProviders.Lookup(r.Context(), key)
	switch {
	case err == nil, errors.Is(err, identityproviders.ErrNotFound):
	default:
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the identity provider definitions could not be read"})
		return
	}

	// A row cannot change protocol under its own key. The two kinds have
	// different documents and different sealed material, so a save that flipped
	// the kind would leave the previous kind's secret sealed under a name the
	// new document never reads. Deleting and re-authoring is the explicit path.
	if existing.Key != "" && existing.Kind != kind {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this provider key already names a definition of a different kind: " +
				"delete it before authoring the other protocol under the same key",
		})
		return
	}

	// Enabled is INHERITED when the body does not state it. A form that omits
	// the field must not silently retire a live provider.
	validated.Enabled = existing.Enabled
	if body.Enabled != nil {
		validated.Enabled = *body.Enabled
	}

	// Signing an outbound SAML request needs the private key that matches the
	// published certificate. The document alone cannot state whether one is
	// sealed, so the check lives here, where the vault reference is in reach —
	// and it runs BEFORE the seal, against the secret this write would leave.
	if validated.SAML != nil && validated.SAML.SignAuthnRequests &&
		!identityProviderWillHoldSecret(body.Secret, existing.SecretRef) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "signing authentication requests needs the service provider private key",
			"field": "secret",
		})
		return
	}

	// SEAL BEFORE THE ROW, CLEAR AFTER IT. The two directions are not
	// symmetrical, and treating them as one step is how a failed row write took
	// a working provider down.
	//
	// Sealing is ADDITIVE: the vault name is derived from the key, so a seal
	// that is followed by a failed row write leaves the row pointing at a value
	// the operator did in fact just supply. Nothing is lost.
	//
	// Clearing is DESTRUCTIVE. Deleting the vault entry first and then failing
	// to write the row leaves the stored row naming an entry that no longer
	// exists, and every login answers 503 until somebody saves again. So the
	// row is written first, and the delete happens after it commits — the same
	// order IdentityProviderDelete uses, and for the same reason.
	clearing := body.Secret != nil && strings.TrimSpace(*body.Secret) == ""
	if !clearing {
		secretRef, sealed := h.sealIdentityProviderSecret(w, r, key, body.Secret, existing.SecretRef)
		if !sealed {
			return
		}
		validated.SecretRef = secretRef
	}

	stored, err := h.identityProviders.Upsert(r.Context(), validated)
	if err != nil {
		var validation identityproviders.ValidationError
		if errors.As(err, &validation) {
			writeIdentityProviderValidationError(w, err)
			return
		}
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the identity provider definition could not be written"})
		return
	}

	// The row no longer names the secret, so removing it is now safe. A failure
	// here leaves an entry nothing reads, which is inert — so it is reported in
	// the response rather than turned into an error that would invite a retry
	// of a save that already succeeded.
	if clearing && existing.SecretRef != "" {
		if err := h.identityProviderVault.DeleteAdminHiddenSecret(r.Context(), existing.SecretRef); err != nil {
			view := identityProviderToView(stored)
			writeJSON(w, http.StatusOK, map[string]any{
				"provider": view,
				"warning": "the provider was saved without a secret; the previously stored value could " +
					"not be deleted and remains in the platform vault",
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, identityProviderToView(stored))
}

// identityProviderWillHoldSecret reports whether the definition still has a
// sealed secret AFTER this write, without performing the write.
//
// It reads the same tri-state sealIdentityProviderSecret acts on, so the two
// cannot disagree about what a save means.
func identityProviderWillHoldSecret(secret *string, current string) bool {
	if secret == nil {
		return current != ""
	}
	return strings.TrimSpace(*secret) != ""
}

// sealIdentityProviderSecret applies the NON-DESTRUCTIVE half of the write's
// tri-state secret and returns the reference the row should carry.
//
// The clear direction is deliberately not here: it is destructive, and the
// caller performs it after the row is written. See the ordering note in
// IdentityProviderSave.
//
// It reports failure through the boolean rather than an error so the caller
// stays a straight line; every failing branch has already answered.
func (h *Handler) sealIdentityProviderSecret(
	w http.ResponseWriter,
	r *http.Request,
	key string,
	secret *string,
	current string,
) (string, bool) {
	if secret == nil || strings.TrimSpace(*secret) == "" {
		return current, true
	}
	name := identityProviderSecretName(key)
	if err := h.identityProviderVault.StoreAdminHiddenSecret(r.Context(), name, *secret); err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the platform secret vault is unavailable"})
		return "", false
	}
	return name, true
}

// IdentityProviderDelete answers `DELETE /admin/identity_providers/administration/{key}`.
func (h *Handler) IdentityProviderDelete(w http.ResponseWriter, r *http.Request) {
	if !h.identityProvidersReady(w) {
		return
	}
	key := identityproviders.NormalizeKey(chi.URLParam(r, "key"))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid provider key"})
		return
	}

	reference, err := h.identityProviders.Delete(r.Context(), key)
	if errors.Is(err, identityproviders.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such identity provider"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "the identity provider definition could not be written"})
		return
	}

	// The row is gone either way. A vault cleanup that fails leaves an entry
	// nothing reads, which is inert — so it is reported rather than turned into
	// a failure that would invite a retry of a delete that already happened.
	if reference != "" {
		if err := h.identityProviderVault.DeleteAdminHiddenSecret(r.Context(), reference); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"deleted": key,
				"warning": "the identity provider was removed; its stored secret could not be " +
					"deleted and remains in the platform vault",
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": key})
}

// writeIdentityProviderValidationError answers 400 and NAMES the field.
//
// A refusal that does not say which value was wrong sends the operator back to
// a form of twenty fields with nothing to go on.
func writeIdentityProviderValidationError(w http.ResponseWriter, err error) {
	var validation identityproviders.ValidationError
	if errors.As(err, &validation) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": validation.Reason,
			"field": validation.Field,
		})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the identity provider definition is not valid"})
}

// identityProviderSecretName derives the vault name for one definition's secret.
// It is derived rather than stored so a row and its secret cannot drift apart.
func identityProviderSecretName(key string) string {
	return secrets.AdminHiddenSecretName(identityProviderSecretFeature, key, "secret")
}
