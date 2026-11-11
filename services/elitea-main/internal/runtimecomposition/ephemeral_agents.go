package runtimecomposition

import (
	"context"

	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/control"
)

// ephemeralAgentStore is repos.EphemeralAgents.
type ephemeralAgentStore interface {
	Record(ctx context.Context, executionID, commandID string, generation uint64, rows []repos.EphemeralAgentRow) error
}

// ephemeralAgentRecorder is control.EphemeralAgents over the repository
// (ADR-0050 decisions 4 and 9d, #1614): the rows are filed under the
// execution, command and generation of the verified fence, and the
// repository reads the project from the execution kernel at that
// generation, never one the worker names.
type ephemeralAgentRecorder struct {
	store ephemeralAgentStore
}

func (r ephemeralAgentRecorder) RecordEphemeralAgents(ctx context.Context, fence runtimedomain.Fence, agents []control.EphemeralAgent) error {
	rows := make([]repos.EphemeralAgentRow, 0, len(agents))
	for _, agent := range agents {
		rows = append(rows, repos.EphemeralAgentRow{
			AgentID: agent.AgentID, ParentCallID: agent.ParentCallID, Ordinal: agent.Ordinal,
			Label: agent.Label, DisplayName: agent.DisplayName, Avatar: agent.Avatar,
			ProfileDigest: agent.ProfileDigest, BriefDigest: agent.BriefDigest, BriefRefs: agent.BriefRefs,
			Status: agent.Status, Steps: agent.Steps, InputTokens: agent.InputTokens, OutputTokens: agent.OutputTokens,
			ErrorCode: agent.ErrorCode, StartedAt: agent.StartedAt, FinishedAt: agent.FinishedAt,
			Depth: agent.Depth, ParentAgentID: agent.ParentAgentID,
		})
	}
	return r.store.Record(ctx, fence.ExecutionID, fence.CommandID, fence.Generation, rows)
}
