package delegationgrant

import (
	"errors"
	"fmt"
	"time"
)

// EdgeKind is how the callee is attached to the caller.
type EdgeKind string

// The edge kinds of a composition (a skill is not an edge: it is text).
const (
	EdgeSubAgent      EdgeKind = "sub_agent"
	EdgeAgentTool     EdgeKind = "agent_tool"
	EdgePipelineChild EdgeKind = "pipeline_child"
	EdgePipelineNode  EdgeKind = "pipeline_node"
)

// Valid reports whether k is a known edge kind.
func (k EdgeKind) Valid() bool {
	switch k {
	case EdgeSubAgent, EdgeAgentTool, EdgePipelineChild, EdgePipelineNode:
		return true
	}
	return false
}

// State is where a grant is in its life.
type State string

// The grant states.
const (
	StatePending    State = "pending"
	StateApproved   State = "approved"
	StateStale      State = "stale"
	StateSuperseded State = "superseded"
	StateRevoked    State = "revoked"
)

// Active reports whether the grant still occupies its edge: at most one
// active grant exists per (caller, callee, edge kind).
func (s State) Active() bool {
	return s == StatePending || s == StateApproved || s == StateStale
}

// Consumable reports whether the worker reads the grant as G(Y to X). A stale
// grant is consumed too: it covers the set it recorded and the drift is
// narrowed out (A1.2 rule 3).
func (s State) Consumable() bool { return s == StateApproved || s == StateStale }

// Origin says what created the grant.
type Origin string

// The grant origins.
const (
	OriginAttach             Origin = "attach"
	OriginMarketplaceHire    Origin = "marketplace_hire"
	OriginMarketplaceUpgrade Origin = "marketplace_upgrade"
	OriginMigration          Origin = "migration"
)

// Valid reports whether o is a known origin.
func (o Origin) Valid() bool {
	switch o {
	case OriginAttach, OriginMarketplaceHire, OriginMarketplaceUpgrade, OriginMigration:
		return true
	}
	return false
}

// Edge names one composition edge.
type Edge struct {
	CallerEntityID int64
	CalleeEntityID int64
	Kind           EdgeKind
}

// Grant is one delegation grant.
type Grant struct {
	ID              int64
	ProjectID       int64
	Edge            Edge
	CalleeVersionID int64
	RequestedSet    GrantSet
	RequestedDigest string
	GrantSet        GrantSet
	Digest          string
	Omitted         []Item
	State           State
	Legacy          bool
	Governed        bool
	Origin          Origin
	ProposedBy      *int64
	ApprovedBy      *int64
	ApprovedAt      *time.Time
	RevokedBy       *int64
	RevokedAt       *time.Time
	SupersededBy    *int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Stale reports whether the callee's current set has drifted from the one the
// grant recorded: the current digest differs from the requested digest.
func (g Grant) Stale(currentDigest string) bool { return g.RequestedDigest != currentDigest }

// Callee is what an agent or pipeline version holds, as a delegation grant
// records it: the grant set of its toolkits, the hosts they reach and the data
// classes the agent declares (FromTools).
type Callee struct {
	ApplicationID int64
	VersionID     int64
	Name          string
	Set           GrantSet
}

// The refusals of a grant operation. The API maps them to statuses.
var (
	// ErrCalleeNotFound: the callee application, or a version that is not a
	// version of it, does not exist in the project.
	ErrCalleeNotFound = errors.New("delegation grant: the callee was not found")
	// ErrGoverned: the grant is approved through its change request, not
	// directly (the governed profile forces maker-checker).
	ErrGoverned = errors.New("delegation grant: a governed grant is approved through its change request")
	// ErrSelfApproval: the approver is the proposer where maker-checker applies.
	ErrSelfApproval = errors.New("delegation grant: the approver must differ from the proposer")
	// ErrNothingToApprove: the approval would list no toolkit, and no data
	// class or host either, so it would grant nothing.
	ErrNothingToApprove = errors.New("delegation grant: nothing is left to approve")
	// ErrNotFound: no such grant in the project.
	ErrNotFound = errors.New("delegation grant: not found")
	// ErrActiveEdgeExists: the edge already has an active grant.
	ErrActiveEdgeExists = errors.New("delegation grant: the edge already has an active grant")
	// ErrWrongState: the operation does not apply to the grant's state.
	ErrWrongState = errors.New("delegation grant: the grant is not in a state this applies to")
	// ErrInvalid: a malformed request (unknown edge kind, same entity, ...).
	ErrInvalid = errors.New("delegation grant: invalid request")
)

// CanTransition reports whether a grant may move from one state to another.
//
//	pending    -> approved, revoked
//	approved   -> stale, superseded, revoked
//	stale      -> approved (the callee came back within the grant), superseded, revoked
//	superseded, revoked: final
func CanTransition(from, to State) bool {
	switch from {
	case StatePending:
		return to == StateApproved || to == StateRevoked
	case StateApproved:
		return to == StateStale || to == StateSuperseded || to == StateRevoked
	case StateStale:
		return to == StateApproved || to == StateSuperseded || to == StateRevoked
	}
	return false
}

// CheckApprover is the maker-checker rule of ADR-0040 decision 3: where the
// governed flag is set the approver must differ from the proposer. Outside
// the governed profile an editor of the caller may approve their own grant.
// A governed grant with no recorded proposer cannot be approved by anyone
// (the table's CHECK says the same).
func CheckApprover(governed bool, proposer *int64, approver int64) error {
	if !governed {
		return nil
	}
	if proposer == nil || *proposer == approver {
		return ErrSelfApproval
	}
	return nil
}

// Access is what the approver holds. An approver may approve only the items
// they hold access to (A1.2 rule 2): each toolkit of the grant and each
// credential reference it names.
type Access interface {
	// HoldsToolkit reports whether the approver may use the toolkit named by
	// key ("toolkit:<id>", "builtin:<type>").
	HoldsToolkit(key string) bool
	// HoldsCredential reports whether the approver may use the credential
	// reference id.
	HoldsCredential(reference string) bool
}

// Approvable is the part of requested the approver may approve: the items
// they chose (choose nil means all of them) and hold access to. The rest is
// returned as omitted, and stays out of the grant, so the callee runs
// without it.
//
// A toolkit the approver does not hold, or whose credential reference they do
// not hold, leaves with its tools. Egress hosts need no holder: they are project bounds, not things an
// approver holds. Data classes are always kept (see below).
func Approvable(requested GrantSet, choose []Item, access Access) (approved GrantSet, omitted []Item, err error) {
	if access == nil {
		return GrantSet{}, nil, fmt.Errorf("%w: the approver's access is required", ErrInvalid)
	}
	if err := requested.Validate(); err != nil {
		return GrantSet{}, nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	all := requested.Items()
	keep := all
	if choose != nil {
		keep = intersectItems(all, choose)
		// Data classes are never left out: a set with no data class imposes
		// none (the worker's None), so dropping one would widen the grant
		// where the approver meant to narrow it.
		for _, item := range all {
			if item.Kind == ItemDataClass {
				keep = append(keep, item)
			}
		}
	}
	held := keep[:0:0]
	for _, item := range keep {
		switch item.Kind {
		case ItemToolkit:
			if !access.HoldsToolkit(item.Name) {
				continue
			}
		case ItemCredential:
			if !access.HoldsCredential(item.Name) {
				continue
			}
		}
		held = append(held, item)
	}
	approved = requested.Restrict(held)
	if len(approved.Toolkits) == 0 && len(approved.DataClasses) == 0 && len(approved.EgressHosts) == 0 {
		return GrantSet{}, nil, ErrNothingToApprove
	}
	return approved, requested.Without(approved), nil
}

func intersectItems(items, choose []Item) []Item {
	chosen := make(map[Item]bool, len(choose))
	for _, item := range choose {
		chosen[item] = true
	}
	out := make([]Item, 0, len(items))
	for _, item := range items {
		if chosen[item] {
			out = append(out, item)
		}
	}
	return out
}

// The security audit actions of a grant (event type
// securityaudit.TypeAgentLifecycle), each appended in the same transaction as
// the change it records.
const (
	ActionProposed   = "agent.delegation.proposed"
	ActionApproved   = "agent.delegation.approved"
	ActionRevoked    = "agent.delegation.revoked"
	ActionStale      = "agent.delegation.stale"
	ActionSuperseded = "agent.delegation.superseded"
)
