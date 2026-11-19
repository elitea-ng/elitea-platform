// Package microsoftoauthapps serves the project-facing half of the Microsoft
// Default app registry (ADR-0057 decision 5; epic 2254 story S1b): what a
// credential form needs to offer "Default app" next to "Custom app".
//
//	GET /api/v2/microsoft_oauth_apps/{projectID}?credential_type=outlook
//
// The answer names the apps that are active, serve the credential type and serve
// the project, with what the form shows: id, name, description, whether token
// refresh is on and the permissions the app allows. It never carries the client id,
// the authority or anything of the secret: a person who may create a credential
// learns which apps they may pick, not how they are configured. An empty registry
// answers an empty list, and the form shows only Custom app.
//
// The route takes the permission that creates a credential in the project, the
// button the form serves. Both ids come from the client, and the registry applies
// the availability rule again at every sign-in and refresh, so listing here is
// convenience, not authorization.
package microsoftoauthapps

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
)

// Path is the route, relative to /api/v2.
const Path = "/microsoft_oauth_apps/{projectID}"

// Source is the resolver (microsoftapps.Resolver).
type Source interface {
	Offers(ctx context.Context, credentialType string, projectID int64) ([]microsoftapps.Offer, error)
	CustomAppsAllowed(ctx context.Context) bool
}

// Handler serves the offers.
type Handler struct{ source Source }

// New returns the handler. A nil source answers 503 on every request.
func New(source Source) *Handler { return &Handler{source: source} }

type offerView struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	AutoRefreshToken bool     `json:"auto_refresh_token"`
	ScopeCeiling     []string `json:"scope_ceiling"`
}

func write(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// List answers the route.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.source == nil {
		write(w, http.StatusServiceUnavailable, map[string]any{"error": "the Microsoft app registry is not available on this deployment"})
		return
	}
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil || projectID <= 0 {
		write(w, http.StatusBadRequest, map[string]any{"error": "invalid project id"})
		return
	}
	credentialType := r.URL.Query().Get("credential_type")
	if !microsoftapps.ValidType(credentialType) {
		write(w, http.StatusBadRequest, map[string]any{
			"error": "credential_type must be one of outlook, teams, ms_graph, dataverse or sharepoint", "field": "credential_type",
		})
		return
	}
	offers, err := h.source.Offers(r.Context(), credentialType, projectID)
	if err != nil {
		slog.ErrorContext(r.Context(), "microsoft apps: the offers were not read", "error", err)
		write(w, http.StatusServiceUnavailable, map[string]any{"error": "the Microsoft apps could not be read"})
		return
	}
	views := make([]offerView, 0, len(offers))
	for _, offer := range offers {
		ceiling := offer.Ceiling
		if ceiling == nil {
			ceiling = []string{}
		}
		views = append(views, offerView{
			ID: offer.ID, Name: offer.Name, Description: offer.Description,
			AutoRefreshToken: offer.AutoRefreshToken, ScopeCeiling: ceiling,
		})
	}
	write(w, http.StatusOK, map[string]any{
		"credential_type":    credentialType,
		"apps":               views,
		"custom_app_allowed": h.source.CustomAppsAllowed(r.Context()),
	})
}
