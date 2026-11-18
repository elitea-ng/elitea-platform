package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// compositionReconciler is the optional half of the delegation applier that
// approves the internal edges a Marketplace change wrote (DelegationGrants).
type compositionReconciler interface {
	ReconcileCompositionTx(ctx context.Context, tx pgx.Tx, in CompositionApproval) error
}

// approverIDsOf lists everyone who approved the change, the final approver
// included, once each.
func approverIDsOf(change agentlifecycle.ChangeRequest, finalApprover int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, id := range append([]int64{finalApprover}, approvedBy(change)...) {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func approvedBy(change agentlifecycle.ChangeRequest) []int64 {
	out := make([]int64, 0, len(change.Approvals))
	for _, recorded := range change.Approvals {
		out = append(out, recorded.ApproverID)
	}
	return out
}

// reconcileComposition approves the internal edges of the agent a hire wrote,
// in the transaction of the hire request's final approval. Without a composed
// reconciler it does nothing.
func (s *AgentLifecycle) reconcileComposition(ctx context.Context, tx pgx.Tx, change agentlifecycle.ChangeRequest, approverID int64, origin delegationgrant.Origin) error {
	reconciler, ok := s.delegationApplier.(compositionReconciler)
	if !ok {
		return nil
	}
	return reconciler.ReconcileCompositionTx(ctx, tx, CompositionApproval{
		ProjectID: change.ProjectID, RootApplicationID: change.ApplicationID, Origin: origin,
		ProposerID: change.ProposedBy, ApproverIDs: approverIDsOf(change, approverID),
	})
}

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
	if change.Kind == agentlifecycle.KindHire {
		// A hire sets the tier of the listing. An upgrade raises it only when
		// the request is applied, and an upstream merge never changes it.
		if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry SET risk_tier = $3, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`, change.ProjectID, change.ApplicationID, string(change.Tier)); err != nil {
			return agentlifecycle.ChangeRequest{}, fmt.Errorf("agent lifecycle: set the tier of the hired agent: %w", err)
		}
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
     decided_by, decided_at, marketplace_listing_id, marketplace_version_id, marketplace_digest, marketplace_payload)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, clock_timestamp(),
        CASE WHEN $8 THEN $7::bigint END, CASE WHEN $8 THEN clock_timestamp() END, $11, $12, $13, $14::jsonb)
RETURNING `+changeRequestColumns,
		change.ProjectID, change.ApplicationID, string(change.Kind), state, string(change.Tier),
		clampText(change.Summary, agentlifecycle.MaxSummary), change.ProposedBy, selfApproved, approvals, governance,
		change.ListingID, change.VersionID, change.Digest[:], payloadText(change.Payload)))
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
		reason := change.ApprovalReason
		if reason == "" {
			reason = "marketplace_personal_self_approve"
		}
		reasons = append(reasons, "policy:"+reason)
	}
	if err := s.append(ctx, tx, lifecycleEvent(change.ProjectID, action, change.ProposedBy,
		"agent_change_request", changeRef(change.ApplicationID, created.ID), reasons...)); err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	return created, nil
}

func payloadText(payload json.RawMessage) string {
	if len(payload) == 0 {
		return "{}"
	}
	return string(payload)
}

// MarketplaceApplier applies an approved upgrade or upstream merge to the
// agent's tenant schema, in the transaction that approves it. The handler that
// owns the tenant schema satisfies it (eliteacore.Handler.ApplyMarketplaceTx),
// composed after construction like the publisher.
type MarketplaceApplier interface {
	ApplyMarketplaceTx(ctx context.Context, tx pgx.Tx, apply agentlifecycle.MarketplaceApply) error
}

// ComposeMarketplaceApplier sets the applier after construction. It is a
// composition-time call, before the store serves.
func (s *AgentLifecycle) ComposeMarketplaceApplier(applier MarketplaceApplier) {
	if s != nil {
		s.marketplaceApplier = applier
	}
}

// finalizeMarketplace is the final approval of a Marketplace change request. A
// hire enables the tools and closes the request as approved, with no release
// and no certification. An upgrade or an upstream merge is applied by the
// applier in this transaction. A change to a version that was yanked, withdrawn
// or delisted since the request was recorded is refused as stale.
func (s *AgentLifecycle) finalizeMarketplace(ctx context.Context, tx pgx.Tx, change agentlifecycle.ChangeRequest, approverID int64) error {
	switch change.Kind {
	case agentlifecycle.KindHire:
		if err := marketplace.ActivateHireTx(ctx, tx, change.ProjectID, change.ApplicationID); err != nil {
			if errors.Is(err, marketplace.ErrNotHireable) {
				return agentlifecycle.Refuse(agentlifecycle.StaleCode,
					"The listing version was yanked or withdrawn after this request was made. Reject it.")
			}
			return fmt.Errorf("agent lifecycle: enable the tools of the hired agent: %w", err)
		}
		// The hire request approves the internal edges of the hired agent.
		if err := s.reconcileComposition(ctx, tx, change, approverID, delegationgrant.OriginMarketplaceHire); err != nil {
			return err
		}
	case agentlifecycle.KindUpgrade, agentlifecycle.KindUpstreamMerge:
		if s.marketplaceApplier == nil {
			return errors.New("agent lifecycle: no Marketplace applier is composed")
		}
		apply := agentlifecycle.MarketplaceApply{
			ProjectID: change.ProjectID, ApplicationID: change.ApplicationID, Kind: change.Kind, ActorID: approverID, Tier: change.Tier,
			ProposerID: change.ProposedBy, ApproverIDs: approverIDsOf(change, approverID),
		}
		var digest, payload []byte
		if err := tx.QueryRow(ctx, `
SELECT marketplace_listing_id, marketplace_version_id, marketplace_digest, marketplace_payload
FROM elitea_lifecycle.change_requests WHERE id = $1`, change.ID).
			Scan(&apply.ListingID, &apply.VersionID, &digest, &payload); err != nil {
			return fmt.Errorf("agent lifecycle: read the Marketplace change request: %w", err)
		}
		copy(apply.Digest[:], digest)
		apply.Payload = payload
		if err := s.marketplaceApplier.ApplyMarketplaceTx(ctx, tx, apply); err != nil {
			if errors.Is(err, marketplace.ErrNotHireable) {
				return agentlifecycle.Refuse(agentlifecycle.StaleCode,
					"The listing version was yanked or withdrawn after this request was made. Reject it.")
			}
			if refusal, ok := agentlifecycle.AsRefusal(err); ok {
				return refusal
			}
			return fmt.Errorf("agent lifecycle: apply the Marketplace change: %w", err)
		}
	default:
		return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "Only a Marketplace change request can be approved here.")
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.change_requests
SET state = 'approved', decided_by = $2, decided_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE id = $1`, change.ID, approverID); err != nil {
		return fmt.Errorf("agent lifecycle: close the Marketplace change request: %w", err)
	}
	return nil
}
