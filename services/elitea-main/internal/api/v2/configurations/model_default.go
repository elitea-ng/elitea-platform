package configurations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentModelDefaultPath       = "/api/v2/configurations/models/{projectID}"
	CurrentModelDefaultMode       = auth.PermissionModeDefault
	CurrentModelDefaultPermission = "configurations.configuration.update"
)

var ErrInvalidCurrentModelDefaultRoute = errors.New("invalid current model default route dependencies")

// CurrentModelDefaultWriter is implemented by CurrentSecretVaultRepository.
// The one method must replace both current default-model keys atomically.
type CurrentModelDefaultWriter interface {
	SetCurrentModelDefault(context.Context, configurationapp.CurrentModelDefaultSelection) error
}

// CurrentModelDefaultRoute preserves the current POST wire and Vault contracts.
// The Python endpoint has no explicit permission decorator; the existing
// configuration-update permission is the closest exact mutation boundary and
// retains the current admin/editor role assignment while excluding viewers.
type CurrentModelDefaultRoute struct {
	handler http.Handler
}

func NewCurrentModelDefaultRoute(
	writer CurrentModelDefaultWriter,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentModelDefaultRoute, error) {
	if writer == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentModelDefaultRoute
	}

	handler := &currentModelDefaultHandler{writer: writer}
	post := http.Handler(http.HandlerFunc(handler.post))
	post = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentModelDefaultMode,
		currentConfigurationProjectID,
		CurrentModelDefaultPermission,
	)(post)
	post = apimw.Auth(authConfig)(post)

	router := chi.NewRouter()
	router.Method(http.MethodPost, CurrentModelDefaultPath, post)
	return &CurrentModelDefaultRoute{handler: router}, nil
}

func (route *CurrentModelDefaultRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(w, r)
		return
	}
	route.handler.ServeHTTP(w, r)
}

type currentModelDefaultHandler struct {
	writer CurrentModelDefaultWriter
}

func (h *currentModelDefaultHandler) post(w http.ResponseWriter, r *http.Request) {
	projectID, ok := currentConfigurationID(chi.URLParam(r, "projectID"))
	if !ok {
		writeCurrentConfigurationError(w, http.StatusBadRequest, "invalid project")
		return
	}

	request, err := decodeCurrentModelDefaultRequest(w, r)
	if err != nil {
		writeCurrentConfigurationError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.writer.SetCurrentModelDefault(r.Context(), configurationapp.CurrentModelDefaultSelection{
		ProjectID:       projectID,
		Name:            request.Name,
		TargetProjectID: request.TargetProjectID,
		Section:         request.Section,
	}); err != nil {
		// The current endpoint maps every Vault failure to this stable response.
		// Do not expose storage, encryption, or secret material through the API.
		writeJSON(w, http.StatusBadRequest, map[string]string{"result": "error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": "success"})
}

type currentModelDefaultRequest struct {
	Name            string
	TargetProjectID int64
	Section         string
}

func decodeCurrentModelDefaultRequest(w http.ResponseWriter, r *http.Request) (currentModelDefaultRequest, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return currentModelDefaultRequest{}, errors.New("request content type is not JSON")
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxConfigurationRequestBytes)
	decoder := json.NewDecoder(r.Body)
	fields := make(map[string]json.RawMessage)
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return currentModelDefaultRequest{}, errors.New("request body is not an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return currentModelDefaultRequest{}, errors.New("request body has trailing data")
	}

	nameRaw, ok := fields["name"]
	if !ok {
		return currentModelDefaultRequest{}, errors.New("name is required")
	}
	var name string
	if err := json.Unmarshal(nameRaw, &name); err != nil {
		return currentModelDefaultRequest{}, errors.New("name must be a string")
	}

	targetRaw, ok := fields["target_project_id"]
	if !ok {
		return currentModelDefaultRequest{}, errors.New("target project is required")
	}
	targetProjectID, ok := currentPydanticInteger(targetRaw)
	if !ok {
		return currentModelDefaultRequest{}, errors.New("target project must be an integer")
	}

	section := "llm"
	if sectionRaw, present := fields["section"]; present {
		if bytes.Equal(bytes.TrimSpace(sectionRaw), []byte("null")) {
			// SetDefaultModel.section is Optional[str]. Python interpolates None
			// into the Vault key as the literal text "None".
			section = "None"
		} else if err := json.Unmarshal(sectionRaw, &section); err != nil {
			return currentModelDefaultRequest{}, errors.New("section must be a string or null")
		}
	}

	return currentModelDefaultRequest{
		Name:            name,
		TargetProjectID: targetProjectID,
		Section:         section,
	}, nil
}

func currentPydanticInteger(raw json.RawMessage) (int64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0, false
	}
	if bytes.Equal(trimmed, []byte("true")) {
		return 1, true
	}
	if bytes.Equal(trimmed, []byte("false")) {
		return 0, true
	}

	value := string(trimmed)
	quoted := trimmed[0] == '"'
	if quoted {
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return 0, false
		}
		value = strings.TrimSpace(value)
		// Pydantic accepts integral decimal strings, but not exponent notation
		// when the JSON value itself is a string.
		if strings.ContainsAny(value, "eE") {
			return 0, false
		}
	}
	if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
		return parsed, true
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) ||
		math.Trunc(parsed) != parsed || parsed < -float64(uint64(1)<<63) || parsed >= float64(uint64(1)<<63) {
		return 0, false
	}
	return int64(parsed), true
}
