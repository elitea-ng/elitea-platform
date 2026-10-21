package configurations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"

	"github.com/go-chi/chi/v5"
)

const maxValidationAdmissionBodyBytes = configurationapp.MaxValidationSettingsBytes + 1024

var ErrValidationForbidden = errors.New("configuration validation is forbidden")

// ValidationAuthorizer derives the tenant, actor and resource/projection
// projects from trusted request identity and persisted authorization state. It
// must not trust any identity a request header claims.
type ValidationAuthorizer interface {
	AuthorizeValidation(ctx context.Context, projectID, configurationRevisionID string) (executionapp.AdmissionIdentity, error)
}

type ValidationSubmitter interface {
	Submit(ctx context.Context, request configurationapp.SubmitValidationRequest) (executionapp.AdmissionOutcome, error)
}

type ValidationHandler struct {
	authorizer ValidationAuthorizer
	submitter  ValidationSubmitter
}

func NewValidationHandler(authorizer ValidationAuthorizer, submitter ValidationSubmitter) (*ValidationHandler, error) {
	if authorizer == nil || submitter == nil {
		return nil, errors.New("validation authorizer and submitter are required")
	}
	return &ValidationHandler{authorizer: authorizer, submitter: submitter}, nil
}

// Submit admits one immutable configuration revision for asynchronous
// validation. The production composition mounts it only when the isolated
// runtime, trusted public authorizer, durable outbox, and private worker
// listeners have all been configured successfully.
func (h *ValidationHandler) Submit(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	revisionID := chi.URLParam(r, "configurationRevisionID")
	if projectID == "" || revisionID == "" {
		writeValidationError(w, http.StatusBadRequest, "project and configuration revision are required")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeValidationError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	identity, err := h.authorizer.AuthorizeValidation(r.Context(), projectID, revisionID)
	if err != nil {
		if errors.Is(err, ErrValidationForbidden) {
			writeValidationError(w, http.StatusForbidden, "forbidden")
			return
		}
		writeValidationError(w, http.StatusInternalServerError, "authorization failed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxValidationAdmissionBodyBytes)
	settings, err := decodeValidationAdmissionBody(r.Body)
	if err != nil {
		writeValidationError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	outcome, err := h.submitter.Submit(r.Context(), configurationapp.SubmitValidationRequest{
		Identity:                identity,
		ConfigurationRevisionID: revisionID,
		IdempotencyKey:          r.Header.Get("Idempotency-Key"),
		Settings:                settings,
	})
	if err != nil {
		var capacityError *executionapp.AdmissionCapacityError
		switch {
		case errors.Is(err, configurationapp.ErrInvalidValidationAdmission), errors.Is(err, executionapp.ErrInvalidAdmission):
			writeValidationError(w, http.StatusBadRequest, "invalid validation request")
		case errors.Is(err, configurationapp.ErrValidationInputLimitExceeded):
			writeValidationError(w, http.StatusRequestEntityTooLarge, "validation input exceeds the approved limit")
		case errors.Is(err, executionapp.ErrIdempotencyConflict):
			writeValidationError(w, http.StatusConflict, "idempotency key conflict")
		case errors.As(err, &capacityError):
			retryAfterSeconds := int64(capacityError.RetryAfter().Seconds())
			w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds, 10))
			writeValidationError(w, http.StatusServiceUnavailable, "validation admission is temporarily at capacity")
		default:
			writeValidationError(w, http.StatusInternalServerError, "validation admission failed")
		}
		return
	}

	writeValidationJSON(w, http.StatusAccepted, struct {
		ExecutionID string `json:"execution_id"`
		CommandID   string `json:"command_id"`
		Created     bool   `json:"created"`
	}{
		ExecutionID: outcome.ExecutionID,
		CommandID:   outcome.CommandID,
		Created:     outcome.Created,
	})
}

func decodeValidationAdmissionBody(body io.Reader) ([]byte, error) {
	exactBody, err := io.ReadAll(io.LimitReader(body, maxValidationAdmissionBodyBytes+1))
	if err != nil || len(exactBody) > maxValidationAdmissionBodyBytes {
		return nil, errors.New("request body exceeds the approved limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(exactBody))
	decoder.UseNumber()
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("request body must be an object")
	}
	var settings json.RawMessage
	settingsStart := -1
	seenSettings := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok || key != "settings" || seenSettings {
			return nil, errors.New("unknown or duplicate request field")
		}
		settingsStart, err = validationSettingsValueStart(exactBody, int(decoder.InputOffset()))
		if err != nil {
			return nil, err
		}
		seenSettings = true
		if err := decoder.Decode(&settings); err != nil {
			return nil, err
		}
		if settingsStart+len(settings) > len(exactBody) || !bytes.Equal(exactBody[settingsStart:settingsStart+len(settings)], settings) {
			return nil, errors.New("settings byte boundary is invalid")
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || !seenSettings || len(settings) == 0 {
		return nil, errors.New("invalid request body")
	}
	closingOffset := int(decoder.InputOffset())
	closingStart := closingOffset - 1
	settingsEnd := settingsStart + len(settings)
	if settingsStart < 0 || settingsEnd > closingStart || !validationJSONWhitespace(exactBody[settingsEnd:closingStart]) {
		return nil, errors.New("settings byte boundary is invalid")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return append([]byte(nil), exactBody[settingsStart:closingStart]...), nil
}

func validationSettingsValueStart(body []byte, offset int) (int, error) {
	for offset < len(body) && validationJSONSpace(body[offset]) {
		offset++
	}
	if offset >= len(body) || body[offset] != ':' {
		return 0, errors.New("settings separator is invalid")
	}
	offset++
	for offset < len(body) && validationJSONSpace(body[offset]) {
		offset++
	}
	if offset >= len(body) {
		return 0, errors.New("settings value is missing")
	}
	return offset, nil
}

func validationJSONWhitespace(value []byte) bool {
	for _, character := range value {
		if !validationJSONSpace(character) {
			return false
		}
	}
	return true
}

func validationJSONSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\n' || character == '\r'
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func writeValidationError(w http.ResponseWriter, status int, message string) {
	writeValidationJSON(w, status, map[string]string{"error": message})
}

func writeValidationJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
