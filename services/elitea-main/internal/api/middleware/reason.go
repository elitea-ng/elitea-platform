package middleware

// The reason of a dangerous action, read on the server (ADR-0059 decision 6,
// "Dangerous actions"; story R3.8a, issue 2539). The constants and the closed
// list of reason codes are internal/adminaccess/reason.go.
//
// For a request that reaches an entry of the action table whose Reason is set
// (every `dangerous` and `critical` action of the administration surface), the
// check reads the reason before the route's gate and handler:
//
//   - the JSON body fields `reason_code` and `reason` where the request has a
//     JSON object body; the headers Elitea-Reason-Code and Elitea-Reason
//     otherwise. A non-empty body field wins over its header; an empty one
//     (a form default) falls back to the header. The body is read with the
//     bound of the other action rules and put back for the handler without
//     the reason fields, which are the check's, not the handler's (several
//     handlers refuse an unknown field); a route whose own request has a
//     `reason` keeps that one, and a route whose body keys are data reads the
//     headers only (adminaccess.ReasonBodyOf);
//   - a header value is percent-encoded UTF-8 of at most
//     adminaccess.MaxReasonHeaderBytes after decoding, a body reason at most
//     adminaccess.MaxReasonBodyBytes;
//   - a code outside adminaccess.ActionReasonCodes, `other` without a text, a
//     header that does not decode or is too long, a text with a control
//     character other than tab and line breaks (a NUL would fail the audit
//     row's insert and lose the row), and a body field that is not a string
//     answer 400 `reason_invalid`. A malformed reason is refused even in the
//     warning period: it is a new field, so nothing sends one by accident;
//   - a request with no code is served during the warning period
//     (adminaccess.ReasonOptionalUntilSunset) and, when the route answers
//     below 400, answers Elitea-Reason-Missing: 1 and is audited as
//     reason_missing. A request the gate or the handler refuses is not an
//     action without a reason, so it carries neither. A text without a code
//     is recorded and still counts as missing: the code is what a review
//     groups by. After the warning period it answers 400 `reason_required`.
//
// What it records: the admin audit row gets the code, the text and
// reason_missing (audit.Annotate); the request context gets the code alone
// (audit.ContextWithReasonCode), which the chained security events built from
// the request carry. A token caller is treated like any other credential.
//
// The check runs above the route gates, like the token and elevation rules,
// so a malformed reason answers 400 before a caller without the permission
// would get 403. That says nothing the docs do not: the marked routes and
// their reason rules are published.
//
// Classifier outcomes: an outcome the request body decides (a user suspend or
// unsuspend, a platform or scoped suspension) is resolved here, so only the
// dangerous outcome asks for a reason. When the request alone cannot tell the
// outcomes apart (a retention write that may shorten or lengthen the policy
// needs the stored row), any outcome that asks for a reason asks for one: the
// console sends a reason on every write of such a route.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
)

// ReasonCheck reads the reason of a dangerous action. Mark it with the
// finished router before it serves; until then it passes every request.
type ReasonCheck struct {
	routes chi.Routes
}

// NewReasonCheck returns an unmarked check.
func NewReasonCheck() *ReasonCheck { return &ReasonCheck{} }

// Mark records the finished router; nothing writes it afterwards.
func (c *ReasonCheck) Mark(routes chi.Routes) { c.routes = routes }

// Middleware is the check. Mount it below Audit, so its refusal and its
// annotation reach the admin audit row, and above the route gates.
func (c *ReasonCheck) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.routes == nil || !mutatingMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		entries, rctx := AdminActionCandidates(c.routes, r)
		if len(entries) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		entries, r = SelectAdminActions(entries, r, rctx)
		bodyRule := adminaccess.ReasonBodyOf(entries[0].Method, entries[0].Pattern)
		var fields map[string]json.RawMessage
		if bodyRule != adminaccess.ReasonBodyHeadersOnly {
			fields, r = jsonBodyFields(r)
		}
		entries = resolveBodyOutcome(entries, fields)
		if !adminaccess.ReasonRequired(entries) {
			next.ServeHTTP(w, r)
			return
		}
		reason, problem := readReason(r, fields)
		if problem != "" {
			writeReasonRefusal(w, adminaccess.CodeReasonInvalid, problem)
			return
		}
		r = withoutReasonFields(r, fields, bodyRule == adminaccess.ReasonBodyKeepText)
		missing := reason.Code == ""
		if missing && !adminaccess.ReasonOptionalUntilSunset {
			writeReasonRefusal(w, adminaccess.CodeReasonRequired, "this administration action needs a reason_code")
			return
		}
		if reason.Code != "" || reason.Text != "" {
			audit.Annotate(r.Context(), audit.Annotation{ReasonCode: reason.Code, Reason: reason.Text})
		}
		if !missing {
			served := false
			audit.Annotate(r.Context(), audit.Annotation{ReasonMissing: &served})
			next.ServeHTTP(w, r.WithContext(audit.ContextWithReasonCode(r.Context(), reason.Code)))
			return
		}
		// The warning period: flag the action once the route has served it.
		flagged := &reasonMissingWriter{ResponseWriter: w}
		next.ServeHTTP(flagged, r)
		if flagged.flagged {
			yes := true
			audit.Annotate(r.Context(), audit.Annotation{ReasonMissing: &yes})
		}
	})
}

// reasonMissingWriter sets Elitea-Reason-Missing on a response below 400.
type reasonMissingWriter struct {
	http.ResponseWriter
	wrote, flagged bool
}

func (w *reasonMissingWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		if status < http.StatusBadRequest {
			w.flagged = true
			w.Header().Set(adminaccess.ReasonMissingHeader, "1")
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *reasonMissingWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *reasonMissingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// resolveBodyOutcome narrows entries to the classifier outcome the body
// states, for the classifiers a body decides on its own. A body that does not
// state it unambiguously keeps every entry: the closed failure.
func resolveBodyOutcome(entries []adminaccess.Action, fields map[string]json.RawMessage) []adminaccess.Action {
	if len(entries) < 2 || fields == nil {
		return entries
	}
	var outcome string
	switch entries[0].Classifier {
	case adminaccess.ClassifyUserSuspend:
		suspended, ok := boolField(fields, "suspended")
		if !ok {
			return entries
		}
		outcome = adminaccess.UserSuspendAction(suspended)
	case adminaccess.ClassifySuspensionScope:
		scope, state := stringField(fields, "scope")
		if state != fieldString {
			return entries
		}
		outcome = adminaccess.SuspensionAction(scope)
	default:
		return entries
	}
	for _, entry := range entries {
		if entry.Name == adminaccess.ActionName(entry.Selector.Value, outcome) {
			return []adminaccess.Action{entry}
		}
	}
	return entries
}

// boolField reads a top-level boolean field, matching its name without regard
// to case; two spellings with different values, or a value that is not a
// boolean, answer false.
func boolField(fields map[string]json.RawMessage, field string) (value, ok bool) {
	for key, encoded := range fields {
		if !strings.EqualFold(key, field) {
			continue
		}
		var spelled bool
		if json.Unmarshal(encoded, &spelled) != nil || (ok && spelled != value) {
			return false, false
		}
		value, ok = spelled, true
	}
	return value, ok
}

// Reason is what a request says about why it is made.
type Reason struct {
	// Code is one of adminaccess.ActionReasonCodes, "" for none.
	Code string
	// Text is the free text, "" for none.
	Text string
}

// readReason reads the reason from the body fields (nil for a body that is
// not read or not a JSON object) and the headers, and answers a problem (""
// when the reason is acceptable, which includes no reason at all).
func readReason(r *http.Request, fields map[string]json.RawMessage) (Reason, string) {
	code, problem := reasonValue(fields, r, adminaccess.ReasonCodeField, adminaccess.ReasonCodeHeader, 0)
	if problem != "" {
		return Reason{}, problem
	}
	text, problem := reasonValue(fields, r, adminaccess.ReasonField, adminaccess.ReasonHeader, adminaccess.MaxReasonBodyBytes)
	if problem != "" {
		return Reason{}, problem
	}
	reason := Reason{Code: strings.TrimSpace(code), Text: strings.TrimSpace(text)}
	switch {
	case reason.Code != "" && !adminaccess.KnownActionReasonCode(reason.Code):
		return Reason{}, "reason_code is not a known reason code; use one of " +
			strings.Join(adminaccess.ActionReasonCodes(), ", ")
	case strings.IndexFunc(reason.Text, forbiddenInReason) >= 0:
		return Reason{}, "the reason holds a control character"
	case reason.Code == adminaccess.ReasonOther && reason.Text == "":
		return Reason{}, "a reason with the code other needs a reason text"
	}
	return reason, ""
}

// forbiddenInReason is a control character other than tab and line breaks.
func forbiddenInReason(r rune) bool {
	return unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r'
}

// withoutReasonFields answers r with a body that no longer carries the reason
// fields (every spelling, as stringField matches them): `reason_code` always,
// `reason` unless keepText. A body without them is left byte for byte.
func withoutReasonFields(r *http.Request, fields map[string]json.RawMessage, keepText bool) *http.Request {
	removed := false
	for key := range fields {
		if strings.EqualFold(key, adminaccess.ReasonCodeField) || (!keepText && strings.EqualFold(key, adminaccess.ReasonField)) {
			delete(fields, key)
			removed = true
		}
	}
	if !removed {
		return r
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return r
	}
	rewritten := r.Clone(r.Context())
	rewritten.Body = io.NopCloser(bytes.NewReader(raw))
	rewritten.ContentLength = int64(len(raw))
	rewritten.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	return rewritten
}

// reasonValue reads one part of the reason: the body field, or the header
// when the body does not carry the field or carries it empty. maxBody bounds
// the body value (0: no bound beyond the body's).
func reasonValue(fields map[string]json.RawMessage, r *http.Request, field, header string, maxBody int) (string, string) {
	value, state := stringField(fields, field)
	switch {
	case state == fieldInvalid:
		return "", "the body field " + field + " must be one string"
	case state == fieldString && strings.TrimSpace(value) != "":
		if maxBody > 0 && len(value) > maxBody {
			return "", "the body field " + field + " is longer than " + strconv.Itoa(maxBody) + " bytes"
		}
		return value, ""
	}
	raw := r.Header.Values(header)
	switch len(raw) {
	case 0:
		return "", ""
	case 1:
	default:
		return "", "the header " + header + " is sent more than once"
	}
	decoded, err := url.PathUnescape(raw[0])
	switch {
	case err != nil:
		return "", "the header " + header + " is not percent-encoded UTF-8"
	case !utf8.ValidString(decoded):
		return "", "the header " + header + " is not UTF-8 after percent-decoding"
	case len(decoded) > adminaccess.MaxReasonHeaderBytes:
		return "", "the header " + header + " is longer than " + strconv.Itoa(adminaccess.MaxReasonHeaderBytes) +
			" bytes after percent-decoding"
	}
	return decoded, ""
}

// writeReasonRefusal answers 400 with code in the shape of the other
// administration refusals.
func writeReasonRefusal(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message, "code": code})
}
