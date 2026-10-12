package spi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
)

// The scaffolding every platform operation (platform.go) shares when it calls
// its engine: the detached invocation context, the reading of the engine's
// result (success or an engine error), the recognition of the engine's
// "busy" refusal by its code, and the reading of a JSON number. DeepWiki's
// and Inventory's runners used to carry a copy each; one copy means one
// contract.

// Engine codes (the `code` of an engine error line) a platform operation
// acts on. They are the engine's contract (`elitea_engine_core::errors::codes`).
const (
	// CodeBusy: another writer holds what the operation needed (an
	// ingestion lease, a wiki's publish lock). Nothing was changed; retry.
	CodeBusy = "busy"
	// CodeToolkitDeleted: the toolkit a write would have gone into was
	// deleted. Nothing was changed, and the call is refused for good.
	CodeToolkitDeleted = "toolkit_deleted"
)

// PlatformContext is the detached Context a platform operation runs its
// engine call under: it has an id of its own for the engine's registry
// ("platform-<op>-<random>"), its progress goes nowhere, and nothing can
// stop it (it is not an invocation).
func PlatformContext(op string) *Context {
	var id [8]byte
	_, _ = rand.Read(id[:])
	return DetachedContext("platform-" + op + "-" + hex.EncodeToString(id[:]))
}

// EngineOutcome reads what an engine call answered: the transport error, a
// missing result, or a result that is not a success — rebuilt as an error
// by engineError (each application's EngineError, which knows its engine's
// error_type hints) and given the result's `code` when it carries one, so
// that a code-based classification (ClassifyBusy) works on this path as it
// does on the error-line path.
func EngineOutcome(name string, result map[string]any, err error, engineError func(map[string]any) error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, Failf(KindRuntime, "%s returned nothing, expected a dict", name)
	}
	if resultTruthy(result["success"]) {
		return result, nil
	}
	failure := engineError(result)
	if code, _ := result["code"].(string); code != "" && CodeOf(failure) == "" {
		failure = NewCodedFailure(KindOf(failure), code, failure)
	}
	return nil, failure
}

// ClassifyBusy recognises the engine's "busy" refusal and gives it the
// sentinel (ErrBusy, ErrIngestRunning) the transport maps to a retryable
// code. The decision is the engine's CODE (CodeBusy); the message is kept
// for the reader.
//
// legacyText is a fallback for the rollout only: an engine of the previous
// release sends no code, so its refusal is recognised by its wording. It is
// consulted only when the error carries NO code at all, and goes once every
// engine sends codes.
func ClassifyBusy(err error, sentinel error, legacyText func(text string) bool) error {
	if err == nil {
		return nil
	}
	code := CodeOf(err)
	busy := code == CodeBusy || (code == "" && legacyText != nil && legacyText(err.Error()))
	if !busy {
		return err
	}
	return NewCodedFailure(KindRuntime, CodeBusy, fmt.Errorf("%w: %s", sentinel, err.Error()))
}

// JSONNumber reads a JSON number (a float64 after decoding) or a Go integer,
// 0 for anything else.
func JSONNumber(value any) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

// JSONInt is JSONNumber as an integer count.
func JSONInt(value any) int64 { return int64(JSONNumber(value)) }

// JSONIDs reads a JSON list of integer ids (numbers or digit strings), in
// order, skipping what is not an id.
func JSONIDs(value any) []int64 {
	entries, _ := value.([]any)
	var ids []int64
	for _, entry := range entries {
		if id, ok := JSONID(entry); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// JSONID reads one integer id: a JSON number or a string of digits.
func JSONID(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case string:
		id, err := strconv.ParseInt(v, 10, 64)
		return id, err == nil
	}
	return 0, false
}

// resultTruthy is Python truthiness for the `success` field of an engine
// result, which the legacy engines wrote as a bool and a fixture may write
// as 1 or "true".
func resultTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case int:
		return v != 0
	case string:
		return v != "" && v != "false" && v != "False" && v != "0"
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}
