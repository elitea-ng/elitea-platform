// Package agentlifecycle is the domain of agent lifecycle governance
// (ADR-0040 decisions 1, 3, 5 and 6, issue #1497): the registry record of
// an agent, change requests with maker-checker approval, recertification
// and attestations.
//
// Releases themselves (decision 2) are package agentrelease. The evaluation
// gate (decision 4) is behind the ReleaseGate interface (gate.go); this
// package never runs an evaluation.
//
// The rules here are pure: what a tier requires, whether an approval
// completes a change request, which state moves are legal and who may
// change a tier. The store applies them inside its transactions, and the
// database repeats the one rule that must hold when code is bypassed: an
// approver is never the proposer (shared 0204).
package agentlifecycle

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Tier is the risk tier of an agent.
type Tier string

const (
	TierLow      Tier = "low"
	TierMedium   Tier = "medium"
	TierHigh     Tier = "high"
	TierCritical Tier = "critical"
)

// Tiers lists the tiers from the lowest to the highest.
var Tiers = []Tier{TierLow, TierMedium, TierHigh, TierCritical}

// Valid reports whether t is a known tier.
func (t Tier) Valid() bool { return t.rank() >= 0 }

func (t Tier) rank() int {
	for i, tier := range Tiers {
		if tier == t {
			return i
		}
	}
	return -1
}

// Above reports whether t is a higher tier than other.
func (t Tier) Above(other Tier) bool { return t.rank() > other.rank() }

// CertificationState is the certification state of an agent.
type CertificationState string

const (
	StateUncertified CertificationState = "uncertified"
	StateCertified   CertificationState = "certified"
	StateDue         CertificationState = "due"
	StateLapsed      CertificationState = "lapsed"
	StateSuspended   CertificationState = "suspended"
)

// Valid reports whether s is a known certification state.
func (s CertificationState) Valid() bool {
	switch s {
	case StateUncertified, StateCertified, StateDue, StateLapsed, StateSuspended:
		return true
	}
	return false
}

// AwaitsRecertification reports a certification that must be granted
// again: due, lapsed or suspended.
func (s CertificationState) AwaitsRecertification() bool {
	return s == StateDue || s == StateLapsed || s == StateSuspended
}

// ChangeKind is what a change request asks for.
type ChangeKind string

const (
	// KindRelease publishes a draft version as a new release.
	KindRelease ChangeKind = "release"
	// KindRecertification certifies the agent's published release again,
	// with no definition change.
	KindRecertification ChangeKind = "recertification"
	// KindLegacyUnreviewed records a version published before change
	// control (shared 0204 backfill). Never created over the API.
	KindLegacyUnreviewed ChangeKind = "legacy_unreviewed"
)

// ChangeState is the state of a change request.
type ChangeState string

const (
	ChangeDraft     ChangeState = "draft"
	ChangeSubmitted ChangeState = "submitted"
	// ChangeApproved is the end state of an approved recertification.
	ChangeApproved  ChangeState = "approved"
	ChangeRejected  ChangeState = "rejected"
	ChangeWithdrawn ChangeState = "withdrawn"
	// ChangePublished is the end state of an approved release: the final
	// approval published the candidate in the same transaction.
	ChangePublished ChangeState = "published"
)

// Open reports whether s still accepts a transition.
func (s ChangeState) Open() bool { return s == ChangeDraft || s == ChangeSubmitted }

// changeMoves lists the legal moves. approved and published are reached
// only through Approve; rejected and withdrawn only from an open state.
var changeMoves = map[ChangeState][]ChangeState{
	ChangeDraft:     {ChangeSubmitted, ChangeWithdrawn},
	ChangeSubmitted: {ChangeApproved, ChangePublished, ChangeRejected, ChangeWithdrawn},
}

// CanMove reports whether a change request in state from may move to to.
func CanMove(from, to ChangeState) bool {
	for _, next := range changeMoves[from] {
		if next == to {
			return true
		}
	}
	return false
}

// AttestationKind is the kind of an external review (decision 6).
type AttestationKind string

const (
	AttestationArchitecture AttestationKind = "architecture"
	AttestationLegal        AttestationKind = "legal"
	AttestationModelRisk    AttestationKind = "model_risk"
	AttestationDataOwner    AttestationKind = "data_owner"
	AttestationDataAccuracy AttestationKind = "data_accuracy"
	AttestationCustom       AttestationKind = "custom"
)

// AttestationKinds lists every kind.
var AttestationKinds = []AttestationKind{
	AttestationArchitecture, AttestationLegal, AttestationModelRisk,
	AttestationDataOwner, AttestationDataAccuracy, AttestationCustom,
}

// Valid reports whether k is a known kind.
func (k AttestationKind) Valid() bool {
	for _, kind := range AttestationKinds {
		if kind == k {
			return true
		}
	}
	return false
}

// SubjectType is what an attestation is about.
type SubjectType string

const (
	SubjectAgent   SubjectType = "agent"
	SubjectRelease SubjectType = "release"
	SubjectToolkit SubjectType = "toolkit"
	SubjectIndex   SubjectType = "index"
)

// Valid reports whether s is a known subject type.
func (s SubjectType) Valid() bool {
	switch s {
	case SubjectAgent, SubjectRelease, SubjectToolkit, SubjectIndex:
		return true
	}
	return false
}

// Bounds of the free text. The database CHECKs repeat them.
const (
	MaxUseText          = 2000
	MaxSummary          = 2000
	MaxReason           = 500
	MaxNote             = 2000
	MaxApprovalNote     = 1000
	MaxAttester         = 256
	MaxEvidenceURL      = 2048
	MaxCustomKind       = 64
	MaxDataClasses      = 32
	MaxVersionName      = 128
	MaxCategory         = 64
	MaxSubjectID        = 64
	MaxRegistryListRows = 500
	// MaxRegistryExportRows bounds one CSV or JSON export.
	MaxRegistryExportRows = 10000
)

var (
	dataClassPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	subjectIDPattern  = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	customKindPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
)

// NormalizeDataClasses trims, lower-cases and de-duplicates the data
// classes, and refuses a malformed or excess list. The platform has no
// classification catalogue, so a class is any short identifier.
func NormalizeDataClasses(classes []string) ([]string, error) {
	out := make([]string, 0, len(classes))
	seen := map[string]bool{}
	for _, class := range classes {
		class = strings.ToLower(strings.TrimSpace(class))
		if class == "" || seen[class] {
			continue
		}
		if !dataClassPattern.MatchString(class) {
			return nil, fmt.Errorf("data class %q is not a short identifier (a-z, 0-9, _ . -)", class)
		}
		seen[class] = true
		out = append(out, class)
	}
	if len(out) > MaxDataClasses {
		return nil, fmt.Errorf("at most %d data classes", MaxDataClasses)
	}
	return out, nil
}

// ValidSubjectID reports whether id is an acceptable subject reference: a
// numeric id or a UUID.
func ValidSubjectID(id string) bool { return subjectIDPattern.MatchString(id) }

// ValidCustomKind reports whether name is an acceptable custom kind label.
func ValidCustomKind(name string) bool { return customKindPattern.MatchString(name) }

// TextWithin reports whether text is at most limit characters.
func TextWithin(text string, limit int) bool { return utf8.RuneCountInString(text) <= limit }
