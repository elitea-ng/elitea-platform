// Package authzcompose is the AND-composed operation decision of ADR-0039
// decision 7 (issue #1116). A governed credential use is permitted only when
// every injected Gate permits it. A gate error, a missing input or an
// unknown value is a deny with a stable reason code.
//
// The request and the decision follow the AuthZEN 1.0 shape (subject,
// action, resource, context). This package is separate from the route
// gates in internal/api/middleware, which keep their any-overlap semantics.
//
// The Decider is built once at the composition root, only with the
// credential governance flag on, and is immutable afterwards.
package authzcompose

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// Subject is who asks (AuthZEN subject).
type Subject struct {
	// PrincipalRef is the authenticated principal ("user:5").
	PrincipalRef string
	// UserID is the Elitea user whose RBAC and entity access are evaluated.
	UserID     int64
	AuthSource auth.AuthenticationSource
	// CallingApplicationID is the inbound client id (gate D8); optional.
	CallingApplicationID string
	// WorkloadID is the caller workload's SPIFFE id.
	WorkloadID string
	// ExternalSubjectID is the IdP subject (Entra oid) of an interactive
	// delegation; empty otherwise.
	ExternalSubjectID string
}

// AgentRef names the agent version the run executes.
type AgentRef struct {
	BindingID            string
	Epoch                int64
	ApplicationID        int64
	ApplicationVersionID int64
	DefinitionDigest     [32]byte
}

// AuthorityKind is the authority source of decision 3.
type AuthorityKind string

const (
	AuthorityMandate    AuthorityKind = "mandate"
	AuthorityDelegation AuthorityKind = "delegation"
)

// AuthorityRef names the mandate or delegation the use rests on.
type AuthorityRef struct {
	Kind  AuthorityKind
	ID    string
	Epoch int64
}

// Action is the operation asked for.
type Action struct {
	OperationID credentialgrant.OperationID
	Capability  credentialgrant.Capability
}

// Resource is what the operation touches.
type Resource struct {
	ToolkitID             int64
	ToolkitDigest         [32]byte
	CredentialReferenceID string
	TargetBindingID       string
	// ResourceRef is the provider resource (for example a project key); empty
	// when the operation names none.
	ResourceRef string
}

// DecisionContext is the AuthZEN context.
type DecisionContext struct {
	TenantProjectID  int64
	PolicyRevisionID string
	PolicyDigest     [32]byte
	ExecutionID      string
	InvocationID     string
}

// OperationRequest is one decision request.
type OperationRequest struct {
	Subject   Subject
	Agent     AgentRef
	Authority AuthorityRef
	Action    Action
	Resource  Resource
	Context   DecisionContext
}

// Effect is the decision result.
type Effect string

const (
	EffectPermit Effect = "permit"
	EffectDeny   Effect = "deny"
)

// Obligation is a duty the enforcement point must fulfil to act on a permit.
type Obligation struct {
	Kind   string
	Params map[string]string
}

// ReasonCode is a stable machine code. It never carries free text.
type ReasonCode string

// AuditCode is the code in the security audit form (lower case).
func (r ReasonCode) AuditCode() string { return strings.ToLower(string(r)) }

// Reason codes. Add a code; never rename one.
const (
	ReasonGateError              ReasonCode = "GATE_ERROR"
	ReasonDecisionIDUnavailable  ReasonCode = "DECISION_ID_UNAVAILABLE"
	ReasonTenantMissing          ReasonCode = "TENANT_MISSING"
	ReasonAgentMissing           ReasonCode = "AGENT_MISSING"
	ReasonAuthorityMissing       ReasonCode = "AUTHORITY_MISSING"
	ReasonAuthorityUnknown       ReasonCode = "AUTHORITY_UNKNOWN"
	ReasonPolicyRevisionMissing  ReasonCode = "POLICY_REVISION_MISSING"
	ReasonCapabilityUnknown      ReasonCode = "CAPABILITY_UNKNOWN"
	ReasonOperationMissing       ReasonCode = "OPERATION_MISSING"
	ReasonResourceMissing        ReasonCode = "RESOURCE_MISSING"
	ReasonSubjectMissing         ReasonCode = "SUBJECT_MISSING"
	ReasonWorkloadMissing        ReasonCode = "WORKLOAD_MISSING"
	ReasonAuthSourceNotAllowed   ReasonCode = "AUTH_SOURCE_NOT_ALLOWED"
	ReasonPermissionMissing      ReasonCode = "PERMISSION_MISSING"
	ReasonEntityAccessDenied     ReasonCode = "ENTITY_ACCESS_DENIED"
	ReasonBindingNotActive       ReasonCode = "BINDING_NOT_ACTIVE"
	ReasonBindingEpochStale      ReasonCode = "BINDING_EPOCH_STALE"
	ReasonBindingDigestMismatch  ReasonCode = "BINDING_DIGEST_MISMATCH"
	ReasonBindingScopeMismatch   ReasonCode = "BINDING_SCOPE_MISMATCH"
	ReasonToolkitNotBound        ReasonCode = "TOOLKIT_NOT_BOUND"
	ReasonToolkitDigestMismatch  ReasonCode = "TOOLKIT_DIGEST_MISMATCH"
	ReasonGrantMissing           ReasonCode = "GRANT_MISSING"
	ReasonGrantNotActive         ReasonCode = "GRANT_NOT_ACTIVE"
	ReasonCapabilityNotGranted   ReasonCode = "CAPABILITY_NOT_GRANTED"
	ReasonCredentialNotUsable    ReasonCode = "CREDENTIAL_NOT_USABLE"
	ReasonCredentialTargetDiffer ReasonCode = "CREDENTIAL_TARGET_MISMATCH"
	ReasonOperationUnknown       ReasonCode = "OPERATION_UNKNOWN"
	ReasonOperationOutOfScope    ReasonCode = "OPERATION_OUT_OF_SCOPE"
	ReasonResourceOutOfScope     ReasonCode = "RESOURCE_OUT_OF_SCOPE"
	ReasonAuthorityNotActive     ReasonCode = "AUTHORITY_NOT_ACTIVE"
	ReasonAuthorityEpochStale    ReasonCode = "AUTHORITY_EPOCH_STALE"
	ReasonAuthoritySubjectDiffer ReasonCode = "AUTHORITY_SUBJECT_MISMATCH"
	ReasonAuthorityScopeMismatch ReasonCode = "AUTHORITY_SCOPE_MISMATCH"
	ReasonPolicyRevisionNotPin   ReasonCode = "POLICY_REVISION_NOT_PINNED"
)

// Arrangement C: the Elitea PDP decides; Vault/OpenBao is workload-scoped
// storage (ADR-0039 decision 9, gate D3 default). Arrangement A is recorded
// by the broker when Vault decides a release (#1113, #1118).
const ArrangementC = "C"

// OperationDecision is the decision. It names references and codes only.
type OperationDecision struct {
	DecisionID       string // uuid v7
	Effect           Effect
	Reasons          []ReasonCode
	Obligations      []Obligation
	PolicyRevisionID string
	EvaluatedEpochs  map[string]int64
	Arrangement      string
	// DeniedBy names the gates that did not permit, in gate order.
	DeniedBy []string
}

// Permitted reports whether the decision is a permit.
func (d OperationDecision) Permitted() bool { return d.Effect == EffectPermit }

// GateResult is one gate's answer. Permit is false unless the gate set it.
type GateResult struct {
	Permit      bool
	Reasons     []ReasonCode
	Obligations []Obligation
	// Epochs are the epochs the gate compared, by record name.
	Epochs map[string]int64
}

// Gate is one independent check of decision 7.
type Gate interface {
	Name() string
	Evaluate(ctx context.Context, r OperationRequest) (GateResult, error)
}

// Decider evaluates every gate and permits only when all permit.
type Decider struct {
	gates []Gate
	newID func() (uuid.UUID, error)
}

// ErrInvalidDecider refuses an empty gate list, a nil gate, an empty name or
// a duplicate name.
var ErrInvalidDecider = errors.New("authzcompose: invalid decider")

// NewDecider builds the decider over gates, in order.
func NewDecider(gates ...Gate) (*Decider, error) {
	if len(gates) == 0 {
		return nil, fmt.Errorf("%w: no gates", ErrInvalidDecider)
	}
	seen := make(map[string]bool, len(gates))
	for _, gate := range gates {
		if gate == nil {
			return nil, fmt.Errorf("%w: a gate is nil", ErrInvalidDecider)
		}
		name := gate.Name()
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%w: a gate has no name", ErrInvalidDecider)
		}
		if seen[name] {
			return nil, fmt.Errorf("%w: duplicate gate %q", ErrInvalidDecider, name)
		}
		seen[name] = true
	}
	return &Decider{gates: slices.Clone(gates), newID: uuid.NewV7}, nil
}

// GateNames returns the gate names in evaluation order.
func (d *Decider) GateNames() []string {
	names := make([]string, len(d.gates))
	for i, gate := range d.gates {
		names[i] = gate.Name()
	}
	return names
}

// inputReasons are the request-level denies: a missing or unknown input is
// a deny whatever the gates answer.
func inputReasons(r OperationRequest) []ReasonCode {
	var reasons []ReasonCode
	if r.Context.TenantProjectID <= 0 {
		reasons = append(reasons, ReasonTenantMissing)
	}
	if r.Agent.BindingID == "" {
		reasons = append(reasons, ReasonAgentMissing)
	}
	switch {
	case r.Authority.ID == "":
		reasons = append(reasons, ReasonAuthorityMissing)
	case r.Authority.Kind != AuthorityMandate && r.Authority.Kind != AuthorityDelegation:
		reasons = append(reasons, ReasonAuthorityUnknown)
	}
	if r.Context.PolicyRevisionID == "" {
		reasons = append(reasons, ReasonPolicyRevisionMissing)
	}
	if !r.Action.Capability.Valid() {
		reasons = append(reasons, ReasonCapabilityUnknown)
	}
	if r.Action.OperationID == "" {
		reasons = append(reasons, ReasonOperationMissing)
	}
	if r.Resource.ToolkitID <= 0 || r.Resource.CredentialReferenceID == "" || r.Resource.TargetBindingID == "" {
		reasons = append(reasons, ReasonResourceMissing)
	}
	return reasons
}

// evaluate runs one gate. A panic is a gate error, so a faulty gate denies
// instead of taking the caller down.
func evaluate(ctx context.Context, gate Gate, r OperationRequest) (result GateResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result, err = GateResult{}, fmt.Errorf("gate %s panicked", gate.Name())
		}
	}()
	return gate.Evaluate(ctx, r)
}

// Decide evaluates every gate, even after a deny, so the decision lists
// every failed check. It permits only when the request is complete and all
// gates permit. Obligations are the union over the gates.
func (d *Decider) Decide(ctx context.Context, r OperationRequest) OperationDecision {
	decision := OperationDecision{
		Effect:           EffectDeny,
		PolicyRevisionID: r.Context.PolicyRevisionID,
		EvaluatedEpochs:  map[string]int64{},
		Arrangement:      ArrangementC,
	}
	reasons := inputReasons(r)
	if id, err := d.newID(); err == nil {
		decision.DecisionID = id.String()
	} else {
		reasons = append(reasons, ReasonDecisionIDUnavailable)
	}
	allPermit := true
	gateError := false
	ctx = withDecisionSnapshot(ctx)
	for _, gate := range d.gates {
		result, err := evaluate(ctx, gate, r)
		if err != nil {
			gateError = true
			allPermit = false
			decision.DeniedBy = append(decision.DeniedBy, gate.Name())
			continue
		}
		for name, epoch := range result.Epochs {
			decision.EvaluatedEpochs[name] = epoch
		}
		if !result.Permit {
			allPermit = false
			decision.DeniedBy = append(decision.DeniedBy, gate.Name())
			if len(result.Reasons) == 0 {
				// A deny with no reason still needs a code.
				reasons = append(reasons, ReasonCode(strings.ToUpper(gate.Name())+"_DENIED"))
			}
			reasons = append(reasons, result.Reasons...)
			continue
		}
		decision.Obligations = unionObligations(decision.Obligations, result.Obligations)
	}
	if gateError {
		reasons = append(reasons, ReasonGateError)
	}
	decision.Reasons = dedupeReasons(reasons)
	if allPermit && len(decision.Reasons) == 0 {
		decision.Effect = EffectPermit
		return decision
	}
	// A deny carries no obligation to act on.
	decision.Obligations = nil
	return decision
}

func dedupeReasons(reasons []ReasonCode) []ReasonCode {
	if len(reasons) == 0 {
		return nil
	}
	out := make([]ReasonCode, 0, len(reasons))
	seen := make(map[ReasonCode]bool, len(reasons))
	for _, reason := range reasons {
		if !seen[reason] {
			seen[reason] = true
			out = append(out, reason)
		}
	}
	return out
}

func obligationKey(o Obligation) string {
	keys := make([]string, 0, len(o.Params))
	for k := range o.Params {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString(o.Kind)
	for _, k := range keys {
		b.WriteString("\x00" + k + "=" + o.Params[k])
	}
	return b.String()
}

func unionObligations(into, add []Obligation) []Obligation {
	for _, o := range add {
		key := obligationKey(o)
		if !slices.ContainsFunc(into, func(existing Obligation) bool { return obligationKey(existing) == key }) {
			into = append(into, o)
		}
	}
	return into
}

// ErrObligationUnsupported is the enforcement point's refusal of an
// obligation it cannot fulfil.
var ErrObligationUnsupported = errors.New("authzcompose: obligation cannot be fulfilled")

// ErrDenied is a deny decision, wrapped by Enforce.
var ErrDenied = errors.New("authzcompose: operation denied")

// Fulfil checks that the enforcement point supports every obligation kind.
// An error means the enforcement point must deny.
func Fulfil(obligations []Obligation, supported map[string]bool) error {
	for _, o := range obligations {
		if !supported[o.Kind] {
			return fmt.Errorf("%w: %q", ErrObligationUnsupported, o.Kind)
		}
	}
	return nil
}

// Enforce is what an enforcement point runs on a decision: nil only for a
// permit whose every obligation it supports.
func Enforce(d OperationDecision, supported map[string]bool) error {
	if !d.Permitted() {
		return ErrDenied
	}
	if err := Fulfil(d.Obligations, supported); err != nil {
		return fmt.Errorf("%w: %w", ErrDenied, err)
	}
	return nil
}
