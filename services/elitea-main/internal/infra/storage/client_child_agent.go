package storage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
)

// ErrChildAgentNotInAgent: the named agent version is not one of the turn's
// agent's own `application` tools, or the turn has no agent at all.
var ErrChildAgentNotInAgent = errors.New("the agent is not one the running agent nests")

// ChildAgentAuthorization is one cloud child turn of a local turn to
// authorize (issue 1239).
type ChildAgentAuthorization struct {
	ProjectID int64
	ActorID   int64
	// TurnApplicationID and TurnVersionID are the live local turn's agent.
	TurnApplicationID int64
	TurnVersionID     int64
	// ApplicationID and VersionID are the nested agent to run in the cloud.
	ApplicationID int64
	VersionID     int64
}

// ChildAgentGrant is what the authorization resolved about the child.
type ChildAgentGrant struct {
	// Name is the child version's agent name, for the child conversation's
	// title; empty when the version does not carry one.
	Name string
	// AgentType is the child version's `agent_type` (`pipeline` for a
	// pipeline); empty when absent.
	AgentType string
}

// AuthorizeChildAgent decides whether a local turn may run a nested agent as
// a cloud child. Only a DIRECT `application` tool of the turn's own saved
// version qualifies: the desktop runs the turn's agent itself and hands each
// of its nested agents to the cloud, where any deeper nesting runs as it
// would in a cloud turn. Any other version (another agent, a grandchild, an
// agent of a version the turn does not run) is ErrChildAgentNotInAgent, so a
// live turn is never a way to start an arbitrary agent.
//
// It answers ErrChildAgentNotInAgent, ErrContentUnavailable or the context's
// error. The child is run under the caller's own grants by the ordinary start
// use case, which freezes and checks it like any chat turn.
func (service *ClientApplicationVersionService) AuthorizeChildAgent(
	ctx context.Context,
	request ChildAgentAuthorization,
) (ChildAgentGrant, error) {
	if service == nil || service.versions == nil || ctx == nil {
		return ChildAgentGrant{}, ErrContentUnavailable
	}
	if err := ctx.Err(); err != nil {
		return ChildAgentGrant{}, err
	}
	parent := ClientVersionIdentity{ProjectID: request.ProjectID, ApplicationID: request.TurnApplicationID, VersionID: request.TurnVersionID}
	child := ClientVersionIdentity{ProjectID: request.ProjectID, ApplicationID: request.ApplicationID, VersionID: request.VersionID}
	if !parent.valid() || !child.valid() || request.ActorID <= 0 || request.ActorID > math.MaxInt32 {
		return ChildAgentGrant{}, ErrChildAgentNotInAgent
	}
	record, err := service.readChildAgentVersion(ctx, parent)
	if err != nil {
		return ChildAgentGrant{}, err
	}
	children, err := savedNestedApplications(record.VersionDetails)
	if err != nil {
		return ChildAgentGrant{}, ErrChildAgentNotInAgent
	}
	nested := false
	for _, candidate := range children {
		if candidate[0] == request.ApplicationID && candidate[1] == request.VersionID {
			nested = true
			break
		}
	}
	if !nested {
		return ChildAgentGrant{}, ErrChildAgentNotInAgent
	}
	childRecord, err := service.readChildAgentVersion(ctx, child)
	if err != nil {
		return ChildAgentGrant{}, err
	}
	var details struct {
		Name      string `json:"name"`
		AgentType string `json:"agent_type"`
	}
	// A version whose name or type is not text still runs; only the title
	// and the type are left empty.
	_ = json.Unmarshal(childRecord.VersionDetails, &details)
	return ChildAgentGrant{Name: details.Name, AgentType: details.AgentType}, nil
}

func (service *ClientApplicationVersionService) readChildAgentVersion(
	ctx context.Context,
	identity ClientVersionIdentity,
) (CurrentApplicationVersionRecord, error) {
	record, err := service.versions.ReadCurrentApplicationVersion(ctx, identity.ProjectID, identity.ApplicationID, identity.VersionID)
	if err == nil {
		return record, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return CurrentApplicationVersionRecord{}, contextErr
	}
	if errors.Is(err, ErrContentNotFound) {
		return CurrentApplicationVersionRecord{}, ErrChildAgentNotInAgent
	}
	return CurrentApplicationVersionRecord{}, ErrContentUnavailable
}
