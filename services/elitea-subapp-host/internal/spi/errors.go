package spi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// The ten error categories of the frozen contract, and the classifier that
// assigns one — ported with its precedence intact, because the precedence is
// what conformance/provider/fixtures/deepwiki/spi/errors.json records and
// what the facade and the web app already read.
//
// The Python shell classified by exception TYPE as well as by message text,
// and the wire carries the type's NAME (`error_type`). Go has no exception
// types, so a runner that wants a particular classification wraps its error
// in one of the Kinds below; each Kind carries the Python type name the
// fixtures pin, so a reader on the other side sees no change.

const (
	CategoryResourceNotFound = "resource_not_found"
	CategoryServiceBusy      = "service_busy"
	CategoryArtifactError    = "artifact_error"
	CategoryOutOfMemory      = "out_of_memory"
	CategoryTimeout          = "timeout_error"
	CategoryTrainingFailed   = "training_failed"
	CategoryInferenceFailed  = "inference_failed"
	CategoryRuntime          = "runtime_error"
	CategoryInvalidInput     = "invalid_input"
	CategoryUnknown          = "unknown_error"
)

// Categories is the closed set.
var Categories = []string{
	CategoryResourceNotFound, CategoryServiceBusy, CategoryArtifactError, CategoryOutOfMemory,
	CategoryTimeout, CategoryTrainingFailed, CategoryInferenceFailed, CategoryRuntime,
	CategoryInvalidInput, CategoryUnknown,
}

// Kind is the exception type a runner's failure stands for. The names are
// Python's because `error_type` on the wire is Python's; a Go runner picks
// the kind whose classification it wants.
type Kind string

const (
	KindNotFound Kind = "FileNotFoundError"
	KindRuntime  Kind = "RuntimeError"
	KindValue    Kind = "ValueError"
	KindMemory   Kind = "MemoryError"
	KindKey      Kind = "KeyError"
	KindGeneric  Kind = "Exception"
)

// Failure is a runner error with a kind. Wrap with NewFailure; classify with
// Classify.
type Failure struct {
	Kind Kind
	Err  error
	// Code is the engine's machine-readable code for the failure (its
	// `code` field, e.g. CodeBusy), empty when it sent none. A caller that
	// must act on a failure matches the code, never the message text.
	Code string
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

// NewFailure wraps err as a failure of the given kind.
func NewFailure(kind Kind, err error) error { return &Failure{Kind: kind, Err: err} }

// NewCodedFailure wraps err as a failure of the given kind that carries the
// engine's machine-readable code (CodeOf reads it back).
func NewCodedFailure(kind Kind, code string, err error) error {
	return &Failure{Kind: kind, Err: err, Code: code}
}

// CodeOf returns the engine code an error was wrapped with, or "" when it
// carries none.
func CodeOf(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

// Failf is NewFailure over a formatted message.
func Failf(kind Kind, format string, args ...any) error {
	return &Failure{Kind: kind, Err: fmt.Errorf(format, args...)}
}

// KindOf returns the kind an error was wrapped with, or KindGeneric.
func KindOf(err error) Kind {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Kind
	}
	return KindGeneric
}

// Classify assigns the category — the recorded precedence, verbatim:
// message text first for the categories the legacy classifier keyed on
// words, then the exception kind.
func Classify(err error) string {
	text := strings.ToLower(err.Error())
	kind := KindOf(err)
	switch {
	case strings.Contains(text, "not found") || kind == KindNotFound:
		return CategoryResourceNotFound
	case strings.Contains(text, "[service_busy]") || strings.Contains(text, "service is busy"):
		return CategoryServiceBusy
	case strings.Contains(text, "download") || strings.Contains(text, "artifact"):
		return CategoryArtifactError
	case strings.Contains(text, "memory") || kind == KindMemory:
		return CategoryOutOfMemory
	case strings.Contains(text, "timeout"):
		return CategoryTimeout
	case kind == KindRuntime:
		switch {
		case strings.Contains(text, "training"):
			return CategoryTrainingFailed
		case strings.Contains(text, "inference") || strings.Contains(text, "generat"):
			return CategoryInferenceFailed
		}
		return CategoryRuntime
	case kind == KindValue:
		return CategoryInvalidInput
	}
	return CategoryUnknown
}

// ResultObject is one entry of the result list a terminal body carries.
type ResultObject map[string]any

// Message is a result object of type message targeting the response.
func Message(text string) ResultObject {
	return ResultObject{
		"object_type":     "message",
		"result_target":   "response",
		"result_encoding": "plain",
		"data":            text,
	}
}

// ToolError is the terminal body of a failed invocation: HTTP 200, status
// Error, one message object, the category and the type. Never a traceback —
// the message is the error's text, logged with its cause here instead.
func ToolError(logger *slog.Logger, invocationID, operation string, err error) map[string]any {
	category := Classify(err)
	kind := KindOf(err)
	text := fmt.Sprintf("%s failed: %s", capitalize(operation), err.Error())
	if logger != nil {
		logger.Warn("invocation failed",
			"invocation", invocationID, "operation", operation, "category", category, "type", string(kind), "error", err)
	}
	result, _ := json.Marshal([]ResultObject{Message(text)})
	return map[string]any{
		"invocation_id":  invocationID,
		"status":         "Error",
		"result":         string(result),
		"result_type":    "String",
		"error_category": category,
		"error_type":     string(kind),
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// TransportError is the envelope for a refusal below the tool: not found,
// bad request, a hop that is not mutually authenticated.
func TransportError(status int, message string) map[string]any {
	return map[string]any{
		"errorCode": fmt.Sprint(status),
		"message":   message,
		"details":   []string{},
	}
}
