package middleware

// Finding the action-table entries a request reaches (ADR-0059 decision 1).
// The token rule (R1.2b), the elevation restrictions (R2.8a) and the reason
// check (R3.8a) each apply one column of the entry before the route's own
// gate, so they share this lookup: the finished router's own Find for the
// pattern, then the entry the request selects.
//
// Picking the entry:
//
//   - one entry with no name: that entry;
//   - a body selector (the `action` field of POST /admin/auth_users/{mode}):
//     the body is read and put back whole for the handler. The field is
//     matched without regard to case, as the handler's struct decode matches
//     it;
//   - a path selector: the placeholder's value;
//   - a selector value the request does not state unambiguously (no field,
//     two spellings with different values, a body that is not one JSON
//     object, a body over MaxAdminActionBody) or that the table does not list:
//     every entry of the route, so a rule decides on all of them rather than
//     trust the handler to read the value the same way;
//   - classifier outcomes the request alone cannot tell apart stay together:
//     each rule says how it decides on them.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
)

// MaxAdminActionBody bounds the body read to find a body selector's value or
// a reason. A larger body is put back whole and what it holds counts as
// unknown: the administration routes take small JSON bodies.
const MaxAdminActionBody = 1 << 20

// AdminActionCandidates answers the entries of the (method, pattern) the
// request routes to in routes, with the route context of the lookup (its path
// parameters). A pattern with a `{mode}` segment outside the administration
// mode has no entry: the same route in another mode is not on the
// administration surface (adminaccess.ActionForRequest).
func AdminActionCandidates(routes chi.Routes, r *http.Request) ([]adminaccess.Action, *chi.Context) {
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	rctx := chi.NewRouteContext()
	pattern := routes.Find(rctx, r.Method, path)
	if pattern == "" {
		return nil, rctx
	}
	return adminaccess.ActionsForRequest(r.Method, pattern, rctx.URLParam), rctx
}

// SelectAdminActions narrows the entries of a route that multiplexes actions
// to the request's action. A value it cannot read, or one the table does not
// list, keeps every entry of the route: the closed failure. The request it
// returns carries the body it read.
func SelectAdminActions(entries []adminaccess.Action, r *http.Request, rctx *chi.Context) ([]adminaccess.Action, *http.Request) {
	selector := entries[0].Selector
	var value string
	switch selector.In {
	case "":
		return entries, r
	case adminaccess.SelectPath:
		value = rctx.URLParam(selector.Field)
	case adminaccess.SelectBody:
		var fields map[string]json.RawMessage
		fields, r = jsonBodyFields(r)
		value, _ = stringField(fields, selector.Field)
	}
	var out []adminaccess.Action
	for _, entry := range entries {
		if value != "" && entry.Selector.Value == value {
			out = append(out, entry)
		}
	}
	if len(out) == 0 {
		return entries, r
	}
	return out, r
}

// jsonBodyFields reads a JSON object body and puts the whole body back for
// the handler. A missing body, a body over MaxAdminActionBody and a body that
// is not one JSON object answer nil fields.
func jsonBodyFields(r *http.Request) (map[string]json.RawMessage, *http.Request) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, r
	}
	original := r.Body
	raw, err := io.ReadAll(io.LimitReader(original, MaxAdminActionBody+1))
	restored := r.Clone(r.Context())
	restored.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(raw), original), original}
	if err != nil || len(raw) > MaxAdminActionBody {
		return nil, restored
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, restored
	}
	return fields, restored
}

// fieldState is what a body says about one top-level field.
type fieldState int

const (
	fieldAbsent fieldState = iota
	fieldString
	// fieldInvalid: not a string, or two spellings with different values.
	fieldInvalid
)

// stringField reads the top-level string field of fields, matching its name
// without regard to case as encoding/json does for a struct. A JSON null is
// absent. A value that is not a string, and two spellings with different
// values, are invalid and answer "".
func stringField(fields map[string]json.RawMessage, field string) (string, fieldState) {
	value, state := "", fieldAbsent
	for key, encoded := range fields {
		if !strings.EqualFold(key, field) || string(encoded) == "null" {
			continue
		}
		var spelled string
		if json.Unmarshal(encoded, &spelled) != nil || (state == fieldString && spelled != value) {
			return "", fieldInvalid
		}
		value, state = spelled, fieldString
	}
	return value, state
}
