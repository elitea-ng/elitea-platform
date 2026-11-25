package adminaccess

// The reason of a dangerous action (ADR-0059 decision 6, "Dangerous actions";
// story R3.8a, issue 2539). Every entry of the action table whose Reason is
// set asks the caller why. The check is internal/api/middleware/reason.go;
// this file holds its constants and the closed list of reason codes, so the
// registry is the one place that says what a reason is.
//
// The reason travels in the JSON body as `reason_code` and `reason`, or, for
// a request without a body (most DELETE requests), in the headers
// Elitea-Reason-Code and Elitea-Reason. A header value is percent-encoded
// UTF-8 of at most MaxReasonHeaderBytes after decoding: a browser refuses a
// header value outside ISO-8859-1, so a reason typed in Cyrillic or CJK would
// otherwise break the action.
//
// The text goes to the admin audit row only (centry.audit_events.reason,
// never hashed into the chain, so it stays erasable); the code also goes into
// the chained security events (admin.access.changed).

import "slices"

// The headers of the reason.
const (
	// ReasonCodeHeader carries the reason code of a request without a body.
	ReasonCodeHeader = "Elitea-Reason-Code"
	// ReasonHeader carries the reason text of a request without a body.
	// Proxy and ingress access logs must not record it: the text can hold
	// personal data.
	ReasonHeader = "Elitea-Reason"
	// ReasonMissingHeader answers `1` on a marked action served without a
	// reason during the warning period. It is the replacement marker the
	// sunset check reads (scripts/ci/check-sunset.mjs).
	ReasonMissingHeader = "Elitea-Reason-Missing"
)

// The JSON body fields of the reason.
const (
	ReasonCodeField = "reason_code"
	ReasonField     = "reason"
)

// ReasonOptionalUntilSunset is the warning period of ADR-0059 decision 6: until
// the sunset date of decision 2 (SunsetAfter) a marked action without a reason
// is served, answers ReasonMissingHeader and is audited as reason_missing.
// The reason check branches on it: false answers 400 CodeReasonRequired.
// Story R4.8 removes it in the removal pull request. It is the legacy marker
// the sunset check reads (scripts/ci/check-sunset.mjs).
const ReasonOptionalUntilSunset = true

// CodeReasonRequired is the refusal of a marked action without a reason code
// once the warning period is over.
const CodeReasonRequired = "reason_required"

// MaxReasonHeaderBytes bounds a reason header after percent-decoding. A
// longer value answers 400 CodeReasonInvalid.
const MaxReasonHeaderBytes = 500

// MaxReasonBodyBytes bounds the body field `reason`: the longest reason a
// marked route's own request already takes (a legal hold release, 2000).
const MaxReasonBodyBytes = 2000

// ReasonBody says where a marked route's reason is read and what the handler
// then reads.
type ReasonBody int

const (
	// ReasonBodyStrip, the default: the body fields `reason_code` and
	// `reason` are read and taken out of the body before the handler decodes
	// it, because several handlers refuse an unknown field.
	ReasonBodyStrip ReasonBody = iota
	// ReasonBodyKeepText: the route's own request has a `reason` field the
	// handler reads (v2.yaml CredentialRevokeRequest,
	// ExecutionSuspensionCreate, LegalHoldRelease). It stays in the body;
	// `reason_code` is taken out.
	ReasonBodyKeepText
	// ReasonBodyHeadersOnly: the body's top-level keys are data (the role
	// map of the permission matrix save names roles), so only the headers
	// carry the reason and the body is left alone.
	ReasonBodyHeadersOnly
)

var reasonBodies = map[string]ReasonBody{
	"POST /api/v2/admin/credential_inventory/revoke":            ReasonBodyKeepText,
	"POST /api/v2/admin/execution_suspensions":                  ReasonBodyKeepText,
	"POST /api/v2/admin/legal_holds/{id}/release":               ReasonBodyKeepText,
	"PUT /api/v2/admin/user_project_permissions/administration": ReasonBodyHeadersOnly,
}

// ReasonBodyOf answers the ReasonBody of (method, pattern).
func ReasonBodyOf(method, pattern string) ReasonBody { return reasonBodies[method+" "+pattern] }

// CodeReasonInvalid is the refusal of a reason the check cannot accept: an
// unknown code, a header that does not decode or is too long, `other` with no
// text, a body field that is not a string or a body reason over
// MaxReasonBodyBytes.
const CodeReasonInvalid = "reason_invalid"

// The reason codes of a dangerous action. One closed list, recorded in the
// docs page and in v2.yaml as an enum.
const (
	ReasonIncident    = "incident"
	ReasonRequest     = "request"
	ReasonMaintenance = "maintenance"
	ReasonCorrection  = "correction"
	ReasonOffboarding = "offboarding"
	// ReasonOther needs a reason text.
	ReasonOther = "other"
)

var actionReasonCodes = []string{
	ReasonIncident, ReasonRequest, ReasonMaintenance, ReasonCorrection, ReasonOffboarding, ReasonOther,
}

// ActionReasonCodes answers the reason codes of a dangerous action, in the
// order the console lists them. The break-glass codes are the elevation's own
// (ElevationReason*, elevation.go): an elevation asks why the person needs
// the access, an action why the change is made.
func ActionReasonCodes() []string { return slices.Clone(actionReasonCodes) }

// KnownActionReasonCode reports whether code is one of ActionReasonCodes.
func KnownActionReasonCode(code string) bool { return slices.Contains(actionReasonCodes, code) }

// ReasonRequired reports whether any of entries asks for a reason. The check
// calls it on the entries a request selects: when the request alone cannot
// tell a route's classifier outcomes apart (a retention write that may
// shorten or lengthen), any outcome that asks for a reason asks for one.
func ReasonRequired(entries []Action) bool {
	return slices.ContainsFunc(entries, func(a Action) bool { return a.Reason })
}
