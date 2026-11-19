package admin

// The admin surface of the Microsoft Default app registry (ADR-0057 decisions 5
// and 11; epic 2254 story S1b).
//
//	GET    /admin/microsoft_oauth_apps               the apps, the settings and the deployment's Entra checklist
//	POST   /admin/microsoft_oauth_apps               create
//	PUT    /admin/microsoft_oauth_apps/{id}          update
//	POST   /admin/microsoft_oauth_apps/{id}/disable  switch off
//	POST   /admin/microsoft_oauth_apps/{id}/enable   switch back on
//	POST   /admin/microsoft_oauth_apps/{id}/test     check what can be checked without a person
//	PUT    /admin/microsoft_oauth_apps/settings      the `microsoft.custom_apps` switch
//
// # The permission
//
// `configuration.microsoft_apps` in administration mode, granted to super_admin
// and admin by shared migration 0255: registering an app decides which Entra
// client signs users in to Microsoft for every project, and its secret is a
// credential.
//
// # What a caller can and cannot see
//
// The client secret is WRITE-ONLY. A view says whether one is set and when it
// expires, never the value, and no route returns, logs or exports it. The
// secret is sealed by the store (AES-256-GCM, AAD-bound) and opened only by
// microsoftapps.Resolver for the code exchange and the refresh inside Main.
//
// # Fail closed
//
// Without the store every route answers 503. A registry that silently accepted a
// write it could not seal would leave a credential pointing at an app with no
// secret.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/publicorigin"
)

// MicrosoftAppStore is the store seam (microsoftapps.Store).
type MicrosoftAppStore interface {
	List(ctx context.Context) ([]microsoftapps.App, error)
	Get(ctx context.Context, id string) (microsoftapps.App, error)
	Create(ctx context.Context, in microsoftapps.Input, actor microsoftapps.Actor) (microsoftapps.App, error)
	Update(ctx context.Context, id string, in microsoftapps.Input, actor microsoftapps.Actor) (microsoftapps.App, error)
	SetDisabled(ctx context.Context, id string, disabled bool, actor microsoftapps.Actor) (microsoftapps.App, error)
	CustomAppsAllowed(ctx context.Context) (bool, error)
	SetCustomAppsAllowed(ctx context.Context, allowed bool, actor microsoftapps.Actor) error
}

// WithMicrosoftApps supplies the registry store, the egress-guarded client a Test
// reads Entra's metadata with, and the deployment's public origin
// (publicorigin.Normalize(DEPLOYMENT_URL), may be empty). Without the store the
// routes answer 503.
func WithMicrosoftApps(store MicrosoftAppStore, doer microsoftapps.HTTPDoer, deploymentOrigin string) Option {
	return func(h *Handler) {
		if store == nil {
			return
		}
		h.microsoftApps, h.microsoftDoer, h.microsoftOrigin = store, doer, deploymentOrigin
	}
}

type microsoftAppBody struct {
	Name             string              `json:"name"`
	Description      string              `json:"description"`
	ClientID         string              `json:"client_id"`
	Authority        string              `json:"authority"`
	CredentialTypes  []string            `json:"credential_types"`
	ScopeCeiling     map[string][]string `json:"scope_ceiling"`
	AutoRefreshToken *bool               `json:"auto_refresh_token"`
	ProjectScope     string              `json:"project_scope"`
	ProjectIDs       []int64             `json:"project_ids"`
	// SecretExpiresAt is an RFC 3339 timestamp or a date; null or "" clears it.
	SecretExpiresAt *string `json:"secret_expires_at"`
	// Secret is a pointer so absent (keep the sealed secret), "" (clear it) and a
	// value (seal it) stay distinct. A form that does not echo secrets must not
	// erase the credential.
	Secret *string `json:"secret"`
}

// microsoftAppView is what a read returns. It has no field that can hold the
// secret.
type microsoftAppView struct {
	ID               string              `json:"id"`
	Name             string              `json:"name"`
	Description      string              `json:"description"`
	Provider         string              `json:"provider"`
	ClientID         string              `json:"client_id"`
	Authority        string              `json:"authority"`
	SecretSet        bool                `json:"secret_set"`
	SecretExpiresAt  *string             `json:"secret_expires_at"`
	CredentialTypes  []string            `json:"credential_types"`
	ScopeCeiling     map[string][]string `json:"scope_ceiling"`
	EffectiveCeiling map[string][]string `json:"effective_ceiling"`
	AutoRefreshToken bool                `json:"auto_refresh_token"`
	ProjectScope     string              `json:"project_scope"`
	ProjectIDs       []int64             `json:"project_ids"`
	State            string              `json:"state"`
	StateReason      string              `json:"state_reason"`
	// ManagedBy is "helm" for a row the chart seeds, so the portal says where its
	// values come from.
	ManagedBy string                  `json:"managed_by,omitempty"`
	CreatedAt string                  `json:"created_at"`
	UpdatedAt string                  `json:"updated_at"`
	Revision  int                     `json:"revision"`
	Checklist microsoftapps.Checklist `json:"checklist"`
}

type microsoftAppSettings struct {
	CustomApps bool `json:"custom_apps"`
}

func (h *Handler) microsoftAppsReady(w http.ResponseWriter) bool {
	if h.microsoftApps != nil {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": "the Microsoft app registry is not available on this deployment",
	})
	return false
}

func (h *Handler) microsoftOriginFor(r *http.Request) string {
	origin, _ := publicorigin.Resolve(h.microsoftOrigin, r)
	return origin
}

func (h *Handler) microsoftAppToView(app microsoftapps.App, origin string) microsoftAppView {
	view := microsoftAppView{
		ID: app.ID, Name: app.Name, Description: app.Description, Provider: "entra",
		ClientID: app.ClientID, Authority: app.Authority, SecretSet: app.SecretSet,
		CredentialTypes: nonNilStrings(app.CredentialTypes), ScopeCeiling: map[string][]string{}, EffectiveCeiling: map[string][]string{},
		AutoRefreshToken: app.AutoRefreshToken, ProjectScope: string(app.ProjectScope),
		ProjectIDs: nonNilInts(app.ProjectIDs), State: string(app.State), StateReason: app.StateReason,
		CreatedAt: app.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: app.UpdatedAt.UTC().Format(time.RFC3339),
		Revision: app.Revision, Checklist: microsoftapps.BuildChecklist(app, origin, time.Now()),
	}
	if app.SecretExpiresAt != nil {
		expires := app.SecretExpiresAt.UTC().Format(time.RFC3339)
		view.SecretExpiresAt = &expires
	}
	if app.SeedKey != "" {
		view.ManagedBy = "helm"
	}
	for _, t := range app.CredentialTypes {
		if t == microsoftapps.TypeDataverse {
			continue
		}
		if saved := app.ScopeCeiling[t]; len(saved) > 0 {
			view.ScopeCeiling[t] = saved
		}
		view.EffectiveCeiling[t] = app.Ceiling(t)
	}
	return view
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonNilInts(in []int64) []int64 {
	if in == nil {
		return []int64{}
	}
	return in
}

func microsoftActor(r *http.Request) microsoftapps.Actor {
	if principal, ok := auth.UserFromContext(r.Context()); ok {
		if id, resolved := principal.OwningUserID(); resolved && id > 0 && id <= int64(^uint32(0)>>1) {
			return microsoftapps.Actor{UserID: int(id)}
		}
	}
	return microsoftapps.Actor{}
}

// MicrosoftAppsList answers `GET /admin/microsoft_oauth_apps`.
func (h *Handler) MicrosoftAppsList(w http.ResponseWriter, r *http.Request) {
	if !h.microsoftAppsReady(w) {
		return
	}
	apps, err := h.microsoftApps.List(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "microsoft apps: the registry was not read", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the Microsoft apps could not be read"})
		return
	}
	custom, err := h.microsoftApps.CustomAppsAllowed(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "microsoft apps: the custom app setting was not read", "error", err)
		custom = true
	}
	origin := h.microsoftOriginFor(r)
	views := make([]microsoftAppView, 0, len(apps))
	for _, app := range apps {
		views = append(views, h.microsoftAppToView(app, origin))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"apps":      views,
		"total":     len(views),
		"settings":  microsoftAppSettings{CustomApps: custom},
		"checklist": microsoftapps.BuildDeploymentChecklist(origin),
	})
}

func (b microsoftAppBody) input() (microsoftapps.Input, error) {
	in := microsoftapps.Input{
		Name: b.Name, Description: b.Description, ClientID: b.ClientID, Authority: b.Authority,
		CredentialTypes: b.CredentialTypes, ScopeCeiling: b.ScopeCeiling,
		AutoRefreshToken: b.AutoRefreshToken == nil || *b.AutoRefreshToken,
		ProjectScope:     microsoftapps.ProjectScope(b.ProjectScope), ProjectIDs: b.ProjectIDs, Secret: b.Secret,
	}
	if b.SecretExpiresAt != nil && *b.SecretExpiresAt != "" {
		expires, err := time.Parse(time.RFC3339, *b.SecretExpiresAt)
		if err != nil {
			date, dateErr := time.Parse("2006-01-02", *b.SecretExpiresAt)
			if dateErr != nil {
				return microsoftapps.Input{}, microsoftapps.ValidationError{Field: "secret_expires_at", Reason: "use a date such as 2027-06-30"}
			}
			expires = date
		}
		in.SecretExpiresAt = &expires
	}
	return in, nil
}

func decodeMicrosoftApp(w http.ResponseWriter, r *http.Request) (microsoftapps.Input, bool) {
	var body microsoftAppBody
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the request body is not a valid Microsoft app"})
		return microsoftapps.Input{}, false
	}
	in, err := body.input()
	if err != nil {
		writeMicrosoftAppError(w, r, err)
		return microsoftapps.Input{}, false
	}
	return in, true
}

// MicrosoftAppsCreate answers `POST /admin/microsoft_oauth_apps`.
func (h *Handler) MicrosoftAppsCreate(w http.ResponseWriter, r *http.Request) {
	if !h.microsoftAppsReady(w) {
		return
	}
	in, ok := decodeMicrosoftApp(w, r)
	if !ok {
		return
	}
	app, err := h.microsoftApps.Create(r.Context(), in, microsoftActor(r))
	if err != nil {
		writeMicrosoftAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, h.microsoftAppToView(app, h.microsoftOriginFor(r)))
}

// MicrosoftAppsUpdate answers `PUT /admin/microsoft_oauth_apps/{id}`.
func (h *Handler) MicrosoftAppsUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.microsoftAppsReady(w) {
		return
	}
	in, ok := decodeMicrosoftApp(w, r)
	if !ok {
		return
	}
	app, err := h.microsoftApps.Update(r.Context(), chi.URLParam(r, "id"), in, microsoftActor(r))
	if err != nil {
		writeMicrosoftAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h.microsoftAppToView(app, h.microsoftOriginFor(r)))
}

// MicrosoftAppsDisable answers `POST /admin/microsoft_oauth_apps/{id}/disable`.
func (h *Handler) MicrosoftAppsDisable(w http.ResponseWriter, r *http.Request) {
	h.microsoftAppsSetDisabled(w, r, true)
}

// MicrosoftAppsEnable answers `POST /admin/microsoft_oauth_apps/{id}/enable`.
func (h *Handler) MicrosoftAppsEnable(w http.ResponseWriter, r *http.Request) {
	h.microsoftAppsSetDisabled(w, r, false)
}

func (h *Handler) microsoftAppsSetDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	if !h.microsoftAppsReady(w) {
		return
	}
	app, err := h.microsoftApps.SetDisabled(r.Context(), chi.URLParam(r, "id"), disabled, microsoftActor(r))
	if err != nil {
		writeMicrosoftAppError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h.microsoftAppToView(app, h.microsoftOriginFor(r)))
}

// MicrosoftAppsTest answers `POST /admin/microsoft_oauth_apps/{id}/test`. It
// checks what can be checked without a person; the first sign-in proves consent.
func (h *Handler) MicrosoftAppsTest(w http.ResponseWriter, r *http.Request) {
	if !h.microsoftAppsReady(w) {
		return
	}
	app, err := h.microsoftApps.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeMicrosoftAppError(w, r, err)
		return
	}
	result := microsoftapps.Test(r.Context(), app, h.microsoftDoer,
		publicorigin.FromRequest(r), h.microsoftOrigin, time.Now())
	writeJSON(w, http.StatusOK, result)
}

// MicrosoftAppsSettings answers `PUT /admin/microsoft_oauth_apps/settings`.
func (h *Handler) MicrosoftAppsSettings(w http.ResponseWriter, r *http.Request) {
	if !h.microsoftAppsReady(w) {
		return
	}
	var body struct {
		CustomApps *bool `json:"custom_apps"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.CustomApps == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "custom_apps must be true or false", "field": "custom_apps"})
		return
	}
	if err := h.microsoftApps.SetCustomAppsAllowed(r.Context(), *body.CustomApps, microsoftActor(r)); err != nil {
		slog.ErrorContext(r.Context(), "microsoft apps: the custom app setting was not written", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the setting could not be written"})
		return
	}
	writeJSON(w, http.StatusOK, microsoftAppSettings{CustomApps: *body.CustomApps})
}

// writeMicrosoftAppError maps the registry's errors to answers. A validation error
// names its field; nothing here echoes a secret, because no error carries one.
func writeMicrosoftAppError(w http.ResponseWriter, r *http.Request, err error) {
	var validation microsoftapps.ValidationError
	switch {
	case errors.As(err, &validation):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": validation.Reason, "field": validation.Field})
	case errors.Is(err, microsoftapps.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such Microsoft app"})
	case errors.Is(err, microsoftapps.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "an app with this name already exists", "field": "name"})
	default:
		slog.ErrorContext(r.Context(), "microsoft apps: the registry write failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the Microsoft app registry is unavailable"})
	}
}
