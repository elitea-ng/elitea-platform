package llmproxy

// The GRANT SCOPE of a platform (catalogue) model, as the gateway enforces it.
//
// # Why the gateway has to know
//
// A catalogue model is a row in the public project's schema with
// `shared = true`, and the resolver reads that scope for every caller (the
// public-scope pass in `query`). `shared` is one bit: it says the row is a
// platform row. elitea-main can now narrow it — a platform model may be
// offered to every project, to none, or to a chosen set — and it applies that
// rule to the model LIST a project reads.
//
// A rule applied only where the list is built is not a rule. The model id a
// caller sends is a string; a project that had once been granted a model, or
// that read the id off somebody's screen, would go on dispatching it after the
// grant was withdrawn, because nothing between the id and the provider looks
// at the row. So the same filter runs here, on the same rows, keyed on the
// request's own project.
//
// # This is a COPY, on purpose
//
// services/elitea-llm-gateway is a separate Go module with no import path into
// elitea-main. The authority is
// elitea-main/internal/application/configurations/model_grant.go; this states
// the same three scopes and the same rules, and each side has its own test.
// A change to one needs the same change to the other.
//
// # An absent or malformed scope means EVERY project
//
// Every catalogue row written before this feature carries neither field, and
// every one of them was offered to every project. Reading an absent scope as
// anything else would withdraw every platform model on every deployment the
// moment this code shipped. A malformed value is read the same way, for the
// same reason: a corrupt field must not become an outage. The write surface
// refuses a malformed value where the operator authors it.

import (
	"encoding/json"
	"math"
	"strconv"
)

// The scopes a catalogue row may carry.
const (
	modelShareScopeAll      = "all"
	modelShareScopeNone     = "none"
	modelShareScopeProjects = "projects"
)

// The two `data` keys the grant lives in.
const (
	modelShareScopeField = "share_scope"
	modelSharedWithField = "shared_with"
)

// modelRowGrantsProject reports whether a catalogue row's `data` grants the row
// to callerProjectID, which is the request's own project as a decimal string.
//
// The document is decoded into a MAP rather than into a typed struct, and that
// is the point of agreement with elitea-main: its own reader
// (`ReadModelGrant`) walks a `map[string]any` too, so the two answer the same
// for every shape a stored row can hold. A typed decode disagreed with it in
// two measurable ways — a `shared_with` that was not a list of numbers failed
// the WHOLE decode, which read as "no grant recorded" and admitted every
// project, and `7.0` was not accepted as project 7.
//
// Anything it cannot read at all answers TRUE — see the file header.
func modelRowGrantsProject(data []byte, callerProjectID string) bool {
	if len(data) == 0 || callerProjectID == "" {
		return true
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return true
	}
	scope, _ := decoded[modelShareScopeField].(string)
	switch scope {
	case modelShareScopeNone:
		return false
	case modelShareScopeProjects:
		return modelGrantNamesProject(decoded[modelSharedWithField], callerProjectID)
	case modelShareScopeAll:
		return true
	default:
		// An absent scope, a scope that is not a string, and any value this
		// gateway does not know.
		return true
	}
}

// modelGrantNamesProject reports whether a `shared_with` value names
// callerProjectID.
//
// A `shared_with` that is not a list, or that is empty, grants the row to
// NOBODY rather than to everybody: the scope says the grant is a list, so an
// unreadable list is a list this project is not on. Widening it here would
// make a corrupt field the way to obtain a model.
func modelGrantNamesProject(raw any, callerProjectID string) bool {
	want := canonicalProjectID(callerProjectID)
	if want == "" {
		return false
	}
	values, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if grantedProjectID(value) == want {
			return true
		}
	}
	return false
}

// grantedProjectID reduces one `shared_with` entry to a comparable id, or "".
//
// Both JSON spellings of an id are accepted — a number, which
// `encoding/json` gives as a float64, and a string, which a client that
// stringifies its ids sends — because a grant that failed to parse is a grant
// that silently does not apply, and the project it named would lose the model
// with no message anywhere.
func grantedProjectID(value any) string {
	switch typed := value.(type) {
	case float64:
		if typed <= 0 || typed != math.Trunc(typed) || typed > math.MaxInt32 {
			return ""
		}
		return strconv.FormatInt(int64(typed), 10)
	case string:
		return canonicalProjectID(typed)
	default:
		return ""
	}
}

// canonicalProjectID reduces a decimal id to a comparable form, and answers ""
// for anything that is not one. Two ids are the same project only when both
// canonicalise to the same non-empty string.
func canonicalProjectID(value string) string {
	digits := ""
	for _, character := range value {
		if character < '0' || character > '9' {
			return ""
		}
		if digits == "" && character == '0' {
			continue
		}
		digits += string(character)
	}
	return digits
}
