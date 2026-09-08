package apierr

import (
	"encoding/json"
	"errors"
	"net/http"
)

type Response struct {
	Error string `json:"error"`
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return e.Message
}

func BadRequest(message string) error {
	return &APIError{Status: http.StatusBadRequest, Code: "bad_request", Message: message}
}

func NotFound(message string) error {
	return &APIError{Status: http.StatusNotFound, Code: "not_found", Message: message}
}

func Unauthorized(message string) error {
	return &APIError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: message}
}

func Forbidden(message string) error {
	return &APIError{Status: http.StatusForbidden, Code: "forbidden", Message: message}
}

func Internal(message string) error {
	return &APIError{Status: http.StatusInternalServerError, Code: "internal_error", Message: message}
}

// NotImplemented is a NAMED refusal for a capability this release does not
// serve. It exists so a handler never has the choice between 200-with-nothing
// and a bare 500: an empty list and a zero score are indistinguishable, in a
// browser, from a feature that works and has no data, while a 501 carrying the
// reason tells the reader which of the two they are looking at. The precedent
// is internal/api/v2/budgets/usage_dimensions.go, and #617's evidence bar asks
// for this shape by name.
func NotImplemented(message string) error {
	return &APIError{Status: http.StatusNotImplemented, Code: "not_implemented", Message: message}
}

func Conflict(message string) error {
	return &APIError{Status: http.StatusConflict, Code: "conflict", Message: message}
}

func Write(w http.ResponseWriter, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(apiErr.Status)
		_ = json.NewEncoder(w).Encode(Response{Error: apiErr.Message})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(Response{Error: "internal server error"})
}

// WriteStatus writes `{"error": message}` under Content-Type: application/json.
//
// WHY IT EXISTS. 118 handler call sites wrote a JSON body through
// `http.Error(w, `+"`"+`{"error":"..."}`+"`"+`, status)`. http.Error hardcodes
// `Content-Type: text/plain; charset=utf-8`, so every one of those responses
// claimed to be plain text while carrying JSON. Measured on a live deployment:
// GET /api/v2/social/author answered `application/json`, while a 403 from
// /api/v2/configurations/configurations/1 and a 503 from /api/v2/auth/token/
// both answered `text/plain; charset=utf-8`. A caller that dispatches on the
// media type cannot parse the error it is given.
//
// The response bytes do not change. http.Error appends one newline to its
// body, and json.Encoder.Encode appends one too. A caller that reads the
// body as text sees exactly what it saw before.
//
// Use Write for a typed *APIError. Use this for a literal status and message.
func WriteStatus(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	// http.Error sets this; keep it, so a browser never sniffs the body.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Response{Error: message})
}
