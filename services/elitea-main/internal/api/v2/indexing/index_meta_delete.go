package indexing

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"unicode"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentIndexMetaDeletePath       = SourceOnlyIndexDeletePath
	CurrentIndexMetaDeleteMode       = SourceOnlyIndexDeleteMode
	CurrentIndexMetaDeletePermission = SourceOnlyIndexDeletePermission
)

var ErrInvalidCurrentIndexMetaDeleteRoute = errors.New(
	"invalid current index metadata delete route dependencies",
)

type CurrentIndexMetaDeleter interface {
	Delete(context.Context, indexmetaapp.DeleteRequest) error
}

var _ CurrentIndexMetaDeleter = (*indexmetaapp.DeleteService)(nil)

// CurrentIndexMetaDeleteRoute remains source-only until independent review
// composes it. Auth and project RBAC always run before the application service.
type CurrentIndexMetaDeleteRoute struct {
	handler http.Handler
}

func NewCurrentIndexMetaDeleteRoute(
	deleter CurrentIndexMetaDeleter,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentIndexMetaDeleteRoute, error) {
	if deleter == nil || authConfig.PrincipalValidator == nil ||
		authConfig.ForwardedIdentityVerifier == nil || permissions == nil {
		return nil, ErrInvalidCurrentIndexMetaDeleteRoute
	}

	endpoint := http.Handler(http.HandlerFunc(
		(&currentIndexMetaDeleteHandler{deleter: deleter}).delete,
	))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentIndexMetaDeleteMode,
		func(request *http.Request) (string, bool) {
			projectID, valid := currentIndexMetaDeleteID(
				chi.URLParam(request, "projectID"),
			)
			return strconv.FormatInt(projectID, 10), valid
		},
		CurrentIndexMetaDeletePermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(
		SourceOnlyIndexDeleteMethod,
		CurrentIndexMetaDeletePath,
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			// Werkzeug's int converter rejects non-decimal path components
			// before authentication while accepting Unicode decimal digits.
			if !currentIndexMetaDeleteIntegerToken(
				chi.URLParam(request, "projectID"),
			) || !currentIndexMetaDeleteIntegerToken(
				chi.URLParam(request, "toolkitID"),
			) {
				http.NotFound(writer, request)
				return
			}
			endpoint.ServeHTTP(writer, request)
		}),
	)
	return &CurrentIndexMetaDeleteRoute{handler: router}, nil
}

func (route *CurrentIndexMetaDeleteRoute) ServeHTTP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentIndexMetaDeleteHandler struct {
	deleter CurrentIndexMetaDeleter
}

func (handler *currentIndexMetaDeleteHandler) delete(
	writer http.ResponseWriter,
	request *http.Request,
) {
	projectID, projectOK := currentIndexMetaDeleteID(
		chi.URLParam(request, "projectID"),
	)
	toolkitID, toolkitOK := currentIndexMetaDeleteID(
		chi.URLParam(request, "toolkitID"),
	)
	principal, authenticated := auth.RuntimePrincipalFromContext(
		request.Context(),
	)
	if !authenticated {
		writeError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	actorUserID, owningUser := principal.OwningUserID()
	if !projectOK || !toolkitOK || !owningUser {
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "Invalid index metadata request",
		})
		return
	}
	deletion := indexmetaapp.DeleteRequest{
		ProjectID:   projectID,
		ActorUserID: actorUserID,
		ToolkitID:   toolkitID,
		IndexMetaID: chi.URLParam(request, "indexMetaID"),
	}
	if err := handler.deleter.Delete(request.Context(), deletion); err != nil {
		writeCurrentIndexMetaDeleteError(
			writer,
			err,
			deletion.ToolkitID,
			deletion.IndexMetaID,
		)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]bool{"ok": true})
}

func writeCurrentIndexMetaDeleteError(
	writer http.ResponseWriter,
	err error,
	toolkitID int64,
	indexMetaID string,
) {
	var toolkitMissing *indexmetaapp.ScheduleToolkitMissingError
	var cleanup *indexmetaapp.ScheduleCleanupError
	switch {
	case errors.Is(err, indexmetaapp.ErrInvalidCurrentIndexMetaRequest):
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "Invalid index metadata request",
		})
	case errors.Is(err, indexmetaapp.ErrCurrentIndexMetaToolkitMissing):
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "Toolkit id is missing for toolkit None",
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
			"ok": false, "error": "index_meta " + indexMetaID + " not found",
		})
	case errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict):
		// Only the registry service returns this: an index whose run is still
		// writing cannot be deleted. The Python path deletes regardless and
		// never reaches this arm.
		writeJSON(writer, http.StatusConflict, map[string]any{
			"ok": false, "error": "index_meta " + indexMetaID + " has a run in progress; stop it before deleting",
		})
	case errors.As(err, &toolkitMissing):
		writeJSON(writer, http.StatusNotFound, map[string]any{
			"ok": false, "error": toolkitMissing.Error(),
		})
	case errors.As(err, &cleanup):
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"ok": false, "error": cleanup.Error(),
		})
	default:
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "Error occurred while deleting index_meta",
		})
	}
}

func currentIndexMetaDeleteIntegerToken(value string) bool {
	if value == "" {
		return false
	}
	digits := 0
	for _, character := range value {
		if !unicode.IsDigit(character) {
			return false
		}
		digits++
	}
	// CPython's default integer-conversion safety limit is 4,300 digits.
	return digits <= 4_300
}

func currentIndexMetaDeleteID(value string) (int64, bool) {
	if !currentIndexMetaDeleteIntegerToken(value) {
		return 0, false
	}
	var parsed int64
	for _, character := range value {
		digit, ok := currentIndexMetaDeleteDigit(character)
		if !ok || parsed > (maxCurrentIndexMetaDeleteID-digit)/10 {
			return 0, false
		}
		parsed = parsed*10 + digit
	}
	return parsed, parsed > 0
}

const maxCurrentIndexMetaDeleteID = int64(^uint64(0) >> 1)

func currentIndexMetaDeleteDigit(character rune) (int64, bool) {
	if character <= '\uffff' {
		for _, characterRange := range unicode.Digit.R16 {
			value := uint16(character)
			if value < characterRange.Lo || value > characterRange.Hi ||
				(value-characterRange.Lo)%characterRange.Stride != 0 {
				continue
			}
			return int64(
				(value-characterRange.Lo)/characterRange.Stride,
			) % 10, true
		}
	}
	for _, characterRange := range unicode.Digit.R32 {
		value := uint32(character)
		if value < characterRange.Lo || value > characterRange.Hi ||
			(value-characterRange.Lo)%characterRange.Stride != 0 {
			continue
		}
		return int64((value-characterRange.Lo)/characterRange.Stride) % 10, true
	}
	return 0, false
}
