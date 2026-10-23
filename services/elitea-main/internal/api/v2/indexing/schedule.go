package indexing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	indexscheduleapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

const (
	CurrentIndexSchedulePath       = SourceOnlyIndexSchedulePath
	CurrentIndexScheduleMode       = SourceOnlyIndexScheduleMode
	CurrentIndexSchedulePermission = SourceOnlyIndexSchedulePermission

	MaxCurrentIndexScheduleBodyBytes = 64 << 10
	currentIndexScheduleTimeout      = 5 * time.Second
)

var ErrInvalidCurrentIndexScheduleRoute = errors.New("invalid current index schedule route dependencies")

// CurrentIndexScheduleUpdater is the application boundary for the current
// schedule PATCH. It does not discover or execute schedules; the legacy runtime
// remains the sole scheduler during this source-only stage.
type CurrentIndexScheduleUpdater interface {
	Update(context.Context, indexscheduleapp.Update) (indexscheduleapp.MutationResult, error)
}

var _ CurrentIndexScheduleUpdater = (*indexscheduleapp.Service)(nil)

// CurrentIndexScheduleRoute preserves the current UI method, path, permission,
// nested JSON persistence, and raw indexes_meta response. It is intentionally
// not composed into elitea-main production startup in this stage.
type CurrentIndexScheduleRoute struct {
	handler http.Handler
}

func NewCurrentIndexScheduleRoute(
	updater CurrentIndexScheduleUpdater,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
) (*CurrentIndexScheduleRoute, error) {
	if updater == nil || !authConfig.CredentialPlaneComposed() || permissions == nil {
		return nil, ErrInvalidCurrentIndexScheduleRoute
	}

	handler := &currentIndexScheduleHandler{updater: updater}
	endpoint := http.Handler(http.HandlerFunc(handler.update))
	endpoint = apimw.RequireResolvedPermissionsForProject(
		permissions,
		CurrentIndexScheduleMode,
		func(request *http.Request) (string, bool) {
			projectID, valid := positiveCurrentIndexMetaID(chi.URLParam(request, "projectID"))
			return strconv.FormatInt(projectID, 10), valid
		},
		CurrentIndexSchedulePermission,
	)(endpoint)
	endpoint = apimw.Auth(authConfig)(endpoint)

	router := chi.NewRouter()
	router.Method(http.MethodPatch, CurrentIndexSchedulePath, endpoint)
	return &CurrentIndexScheduleRoute{handler: router}, nil
}

func (route *CurrentIndexScheduleRoute) ServeHTTP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if route == nil || route.handler == nil {
		http.NotFound(writer, request)
		return
	}
	route.handler.ServeHTTP(writer, request)
}

type currentIndexScheduleHandler struct {
	updater CurrentIndexScheduleUpdater
}

func (handler *currentIndexScheduleHandler) update(
	writer http.ResponseWriter,
	request *http.Request,
) {
	projectID, projectOK := positiveCurrentIndexMetaID(chi.URLParam(request, "projectID"))
	toolkitID, toolkitOK := positiveCurrentIndexMetaID(chi.URLParam(request, "toolkitID"))
	indexMetaID := chi.URLParam(request, "indexMetaID")
	principal, authenticated := auth.RuntimePrincipalFromContext(request.Context())
	if !authenticated {
		writeCurrentIndexScheduleError(writer, http.StatusUnauthorized, "authentication required")
		return
	}
	actorUserID, owningUser := principal.OwningUserID()
	if !projectOK || !toolkitOK || !owningUser ||
		indexMetaID == "" || len(indexMetaID) > indexscheduleapp.MaxIndexMetaIDBytes {
		writeCurrentIndexScheduleValidation(writer)
		return
	}
	if !currentScheduleJSONMediaType(request.Header.Get("Content-Type")) {
		writeCurrentIndexScheduleError(
			writer,
			http.StatusUnsupportedMediaType,
			"Unsupported Media Type",
		)
		return
	}

	body, err := decodeCurrentIndexScheduleBody(writer, request)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeCurrentIndexScheduleError(
				writer,
				http.StatusRequestEntityTooLarge,
				"Index schedule request body is too large",
			)
			return
		}
		writeCurrentIndexScheduleValidation(writer)
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), currentIndexScheduleTimeout)
	defer cancel()
	result, err := handler.updater.Update(ctx, indexscheduleapp.Update{
		ProjectID:       projectID,
		ActorUserID:     actorUserID,
		ToolkitID:       toolkitID,
		IndexMetaID:     indexMetaID,
		Cron:            body.Cron,
		Enabled:         body.Enabled,
		RequestedUserID: body.UserID,
		Credentials:     body.Credentials,
		Timezone:        body.Timezone,
	})
	if err != nil {
		writeCurrentIndexScheduleApplicationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result.IndexesMeta)
}

func currentScheduleJSONMediaType(value string) bool {
	// Flask/Werkzeug decides request.is_json from the media type token and is
	// intentionally tolerant of incomplete parameters such as "; charset".
	if separator := strings.IndexByte(value, ';'); separator >= 0 {
		value = value[:separator]
	}
	mediaType := strings.ToLower(strings.TrimSpace(value))
	return mediaType == "application/json" ||
		(strings.HasPrefix(mediaType, "application/") &&
			strings.HasSuffix(mediaType, "+json"))
}

type currentIndexScheduleBody struct {
	Cron        string
	Enabled     bool
	UserID      int64
	Credentials *indexscheduleapp.Credentials
	Timezone    string
}

func decodeCurrentIndexScheduleBody(
	writer http.ResponseWriter,
	request *http.Request,
) (currentIndexScheduleBody, error) {
	request.Body = http.MaxBytesReader(writer, request.Body, MaxCurrentIndexScheduleBodyBytes)
	decoder := json.NewDecoder(request.Body)
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return currentIndexScheduleBody{}, firstError(err, indexscheduleapp.ErrInvalidRequest)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return currentIndexScheduleBody{}, firstError(err, indexscheduleapp.ErrInvalidRequest)
	}

	cron, err := requiredCurrentScheduleString(raw, "cron")
	if err != nil {
		return currentIndexScheduleBody{}, err
	}
	timezone, err := requiredCurrentScheduleString(raw, "timezone")
	if err != nil {
		return currentIndexScheduleBody{}, err
	}
	enabled := false
	if value, exists := raw["enabled"]; exists {
		enabled, err = currentScheduleBool(value)
		if err != nil {
			return currentIndexScheduleBody{}, err
		}
	}
	userID := int64(-1)
	if value, exists := raw["user_id"]; exists {
		// Current Pydantic accepts null and later uses it as a schedule-map key.
		// Rejecting that ambiguous key is an intentional security correction.
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return currentIndexScheduleBody{}, indexscheduleapp.ErrInvalidRequest
		}
		userID, err = currentScheduleInt(value)
		if err != nil {
			return currentIndexScheduleBody{}, err
		}
	}
	credentials, err := currentScheduleCredentials(raw["credentials"])
	if err != nil {
		return currentIndexScheduleBody{}, err
	}

	return currentIndexScheduleBody{
		Cron: cron, Enabled: enabled, UserID: userID,
		Credentials: credentials, Timezone: timezone,
	}, nil
}

func requiredCurrentScheduleString(
	raw map[string]json.RawMessage,
	field string,
) (string, error) {
	value, exists := raw[field]
	if !exists {
		return "", indexscheduleapp.ErrInvalidRequest
	}
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		return "", indexscheduleapp.ErrInvalidRequest
	}
	return decoded, nil
}

func currentScheduleCredentials(
	raw json.RawMessage,
) (*indexscheduleapp.Credentials, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, indexscheduleapp.ErrInvalidRequest
	}
	title, err := requiredCurrentScheduleString(object, "elitea_title")
	if err != nil {
		return nil, err
	}
	privateValue := false
	private := &privateValue
	if value, exists := object["private"]; exists {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			private = nil
		} else {
			privateValue, err = currentScheduleBool(value)
			if err != nil {
				return nil, err
			}
		}
	}
	return &indexscheduleapp.Credentials{
		Private: private, EliteaTitle: title,
	}, nil
}

func currentScheduleBool(raw json.RawMessage) (bool, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false, indexscheduleapp.ErrInvalidRequest
	}
	switch typed := value.(type) {
	case bool:
		return typed, nil
	case json.Number:
		number, err := typed.Float64()
		if err == nil && (number == 0 || number == 1) {
			return number == 1, nil
		}
	case string:
		switch strings.ToLower(typed) {
		case "0", "f", "false", "n", "no", "off":
			return false, nil
		case "1", "on", "t", "true", "y", "yes":
			return true, nil
		}
	}
	return false, indexscheduleapp.ErrInvalidRequest
}

func currentScheduleInt(raw json.RawMessage) (int64, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return 0, indexscheduleapp.ErrInvalidRequest
	}
	switch typed := value.(type) {
	case bool:
		if typed {
			return 1, nil
		}
		return 0, nil
	case json.Number:
		return currentScheduleJSONInteger(string(typed))
	case string:
		return currentScheduleStringInteger(typed)
	default:
		return 0, indexscheduleapp.ErrInvalidRequest
	}
}

func currentScheduleJSONInteger(value string) (int64, error) {
	if integer, err := strconv.ParseInt(value, 10, 64); err == nil {
		return integer, nil
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsInf(number, 0) || math.IsNaN(number) ||
		number != math.Trunc(number) || number < math.MinInt64 || number > math.MaxInt64 {
		return 0, indexscheduleapp.ErrInvalidRequest
	}
	return int64(number), nil
}

func currentScheduleStringInteger(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if integer, err := strconv.ParseInt(value, 10, 64); err == nil {
		return integer, nil
	}
	whole, fraction, decimal := strings.Cut(value, ".")
	if !decimal || whole == "" || fraction == "" {
		return 0, indexscheduleapp.ErrInvalidRequest
	}
	for _, digit := range fraction {
		if digit != '0' {
			return 0, indexscheduleapp.ErrInvalidRequest
		}
	}
	integer, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, indexscheduleapp.ErrInvalidRequest
	}
	return integer, nil
}

func firstError(err error, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func writeCurrentIndexScheduleApplicationError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, indexscheduleapp.ErrToolkitNotFound):
		writeCurrentIndexScheduleError(writer, http.StatusNotFound, "Toolkit not found")
	case errors.Is(err, indexscheduleapp.ErrScheduleResultTooLarge):
		writeCurrentIndexScheduleError(
			writer,
			http.StatusRequestEntityTooLarge,
			"Index schedule metadata is too large",
		)
	case errors.Is(err, indexscheduleapp.ErrInvalidRequest),
		errors.Is(err, indexscheduleapp.ErrInvalidCron),
		errors.Is(err, indexscheduleapp.ErrFrequencyAboveDaily),
		errors.Is(err, indexscheduleapp.ErrInvalidTimezone):
		writeCurrentIndexScheduleValidation(writer)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeCurrentIndexScheduleError(
			writer,
			http.StatusGatewayTimeout,
			"Index schedule request timed out",
		)
	default:
		writeCurrentIndexScheduleError(
			writer,
			http.StatusBadRequest,
			"Error occurred while updating index_meta",
		)
	}
}

func writeCurrentIndexScheduleValidation(writer http.ResponseWriter) {
	writeCurrentIndexScheduleError(
		writer,
		http.StatusBadRequest,
		"Validation error on index schedule update: invalid request body",
	)
}

func writeCurrentIndexScheduleError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}{Error: message})
}
