package executioninterrupt

import "encoding/json"

// Permissions the ledger rechecks inside its transactions (contract §6).
const (
	PermissionList   = "models.applications.task.get"
	PermissionDecide = "models.chat.messages.create"
)

// Selector names one root response for one actor. The actor and project come
// from the authenticated request; the response id only selects a row after
// authorization.
type Selector struct {
	ProjectID         int64
	ActorUserID       int64
	ResponseMessageID string
}

func (s Selector) Valid() bool {
	return s.ProjectID > 0 && s.ActorUserID > 0 && ValidResponseMessageID(s.ResponseMessageID)
}

// RaiseInput is one accepted agent_interrupt_pending card (Wave 2 caller:
// the NodeEvent projection, inside the frame-accept transaction).
type RaiseInput struct {
	ProjectID      int64
	RootResponseID string
	ExecutionID    string
	Generation     int64
	SourceEventID  string
	SourceClaimID  string
	Frontier       Frontier
	Card           []byte
}

type RaiseResult struct {
	InterruptKey string
	Replay       bool
}

// DecideInput is one parsed public decision. Canonical is the canonical body
// returned by ParseDecisionRequest.
type DecideInput struct {
	Selector     Selector
	InterruptKey string
	Decision     Decision
	Canonical    []byte
}

// DecideResult is the 200 body plus what Wave 2 needs for the
// agent_hitl_resolved frame (never the value).
type DecideResult struct {
	InterruptKey     string
	InterruptID      string
	Action           Action
	State            State
	Revision         int64
	RequestID        string
	Replay           bool
	DecisionRevision int64
}

// ListedInterrupt is one open card. Card is the stored canonical card.
type ListedInterrupt struct {
	Card     json.RawMessage
	State    State
	Revision int64
}

type List struct {
	ResponseMessageID string
	DecisionRevision  int64
	Interrupts        []ListedInterrupt
}

// ClaimFence identifies the private caller: a live execution claim whose
// workload identity the transport already verified from the mTLS certificate.
type ClaimFence struct {
	ClaimID          string
	ExecutionID      string
	Generation       int64
	WorkloadIdentity string
	FenceToken       []byte
}

func (f ClaimFence) Valid() bool {
	return f.ClaimID != "" && len(f.ClaimID) <= MaxClaimIDBytes && ValidExecutionID(f.ExecutionID) &&
		f.Generation > 0 && f.WorkloadIdentity != "" && len(f.FenceToken) == 32
}

type AckResult struct {
	InterruptKey string
	State        State
	Revision     int64
	Replay       bool
}

// Closer names who closed open cards: a human (stop, regenerate) or a claim
// (fail-after-drain). Exactly one is set.
type Closer struct {
	ActorUserID int64
	ClaimID     string
}

func (c Closer) Valid() bool {
	return (c.ActorUserID > 0) != (c.ClaimID != "" && len(c.ClaimID) <= MaxClaimIDBytes)
}

// Resolved is one card closed by cancel or supersede, for the
// agent_hitl_resolved frame.
type Resolved struct {
	InterruptKey string
	InterruptID  string
	State        State
	Revision     int64
}
