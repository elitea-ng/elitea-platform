package agentlifecycle

import "time"

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
