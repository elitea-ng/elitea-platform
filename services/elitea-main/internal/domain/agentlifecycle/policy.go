package agentlifecycle

import (
	"slices"
	"time"
)

// Requirement is what one tier asks of a change request.
type Requirement struct {
	// Approvals is how many distinct approvers, none of them the proposer,
	// a change request needs. 0 keeps today's single-click publish.
	Approvals int
	// GovernanceApprovals is how many of those approvers must hold the
	// governance role (GovernancePermission).
	GovernanceApprovals int
	// RecertifyEvery is the certification cadence. 0 is no cadence: the
	// certification never lapses by time.
	RecertifyEvery time.Duration
	// Attestations are the attestation kinds a change request needs, valid
	// and not revoked, before any approval.
	Attestations []AttestationKind
}

// RequiresApproval reports whether the tier needs a change request. A tier
// that needs none publishes directly and is never certified.
func (r Requirement) RequiresApproval() bool { return r.Approvals > 0 }

// LapseAction is what a lapsed certification does.
type LapseAction string

const (
	// LapseWarn shows the lapse and lets the agent run (the default).
	LapseWarn LapseAction = "warn"
	// LapseSuspend refuses runs of the lapsed agent with
	// LapsedCode, through an agent suspension of source certification.
	LapseSuspend LapseAction = "suspend"
)

// Valid reports whether a is a known action.
func (a LapseAction) Valid() bool { return a == LapseWarn || a == LapseSuspend }

// Policy is the platform's tier table (the agent_lifecycle Configuration
// section).
type Policy struct {
	Tiers       map[Tier]Requirement
	LapseAction LapseAction
}

// Floors are the ADR-0040 decision 3 minimums. The administrator may raise
// a tier's requirement and never lower it below these, so the tier table
// of the ADR is the weakest a deployment can be.
var Floors = map[Tier]Requirement{
	TierLow:      {Approvals: 0, GovernanceApprovals: 0},
	TierMedium:   {Approvals: 1, GovernanceApprovals: 0},
	TierHigh:     {Approvals: 1, GovernanceApprovals: 1},
	TierCritical: {Approvals: 2, GovernanceApprovals: 1},
}

// MaxApprovals bounds the approvals any tier may ask for.
const MaxApprovals = 5

// MaxRecertifyDays bounds a cadence: three years.
const MaxRecertifyDays = 3 * 365

// DueWindow is how long before recertify_by a certified agent becomes due.
const DueWindow = 30 * 24 * time.Hour

// Day is one cadence unit.
const Day = 24 * time.Hour

// DefaultPolicy is the ADR's table with its default cadences: critical 90
// days, high 180, medium 365, low none; critical needs a model_risk and a
// legal attestation; a lapse warns.
func DefaultPolicy() Policy {
	return Policy{
		Tiers: map[Tier]Requirement{
			TierLow:    {Approvals: 0, GovernanceApprovals: 0},
			TierMedium: {Approvals: 1, GovernanceApprovals: 0, RecertifyEvery: 365 * Day},
			TierHigh:   {Approvals: 1, GovernanceApprovals: 1, RecertifyEvery: 180 * Day},
			TierCritical: {Approvals: 2, GovernanceApprovals: 1, RecertifyEvery: 90 * Day,
				Attestations: []AttestationKind{AttestationModelRisk, AttestationLegal}},
		},
		LapseAction: LapseWarn,
	}
}

// Requirement is what tier asks for, never below the floor. An unknown
// tier is treated as critical: the strictest reading of a value the code
// does not know.
func (p Policy) Requirement(tier Tier) Requirement {
	if !tier.Valid() {
		tier = TierCritical
	}
	requirement, ok := p.Tiers[tier]
	if !ok {
		requirement = DefaultPolicy().Tiers[tier]
	}
	return clampRequirement(tier, requirement)
}

func clampRequirement(tier Tier, r Requirement) Requirement {
	floor := Floors[tier]
	r.Approvals = min(max(r.Approvals, floor.Approvals), MaxApprovals)
	r.GovernanceApprovals = min(max(r.GovernanceApprovals, floor.GovernanceApprovals), r.Approvals)
	if r.RecertifyEvery < 0 {
		r.RecertifyEvery = 0
	}
	if r.RecertifyEvery > MaxRecertifyDays*Day {
		r.RecertifyEvery = MaxRecertifyDays * Day
	}
	r.Attestations = slices.Clone(r.Attestations)
	return r
}

// RecertifyBy is the deadline of a certification granted at now under r,
// nil when the tier has no cadence.
func (r Requirement) RecertifyBy(now time.Time) *time.Time {
	if r.RecertifyEvery <= 0 {
		return nil
	}
	deadline := now.Add(r.RecertifyEvery)
	return &deadline
}

// Approval is one approval already recorded on a change request.
type Approval struct {
	ApproverID int64
	Governance bool
}

// ApprovalRequest is one approval asked for.
type ApprovalRequest struct {
	Requirement Requirement
	ProposerID  int64
	ApproverID  int64
	// ApproverGovernance is whether the approver holds the governance role
	// in the change request's project.
	ApproverGovernance bool
	Existing           []Approval
}

// EvaluateApproval applies the maker-checker rule. It answers whether the
// approval completes the change request, or the refusal:
//
//   - the proposer never approves their own change (SelfApprovalCode);
//   - an approver approves once (AlreadyApprovedCode);
//   - an approver without the governance role is refused when the slots
//     left after them could no longer seat the governance approvals the
//     tier needs (ApproverRoleCode). A non-governance approval is never
//     accepted only to leave the request impossible to complete.
func EvaluateApproval(request ApprovalRequest) (complete bool, err error) {
	if request.ApproverID <= 0 {
		return false, Refuse(SelfApprovalCode, "The approval needs a signed-in user.")
	}
	if request.ApproverID == request.ProposerID {
		return false, Refuse(SelfApprovalCode, "The author of a change request cannot approve it.")
	}
	approvals, governance := 0, 0
	for _, existing := range request.Existing {
		if existing.ApproverID == request.ApproverID {
			return false, Refuse(AlreadyApprovedCode, "You already approved this change request.")
		}
		approvals++
		if existing.Governance {
			governance++
		}
	}
	required := request.Requirement
	if approvals >= required.Approvals && governance >= required.GovernanceApprovals {
		// Already complete: only a race reaches this, and the store's row
		// lock makes it impossible. Accept nothing more.
		return false, Refuse(StateConflictCode, "The change request already has every approval it needs.")
	}
	approvals++
	if request.ApproverGovernance {
		governance++
	}
	slotsLeft := required.Approvals - approvals
	governanceLeft := required.GovernanceApprovals - governance
	if slotsLeft < 0 {
		slotsLeft = 0
	}
	if governanceLeft > slotsLeft {
		return false, Refuse(ApproverRoleCode,
			"This approval must come from a holder of the governance role ("+GovernancePermission+").")
	}
	return approvals >= required.Approvals && governance >= required.GovernanceApprovals, nil
}

// CheckGate refuses an approval the gate does not allow: pending, or failed
// without an override. gateID 0 (no gate suite) always allows.
func CheckGate(gateID int64, result GateResult, overridden bool) error {
	if gateID == 0 || overridden {
		return nil
	}
	switch result.State {
	case GatePassed:
		return nil
	case GatePending:
		return Refuse(GatePendingCode, "The release gate is still running. Approve after it passes.")
	default:
		failures := result.Failures
		if failures == nil {
			failures = []string{}
		}
		return RefuseWith(GateFailedCode,
			"The release gate failed. Fix the agent, or ask a governance role holder to override the gate.",
			map[string]any{"failures": failures})
	}
}

// MissingAttestations lists the required kinds that present does not hold,
// in the order required lists them.
func MissingAttestations(required, present []AttestationKind) []AttestationKind {
	missing := []AttestationKind{}
	for _, kind := range required {
		if !slices.Contains(present, kind) && !slices.Contains(missing, kind) {
			missing = append(missing, kind)
		}
	}
	return missing
}

// TierChange is a request to move an agent's tier.
type TierChange struct {
	From, To Tier
	// CallerIsOwner: the caller is the business or technical owner.
	CallerIsOwner bool
	// CallerGovernance: the caller holds the governance role.
	CallerGovernance bool
}

// CheckTierChange applies the tier rule: raising needs the owner or the
// governance role; lowering needs the governance role, so an author cannot
// lower an agent to low to skip approval.
func CheckTierChange(change TierChange) error {
	if !change.To.Valid() {
		return Refuse(InvalidCode, "risk_tier must be one of low, medium, high, critical.")
	}
	switch {
	case change.To == change.From:
		return nil
	case change.To.Above(change.From):
		if change.CallerIsOwner || change.CallerGovernance {
			return nil
		}
		return Refuse(OwnerRequiredCode, "Only an owner of the agent or a governance role holder can raise its tier.")
	default:
		if change.CallerGovernance {
			return nil
		}
		return Refuse(TierLowerCode, "Only a governance role holder can lower the tier of an agent.")
	}
}
