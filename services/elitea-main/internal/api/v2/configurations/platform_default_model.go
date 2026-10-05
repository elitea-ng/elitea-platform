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
	Usage(context.Context, configurationapp.PlatformModelDefaultModelRow) (configurationapp.PlatformModelDefaultUsage, error)
	ReleaseDeletedPlatformModelDefault(context.Context, configurationapp.PlatformModelDefaultModelRow) (bool, error)
	ReleaseDeletedProjectModelDefaults(context.Context, configurationapp.PlatformModelDefaultModelRow) error
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
	usage, err := h.modelDefaults.Usage(r.Context(), identity.modelRow(int32(h.publicProjectID)))
	if err != nil {
		writePlatformDefaultModelFailure(r.Context(), w, "count platform model default usage failed", err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// releaseDeletedModelDefault clears the stored defaults that name a deleted
// model. It runs after the delete committed, so a failure is logged and does
// not change the response: the catalogue precedence skips the stored id.
//
// The first step is short and runs before the response: it clears the
// platform-level defaults, or the owning project's default. The per-project
// fan-out of a platform model runs after the response, in the background, so
// the delete does not wait for every project vault.
//
// A public-project row is released only when the delete came through the
// governance-gated admin route (DeleteGlobalModel). A delete through the
// project route changes no platform-wide default: the admin console then
// reports the platform default as no longer available.
func (h *Handler) releaseDeletedModelDefault(ctx context.Context, projectID string, deleted deletedModelRow) {
	if h.modelDefaults == nil || deleted.name == "" ||
		!configurationapp.IsSupportedCurrentModelSection(deleted.section) {
		return
	}
	owner, err := strconv.ParseInt(projectID, 10, 32)
	if err != nil || owner <= 0 {
		return
	}
	if h.publicProjectID > 0 && owner == int64(h.publicProjectID) && !isPlatformModelDelete(ctx) {
		return
	}
	row := deleted.modelRow(int32(owner))
	// After a delete the row is gone, so no row is left out of the survivor
	// check.
	row.RowID = 0
	// Detached from the request: the row is already gone, so a client that
	// disconnects must not leave half of the release undone. Bounded, so a
	// stuck vault cannot hold the handler.
	platformCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseDeletedPlatformDefaultTimeout)
	fanOut, err := h.modelDefaults.ReleaseDeletedPlatformModelDefault(platformCtx, row)
	cancel()
	if err != nil {
		slog.WarnContext(ctx, "the deleted model is still named as a stored default; the catalogue skips it",
			"project_id", projectID, "section", string(deleted.section), "err", err)
	}
	if !fanOut {
		return
	}
	background := context.WithoutCancel(ctx)
	h.runModelDefaultRelease(func() {
		releaseCtx, cancel := context.WithTimeout(background, releaseDeletedModelDefaultTimeout)
		defer cancel()
		if err := h.modelDefaults.ReleaseDeletedProjectModelDefaults(releaseCtx, row); err != nil {
			slog.WarnContext(releaseCtx, "some projects still name the deleted model as their default; the catalogue skips it",
				"project_id", projectID, "section", string(deleted.section), "err", err)
		}
	})
}

// runModelDefaultRelease runs one background fan-out. A test replaces it
// through modelDefaultRelease to run the fan-out inline. At most
// maxModelDefaultReleases run at the same time; the others wait.
func (h *Handler) runModelDefaultRelease(release func()) {
	if h.modelDefaultRelease != nil {
		h.modelDefaultRelease(release)
		return
	}
	go func() {
		modelDefaultReleaseSlots <- struct{}{}
		defer func() { <-modelDefaultReleaseSlots }()
		release()
	}()
}

const (
	// releaseDeletedPlatformDefaultTimeout bounds the short first step of a
	// release, which runs before the response.
	releaseDeletedPlatformDefaultTimeout = 15 * time.Second
	// releaseDeletedModelDefaultTimeout bounds the background fan-out of one
	// delete.
	releaseDeletedModelDefaultTimeout = 10 * time.Minute
	// maxModelDefaultReleases bounds the background fan-outs of this process.
	maxModelDefaultReleases = 2
)

var modelDefaultReleaseSlots = make(chan struct{}, maxModelDefaultReleases)

// platformModelDeleteKey marks a delete that came through DeleteGlobalModel.
type platformModelDeleteKey struct{}

func withPlatformModelDelete(ctx context.Context) context.Context {
	return context.WithValue(ctx, platformModelDeleteKey{}, true)
}

func isPlatformModelDelete(ctx context.Context) bool {
	marked, _ := ctx.Value(platformModelDeleteKey{}).(bool)
	return marked
}

// deletedModelRow is what a configuration delete reports about its row.
type deletedModelRow struct {
	id      int32
	section configurationapp.CurrentModelSection
	// name is the value a stored default names: data.name for a model, and
	// elitea_title for a vector storage row (repos/models.go reads the same).
	name   string
	shared bool
}

func deletedModelRowFrom(id int32, section, title, dataName string, shared bool) deletedModelRow {
	row := deletedModelRow{id: id, section: configurationapp.CurrentModelSection(section), name: dataName, shared: shared}
	if row.section == configurationapp.CurrentModelSectionVectorStorage {
		row.name = title
	}
	return row
}

func (row deletedModelRow) modelRow(owner int32) configurationapp.PlatformModelDefaultModelRow {
	return configurationapp.PlatformModelDefaultModelRow{
		OwnerProjectID: owner, RowID: row.id, Section: row.section, Name: row.name, Shared: row.shared,
	}
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
	var id int32
	var section, title, dataName string
	var shared bool
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT id, section, COALESCE(elitea_title, ''), COALESCE(data->>'name', ''), COALESCE(shared, false)
		  FROM %s.configuration
		 WHERE %s = $1`, schema, configurationIDColumn(configID)), configID,
	).Scan(&id, &section, &title, &dataName, &shared)
	if errors.Is(err, pgx.ErrNoRows) {
		return deletedModelRow{}, false, nil
	}
	if err != nil {
		return deletedModelRow{}, false, err
	}
	return deletedModelRowFrom(id, section, title, dataName, shared), true, nil
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
