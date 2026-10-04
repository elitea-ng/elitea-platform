package configurations

// Platform default model — `/api/v2/admin/gateway/default_model` (#6826).
//
// The value, its precedence against a project's own default, and what happens
// when the model is removed are documented once, in
// internal/application/configurations/platform_default_model.go. This file is
// the HTTP surface only:
//
//	GET    /admin/gateway/default_model?section=llm   the stored value and the choices
//	PUT    /admin/gateway/default_model?section=llm   {"model_name": "..."}
//	DELETE /admin/gateway/default_model?section=llm   clear it
//	GET    /admin/gateway/platform_models/{configID}/default_usage
//
// The last one is what the delete confirmation reads: how many projects name
// the model as their default, and whether it is the platform default.
//
// The routes mount inside the `configuration.governance` administration-mode
// group with the other platform-model routes. They apply no gate themselves.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// ModelDefaultsManager is the application service behind these routes and the
// delete hook. *configurationapp.PlatformModelDefaultService implements it.
type ModelDefaultsManager interface {
	Get(context.Context, configurationapp.CurrentModelSection) (configurationapp.PlatformModelDefaultView, error)
	Set(context.Context, configurationapp.CurrentModelSection, string) error
	Clear(context.Context, configurationapp.CurrentModelSection) error
	Usage(context.Context, int32, configurationapp.CurrentModelSection, string) (configurationapp.PlatformModelDefaultUsage, error)
	ReleaseDeletedModelDefault(context.Context, int32, configurationapp.CurrentModelSection, string) error
}

var _ ModelDefaultsManager = (*configurationapp.PlatformModelDefaultService)(nil)

// WithModelDefaults wires the platform default model service. Without it the
// default_model routes answer 503, and a delete releases no stored default.
// The catalogue precedence still skips a default that names a deleted model.
func WithModelDefaults(manager ModelDefaultsManager) Option {
	return func(handler *Handler) {
		handler.modelDefaults = manager
	}
}

// PlatformDefaultModelRoutes is the admin surface for the platform default
// model. Mount it behind the central gate.
func (h *Handler) PlatformDefaultModelRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.GetPlatformDefaultModel)
	r.Put("/", h.SetPlatformDefaultModel)
	r.Delete("/", h.ClearPlatformDefaultModel)
	return r
}

// GetPlatformDefaultModel serves GET /admin/gateway/default_model.
func (h *Handler) GetPlatformDefaultModel(w http.ResponseWriter, r *http.Request) {
	section, ok := h.platformDefaultModelRequest(w, r)
	if !ok {
		return
	}
	view, err := h.modelDefaults.Get(r.Context(), section)
	if err != nil {
		writePlatformDefaultModelFailure(r.Context(), w, "read platform default model failed", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// SetPlatformDefaultModel serves PUT /admin/gateway/default_model.
func (h *Handler) SetPlatformDefaultModel(w http.ResponseWriter, r *http.Request) {
	section, ok := h.platformDefaultModelRequest(w, r)
	if !ok {
		return
	}
	body, ok := decodeGlobalBody(w, r)
	if !ok {
		return
	}
	name, isString := body["model_name"].(string)
	if !isString || strings.TrimSpace(name) == "" {
		apierr.WriteStatus(w, http.StatusBadRequest, "model_name is required")
		return
	}
	if err := h.modelDefaults.Set(r.Context(), section, strings.TrimSpace(name)); err != nil {
		writePlatformDefaultModelFailure(r.Context(), w, "write platform default model failed", err)
		return
	}
	h.GetPlatformDefaultModel(w, r)
}

// ClearPlatformDefaultModel serves DELETE /admin/gateway/default_model.
func (h *Handler) ClearPlatformDefaultModel(w http.ResponseWriter, r *http.Request) {
	section, ok := h.platformDefaultModelRequest(w, r)
	if !ok {
		return
	}
	if err := h.modelDefaults.Clear(r.Context(), section); err != nil {
		writePlatformDefaultModelFailure(r.Context(), w, "clear platform default model failed", err)
		return
	}
	h.GetPlatformDefaultModel(w, r)
}

// GlobalModelDefaultUsage serves
// GET /admin/gateway/platform_models/{configID}/default_usage.
func (h *Handler) GlobalModelDefaultUsage(w http.ResponseWriter, r *http.Request) {
	if h.modelDefaults == nil {
		writePlatformDefaultModelUnavailable(w)
		return
	}
	if h.publicProjectID <= 0 {
		writePlatformDefaultModelUnavailable(w)
		return
	}
	identity, found, err := h.lookupModelRow(r.Context(), h.publicProjectID, chi.URLParam(r, "configID"))
	if err != nil {
		writePlatformDefaultModelFailure(r.Context(), w, "read platform model failed", err)
		return
	}
	if !found || !identity.shared {
		apierr.WriteStatus(w, http.StatusNotFound, "configuration not found")
		return
	}
	usage, err := h.modelDefaults.Usage(r.Context(), int32(h.publicProjectID), identity.section, identity.name)
	if err != nil {
		writePlatformDefaultModelFailure(r.Context(), w, "count platform model default usage failed", err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// releaseDeletedModelDefault clears the stored defaults that name a deleted
// model. It runs after the delete committed, so a failure is logged and does
// not change the response: the catalogue precedence skips the stored id.
func (h *Handler) releaseDeletedModelDefault(ctx context.Context, projectID string, deleted deletedModelRow) {
	if h.modelDefaults == nil || deleted.name == "" ||
		!configurationapp.IsSupportedCurrentModelSection(deleted.section) {
		return
	}
	owner, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil || owner <= 0 {
		return
	}
	// Detached from the request: the row is already gone, so a client that
	// disconnects must not leave half of the release undone. Bounded, so a
	// stuck vault cannot hold the handler.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseDeletedModelDefaultTimeout)
	defer cancel()
	if err := h.modelDefaults.ReleaseDeletedModelDefault(
		releaseCtx, int32(owner), deleted.section, deleted.name,
	); err != nil {
		slog.WarnContext(ctx, "the deleted model is still named as a stored default; the catalogue skips it",
			"project_id", projectID, "section", string(deleted.section), "err", err)
	}
}

// releaseDeletedModelDefaultTimeout bounds the release fan-out of one delete.
const releaseDeletedModelDefaultTimeout = 2 * time.Minute

// deletedModelRow is what a configuration delete reports about its row.
type deletedModelRow struct {
	section configurationapp.CurrentModelSection
	// name is the value a stored default names: data.name for a model, and
	// elitea_title for a vector storage row (repos/models.go reads the same).
	name   string
	shared bool
}

func deletedModelRowFrom(section, title, dataName string, shared bool) deletedModelRow {
	row := deletedModelRow{section: configurationapp.CurrentModelSection(section), name: dataName, shared: shared}
	if row.section == configurationapp.CurrentModelSectionVectorStorage {
		row.name = title
	}
	return row
}

// lookupModelRow reads the identity of one configuration row. A test replaces
// it through modelRowLookup.
func (h *Handler) lookupModelRow(ctx context.Context, projectID int, configID string) (deletedModelRow, bool, error) {
	if h.modelRowLookup != nil {
		return h.modelRowLookup(ctx, projectID, configID)
	}
	if h.pool == nil {
		return deletedModelRow{}, false, errors.New("the configuration store is not available")
	}
	if configID == "" {
		return deletedModelRow{}, false, nil
	}
	schema := pgx.Identifier{fmt.Sprintf("p_%d", projectID)}.Sanitize()
	var section, title, dataName string
	var shared bool
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT section, COALESCE(elitea_title, ''), COALESCE(data->>'name', ''), COALESCE(shared, false)
		  FROM %s.configuration
		 WHERE %s = $1`, schema, configurationIDColumn(configID)), configID,
	).Scan(&section, &title, &dataName, &shared)
	if errors.Is(err, pgx.ErrNoRows) {
		return deletedModelRow{}, false, nil
	}
	if err != nil {
		return deletedModelRow{}, false, err
	}
	return deletedModelRowFrom(section, title, dataName, shared), true, nil
}

// platformDefaultModelRequest checks the composition and reads ?section=.
func (h *Handler) platformDefaultModelRequest(
	w http.ResponseWriter, r *http.Request,
) (configurationapp.CurrentModelSection, bool) {
	if h.modelDefaults == nil {
		writePlatformDefaultModelUnavailable(w)
		return "", false
	}
	section := configurationapp.CurrentModelSection(strings.ToLower(r.URL.Query().Get("section")))
	if section == "" {
		section = configurationapp.CurrentModelSectionLLM
	}
	if !configurationapp.IsSupportedCurrentModelSection(section) {
		apierr.WriteStatus(w, http.StatusBadRequest, "unknown model section")
		return "", false
	}
	return section, true
}

func writePlatformDefaultModelUnavailable(w http.ResponseWriter) {
	apierr.WriteStatus(w, http.StatusServiceUnavailable,
		"the platform default model is not available: this deployment composes no Configurations runtime")
}

// writePlatformDefaultModelFailure maps the service errors to safe responses
// and logs the cause of every other failure.
func writePlatformDefaultModelFailure(ctx context.Context, w http.ResponseWriter, message string, err error) {
	switch {
	case errors.Is(err, configurationapp.ErrPlatformModelDefaultIneligible):
		apierr.WriteStatus(w, http.StatusBadRequest,
			"choose a platform model that is shared and available to all projects. "+
				"A new project is in no project list, so a narrower grant cannot be its default.")
	case errors.Is(err, configurationapp.ErrInvalidPlatformModelDefault):
		apierr.WriteStatus(w, http.StatusBadRequest, "invalid platform default model request")
	case errors.Is(err, configurationapp.ErrCurrentConfigurationLifecycleInternalLimit):
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "too many projects to count")
	case ctx.Err() != nil:
		return
	default:
		slog.ErrorContext(ctx, message, "err", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "internal server error")
	}
}
