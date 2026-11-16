package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// The Marketplace side of agent lifecycle governance (ADR-0051 decision 3).
//
// A hired agent has a registry row at the tier of its listing. A hire above
// low tier, or with write tools, is created with every tool disabled and a
// change request of kind hire. The request is born submitted: there is no
// draft to edit, since the version and the digest are the listing's. The final
// approval enables the tools in its own transaction (finalize).

// RecordMarketplaceChangeTx sets the registry tier of a hired agent and, when
// the hire needs a person, records its change request. It runs in the hire's
// transaction, so a hire either has its record or does not exist. It answers
// the request, or the zero value when the hire needs none.
func (s *AgentLifecycle) RecordMarketplaceChangeTx(ctx context.Context, tx pgx.Tx, change agentlifecycle.NewMarketplaceChange) (agentlifecycle.ChangeRequest, error) {
	if !change.Tier.Valid() {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The listing has no valid risk tier.")
	}
	if err := EnsureRegistryTx(ctx, tx, change.ProjectID, change.ApplicationID, change.ProposedBy); err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry SET risk_tier = $3, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`, change.ProjectID, change.ApplicationID, string(change.Tier)); err != nil {
		return agentlifecycle.ChangeRequest{}, fmt.Errorf("agent lifecycle: set the tier of the hired agent: %w", err)
	}
	if !change.NeedsChange {
		return agentlifecycle.ChangeRequest{}, nil
	}
	if !change.Kind.IsMarketplace() {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "kind must be hire, upgrade or upstream_merge.")
	}
	policy, err := s.currentPolicy(ctx, tx)
	if err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	requirement := policy.Requirement(change.Tier)
	// A hire needs a person even when the tier's table asks for none: the
	// reason it needs a request is the write tool, not the tier.
	approvals := max(1, requirement.Approvals)
	governance := min(requirement.GovernanceApprovals, approvals)
	state, selfApproved := "submitted", false
	if change.SelfApprove {
		state, selfApproved, approvals, governance = "approved", true, 0, 0
	}
	created, err := scanChangeRequest(tx.QueryRow(ctx, `
INSERT INTO elitea_lifecycle.change_requests
    (tenant_project_id, application_id, kind, state, risk_tier, summary, proposed_by, self_approved,
     required_approvals, required_governance_approvals, submitted_at,
     decided_by, decided_at, marketplace_listing_id, marketplace_version_id, marketplace_digest)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, clock_timestamp(),
        CASE WHEN $8 THEN $7::bigint END, CASE WHEN $8 THEN clock_timestamp() END, $11, $12, $13)
RETURNING `+changeRequestColumns,
		change.ProjectID, change.ApplicationID, string(change.Kind), state, string(change.Tier),
		clampText(change.Summary, agentlifecycle.MaxSummary), change.ProposedBy, selfApproved, approvals, governance,
		change.ListingID, change.VersionID, change.Digest[:]))
	if isUniqueViolation(err, "change_requests_one_open") {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.OpenChangeCode,
			"The agent already has an open change request. Withdraw it or finish it first.")
	}
	if err != nil {
		return agentlifecycle.ChangeRequest{}, fmt.Errorf("agent lifecycle: record the hire change request: %w", err)
	}
	created.Approvals = []agentlifecycle.RecordedApproval{}
	reasons := []string{"kind:" + string(change.Kind), "tier:" + string(change.Tier),
		"listing:" + strconv.FormatInt(change.ListingID, 10), "version:" + strconv.FormatInt(change.VersionID, 10)}
	action := agentlifecycle.ActionChangeSubmitted
	if change.SelfApprove {
		action = agentlifecycle.ActionChangeApproved
		reasons = append(reasons, "policy:marketplace_personal_self_approve")
	}
	if err := s.append(ctx, tx, lifecycleEvent(change.ProjectID, action, change.ProposedBy,
		"agent_change_request", changeRef(change.ApplicationID, created.ID), reasons...)); err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	return created, nil
}

// finalizeMarketplace is the final approval of a hire: it enables the tools and
// closes the request as approved, with no release and no certification. A hire
// of a version that was yanked, withdrawn or delisted since the request was
// recorded is refused as stale.
func (s *AgentLifecycle) finalizeMarketplace(ctx context.Context, tx pgx.Tx, change agentlifecycle.ChangeRequest, approverID int64) error {
	if change.Kind != agentlifecycle.KindHire {
		return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "Only a hire change request can be approved in this version.")
	}
	if err := marketplace.ActivateHireTx(ctx, tx, change.ProjectID, change.ApplicationID); err != nil {
		if errors.Is(err, marketplace.ErrNotHireable) {
			return agentlifecycle.Refuse(agentlifecycle.StaleCode,
				"The listing version was yanked or withdrawn after this request was made. Reject it.")
		}
		return fmt.Errorf("agent lifecycle: enable the tools of the hired agent: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.change_requests
SET state = 'approved', decided_by = $2, decided_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE id = $1`, change.ID, approverID); err != nil {
		return fmt.Errorf("agent lifecycle: close the hire change request: %w", err)
	}
	return nil
}
