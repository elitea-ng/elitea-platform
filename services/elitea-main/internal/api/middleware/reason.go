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
//     otherwise. A body field wins over its header when both are present. The
//     body is read with the bound of the other action rules and put back for
//     the handler without the reason fields, which are the check's, not the
//     handler's (several handlers refuse an unknown field); a route whose own
//     request has a `reason` keeps that one, and a route whose body keys are
//     data reads the headers only (adminaccess.ReasonBodyOf);
//   - a header value is percent-encoded UTF-8 of at most
//     adminaccess.MaxReasonHeaderBytes after decoding;
//   - a code outside adminaccess.ActionReasonCodes, `other` without a text, a
//     header that does not decode or is too long, and a body field that is
//     not a string answer 400 `reason_invalid`. A malformed reason is refused
//     even in the warning period: it is a new field, so nothing sends one by
//     accident;
//   - a request with no code is served during the warning period
//     (adminaccess.ReasonOptionalUntilSunset), answers Elitea-Reason-Missing:
//     1 and is audited as reason_missing. A text without a code is recorded
//     and still counts as missing: the code is what a review groups by.
//
// What it records: the admin audit row gets the code, the text and
// reason_missing (audit.Annotate); the request context gets the code alone
// (audit.ContextWithReasonCode), which the chained security events built from
// the request carry. A token caller is treated like any other credential.
//
// When the request alone cannot tell a route's classifier outcomes apart (a
// retention write that may shorten or lengthen the policy), any outcome that
// asks for a reason asks for one: the console sends a reason on every write of
// such a route.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
		if !adminaccess.ReasonRequired(entries) {
			next.ServeHTTP(w, r)
			return
		}
		reason, request, problem := ReadReason(r, adminaccess.ReasonBodyOf(entries[0].Method, entries[0].Pattern))
		r = request
		if problem != "" {
			writeReasonRefusal(w, problem)
			return
		}
		missing := reason.Code == ""
		audit.Annotate(r.Context(), audit.Annotation{
			ReasonCode: reason.Code, Reason: reason.Text, ReasonMissing: &missing,
		})
		if missing {
			// The warning period (adminaccess.ReasonOptionalUntilSunset):
			// served and flagged. R4.8 answers 400 reason_required instead.
			w.Header().Set(adminaccess.ReasonMissingHeader, "1")
		}
		next.ServeHTTP(w, r.WithContext(audit.ContextWithReasonCode(r.Context(), reason.Code)))
	})
}

// Reason is what a request says about why it is made.
type Reason struct {
	// Code is one of adminaccess.ActionReasonCodes, "" for none.
	Code string
	// Text is the free text, "" for none.
	Text string
}

// ReadReason reads the reason of r: the body fields where the body is a JSON
// object, the headers for a field the body does not carry. It answers the
// request with its body put back, and a problem ("" when the reason is
// acceptable, which includes no reason at all).
//
// body says what the route's body is for (adminaccess.ReasonBodyOf): by
// default both reason fields are taken out of the body the handler reads,
// since several handlers refuse an unknown field; a handler that reads
// `reason` itself keeps it; a body whose keys are data is not read at all.
func ReadReason(r *http.Request, body adminaccess.ReasonBody) (Reason, *http.Request, string) {
	var fields map[string]json.RawMessage
	if body != adminaccess.ReasonBodyHeadersOnly {
		fields, r = jsonBodyFields(r)
	}
	code, problem := reasonValue(fields, r, adminaccess.ReasonCodeField, adminaccess.ReasonCodeHeader)
	if problem != "" {
		return Reason{}, r, problem
	}
	text, problem := reasonValue(fields, r, adminaccess.ReasonField, adminaccess.ReasonHeader)
	if problem != "" {
		return Reason{}, r, problem
	}
	if _, state := stringField(fields, adminaccess.ReasonField); state == fieldString && len(text) > adminaccess.MaxReasonBodyBytes {
		return Reason{}, r, "the body field reason is longer than " + strconv.Itoa(adminaccess.MaxReasonBodyBytes) + " bytes"
	}
	r = withoutReasonFields(r, fields, body == adminaccess.ReasonBodyKeepText)
	reason := Reason{Code: strings.TrimSpace(code), Text: strings.TrimSpace(text)}
	switch {
	case reason.Code != "" && !adminaccess.KnownActionReasonCode(reason.Code):
		return Reason{}, r, "reason_code is not a known reason code; use one of " +
			strings.Join(adminaccess.ActionReasonCodes(), ", ")
	case reason.Code == adminaccess.ReasonOther && reason.Text == "":
		return Reason{}, r, "a reason with the code other needs a reason text"
	}
	return reason, r, ""
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
// when the body does not carry the field.
func reasonValue(fields map[string]json.RawMessage, r *http.Request, field, header string) (string, string) {
	value, state := stringField(fields, field)
	switch state {
	case fieldString:
		return value, ""
	case fieldInvalid:
		return "", "the body field " + field + " must be one string"
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

// writeReasonRefusal answers 400 reason_invalid in the shape of the other
// administration refusals.
func writeReasonRefusal(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message, "code": adminaccess.CodeReasonInvalid})
}
