// Package desktopops serves the platform operations a desktop client's local
// agent runtime needs from the cloud (ADR-0029 decision 5, client contract
// 1.6): resolveApplicationVersion (5a) and executeRemoteToolkitTool (5b). The
// local turn operations (5c) live in internal/api/v2/localturns.
package desktopops

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

const (
	ResolvedVersionPath = "/api/v2/elitea_core/resolved_version/prompt_lib/{projectID}/{applicationID}/{versionID}"
	// ResolvedVersionPermission is the version read every version-details
	// route gates on. The document is that version, frozen for execution and
	// stripped of credentials, so reading it needs nothing more.
	ResolvedVersionPermission = "models.applications.version.details"
	mode                      = auth.PermissionModeDefault
)

var ErrInvalidRoute = errors.New("invalid desktop operation route dependencies")

// ResolvedVersionUseCase is *storage.ClientApplicationVersionService.
type ResolvedVersionUseCase interface {
	Resolve(ctx context.Context, projectID, actorID int64, applicationID, versionID uint64) (storage.ClientApplicationVersion, error)
}

var _ ResolvedVersionUseCase = (*storage.ClientApplicationVersionService)(nil)

// NewResolvedVersionRoute composes GET resolveApplicationVersion behind Auth and
// the project's version-read permission.
//
// useCase may be nil: the shared freeze exists only where the agent execution
// plane is composed, and a deployment without it still mounts the route, which
// then answers 501 `resolved_version_unavailable` — a client reads a 404 as
// "server too old".
//
// Any authenticated caller is served, browser sessions included: it is a safe
// read of a document that carries less than the version-details route already
// returns (no toolkit settings at all), so a cookie adds no exposure a
// cross-site page could use — it cannot read the response.
func NewResolvedVersionRoute(
	useCase ResolvedVersionUseCase,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (http.Handler, error) {
	if authConfig.PrincipalValidator == nil || permissions == nil {
		return nil, ErrInvalidRoute
	}
	h := &resolvedVersionHandler{useCase: useCase}
	router := chi.NewRouter()
	router.Method(http.MethodGet, ResolvedVersionPath, gate(authConfig, permissions, ResolvedVersionPermission, h.serve))
	return router, nil
}

// gate is the composition every desktop operation shares: authenticate, then
// resolve the project permission from the {projectID} path parameter.
func gate(authConfig apimw.AuthConfig, permissions auth.PermissionResolver, permission string, endpoint http.HandlerFunc) http.Handler {
	wrapped := apimw.RequireResolvedPermissionsForProject(
		permissions, mode,
		func(request *http.Request) (string, bool) {
			projectID := chi.URLParam(request, "projectID")
			_, valid := positiveID(projectID)
			return projectID, valid
		},
		permission,
	)(endpoint)
	return apimw.Auth(authConfig)(wrapped)
}

type resolvedVersionHandler struct {
	useCase ResolvedVersionUseCase
}

func (h *resolvedVersionHandler) serve(writer http.ResponseWriter, request *http.Request) {
	projectID, okProject := positiveID(chi.URLParam(request, "projectID"))
	applicationID, okApplication := positiveID(chi.URLParam(request, "applicationID"))
	versionID, okVersion := positiveID(chi.URLParam(request, "versionID"))
	if !okProject || !okApplication || !okVersion {
		writeError(writer, http.StatusBadRequest, "invalid_resolved_version_request", "Invalid project, application or version id.")
		return
	}
	user, ok := auth.UserFromContext(request.Context())
	if !ok {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	actor, ok := user.OwningUserID()
	if !ok {
		writeError(writer, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if h.useCase == nil {
		writeError(writer, http.StatusNotImplemented, "resolved_version_unavailable",
			"This deployment does not run cloud agents, so it cannot resolve a version for execution.")
		return
	}
	resolved, err := h.useCase.Resolve(request.Context(), projectID, actor, uint64(applicationID), uint64(versionID))
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrContentNotFound):
			writeError(writer, http.StatusNotFound, "application_version_not_found",
				"The application version was not found in this project.")
		case errors.Is(err, storage.ErrClientApplicationVersionUnresolvable):
			writeError(writer, http.StatusUnprocessableEntity, "application_version_unresolvable",
				"This version cannot be resolved for execution: a cloud run of it would be refused too. "+
					"Check its model and the credentials its toolkits use.")
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			writeError(writer, http.StatusServiceUnavailable, "resolved_version_unavailable",
				"The version could not be resolved in time. Retry.")
		default:
			if !errors.Is(err, storage.ErrContentUnavailable) {
				slog.ErrorContext(request.Context(), "resolved application version failed", "err", err)
			}
			writer.Header().Set("Retry-After", "1")
			writeError(writer, http.StatusServiceUnavailable, "resolved_version_unavailable",
				"The version cannot be resolved right now. Retry.")
		}
		return
	}
	encoded, err := json.Marshal(resolved)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "resolved_version_failed", "The version could not be encoded.")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "private, no-store")
	writer.Header().Set("ETag", `"`+resolved.DefinitionSHA256+`"`)
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(encoded)
}

func positiveID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0 && id <= math.MaxInt32 && strconv.FormatInt(id, 10) == raw
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]string{"error": code, "message": message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
