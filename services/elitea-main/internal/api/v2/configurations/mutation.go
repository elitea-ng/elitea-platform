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
	"sort"
	"strings"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	MaxCurrentConfigurationMutationBodyBytes = int64(1 << 20)
	maxCurrentConfigurationMutationJSONDepth = 128

	CurrentConfigurationMutationMode     = auth.PermissionModeDefault
	CurrentConfigurationCreatePermission = "configurations.configuration.create"
	CurrentConfigurationUpdatePermission = "configurations.configuration.update"
	CurrentConfigurationDeletePermission = "configurations.configuration.delete"
)

var ErrInvalidCurrentConfigurationMutationRoute = errors.New("invalid current configuration mutation route dependencies")

// CurrentConfigurationMutator is the application boundary required by the
// current Configurations mutation endpoints. The application commands contain
// only caller-owned fields plus server-derived project and author identities.
type CurrentConfigurationMutator interface {
	Create(context.Context, configurationapp.CurrentConfigurationCreateRequest) (configurationapp.CurrentConfiguration, error)
	Update(context.Context, configurationapp.CurrentConfigurationUpdateRequest) (configurationapp.CurrentConfiguration, error)
	Delete(context.Context, configurationapp.CurrentConfigurationDeleteRequest) error
}

var _ CurrentConfigurationMutator = (*configurationapp.CurrentConfigurationMutationService)(nil)

// CurrentConfigurationMutationRoute owns the exact current POST, PUT, and
// DELETE paths. Construction binds authentication and project-scoped RBAC;
// production composition may mount the route only with the complete mutation
// service (registry validation, secret lifecycle, events, and persistence).
type CurrentConfigurationMutationRoute struct {
	handler http.Handler
}

func NewCurrentConfigurationMutationRoute(
	mutator CurrentConfigurationMutator,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentConfigurationMutationRoute, error) {
	if mutator == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentConfigurationMutationRoute
	}

	handler := &currentConfigurationMutationHandler{mutator: mutator}
	post := currentConfigurationMutationEndpoint(
		handler.create,
		CurrentConfigurationCreatePermission,
		authConfig,
		permissions,
	)
	put := currentConfigurationMutationEndpoint(
		handler.update,
		CurrentConfigurationUpdatePermission,
		authConfig,
		permissions,
	)
	deleteEndpoint := currentConfigurationMutationEndpoint(
		handler.delete,
		CurrentConfigurationDeletePermission,
		authConfig,
		permissions,
	)

	router := chi.NewRouter()
	router.Method(http.MethodPost, CurrentConfigurationListPath, post)
	router.Method(http.MethodPut, CurrentConfigurationDetailsPath, put)
	router.Method(http.MethodDelete, CurrentConfigurationDetailsPath, deleteEndpoint)
	return &CurrentConfigurationMutationRoute{handler: router}, nil
}

func currentConfigurationMutationEndpoint(
	handler http.HandlerFunc,
	permission string,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) http.Handler {
	endpoint := http.Handler(handler)
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentConfigurationMutationMode,
		currentConfigurationProjectID,
		permission,
	)(endpoint)
	return apimw.Auth(authConfig)(endpoint)
}

func (route *CurrentConfigurationMutationRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if route == nil || route.handler == nil {
		http.NotFound(w, r)
		return
	}
	route.handler.ServeHTTP(w, r)
}

type currentConfigurationMutationHandler struct {
	mutator CurrentConfigurationMutator
}

func (h *currentConfigurationMutationHandler) create(w http.ResponseWriter, r *http.Request) {
	projectID, ok := currentConfigurationID(chi.URLParam(r, "projectID"))
	if !ok {
		writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "invalid project", "project_id")
		return
	}
	authorID, ok := currentConfigurationMutationAuthorID(r.Context())
	if !ok {
		writeCurrentConfigurationMutationError(w, http.StatusUnauthorized, "authentication required", "author_id")
		return
	}

	fields, requestError := decodeCurrentConfigurationMutationObject(w, r, currentConfigurationCreateField)
	if requestError != nil {
		writeCurrentConfigurationMutationError(w, requestError.status, requestError.message, requestError.field)
		return
	}
	eliteaTitle, requestError := requiredCurrentConfigurationString(fields, "elitea_title")
	if requestError != nil {
		writeCurrentConfigurationMutationError(w, requestError.status, requestError.message, requestError.field)
		return
	}
	var label *string
	if raw, present := fields["label"]; present {
		if err := decodeCurrentConfigurationMutationScalar(raw, &label, true); err != nil {
			writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "invalid field value", "label")
			return
		}
	}
	configurationType, requestError := requiredCurrentConfigurationString(fields, "type")
	if requestError != nil {
		writeCurrentConfigurationMutationError(w, requestError.status, requestError.message, requestError.field)
		return
	}
	data, requestError := requiredCurrentConfigurationObject(fields, "data")
	if requestError != nil {
		writeCurrentConfigurationMutationError(w, requestError.status, requestError.message, requestError.field)
		return
	}
	shared := false
	if raw, present := fields["shared"]; present {
		if err := decodeCurrentConfigurationMutationScalar(raw, &shared, false); err != nil {
			writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "invalid field value", "shared")
			return
		}
	}

	configuration, err := h.mutator.Create(r.Context(), configurationapp.CurrentConfigurationCreateRequest{
		ProjectID:   projectID,
		AuthorID:    authorID,
		EliteaTitle: eliteaTitle,
		Label:       label,
		Type:        configurationType,
		Shared:      shared,
		Data:        data,
	})
	if err != nil {
		writeCurrentConfigurationMutationServiceError(w, err)
		return
	}
	writeCurrentConfigurationMutationDTO(w, configuration)
}

func (h *currentConfigurationMutationHandler) update(w http.ResponseWriter, r *http.Request) {
	projectID, configurationID, ok := currentConfigurationMutationIDs(r)
	if !ok {
		writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "invalid configuration", "configuration_id")
		return
	}
	authorID, ok := currentConfigurationMutationAuthorID(r.Context())
	if !ok {
		writeCurrentConfigurationMutationError(w, http.StatusUnauthorized, "authentication required", "author_id")
		return
	}

	fields, requestError := decodeCurrentConfigurationMutationObject(w, r, currentConfigurationUpdateField)
	if requestError != nil {
		writeCurrentConfigurationMutationError(w, requestError.status, requestError.message, requestError.field)
		return
	}
	request := configurationapp.CurrentConfigurationUpdateRequest{
		ProjectID:       projectID,
		ConfigurationID: configurationID,
		AuthorID:        authorID,
	}
	if raw, present := fields["elitea_title"]; present {
		var value string
		if err := decodeCurrentConfigurationMutationScalar(raw, &value, false); err != nil {
			writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "invalid field value", "elitea_title")
			return
		}
		request.EliteaTitle = &value
	}
	if raw, present := fields["label"]; present {
		request.LabelSet = true
		if err := decodeCurrentConfigurationMutationScalar(raw, &request.Label, true); err != nil {
			writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "invalid field value", "label")
			return
		}
	}
	if raw, present := fields["data"]; present {
		request.DataSet = true
		value, err := decodeCurrentConfigurationMutationObjectField(raw, true)
		if err != nil {
			writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "invalid field value", "data")
			return
		}
		request.Data = value
	}
	if raw, present := fields["meta"]; present {
		request.MetaSet = true
		value, err := decodeCurrentConfigurationMutationObjectField(raw, true)
		if err != nil {
			writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "invalid field value", "meta")
			return
		}
		request.Meta = value
	}
	if raw, present := fields["shared"]; present {
		var value bool
		if err := decodeCurrentConfigurationMutationScalar(raw, &value, false); err != nil {
			writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "invalid field value", "shared")
			return
		}
		request.Shared = &value
	}

	configuration, err := h.mutator.Update(r.Context(), request)
	if err != nil {
		writeCurrentConfigurationMutationServiceError(w, err)
		return
	}
	writeCurrentConfigurationMutationDTO(w, configuration)
}

func (h *currentConfigurationMutationHandler) delete(w http.ResponseWriter, r *http.Request) {
	projectID, configurationID, ok := currentConfigurationMutationIDs(r)
	if !ok {
		writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "invalid configuration", "configuration_id")
		return
	}
	authorID, ok := currentConfigurationMutationAuthorID(r.Context())
	if !ok {
		writeCurrentConfigurationMutationError(w, http.StatusUnauthorized, "authentication required", "author_id")
		return
	}
	if err := h.mutator.Delete(r.Context(), configurationapp.CurrentConfigurationDeleteRequest{
		ProjectID:       projectID,
		ConfigurationID: configurationID,
		AuthorID:        authorID,
	}); err != nil {
		writeCurrentConfigurationMutationServiceError(w, err)
		return
	}
	setCurrentConfigurationMutationResponseHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

func currentConfigurationMutationIDs(r *http.Request) (int32, int32, bool) {
	projectID, projectOK := currentConfigurationID(chi.URLParam(r, "projectID"))
	configurationID, configurationOK := currentConfigurationID(chi.URLParam(r, "configID"))
	return projectID, configurationID, projectOK && configurationOK
}

func currentConfigurationMutationAuthorID(ctx context.Context) (int32, bool) {
	principal, ok := auth.UserFromContext(ctx)
	if !ok {
		return 0, false
	}
	authorID, ok := principal.OwningUserID()
	if !ok || authorID > math.MaxInt32 {
		return 0, false
	}
	return int32(authorID), true
}

func currentConfigurationCreateField(field string) bool {
	switch field {
	case "elitea_title", "label", "type", "shared", "data":
		return true
	default:
		return false
	}
}

func currentConfigurationUpdateField(field string) bool {
	switch field {
	case "elitea_title", "label", "data", "meta", "shared":
		return true
	default:
		return false
	}
}

type currentConfigurationMutationRequestError struct {
	status  int
	message string
	field   string
}

func decodeCurrentConfigurationMutationObject(
	w http.ResponseWriter,
	r *http.Request,
	allowed func(string) bool,
) (map[string]json.RawMessage, *currentConfigurationMutationRequestError) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return nil, currentConfigurationMutationBadRequest("invalid request body", "body")
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxCurrentConfigurationMutationBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return nil, &currentConfigurationMutationRequestError{
				status: http.StatusRequestEntityTooLarge, message: "request body too large", field: "body",
			}
		}
		return nil, currentConfigurationMutationBadRequest("invalid request body", "body")
	}
	if err := validateCurrentConfigurationMutationJSON(raw); err != nil {
		return nil, currentConfigurationMutationBadRequest("invalid request body", "body")
	}

	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, currentConfigurationMutationBadRequest("invalid request body", "body")
	}
	unknown := make([]string, 0)
	for field := range fields {
		if !allowed(field) {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) != 0 {
		sort.Strings(unknown)
		return nil, currentConfigurationMutationBadRequest("field is not allowed", safeCurrentConfigurationMutationField(unknown[0]))
	}
	return fields, nil
}

func requiredCurrentConfigurationString(
	fields map[string]json.RawMessage,
	field string,
) (string, *currentConfigurationMutationRequestError) {
	raw, present := fields[field]
	if !present {
		return "", currentConfigurationMutationBadRequest("field is required", field)
	}
	var value string
	if err := decodeCurrentConfigurationMutationScalar(raw, &value, false); err != nil {
		return "", currentConfigurationMutationBadRequest("invalid field value", field)
	}
	return value, nil
}

func requiredCurrentConfigurationObject(
	fields map[string]json.RawMessage,
	field string,
) (map[string]any, *currentConfigurationMutationRequestError) {
	raw, present := fields[field]
	if !present {
		return nil, currentConfigurationMutationBadRequest("field is required", field)
	}
	value, err := decodeCurrentConfigurationMutationObjectField(raw, false)
	if err != nil {
		return nil, currentConfigurationMutationBadRequest("invalid field value", field)
	}
	return value, nil
}

func decodeCurrentConfigurationMutationScalar(raw json.RawMessage, target any, nullable bool) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && !nullable {
		return errors.New("null is not allowed")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("field contains trailing JSON")
	}
	return nil
}

func decodeCurrentConfigurationMutationObjectField(raw json.RawMessage, nullable bool) (map[string]any, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if nullable {
			return nil, nil
		}
		return nil, errors.New("null is not allowed")
	}
	var value map[string]any
	if err := decodeCurrentConfigurationMutationScalar(raw, &value, false); err != nil || value == nil {
		return nil, errors.New("field must be an object")
	}
	return value, nil
}

func validateCurrentConfigurationMutationJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("request body must be an object")
	}
	if err := consumeCurrentConfigurationMutationJSONObject(decoder, 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body contains trailing JSON")
		}
		return err
	}
	return nil
}

func consumeCurrentConfigurationMutationJSONObject(decoder *json.Decoder, depth int) error {
	if depth > maxCurrentConfigurationMutationJSONDepth {
		return errors.New("request body nesting is too deep")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := token.(string)
		if !ok {
			return errors.New("object field is not a string")
		}
		if _, duplicate := seen[field]; duplicate {
			return errors.New("duplicate object field")
		}
		seen[field] = struct{}{}
		if err := consumeCurrentConfigurationMutationJSONValue(decoder, depth); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return errors.New("object is not closed")
	}
	return nil
}

func consumeCurrentConfigurationMutationJSONValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		return consumeCurrentConfigurationMutationJSONObject(decoder, depth+1)
	case '[':
		if depth >= maxCurrentConfigurationMutationJSONDepth {
			return errors.New("request body nesting is too deep")
		}
		for decoder.More() {
			if err := consumeCurrentConfigurationMutationJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("array is not closed")
		}
		return nil
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func currentConfigurationMutationBadRequest(message, field string) *currentConfigurationMutationRequestError {
	return &currentConfigurationMutationRequestError{status: http.StatusBadRequest, message: message, field: field}
}

func safeCurrentConfigurationMutationField(field string) string {
	if field == "" || len(field) > 128 {
		return "body"
	}
	for _, character := range field {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' || character == '.' {
			continue
		}
		return "body"
	}
	return field
}

func writeCurrentConfigurationMutationDTO(w http.ResponseWriter, configuration configurationapp.CurrentConfiguration) {
	setCurrentConfigurationMutationResponseHeaders(w)
	writeJSON(w, http.StatusOK, newCurrentConfigurationDTO(configuration))
}

func setCurrentConfigurationMutationResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

func writeCurrentConfigurationMutationServiceError(w http.ResponseWriter, err error) {
	var mutationError *configurationapp.CurrentConfigurationMutationError
	if errors.As(err, &mutationError) {
		field := mutationError.Field
		if field == "" {
			field = "unknown"
		}
		switch mutationError.Code {
		case configurationapp.CurrentConfigurationMutationInvalid:
			writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "Invalid configuration", field)
		case configurationapp.CurrentConfigurationMutationUnknownType:
			writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "Unknown configuration type", field)
		case configurationapp.CurrentConfigurationMutationNormalizationRequired:
			writeCurrentConfigurationMutationError(
				w,
				http.StatusNotImplemented,
				"Configuration normalization is not implemented for this type",
				field,
			)
		case configurationapp.CurrentConfigurationMutationImmutable:
			writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "Configuration field is immutable", field)
		default:
			writeCurrentConfigurationMutationError(w, http.StatusInternalServerError, "Unexpected error", "unknown")
		}
		return
	}
	switch {
	case errors.Is(err, configurationapp.ErrCurrentConfigurationNotFound):
		writeCurrentConfigurationMutationError(w, http.StatusNotFound, "Configuration not found", "configuration_id")
	case errors.Is(err, configurationapp.ErrCurrentConfigurationConflict):
		writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "Configuration already exists", "elitea_title")
	case errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationRequest),
		errors.Is(err, configurationapp.ErrInvalidCurrentConfigurationMutation):
		writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "Invalid configuration", "unknown")
	case errors.Is(err, configurationapp.ErrUnknownCurrentConfigurationType):
		writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "Unknown configuration type", "type")
	case errors.Is(err, configurationapp.ErrCurrentConfigurationNormalizationRequired):
		writeCurrentConfigurationMutationError(
			w,
			http.StatusNotImplemented,
			"Configuration normalization is not implemented for this type",
			"data",
		)
	case errors.Is(err, configurationapp.ErrImmutableCurrentConfigurationField):
		writeCurrentConfigurationMutationError(w, http.StatusBadRequest, "Configuration field is immutable", "unknown")
	default:
		writeCurrentConfigurationMutationError(w, http.StatusInternalServerError, "Unexpected error", "unknown")
	}
}

func writeCurrentConfigurationMutationError(w http.ResponseWriter, status int, message, field string) {
	setCurrentConfigurationMutationResponseHeaders(w)
	writeJSON(w, status, struct {
		Error string `json:"error"`
		Field string `json:"field"`
	}{
		Error: message,
		Field: safeCurrentConfigurationMutationField(field),
	})
}
