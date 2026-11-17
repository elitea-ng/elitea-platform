package agentlifecycle

import (
	"encoding/json"
	"time"
)

// Registry is one agent's registry record (decision 1).
type Registry struct {
	ProjectID          int64
	ApplicationID      int64
	ApplicationName    string
	BusinessOwnerID    int64
	TechnicalOwnerID   int64
	Tier               Tier
	IntendedUse        string
	OutOfScopeUse      string
	DataClasses        []string
	State              CertificationState
	CertifiedReleaseID int64
	CertifiedAt        *time.Time
	RecertifyBy        *time.Time
	// StateReason says why the state last moved (a due reason, a lapse).
	StateReason    string
	StateChangedAt time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// SuspensionID is the active agent suspension (ADR-0041), 0 when none.
	SuspensionID int64
	// CircuitBreaker is the agent's circuit breaker override (ADR-0041
	// decision 2), a JSON object; {} for none. Package breaker parses and
	// judges it.
	CircuitBreaker []byte
}

// IsOwner reports whether userID is the business or technical owner.
func (r Registry) IsOwner(userID int64) bool {
	return userID > 0 && (userID == r.BusinessOwnerID || userID == r.TechnicalOwnerID)
}

// RegistryUpdate is a registry edit; a nil field is unchanged. A zero
// owner id clears the owner.
type RegistryUpdate struct {
	BusinessOwnerID  *int64
	TechnicalOwnerID *int64
	Tier             *Tier
	IntendedUse      *string
	OutOfScopeUse    *string
	DataClasses      *[]string
	// CircuitBreaker replaces the circuit breaker override (a JSON object,
	// {} to clear it). It must not loosen the tier default.
	CircuitBreaker []byte
}

// RegistryFilter narrows the cross-project registry listing.
type RegistryFilter struct {
	Tier      Tier
	State     CertificationState
	OwnerID   int64
	ProjectID int64
	Limit     int
	Offset    int
}

// ChangeRequest is one change request (decision 3).
type ChangeRequest struct {
	ID                          int64
	ProjectID                   int64
	ApplicationID               int64
	Kind                        ChangeKind
	State                       ChangeState
	Tier                        Tier
	CandidateVersionID          int64
	CandidateRevisionID         int64
	VersionName                 string
	Category                    string
	Summary                     string
	ProposedBy                  int64
	SelfApproved                bool
	RequiredApprovals           int
	RequiredGovernanceApprovals int
	GateID                      int64
	OverrideReason              string
	OverriddenBy                int64
	OverriddenAt                *time.Time
	CurrentReleaseID            int64
	ReleaseID                   int64
	DecidedBy                   int64
	DecidedAt                   *time.Time
	DecisionReason              string
	CreatedAt                   time.Time
	SubmittedAt                 *time.Time
	PublishedAt                 *time.Time
	Approvals                   []RecordedApproval
}

// RecordedApproval is a stored approval.
type RecordedApproval struct {
	ApproverID int64
	Governance bool
	Note       string
	CreatedAt  time.Time
}

// NewChangeRequest is a draft to create.
type NewChangeRequest struct {
	ProjectID          int64
	ApplicationID      int64
	Kind               ChangeKind
	CandidateVersionID int64
	VersionName        string
	Category           string
	Summary            string
	ProposedBy         int64
}

// DirectPublish is a publish made without a change request, by a tier that
// needs no approval. Its change request is recorded self-approved.
type DirectPublish struct {
	ProjectID          int64
	ApplicationID      int64
	SourceVersionID    int64
	PublishedVersionID int64
	ReleaseID          int64
	VersionName        string
	Category           string
	ActorID            int64
}

// PublishCandidate is a change request's candidate, as the publish path
// takes it.
type PublishCandidate struct {
	ProjectID     int64
	ApplicationID int64
	VersionID     int64
	VersionName   string
	Category      string
	// PublisherID is the proposer: the published row and the release name
	// them, and the approvals name the approvers.
	PublisherID int64
	// ReleaseNotes is the request's summary: the Marketplace listing shows it
	// as the release notes of the published version.
	ReleaseNotes string
}

// PublishedCandidate is what the publish of a candidate wrote.
type PublishedCandidate struct {
	ApplicationID      int64
	PublishedVersionID int64
	SourceVersionID    int64
	ReleaseID          int64
	VersionName        string
}

// Attestation is one recorded external review (decision 6).
type Attestation struct {
	ID             int64
	ProjectID      int64
	Kind           AttestationKind
	CustomKind     string
	SubjectType    SubjectType
	SubjectID      string
	Attester       string
	AttesterUserID int64
	ValidUntil     *time.Time
	EvidenceURL    string
	Note           string
	RecordedBy     int64
	CreatedAt      time.Time
	RevokedAt      *time.Time
	RevokedBy      int64
	RevokeReason   string
}

// Valid reports whether the attestation counts at now: not revoked and not
// expired.
func (a Attestation) Valid(now time.Time) bool {
	return a.RevokedAt == nil && (a.ValidUntil == nil || a.ValidUntil.After(now))
}

// NewMarketplaceChange is the record of a hire in the registry and, when the
// hire needs a person, its change request (ADR-0051 decision 3).
type NewMarketplaceChange struct {
	ProjectID     int64
	ApplicationID int64
	// Tier is the tier of the listing: the agent's registry row takes it.
	Tier       Tier
	ProposedBy int64
	// NeedsChange asks for a change request of kind Kind. A hire of a low-tier
	// agent with no write tool needs none, and only the registry row is set.
	NeedsChange bool
	Kind        ChangeKind
	Summary     string
	ListingID   int64
	VersionID   int64
	Digest      [32]byte
	// SelfApprove records the request as approved by its proposer, for a
	// personal project under the platform policy
	// marketplace_personal_self_approve. The audit event names the policy.
	SelfApprove bool
	// ApprovalReason replaces that policy in the audit event of a request
	// recorded as approved: an upgrade of a low-tier agent is one click and
	// names the rule (direct_low_tier), and an automatic upgrade names its
	// policy (marketplace_auto_upgrade).
	ApprovalReason string
	// Payload is what the approved request applies and its other columns
	// cannot hold: the bindings of an upgrade, the draft of an upstream merge.
	Payload json.RawMessage
}

// NewDelegationChange is the change request that approves one delegation grant
// under the governed profile (ADR-0044 amendment 1). It is recorded in the
// transaction that proposes the grant.
type NewDelegationChange struct {
	ProjectID int64
	// ApplicationID is the CALLER of the edge: the agent whose editors approve.
	ApplicationID int64
	GrantID       int64
	ProposedBy    int64
	Summary       string
}

// DelegationApply is one approved delegation change request, handed to the
// component that owns the grant table, which approves the grant in the same
// transaction. Approvers are every person who approved the request, the final
// one included: the grant lists only what ALL of them hold access to.
type DelegationApply struct {
	ProjectID     int64
	ApplicationID int64
	GrantID       int64
	ProposerID    int64
	ApproverIDs   []int64
	// FinalApproverID is the approval that completed the request.
	FinalApproverID int64
}

// MarketplaceApply is one approved Marketplace change, read back by the final
// approval (or by the upgrade that is recorded already approved) and handed to
// the component that owns the tenant schema, which applies it in the same
// transaction.
type MarketplaceApply struct {
	ProjectID     int64
	ApplicationID int64
	Kind          ChangeKind
	ListingID     int64
	VersionID     int64
	Digest        [32]byte
	Payload       json.RawMessage
	// ActorID is the person who approved, or proposed a direct change.
	ActorID int64
	// ProposerID asked for the change and ApproverIDs approved it, for the
	// delegation grants of the agent's internal edges (ADR-0044 amendment 1,
	// A1.2 rule 3): the request approves them, so every approver must hold what
	// they list. Both are empty for a change that records no request, and then
	// ActorID is both.
	ProposerID  int64
	ApproverIDs []int64
	// Tier is the tier the request was recorded at.
	Tier Tier
}
