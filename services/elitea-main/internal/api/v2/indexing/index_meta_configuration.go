package indexing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	// CurrentIndexConfigurationMethod/Path is the SAVE half of the index
	// editor's "Save" / "Save & Reindex" split. It is a sibling of the
	// schedule PATCH on the same prefix and takes the same permission: both
	// edit one index's stored settings, and neither starts a run.
	CurrentIndexConfigurationMethod = http.MethodPut
	CurrentIndexConfigurationPath   = "/api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}/configuration"
	CurrentIndexConfigurationMode   = auth.PermissionModeDefault
	// The EDIT permission, not the run permission
	// (`models.applications.tool.patch`). Saving a configuration is not
	// running a tool, and a member who may configure an index but not spend
	// worker time on it must be able to do exactly this.
	CurrentIndexConfigurationPermission = SourceOnlyIndexSchedulePermission
	// maxCurrentIndexConfigurationBodyBytes bounds the decoded request. It is
	// the application's own configuration ceiling plus room for the one-key
	// envelope around it, so a body the application would accept is never
	// refused by the reader instead.
	maxCurrentIndexConfigurationBodyBytes = int64(indexmetaapp.MaxCurrentIndexConfigurationBytes) + (4 << 10)
)

var ErrInvalidCurrentIndexConfigurationRoute = errors.New(
	"invalid current index configuration route dependencies",
)

// CurrentIndexConfigurationSaver is the application boundary this route calls.
type CurrentIndexConfigurationSaver interface {
	SaveConfiguration(context.Context, indexmetaapp.ConfigurationRequest) error
}

var _ CurrentIndexConfigurationSaver = (*indexmetaapp.ConfigurationService)(nil)

// CurrentIndexConfigurationRoute binds the exact path and method to trusted
// authentication and project RBAC, which always run before the application
// service sees the request.
type CurrentIndexConfigurationRoute struct {
	handler http.Handler
}

func NewCurrentIndexConfigurationRoute(
	saver CurrentIndexConfigurationSaver,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentIndexConfigurationRoute, error) {
	if saver == nil || authConfig.PrincipalValidator == nil ||
		authConfig.ForwardedIdentityVerifier == nil || permissions == nil {
		return nil, ErrInvalidCurrentIndexConfigurationRoute
	}
	handler := &currentIndexConfigurationHandler{saver: saver}
	endpoint := http.Handler(http.HandlerFunc(handler.save))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentIndexConfigurationMode,
		func(request *http.Request) (string, bool) {
			projectID, valid := positiveCurrentIndexMetaID(chi.URLParam(request, "projectID"))
			return strconv.FormatInt(projectID, 10), valid
		},
		CurrentIndexConfigurationPermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(CurrentIndexConfigurationMethod, CurrentIndexConfigurationPath, endpoint)
	return &CurrentIndexConfigurationRoute{handler: router}, nil
}

func (route *CurrentIndexConfigurationRoute) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentIndexConfigurationHandler struct {
	saver CurrentIndexConfigurationSaver
}

// currentIndexConfigurationBody is the request envelope.
//
// A NAMED key rather than the bare configuration object, for one reason: the
// bare form has no room to grow, and this route's next question is already
// visible (which generation the caller believed it was editing). The client
// sends `{"index_configuration": {...}}`; anything else is refused.
type currentIndexConfigurationBody struct {
	IndexConfiguration json.RawMessage `json:"index_configuration"`
}

func (handler *currentIndexConfigurationHandler) save(writer http.ResponseWriter, request *http.Request) {
	projectID, projectOK := positiveCurrentIndexMetaID(chi.URLParam(request, "projectID"))
	toolkitID, toolkitOK := positiveCurrentIndexMetaID(chi.URLParam(request, "toolkitID"))
	principal, authenticated := auth.RuntimePrincipalFromContext(request.Context())
	if !authenticated {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	actorUserID, owningUser := principal.OwningUserID()
	if !projectOK || !toolkitOK || !owningUser {
		writeCurrentIndexConfigurationError(
			writer,
			indexmetaapp.ErrInvalidCurrentIndexMetaRequest,
			toolkitID,
		)
		return
	}
	body, err := decodeCurrentIndexConfigurationBody(request)
	if err != nil {
		writeCurrentIndexConfigurationError(writer, err, toolkitID)
		return
	}
	save := indexmetaapp.ConfigurationRequest{
		ProjectID:     projectID,
		ActorUserID:   actorUserID,
		ToolkitID:     toolkitID,
		IndexName:     chi.URLParam(request, "indexMetaID"),
		Configuration: body.IndexConfiguration,
	}
	if err := handler.saver.SaveConfiguration(request.Context(), save); err != nil {
		writeCurrentIndexConfigurationError(writer, err, toolkitID)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

func decodeCurrentIndexConfigurationBody(request *http.Request) (currentIndexConfigurationBody, error) {
	var body currentIndexConfigurationBody
	if request.Body == nil {
		return body, indexmetaapp.ErrCurrentIndexConfigurationInvalid
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxCurrentIndexConfigurationBodyBytes))
	if err := decoder.Decode(&body); err != nil {
		return currentIndexConfigurationBody{}, indexmetaapp.ErrCurrentIndexConfigurationInvalid
	}
	return body, nil
}

// writeCurrentIndexConfigurationError keeps every storage detail out of the
// response. The envelope is the one every index write path already answers
// with, so the client reads `ok` exactly as it does for delete and cancel.
func writeCurrentIndexConfigurationError(writer http.ResponseWriter, err error, toolkitID int64) {
	switch {
	case errors.Is(err, indexmetaapp.ErrCurrentIndexConfigurationInvalid):
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "index_configuration must be a JSON object",
		})
	case errors.Is(err, indexmetaapp.ErrInvalidCurrentIndexMetaRequest):
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "Invalid index metadata request",
		})
	case errors.Is(err, indexmetaapp.ErrCurrentIndexMetaToolkitMissing):
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "Toolkit id is missing for toolkit " + strconv.FormatInt(toolkitID, 10),
		})
	case errors.Is(err, indexmetaapp.ErrCurrentIndexMetaConnectionMissing):
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"ok": false,
			"error": "Connection string is missing in PGVector configuration for toolkit " +
				strconv.FormatInt(toolkitID, 10),
		})
	case errors.Is(err, indexmetaapp.ErrCurrentIndexMetaTargetMissing):
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"ok": false,
			"error": "PGVector configuration is missing for toolkit " +
				strconv.FormatInt(toolkitID, 10),
		})
	case errors.Is(err, indexmetaapp.ErrCurrentIndexMetaNotFound):
		writeJSON(writer, http.StatusNotFound, map[string]any{
			"ok": false, "error": "index_meta not found",
		})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeJSON(writer, http.StatusGatewayTimeout, map[string]any{
			"ok": false, "error": "Index configuration request timed out",
		})
	default:
		writeJSON(writer, http.StatusBadGateway, map[string]any{
			"ok": false, "error": "Error occurred while saving the index configuration",
		})
	}
}
