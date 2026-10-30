package connectorapproval

import (
	"errors"
	"net/http"
)

// The typed refusal codes of the request API.
const (
	// InvalidCode is a malformed request (400).
	InvalidCode = "invalid_request"
	// NotFoundCode is an unknown request, or one of another project (404).
	NotFoundCode = "not_found"
	// StateConflictCode refuses a move the request's state does not allow
	// (409).
	StateConflictCode = "connector_request_state_conflict"
	// SelfApprovalCode refuses the requester's own approval or rejection
	// (403).
	SelfApprovalCode = "self_approval_refused"
	// RequesterOnlyCode refuses a submit or withdraw by someone other than
	// the requester (403).
	RequesterOnlyCode = "connector_request_requester_only"
	// AttestationMissingCode refuses an approval while the platform requires
	// an architecture attestation and the request has no valid one (409).
	AttestationMissingCode = "attestation_missing"
	// TypeDisabledCode refuses an approval of a toolkit type the deployment
	// policy disables (409): the operator changes it under Admin › Toolkits
	// first.
	TypeDisabledCode = "toolkit_type_disabled"
	// EgressLimitCode refuses an approval whose hosts would push an
	// allowlist over its bound (409).
	EgressLimitCode = "egress_allowlist_full"
	// ConnectorNotApprovedCode refuses a toolkit write naming an
	// unapproved connector under approved_only (403).
	ConnectorNotApprovedCode = "connector_not_approved"
	// CatalogueUnavailableCode is a gate that could not read the mode or the
	// approvals (503): nothing is admitted on a failed read.
	CatalogueUnavailableCode = "connector_catalogue_unavailable"
	// RequestScopeCode refuses a request naming a project the requester may
	// not request connectors in, or the whole platform from a requester who
	// is not a connector approver (403).
	RequestScopeCode = "connector_request_scope_refused"
)

var statuses = map[string]int{
	InvalidCode:              http.StatusBadRequest,
	NotFoundCode:             http.StatusNotFound,
	StateConflictCode:        http.StatusConflict,
	SelfApprovalCode:         http.StatusForbidden,
	RequesterOnlyCode:        http.StatusForbidden,
	AttestationMissingCode:   http.StatusConflict,
	TypeDisabledCode:         http.StatusConflict,
	EgressLimitCode:          http.StatusConflict,
	ConnectorNotApprovedCode: http.StatusForbidden,
	CatalogueUnavailableCode: http.StatusServiceUnavailable,
	RequestScopeCode:         http.StatusForbidden,
}

// Refusal is a typed, caller-safe refusal.
type Refusal struct {
	Code    string
	Message string
	Detail  map[string]any
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// Status is the HTTP status of the refusal.
func (r *Refusal) Status() int {
	if status, ok := statuses[r.Code]; ok {
		return status
	}
	return http.StatusConflict
}

// Body is the JSON error body: {"error", "code", "message", ...detail}.
func (r *Refusal) Body() map[string]any {
	body := map[string]any{"error": r.Code, "code": r.Code, "message": r.Message}
	for key, value := range r.Detail {
		if _, taken := body[key]; !taken {
			body[key] = value
		}
	}
	return body
}

// Refuse builds a refusal.
func Refuse(code, message string) *Refusal { return &Refusal{Code: code, Message: message} }

// AsRefusal returns the refusal in err's chain; a ValidationError is an
// InvalidCode refusal.
func AsRefusal(err error) (*Refusal, bool) {
	var refusal *Refusal
	if errors.As(err, &refusal) && refusal != nil {
		return refusal, true
	}
	var validation ValidationError
	if errors.As(err, &validation) {
		return Refuse(InvalidCode, validation.Message), true
	}
	return nil, false
}

// NotApproved is the refusal of a toolkit write naming an unapproved
// connector: the message, and the target a "Request connector" flow starts
// from.
func NotApproved(decision Decision) *Refusal {
	return &Refusal{Code: ConnectorNotApprovedCode, Message: RefusalMessage(decision), Detail: map[string]any{
		"target_kind": string(decision.Kind), "target": decision.Target,
	}}
}

// Audit actions (security audit type connector.approval).
const (
	ActionCreated   = "connector.request.created"
	ActionSubmitted = "connector.request.submitted"
	ActionWithdrawn = "connector.request.withdrawn"
	ActionApproved  = "connector.request.approved"
	ActionRejected  = "connector.request.rejected"
)
