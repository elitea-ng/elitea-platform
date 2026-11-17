package repos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// The Marketplace hook of delegation grants (ADR-0044 amendment 1, A1.2 rule 3;
// ADR-0051; delegation epic 2156 story S4).
//
// An installed agent's internal edges come from its listing version snapshot.
// A hire, an upgrade and an upstream merge write them (installNested,
// AttachTools), and the change request that carries the Marketplace change is
// the approval of those edges: the approvers of the request are the approvers
// of every grant. ReconcileCompositionTx runs in the same transaction as the
// apply and
//
//   - revokes the grants of edges whose agents the apply removed;
//   - proposes and approves the grant of an edge that has none, listing what the
//     approvers hold access to (the approvers' own access limits it, as for any
//     approval);
//   - re-approves the grant of an edge whose callee now holds more than the
//     grant recorded (the widened edges the change request listed);
//   - leaves an edge whose callee is within its grant alone.
//
// An edge nobody holding access can approve stays pending (or stale), so the
// callee runs without it and the caller's editors see the grant to review.

// CompositionApproval is the Marketplace change that wrote a composition.
type CompositionApproval struct {
	ProjectID int64
	// RootApplicationID is the installed or forked agent.
	RootApplicationID int64
	Origin            dg.Origin
	// ProposerID asked for the change and ApproverIDs approved it (the same
	// person for a direct, low-tier change).
	ProposerID  int64
	ApproverIDs []int64
}

// maxCompositionDepth bounds how deep nested agents are followed (the
// Marketplace snapshot's own bound).
const maxCompositionDepth = 5

type compositionEdge struct {
	caller, callee, calleeVersion int64
	pipeline                      bool
}

// ReconcileCompositionTx brings the grants of an installed agent's composition
// in line with what the Marketplace change wrote. It does nothing when the
// store has no callee reader or approver access resolver composed.
func (s *DelegationGrants) ReconcileCompositionTx(ctx context.Context, tx pgx.Tx, in CompositionApproval) error {
	if s == nil || s.callees == nil || s.access == nil || in.RootApplicationID <= 0 || len(in.ApproverIDs) == 0 {
		return nil
	}
	schema, err := tenantSchema(strconv.FormatInt(in.ProjectID, 10))
	if err != nil {
		return err
	}
	actor := in.ApproverIDs[0]
	if err := s.revokeOrphanedTx(ctx, tx, schema, in.ProjectID, actor); err != nil {
		return err
	}
	edges, err := compositionEdges(ctx, tx, schema, in.RootApplicationID)
	if err != nil {
		return err
	}
	holders := make([]dg.Access, 0, len(in.ApproverIDs))
	for _, approver := range in.ApproverIDs {
		access, err := s.access.AccessFor(ctx, in.ProjectID, approver)
		if err != nil {
			return fmt.Errorf("delegation grants: resolve the access of approver %d: %w", approver, err)
		}
		holders = append(holders, access)
	}
	// Maker-checker applies where the profile is governed AND the approvers are
	// not the proposer: a direct change by one person has no second one.
	governed := s.governed != nil && s.governed(ctx) && in.ProposerID > 0 && !slices.Contains(in.ApproverIDs, in.ProposerID)
	for _, edge := range edges {
		// A savepoint per edge: a grant that cannot be recorded (a read of the
		// callee, an audit append, a maker-checker refusal) must not take the
		// Marketplace change with it. The edge keeps its pending or stale grant,
		// so its callee runs without the new items and the editors see it.
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if err := s.reconcileEdgeTx(ctx, savepoint, schema, in, edge, allHold(holders), governed); err != nil {
			slog.ErrorContext(ctx, "delegation grants: an edge of the Marketplace change was not reconciled",
				"project_id", in.ProjectID, "caller_application_id", edge.caller, "callee_application_id", edge.callee, "error", err)
			if err := savepoint.Rollback(ctx); err != nil {
				return err
			}
			continue
		}
		if err := savepoint.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *DelegationGrants) reconcileEdgeTx(ctx context.Context, tx pgx.Tx, schema string, in CompositionApproval, edge compositionEdge, access dg.Access, governed bool) error {
	callee, err := s.callees.Snapshot(ctx, in.ProjectID, edge.callee, edge.calleeVersion)
	if errors.Is(err, dg.ErrCalleeNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(callee.Set.Toolkits) == 0 && len(callee.Set.EgressHosts) == 0 && len(callee.Set.DataClasses) == 0 {
		return nil // a callee that holds nothing needs no grant
	}
	kind := dg.EdgeAgentTool
	if edge.pipeline {
		kind = dg.EdgePipelineChild
	}
	dgEdge := dg.Edge{CallerEntityID: edge.caller, CalleeEntityID: edge.callee, Kind: kind}
	existing, found, err := activeForEdge(ctx, tx, schema, dgEdge, true)
	if err != nil {
		return err
	}
	var proposer *int64
	if in.ProposerID > 0 {
		proposer = &in.ProposerID
	}
	approver := in.ApproverIDs[0]
	switch {
	case !found:
		grant, err := s.ProposeTx(ctx, tx, ProposeInput{
			ProjectID: in.ProjectID, Edge: dgEdge, CalleeVersionID: edge.calleeVersion, Requested: callee.Set,
			Origin: in.Origin, Governed: governed, ProposedBy: proposer, NoChangeRequest: true,
		})
		if err != nil {
			return err
		}
		return ignoreNothingToApprove(s.approveTx(ctx, tx, in.ProjectID, grant.ID, approver, nil, access))
	case existing.State == dg.StatePending:
		return ignoreNothingToApprove(s.approveTx(ctx, tx, in.ProjectID, existing.ID, approver, nil, access))
	case existing.State == dg.StateStale || len(existing.RequestedSet.Missing(callee.Set)) > 0:
		// The callee holds more than the grant recorded: approving the change
		// request re-approves the edge.
		_, err := s.reapproveTx(ctx, tx, schema, ReapproveInput{
			ProjectID: in.ProjectID, GrantID: existing.ID, CalleeVersionID: edge.calleeVersion, Requested: callee.Set,
			Access: access, Origin: in.Origin, Governed: existing.Governed || governed, ProposedBy: proposer, ApprovedBy: approver,
		})
		return ignoreNothingToApprove2(err)
	}
	return nil
}

func ignoreNothingToApprove(_ dg.Grant, err error) error { return ignoreNothingToApprove2(err) }

// ignoreNothingToApprove2: approvers who hold access to nothing the callee
// lists leave the grant pending (or stale); the apply itself goes on.
func ignoreNothingToApprove2(err error) error {
	if errors.Is(err, dg.ErrNothingToApprove) {
		return nil
	}
	return err
}

// revokeOrphanedTx revokes the active grants of edges whose caller or callee
// application no longer exists (an upgrade removes the old nested agents and
// writes new ones; a deleted agent takes its edges with it).
func (s *DelegationGrants) revokeOrphanedTx(ctx context.Context, tx pgx.Tx, schema string, projectID, actor int64) error {
	rows, err := tx.Query(ctx, fmt.Sprintf(`
SELECT id FROM %[1]s.delegation_grants AS grant_row
WHERE state IN ('pending', 'approved', 'stale')
  AND (NOT EXISTS (SELECT 1 FROM %[1]s.applications WHERE id = grant_row.caller_entity_id)
       OR NOT EXISTS (SELECT 1 FROM %[1]s.applications WHERE id = grant_row.callee_entity_id))`, schema))
	if err != nil {
		return fmt.Errorf("delegation grants: find the orphaned grants: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.RevokeTx(ctx, tx, projectID, id, actor); err != nil && !errors.Is(err, dg.ErrWrongState) {
			return err
		}
	}
	return nil
}

// compositionEdges lists the agent-as-tool edges below the root, breadth first
// to maxCompositionDepth, each agent once: the `application` toolkit rows of
// every version of the caller.
func compositionEdges(ctx context.Context, tx pgx.Tx, schema string, root int64) ([]compositionEdge, error) {
	var out []compositionEdge
	seen := map[int64]bool{root: true}
	frontier := []int64{root}
	for depth := 0; depth < maxCompositionDepth && len(frontier) > 0; depth++ {
		var next []int64
		for _, caller := range frontier {
			var pipeline bool
			if err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE((SELECT agent_type = 'pipeline' FROM %s.application_versions WHERE application_id = $1 ORDER BY id LIMIT 1), false)`, schema), caller).Scan(&pipeline); err != nil {
				return nil, fmt.Errorf("delegation grants: read the caller type: %w", err)
			}
			rows, err := tx.Query(ctx, fmt.Sprintf(`
SELECT DISTINCT (tool.settings ->> 'application_id')::bigint, (%[1]s)::bigint
FROM %[2]s.entity_tool_mapping AS mapping
JOIN %[2]s.elitea_tools AS tool ON tool.id = mapping.tool_id
JOIN %[2]s.application_versions AS caller_version ON caller_version.id = mapping.entity_version_id
WHERE mapping.entity_type = 'agent' AND tool.type = 'application' AND caller_version.application_id = $1
  AND tool.settings ->> 'application_id' ~ '^[0-9]+$' AND (%[1]s) ~ '^[0-9]+$'
ORDER BY 1, 2`, ApplicationToolSettingsVersionSQL, schema), caller)
			if err != nil {
				return nil, fmt.Errorf("delegation grants: read the composition edges: %w", err)
			}
			for rows.Next() {
				edge := compositionEdge{caller: caller, pipeline: pipeline}
				if err := rows.Scan(&edge.callee, &edge.calleeVersion); err != nil {
					rows.Close()
					return nil, err
				}
				if edge.callee == caller {
					continue
				}
				out = append(out, edge)
				if !seen[edge.callee] {
					seen[edge.callee] = true
					next = append(next, edge.callee)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
		frontier = next
	}
	return out, nil
}
