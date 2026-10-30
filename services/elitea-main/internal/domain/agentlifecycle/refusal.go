package agentlifecycle

import (
	"errors"
	"net/http"
)

// The permissions of agent lifecycle governance. Shared 0204 seeds them.
const (
	// GovernancePermission is the governance role: approve at high and
	// critical, lower a tier, override a failed gate. Project-resolved for
	// the project routes; central (administration mode) for the
	// cross-project registry.
	GovernancePermission = "models.applications.governance"
	// AttestationsWritePermission records and revokes attestations, for a
	// GRC tool with an API token.
	AttestationsWritePermission = "lifecycle.attestations.write"
	// MarkDuePermission sets a certified agent to due, for an external
	// monitoring tool with an API token.
	MarkDuePermission = "lifecycle.registry.mark_due"
)

// The typed refusal codes.
const (
	// ChangeRequiredCode refuses a direct publish of an agent whose tier
	// needs approval (409).
	ChangeRequiredCode = "change_request_required"
	// ChangeNotRequiredCode refuses a change request for a tier that needs
	// no approval: publish directly (409).
	ChangeNotRequiredCode = "change_request_not_required"
	// SelfApprovalCode refuses the proposer's own approval (403).
	SelfApprovalCode = "self_approval_refused"
	// ApproverRoleCode refuses an approval that must come from the
	// governance role (403).
	ApproverRoleCode = "approver_role_required"
	// AlreadyApprovedCode refuses a second approval by one approver (409).
	AlreadyApprovedCode = "change_request_already_approved"
	// GatePendingCode refuses an approval while the release gate runs (409).
	GatePendingCode = "release_gate_pending"
	// GateFailedCode refuses an approval after the release gate failed and
	// no governance role holder overrode it (409).
	GateFailedCode = "release_gate_failed"
	// GateNotOverridableCode refuses an override of a gate that did not
	// fail or that is already overridden (409).
	GateNotOverridableCode = "release_gate_not_overridable"
	// AttestationMissingCode refuses an approval while a mandatory
	// attestation is missing, expired or revoked (409).
	AttestationMissingCode = "attestation_missing"
	// StateConflictCode refuses a move the change request's state does not
	// allow (409).
	StateConflictCode = "change_request_state_conflict"
	// OpenChangeCode refuses a second open change request for an agent (409).
	OpenChangeCode = "change_request_open"
	// StaleCode refuses an approval after the candidate version was saved
	// again or published since submission (409).
	StaleCode = "change_request_stale"
	// ProposerOnlyCode refuses a submit by someone other than the proposer (403).
	ProposerOnlyCode = "change_request_proposer_only"
	// NotReleasedCode refuses a recertification of an agent with no
	// published release, and a mark-due of an uncertified agent (409).
	NotReleasedCode = "agent_not_certifiable"
	// OwnerRequiredCode refuses a registry edit by someone who is neither
	// an owner nor a governance role holder (403).
	OwnerRequiredCode = "registry_owner_required"
	// TierLowerCode refuses a tier lowering without the governance role (403).
	TierLowerCode = "tier_lowering_requires_governance"
	// GovernanceRequiredCode refuses a gate override without the
	// governance role (403).
	GovernanceRequiredCode = "governance_role_required"
	// InvalidCode is a malformed request (400).
	InvalidCode = "invalid_request"
	// NotFoundCode is an unknown change request or attestation (404).
	NotFoundCode = "not_found"
	// LapsedCode refuses a run of an agent whose certification lapsed under
	// the suspend lapse action (409). safety.SuspendedError answers it for
	// a suspension of source certification.
	LapsedCode = "agent_certification_lapsed"
)

var statuses = map[string]int{
	ChangeRequiredCode:     http.StatusConflict,
	ChangeNotRequiredCode:  http.StatusConflict,
	SelfApprovalCode:       http.StatusForbidden,
	ApproverRoleCode:       http.StatusForbidden,
	AlreadyApprovedCode:    http.StatusConflict,
	GatePendingCode:        http.StatusConflict,
	GateFailedCode:         http.StatusConflict,
	GateNotOverridableCode: http.StatusConflict,
	AttestationMissingCode: http.StatusConflict,
	StateConflictCode:      http.StatusConflict,
	OpenChangeCode:         http.StatusConflict,
	StaleCode:              http.StatusConflict,
	ProposerOnlyCode:       http.StatusForbidden,
	NotReleasedCode:        http.StatusConflict,
	OwnerRequiredCode:      http.StatusForbidden,
	TierLowerCode:          http.StatusForbidden,
	GovernanceRequiredCode: http.StatusForbidden,
	InvalidCode:            http.StatusBadRequest,
	NotFoundCode:           http.StatusNotFound,
	LapsedCode:             http.StatusConflict,
}

// Refusal is a typed, caller-safe refusal. Detail carries bounded,
// non-secret fields (attestation kinds, gate failures, ids).
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

// RefuseWith builds a refusal with detail.
func RefuseWith(code, message string, detail map[string]any) *Refusal {
	return &Refusal{Code: code, Message: message, Detail: detail}
}

// AsRefusal returns the refusal in err's chain.
func AsRefusal(err error) (*Refusal, bool) {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal, true
	}
	return nil, false
}

// PublishRefusal is a refusal of the publish path itself (a version name
// that exists, a failed validation, a blocked deployment), passed through
// unchanged when a final approval publishes the candidate.
type PublishRefusal struct {
	Status int
	Body   map[string]any
}

func (r *PublishRefusal) Error() string { return "publish refused" }

// AsPublishRefusal returns the publish refusal in err's chain.
func AsPublishRefusal(err error) (*PublishRefusal, bool) {
	var refusal *PublishRefusal
	if errors.As(err, &refusal) {
		return refusal, true
	}
	return nil, false
}

// BreakerOverrideLoosensCode refuses a circuit breaker override that is
// looser than the tier default (422, ADR-0041 decision 2). The refusal's
// "fields" names each looser limit.
const BreakerOverrideLoosensCode = "circuit_breaker_override_loosens"

func init() { statuses[BreakerOverrideLoosensCode] = http.StatusUnprocessableEntity }
