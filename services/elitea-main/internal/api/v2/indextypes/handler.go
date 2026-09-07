package indextypes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentIndexTypesPath       = "/api/v2/elitea_core/index_types/prompt_lib/{projectID}"
	CurrentIndexTypesMode       = auth.PermissionModeDefault
	CurrentIndexTypesPermission = "models.applications.index_types.details"
)

const currentIndexTypesRoutePath = "/api/v2/elitea_core/index_types/prompt_lib/{projectID:[0-9]+}"

var ErrInvalidCurrentIndexTypesRoute = errors.New("invalid current index-types route dependencies")

// CurrentIndexTypes is the extension-to-MIME projection the unchanged EliteaUI
// useFileTypes hook consumes. Do not wrap it in an index_types envelope.
type CurrentIndexTypes struct {
	DocumentTypes map[string]string `json:"document_types"`
	ImageTypes    map[string]string `json:"image_types"`
	CodeTypes     map[string]string `json:"code_types"`
}

// CurrentIndexTypeCategory is one entry of the published `items` array. Its
// four fields are the ones DocumentLoadersResponse makes required in
// api/openapi/v2.yaml, so a client generated from that spec reads it.
type CurrentIndexTypeCategory struct {
	Type                string   `json:"type"`
	Name                string   `json:"name"`
	Description         string   `json:"description"`
	SupportedExtensions []string `json:"supported_extensions"`
}

// currentIndexTypesResponse answers BOTH shipped clients from one body.
//
// The Pylon keys document_types, image_types and code_types are unchanged.
// apps/elitea-ui reads exactly those three maps (src/api/applications.js:849
// calls this path, src/slices/fileTypes.js:26-28 reads the maps). The pinned
// SDK fixture in testdata pins their contents.
//
// The `items` and `total` keys are the PUBLISHED contract for this path —
// DocumentLoadersResponse in api/openapi/v2.yaml. apps/elitea-web holds a
// client generated from that schema, and shared/api/unwrap.ts takes `items`
// first. Before this envelope carried them, a body with no `items` key read
// as an unrecognised shape, so ELITEA_INDEX_TYPES_ENABLED could not be turned
// on without breaking that client (#394). #395 answered the same question the
// same way for the attached-skills read.
//
// The two halves project the SAME rows, so they cannot disagree: every
// `supported_extensions` list is the sorted key set of the map named by the
// same `type`. Extra keys are contract-legal — DocumentLoadersResponse does
// not close its object, so a generated client ignores what it did not ask for.
type currentIndexTypesResponse struct {
	Items []CurrentIndexTypeCategory `json:"items"`
	Total int                        `json:"total"`

	DocumentTypes map[string]string `json:"document_types"`
	ImageTypes    map[string]string `json:"image_types"`
	CodeTypes     map[string]string `json:"code_types"`
}

// currentIndexTypeCategoryLabels names the three categories the pinned SDK
// snapshot projects. The prototype handler this route replaces listed six
// ingestion sources that no data backs; these three describe the rows the
// route actually serves.
var currentIndexTypeCategoryLabels = [3]struct {
	Type        string
	Name        string
	Description string
}{
	{
		Type:        "document_types",
		Name:        "Document types",
		Description: "File extensions the indexer loads as documents.",
	},
	{
		Type:        "image_types",
		Name:        "Image types",
		Description: "File extensions the indexer loads as images.",
	},
	{
		Type:        "code_types",
		Name:        "Code types",
		Description: "File extensions the indexer loads as plain-text code.",
	},
}

// newCurrentIndexTypesResponse builds both halves from one snapshot read.
//
// The category order is fixed, and every extension list is sorted, so the same
// request always gets the same bytes.
func newCurrentIndexTypesResponse(
	result CurrentIndexTypes,
) currentIndexTypesResponse {
	if result.DocumentTypes == nil {
		result.DocumentTypes = map[string]string{}
	}
	if result.ImageTypes == nil {
		result.ImageTypes = map[string]string{}
	}
	if result.CodeTypes == nil {
		result.CodeTypes = map[string]string{}
	}

	byType := map[string]map[string]string{
		"document_types": result.DocumentTypes,
		"image_types":    result.ImageTypes,
		"code_types":     result.CodeTypes,
	}
	items := make([]CurrentIndexTypeCategory, 0, len(currentIndexTypeCategoryLabels))
	for _, label := range currentIndexTypeCategoryLabels {
		extensions := make([]string, 0, len(byType[label.Type]))
		for extension := range byType[label.Type] {
			extensions = append(extensions, extension)
		}
		slices.Sort(extensions)
		items = append(items, CurrentIndexTypeCategory{
			Type:                label.Type,
			Name:                label.Name,
			Description:         label.Description,
			SupportedExtensions: extensions,
		})
	}

	return currentIndexTypesResponse{
		Items:         items,
		Total:         len(items),
		DocumentTypes: result.DocumentTypes,
		ImageTypes:    result.ImageTypes,
		CodeTypes:     result.CodeTypes,
	}
}

type CurrentIndexTypesReader interface {
	GetCurrentIndexTypes(context.Context, int32) (CurrentIndexTypes, error)
}

// CurrentIndexTypesRoute owns only the current read-only GET contract.
// Production composition mounts it atomically behind an explicit default-off
// feature gate.
type CurrentIndexTypesRoute struct {
	handler http.Handler
}

func NewCurrentIndexTypesRoute(
	reader CurrentIndexTypesReader,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentIndexTypesRoute, error) {
	// ForwardedIdentityVerifier is OPTIONAL, PrincipalValidator is not.
	//
	// The verifier proves that an X-Auth-* request arrived over the
	// header-stripping ingress; only the Form plane builds one. An OIDC or SAML
	// install authenticates the same caller through a session cookie or a
	// bearer token, and apimw.Auth reads those without a verifier. Demanding it
	// here refused the whole capability to every install that has no Form
	// authentication document — and this route is the ONLY handler for its path
	// now that the prototype fallback is deleted (#394), so the refusal became
	// a 404 on a path the published contract declares.
	//
	// PrincipalValidator stays mandatory: without it a deactivated user's
	// unexpired credential still passes, because RBAC rows survive deactivation
	// (#301, #314, #370). internal/api/v2/projects/production.go states the same
	// split for the same reason.
	if reader == nil || authConfig.PrincipalValidator == nil || permissions == nil {
		return nil, ErrInvalidCurrentIndexTypesRoute
	}

	endpoint := http.Handler(http.HandlerFunc(
		(&currentIndexTypesHandler{reader: reader}).get,
	))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentIndexTypesMode,
		currentIndexTypesProjectID,
		CurrentIndexTypesPermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(http.MethodGet, currentIndexTypesRoutePath, endpoint)
	return &CurrentIndexTypesRoute{handler: router}, nil
}

func (route *CurrentIndexTypesRoute) ServeHTTP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentIndexTypesHandler struct {
	reader CurrentIndexTypesReader
}

func (handler *currentIndexTypesHandler) get(
	writer http.ResponseWriter,
	request *http.Request,
) {
	projectID, ok := currentIndexTypesID(chi.URLParam(request, "projectID"))
	if !ok {
		writeCurrentIndexTypesFailure(writer)
		return
	}

	result, err := handler.reader.GetCurrentIndexTypes(request.Context(), projectID)
	if err != nil {
		writeCurrentIndexTypesFailure(writer)
		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(newCurrentIndexTypesResponse(result))
}

func currentIndexTypesProjectID(request *http.Request) (string, bool) {
	projectID, ok := currentIndexTypesID(chi.URLParam(request, "projectID"))
	if !ok {
		return "", false
	}
	return strconv.FormatInt(int64(projectID), 10), true
}

func currentIndexTypesID(value string) (int32, bool) {
	if value == "" {
		return 0, false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	return int32(parsed), err == nil && parsed > 0
}

func writeCurrentIndexTypesFailure(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(writer).Encode(map[string]string{
		"error": "Failed to get index types",
	})
}
