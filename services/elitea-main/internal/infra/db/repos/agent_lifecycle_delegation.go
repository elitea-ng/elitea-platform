package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	dg "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
)

// The delegation side of agent lifecycle governance (ADR-0044 amendment 1,
// A1.2 rule 2).
//
// Under the governed profile a delegation grant for the edge Y to X is
// approved through a change request of kind delegation on the CALLER Y. The
// request is born submitted (there is no draft to edit: the set and its digest
// are the grant's), needs at least one approval at every tier, low included,
// and the approver differs from the proposer like for any change request. The
// final approval approves the grant in its own transaction. A rejected or
// withdrawn request revokes the grant it asked for.

// DelegationApplier approves and revokes the grant a delegation change request
// carries, in the transaction of the approval or the close. The component
// that owns the tenant schema satisfies it (DelegationGrants).
type DelegationApplier interface {
	ApplyDelegationTx(ctx context.Context, tx pgx.Tx, apply agentlifecycle.DelegationApply) error
	RevokeDelegationTx(ctx context.Context, tx pgx.Tx, projectID, grantID, actorID int64) error
}

// ComposeDelegationApplier sets the applier after construction. It is a
// composition-time call, before the store serves.
func (s *AgentLifecycle) ComposeDelegationApplier(applier DelegationApplier) {
	if s != nil {
		s.delegationApplier = applier
	}
}

// RecordDelegationChangeTx records the change request of a governed grant. It
// runs in the transaction that proposes the grant, so a grant either has its
// request or does not exist. It answers the request.
func (s *AgentLifecycle) RecordDelegationChangeTx(ctx context.Context, tx pgx.Tx, change agentlifecycle.NewDelegationChange) (agentlifecycle.ChangeRequest, error) {
	if change.ProposedBy <= 0 || change.GrantID <= 0 || change.ApplicationID <= 0 {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "A delegation change request needs a signed-in user and a grant.")
	}
	if err := EnsureRegistryTx(ctx, tx, change.ProjectID, change.ApplicationID, change.ProposedBy); err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	registry, err := readRegistry(ctx, tx, change.ProjectID, change.ApplicationID, false)
	if err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	policy, err := s.currentPolicy(ctx, tx)
	if err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	requirement := policy.Requirement(registry.Tier)
	// A delegation needs a person at every tier: the governed profile forces
	// maker-checker even where the tier's table asks for no approval.
	approvals := max(1, requirement.Approvals)
	governance := min(requirement.GovernanceApprovals, approvals)
	created, err := scanChangeRequest(tx.QueryRow(ctx, `
INSERT INTO elitea_lifecycle.change_requests
    (tenant_project_id, application_id, kind, state, risk_tier, summary, proposed_by, self_approved,
     required_approvals, required_governance_approvals, submitted_at, delegation_grant_id)
VALUES ($1, $2, 'delegation', 'submitted', $3, $4, $5, false, $6, $7, clock_timestamp(), $8)
RETURNING `+changeRequestColumns,
		change.ProjectID, change.ApplicationID, string(registry.Tier),
		clampText(change.Summary, agentlifecycle.MaxSummary), change.ProposedBy, approvals, governance, change.GrantID))
	if isUniqueViolation(err, "change_requests_one_open_delegation") {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.OpenChangeCode,
			"The delegation already has an open change request.")
	}
	if err != nil {
		return agentlifecycle.ChangeRequest{}, fmt.Errorf("agent lifecycle: record the delegation change request: %w", err)
	}
	created.Approvals = []agentlifecycle.RecordedApproval{}
	if err := s.append(ctx, tx, lifecycleEvent(change.ProjectID, agentlifecycle.ActionChangeSubmitted, change.ProposedBy,
		"agent_change_request", changeRef(change.ApplicationID, created.ID),
		"kind:delegation", "tier:"+string(registry.Tier), "grant:"+strconv.FormatInt(change.GrantID, 10))); err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	return created, nil
}

// DelegationChangeForGrant answers the open or decided change request of a
// grant, newest first, or false when the grant has none (it was not governed).
func (s *AgentLifecycle) DelegationChangeForGrant(ctx context.Context, projectID, grantID int64) (agentlifecycle.ChangeRequest, bool, error) {
	change, err := scanChangeRequest(s.pool.QueryRow(ctx, `
SELECT `+changeRequestColumns+` FROM elitea_lifecycle.change_requests
WHERE tenant_project_id = $1 AND kind = 'delegation' AND delegation_grant_id = $2
ORDER BY id DESC LIMIT 1`, projectID, grantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return agentlifecycle.ChangeRequest{}, false, nil
	}
	if err != nil {
		return agentlifecycle.ChangeRequest{}, false, fmt.Errorf("agent lifecycle: read the delegation change request: %w", err)
	}
	return change, true, nil
}

func delegationGrantID(ctx context.Context, tx pgx.Tx, changeID int64) (int64, error) {
	var grantID int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(delegation_grant_id, 0) FROM elitea_lifecycle.change_requests WHERE id = $1`, changeID).Scan(&grantID); err != nil {
		return 0, fmt.Errorf("agent lifecycle: read the delegation change request: %w", err)
	}
	if grantID <= 0 {
		return 0, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The change request names no delegation grant.")
	}
	return grantID, nil
}

// finalizeDelegation is the final approval of a delegation change request: it
// approves the grant (listing only what every approver holds access to) and
// closes the request as approved, with no release and no certification.
func (s *AgentLifecycle) finalizeDelegation(ctx context.Context, tx pgx.Tx, change agentlifecycle.ChangeRequest, approverID int64) error {
	if s.delegationApplier == nil {
		return errors.New("agent lifecycle: no delegation applier is composed")
	}
	grantID, err := delegationGrantID(ctx, tx, change.ID)
	if err != nil {
		return err
	}
	approvers := make([]int64, 0, len(change.Approvals)+1)
	for _, recorded := range change.Approvals {
		approvers = append(approvers, recorded.ApproverID)
	}
	approvers = append(approvers, approverID)
	if err := s.delegationApplier.ApplyDelegationTx(ctx, tx, agentlifecycle.DelegationApply{
		ProjectID: change.ProjectID, ApplicationID: change.ApplicationID, GrantID: grantID,
		ProposerID: change.ProposedBy, ApproverIDs: approvers, FinalApproverID: approverID,
	}); err != nil {
		switch {
		case errors.Is(err, dg.ErrWrongState), errors.Is(err, dg.ErrNotFound):
			return agentlifecycle.Refuse(agentlifecycle.StaleCode,
				"The delegation grant changed or was revoked after this request was made. Reject it.")
		case errors.Is(err, dg.ErrNothingToApprove):
			return agentlifecycle.Refuse(agentlifecycle.InvalidCode,
				"The approvers do not hold access to anything this grant lists. Reject it, or ask someone with access.")
		}
		if refusal, ok := agentlifecycle.AsRefusal(err); ok {
			return refusal
		}
		return fmt.Errorf("agent lifecycle: approve the delegation grant: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.change_requests
SET state = 'approved', decided_by = $2, decided_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE id = $1`, change.ID, approverID); err != nil {
		return fmt.Errorf("agent lifecycle: close the delegation change request: %w", err)
	}
	return nil
}

// closeDelegation revokes the grant of a delegation request that was rejected
// or withdrawn. A grant already revoked (a detach got there first) is left as
// it is.
func (s *AgentLifecycle) closeDelegation(ctx context.Context, tx pgx.Tx, change agentlifecycle.ChangeRequest, actorID int64) error {
	if s.delegationApplier == nil {
		return errors.New("agent lifecycle: no delegation applier is composed")
	}
	grantID, err := delegationGrantID(ctx, tx, change.ID)
	if err != nil {
		return err
	}
	if err := s.delegationApplier.RevokeDelegationTx(ctx, tx, change.ProjectID, grantID, actorID); err != nil &&
		!errors.Is(err, dg.ErrWrongState) && !errors.Is(err, dg.ErrNotFound) {
		return fmt.Errorf("agent lifecycle: revoke the delegation grant: %w", err)
	}
	return nil
}
