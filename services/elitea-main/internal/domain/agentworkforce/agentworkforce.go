// Package agentworkforce is the agent workforce of ADR-0050 decisions 5b,
// 5g-5j, 5l and 5m (issue 1616): the daily scorecard, the dismissal ladder
// with the retired registry state, dismissal and demotion proposals from
// people and HR agents, the guards on who may propose what, the appeal and
// the reinstatement.
//
// THE LADDER. Every stage but the last can be undone, and each reuses
// existing machinery:
//
//   - watch: the registry's under_review flag and a notice to the owner.
//   - demote: the narrowing (Narrowing) is written onto the agent's draft
//     version and an ADR-0040 release change request opens for it when the
//     agent's tier asks approval; a lead role is dropped at once.
//   - suspend: an ADR-0041 agent suspension of source dismissal; runs are
//     refused with execution_suspended and in-flight runs are cancelled.
//   - retire: the new registry state retired. The agent is unpublished, taken
//     out of conversations' rosters and leads, its schedules turned off; a
//     run of it is refused with agent_retired and a run of an agent that
//     nests it with nested_agent_retired. Reinstatement within 30 days
//     restores what retirement changed (the retire snapshot).
//   - delete: after the reinstate window, never under a legal hold, the
//     definition and memory are purged. The registry row, the audit, the
//     traces and the scorecards stay (ADR-0042).
//
// WHO. A person with ManagePermission (the agent manager) or the agent's
// owner proposes; an HR agent (a version with capabilities.can_review_agents,
// tier medium or higher) proposes through propose_dismissal. Only a person
// decides: the agent manager, or the owner for their own agent. An appeal is
// reviewed by an agent manager other than the one who decided.
//
// An agent's soul or memory cannot resist any of this: the platform decides.
package agentworkforce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/proactivity"
)

const (
	// ManagePermission is the agent manager ("HR"): decide workforce
	// proposals, apply a stage, reinstate, review an appeal. Shared 0236
	// grants it to the project admin.
	ManagePermission = "agents.workforce.manage"
	// CapabilitiesKey is the version meta key of an agent's capabilities.
	CapabilitiesKey = "capabilities"
	// ReviewCapability makes an agent an HR agent.
	ReviewCapability = "can_review_agents"
	// ProposeCapability is ADR-0050 decision 5a's hiring capability.
	ProposeCapability = "can_propose_agents"

	// ReinstateWindow is how long a retired agent can be reinstated, and how
	// long an owner may appeal a decision.
	ReinstateWindow = 30 * 24 * time.Hour
	// ScorecardRetention is how long a daily scorecard is kept.
	ScorecardRetention = 400 * 24 * time.Hour

	MaxRationaleBytes = 4 * 1024
	MaxNoteBytes      = 2 * 1024
	MaxRemoveToolkits = 64
)

// Stage is a rung of the dismissal ladder.
type Stage string

const (
	StageWatch   Stage = "watch"
	StageDemote  Stage = "demote"
	StageSuspend Stage = "suspend"
	StageRetire  Stage = "retire"
	StageDelete  Stage = "delete"
)

// Stages lists the ladder from the mildest stage.
var Stages = []Stage{StageWatch, StageDemote, StageSuspend, StageRetire, StageDelete}

// Valid reports a known stage.
func (s Stage) Valid() bool { return slices.Contains(Stages, s) }

// Kind is demote for the demote stage and dismiss for every other.
func (s Stage) Kind() string {
	if s == StageDemote {
		return "demote"
	}
	return "dismiss"
}

// Lifecycle states of the registry (agent_registry.lifecycle_state).
const (
	StateActive  = "active"
	StateRetired = "retired"
	StateDeleted = "deleted"
)

// Proposal statuses.
const (
	StatusProposed   = "proposed"
	StatusAskedOwner = "asked_owner"
	StatusApproved   = "approved"
	StatusRejected   = "rejected"
)

// Proposal sources.
const (
	SourcePerson  = "person"
	SourceHRAgent = "hr_agent"
)

// Appeal states.
const (
	AppealOpen    = "open"
	AppealGranted = "granted"
	AppealUpheld  = "upheld"
)

// Tool outcome codes propose_dismissal answers with.
const (
	OutcomeProposed     = "proposed"
	OutcomeAlreadyOpen  = "proposal_open"
	OutcomeNotAllowed   = "not_allowed"
	OutcomeGuardRefused = "guard_refused"
	OutcomeUnknownAgent = "agent_unknown"
	OutcomeInvalid      = "invalid_arguments"
)

// Refusal codes.
const (
	InvalidCode         = "workforce_invalid"
	NotFoundCode        = "not_found"
	ForbiddenCode       = "workforce_forbidden"
	GuardCode           = "workforce_guard_refused"
	OpenProposalCode    = "workforce_proposal_open"
	StateConflictCode   = "workforce_state_conflict"
	ConfirmNestedCode   = "workforce_confirm_nested"
	LegalHoldCode       = "workforce_legal_hold"
	RetentionCode       = "workforce_retention_pending"
	ReinstateClosedCode = "workforce_reinstate_closed"
	AppealClosedCode    = "workforce_appeal_closed"

	// AgentRetiredCode refuses a run of a retired agent.
	AgentRetiredCode = "agent_retired"
	// NestedAgentRetiredCode refuses a run of an agent that nests a retired
	// agent (ADR-0050 decision 5g): it fails until someone fixes it.
	NestedAgentRetiredCode = "nested_agent_retired"
)

var statuses = map[string]int{
	InvalidCode:         http.StatusUnprocessableEntity,
	NotFoundCode:        http.StatusNotFound,
	ForbiddenCode:       http.StatusForbidden,
	GuardCode:           http.StatusForbidden,
	OpenProposalCode:    http.StatusConflict,
	StateConflictCode:   http.StatusConflict,
	ConfirmNestedCode:   http.StatusConflict,
	LegalHoldCode:       http.StatusConflict,
	RetentionCode:       http.StatusConflict,
	ReinstateClosedCode: http.StatusConflict,
	AppealClosedCode:    http.StatusConflict,
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

// Body is the JSON error body.
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

// AsRefusal answers the refusal in err's chain.
func AsRefusal(err error) (*Refusal, bool) {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal, true
	}
	return nil, false
}

// RetiredError is the typed refusal of a run start: the agent is retired
// (agent_retired), or an agent the run nests is (nested_agent_retired).
type RetiredError struct {
	ApplicationID int64
	Nested        bool
}

// Code is agent_retired or nested_agent_retired.
func (e *RetiredError) Code() string {
	if e.Nested {
		return NestedAgentRetiredCode
	}
	return AgentRetiredCode
}

func (e *RetiredError) Error() string {
	return fmt.Sprintf("%s: agent %d", e.Code(), e.ApplicationID)
}

// Message is the caller-safe sentence.
func (e *RetiredError) Message() string {
	if e.Nested {
		return "This agent uses a sub-agent that was retired. Remove or replace it before running the agent again."
	}
	return "This agent was retired. An agent manager can reinstate it within 30 days of its retirement."
}

// HTTPRefusal maps a refused start to 409 with the typed body. ok is false
// for every other error.
func HTTPRefusal(err error) (status int, body map[string]any, ok bool) {
	var retired *RetiredError
	if !errors.As(err, &retired) {
		return 0, nil, false
	}
	return http.StatusConflict, map[string]any{
		"error": retired.Message(), "code": retired.Code(), "message": retired.Message(),
		"application_id": retired.ApplicationID,
	}, true
}

// HasCapability reads meta.capabilities.<key> of a version's meta: anything
// but a literal true is false.
func HasCapability(meta map[string]any, key string) bool {
	capabilities, _ := meta[CapabilitiesKey].(map[string]any)
	allowed, _ := capabilities[key].(bool)
	return allowed
}

// IsHRAgent reports whether a version's meta makes the agent an HR agent.
func IsHRAgent(meta map[string]any) bool { return HasCapability(meta, ReviewCapability) }

// Narrowing is the exact narrowing of a demotion (ADR-0050 decision 5g).
// Every field only takes away.
type Narrowing struct {
	// RemoveToolkitIDs detaches these toolkits from the agent's draft.
	RemoveToolkitIDs []int64 `json:"remove_toolkit_ids,omitempty"`
	// MemoryReview sets the memory write mode to review.
	MemoryReview bool `json:"memory_review,omitempty"`
	// ProactivityOff turns the heartbeat off: the draft's proactivity block
	// (#1618) gets mode "off", and the agent's heartbeat settings row is
	// disabled at once, since the heartbeat reads the RELEASE's block and
	// that keeps its block until the narrowed draft is published.
	ProactivityOff bool `json:"proactivity_off,omitempty"`
	// RemoveCapabilities removes can_propose_agents and can_review_agents.
	RemoveCapabilities []string `json:"remove_capabilities,omitempty"`
	// RemoveLead drops the agent as the lead of every conversation it leads.
	RemoveLead bool `json:"remove_lead,omitempty"`
	// ToolkitTypeCeiling keeps only toolkits of these types on the draft
	// (an allowlist; empty is no ceiling).
	ToolkitTypeCeiling []string `json:"toolkit_type_ceiling,omitempty"`
}

// Empty reports whether the narrowing takes nothing away.
func (n Narrowing) Empty() bool {
	return len(n.RemoveToolkitIDs) == 0 && !n.MemoryReview && !n.ProactivityOff &&
		len(n.RemoveCapabilities) == 0 && !n.RemoveLead && len(n.ToolkitTypeCeiling) == 0
}

// TouchesVersion reports whether the narrowing edits the version.
func (n Narrowing) TouchesVersion() bool {
	return len(n.RemoveToolkitIDs) > 0 || n.MemoryReview || n.ProactivityOff ||
		len(n.RemoveCapabilities) > 0 || len(n.ToolkitTypeCeiling) > 0
}

// CheckNarrowing bounds a narrowing and answers it canonical.
func CheckNarrowing(n Narrowing) (Narrowing, error) {
	if len(n.RemoveToolkitIDs) > MaxRemoveToolkits || len(n.ToolkitTypeCeiling) > MaxRemoveToolkits {
		return Narrowing{}, Refuse(InvalidCode, fmt.Sprintf("A narrowing names at most %d toolkits.", MaxRemoveToolkits))
	}
	for _, id := range n.RemoveToolkitIDs {
		if id <= 0 {
			return Narrowing{}, Refuse(InvalidCode, "remove_toolkit_ids are positive toolkit ids.")
		}
	}
	slices.Sort(n.RemoveToolkitIDs)
	n.RemoveToolkitIDs = slices.Compact(n.RemoveToolkitIDs)
	for i, capability := range n.RemoveCapabilities {
		capability = strings.TrimSpace(capability)
		if capability != ProposeCapability && capability != ReviewCapability {
			return Narrowing{}, Refuse(InvalidCode, "remove_capabilities names can_propose_agents or can_review_agents.")
		}
		n.RemoveCapabilities[i] = capability
	}
	slices.Sort(n.RemoveCapabilities)
	n.RemoveCapabilities = slices.Compact(n.RemoveCapabilities)
	for i, kind := range n.ToolkitTypeCeiling {
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind == "" || len(kind) > 128 || !oneLine(kind) {
			return Narrowing{}, Refuse(InvalidCode, "toolkit_type_ceiling lists toolkit types.")
		}
		n.ToolkitTypeCeiling[i] = kind
	}
	slices.Sort(n.ToolkitTypeCeiling)
	n.ToolkitTypeCeiling = slices.Compact(n.ToolkitTypeCeiling)
	if n.Empty() {
		return Narrowing{}, Refuse(InvalidCode, "A demotion names what it takes away.")
	}
	return n, nil
}

// NarrowMeta applies the version-level narrowing to a copy of meta.
func NarrowMeta(meta map[string]any, n Narrowing) map[string]any {
	out := make(map[string]any, len(meta)+2)
	for key, value := range meta {
		out[key] = value
	}
	if len(n.RemoveCapabilities) > 0 {
		if capabilities, ok := out[CapabilitiesKey].(map[string]any); ok {
			kept := make(map[string]any, len(capabilities))
			for key, value := range capabilities {
				if !slices.Contains(n.RemoveCapabilities, key) {
					kept[key] = value
				}
			}
			out[CapabilitiesKey] = kept
		}
	}
	if n.MemoryReview {
		memory, _ := out["memory"].(map[string]any)
		next := make(map[string]any, len(memory)+1)
		for key, value := range memory {
			next[key] = value
		}
		next["write"] = "review"
		out["memory"] = next
	}
	if n.ProactivityOff {
		block, _ := out[proactivity.MetaKey].(map[string]any)
		next := make(map[string]any, len(block)+1)
		for key, value := range block {
			next[key] = value
		}
		next["mode"] = proactivity.ModeOff
		out[proactivity.MetaKey] = next
	}
	return out
}

// ProposalInput is a new proposal.
type ProposalInput struct {
	Stage     Stage     `json:"stage"`
	Rationale string    `json:"rationale"`
	Narrowing Narrowing `json:"narrowing"`
}

// CheckProposal bounds a proposal and answers it trimmed.
func CheckProposal(in ProposalInput) (ProposalInput, error) {
	in.Rationale = strings.TrimSpace(in.Rationale)
	if !in.Stage.Valid() {
		return ProposalInput{}, Refuse(InvalidCode, "stage is watch, demote, suspend, retire or delete.")
	}
	if in.Rationale == "" || len(in.Rationale) > MaxRationaleBytes || !plain(in.Rationale) {
		return ProposalInput{}, Refuse(InvalidCode, fmt.Sprintf("A rationale of at most %d bytes is required.", MaxRationaleBytes))
	}
	if in.Stage == StageDemote {
		narrowing, err := CheckNarrowing(in.Narrowing)
		if err != nil {
			return ProposalInput{}, err
		}
		in.Narrowing = narrowing
	} else {
		in.Narrowing = Narrowing{}
	}
	return in, nil
}

// CheckNote bounds a decision or appeal note.
func CheckNote(note string, limit int) (string, error) {
	note = strings.TrimSpace(note)
	if len(note) > limit || !plain(note) {
		return "", Refuse(InvalidCode, fmt.Sprintf("The note is at most %d bytes.", limit))
	}
	return note, nil
}

// Tier ranks (agent_registry.risk_tier).
var tierRank = map[string]int{"low": 0, "medium": 1, "high": 2, "critical": 3}

// TierAbove reports whether tier a is above tier b. An unknown tier ranks
// as low.
func TierAbove(a, b string) bool { return tierRank[a] > tierRank[b] }

// GuardSubject is what the guards read about the proposing HR agent and its
// target.
type GuardSubject struct {
	ProposerApplicationID int64
	ProposerTier          string
	// ProposerIsHR is the proposer version's can_review_agents.
	ProposerIsHR        bool
	TargetApplicationID int64
	TargetTier          string
	// TargetIsHR: some version of the target sets can_review_agents.
	TargetIsHR bool
	// TargetHiredProposer: the target proposed the agent that is proposing
	// now (agent_proposals created_application_id), i.e. it hired it.
	TargetHiredProposer bool
	// TargetLegalHold: an active legal hold covers the target or its project.
	TargetLegalHold bool
	TargetState     string
}

// CheckAgentProposal applies the guards of ADR-0050 decision 5j to a
// proposal an agent makes. A nil error lets it through.
func CheckAgentProposal(subject GuardSubject, stage Stage) error {
	switch {
	case !subject.ProposerIsHR:
		return Refuse(OutcomeNotAllowed, "This agent is not an HR agent: its version does not set capabilities.can_review_agents.")
	case tierRank[subject.ProposerTier] < tierRank["medium"]:
		return Refuse(OutcomeNotAllowed, "An HR agent must be at least medium tier.")
	case subject.ProposerApplicationID == subject.TargetApplicationID:
		return Refuse(OutcomeGuardRefused, "An agent cannot propose its own demotion or dismissal.")
	case subject.TargetHiredProposer:
		return Refuse(OutcomeGuardRefused, "An agent cannot propose dismissing the agent that proposed or hired it.")
	case subject.TargetIsHR:
		return Refuse(OutcomeGuardRefused, "An HR agent cannot propose dismissing another HR agent.")
	case TierAbove(subject.TargetTier, subject.ProposerTier):
		return Refuse(OutcomeGuardRefused, "An HR agent cannot propose dismissing an agent of a higher tier than its own.")
	case subject.TargetTier == "critical":
		return Refuse(OutcomeGuardRefused, "A critical-tier agent is never dismissed on an agent's proposal.")
	case subject.TargetLegalHold:
		return Refuse(OutcomeGuardRefused, "An agent under legal hold is never dismissed on an agent's proposal.")
	case subject.TargetState != StateActive:
		return Refuse(OutcomeGuardRefused, "The agent is already retired.")
	case stage == StageDelete:
		return Refuse(OutcomeGuardRefused, "An HR agent cannot propose deleting an agent; propose retire.")
	}
	return nil
}

// ExemptFromAutomatic reports whether an agent may never be dismissed
// automatically: a critical-tier agent or one under legal hold.
func ExemptFromAutomatic(tier string, legalHold bool) bool {
	return tier == "critical" || legalHold
}

// Scorecard is one agent's daily scorecard (ADR-0050 decision 5h): platform
// data only, no transcript.
type Scorecard struct {
	Day                   string     `json:"day"`
	Runs30d               int        `json:"runs_30d"`
	Failed30d             int        `json:"failed_30d"`
	CapReached30d         int        `json:"cap_reached_30d"`
	FailureRate30d        float64    `json:"failure_rate_30d"`
	BreakerTrips7d        int        `json:"breaker_trips_7d"`
	ApprovalRejections30d int        `json:"approval_rejections_30d"`
	GuardrailHits30d      int        `json:"guardrail_hits_30d"`
	Spend30dUSD           float64    `json:"spend_30d_usd"`
	ProjectMedianSpendUSD float64    `json:"project_median_spend_usd"`
	ThumbsUp30d           int        `json:"thumbs_up_30d"`
	ThumbsDown30d         int        `json:"thumbs_down_30d"`
	LastUsedAt            *time.Time `json:"last_used_at"`
	UnusedDays            *int       `json:"unused_days"`
	CertificationState    string     `json:"certification_state"`
	ComputedAt            time.Time  `json:"computed_at"`
}

// Finish fills the derived fields: the failure rate (cap stops are not
// failures) and the unused days against now.
func (s *Scorecard) Finish(now time.Time) {
	judged := s.Runs30d - s.CapReached30d
	if judged > 0 {
		s.FailureRate30d = float64(s.Failed30d) / float64(judged)
	}
	if s.LastUsedAt != nil {
		days := int(now.Sub(*s.LastUsedAt).Hours() / 24)
		if days < 0 {
			days = 0
		}
		s.UnusedDays = &days
	}
}

// EvidenceDigest is the SHA-256 of an evidence document's canonical JSON.
func EvidenceDigest(evidence map[string]any) string {
	encoded, err := json.Marshal(evidence)
	if err != nil {
		encoded = []byte("{}")
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func plain(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsFunc(text, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\t'
	})
}

func oneLine(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsFunc(text, unicode.IsControl)
}
