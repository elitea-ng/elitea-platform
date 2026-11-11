package repos

// The propose_dismissal runtime tool's shared side (ADR-0050 decision 5j,
// issue 1616): the claimed run says which agent is proposing and whether its
// frozen version is an HR agent; the guards run here, under the target's
// registry row lock, against the target's tier, capabilities, hiring
// history and legal holds.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

// ResolveProposeDismissalRun reads the claimed run. The caller has
// authorized the claim; projectID is the project it resolved.
func (s *AgentWorkforce) ResolveProposeDismissalRun(ctx context.Context, projectID int64, executionID string, generation uint64) (storage.ProposeDismissalRun, error) {
	if generation == 0 || generation > 1<<63-1 {
		return storage.ProposeDismissalRun{}, storage.ErrContentUnauthorized
	}
	var (
		applicationID int64
		actor, stream string
		input         []byte
	)
	err := s.pool.QueryRow(ctx, proposeRunSQL, executionID, int64(generation), projectID).
		Scan(&applicationID, &actor, &stream, &input)
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.ProposeDismissalRun{}, storage.ErrContentUnauthorized
	}
	if err != nil {
		return storage.ProposeDismissalRun{}, fmt.Errorf("agent workforce: read the run: %w", err)
	}
	actorID, err := strconv.ParseInt(actor, 10, 64)
	if err != nil || actorID <= 0 {
		return storage.ProposeDismissalRun{}, storage.ErrContentRejected
	}
	run := storage.ProposeDismissalRun{ProjectID: projectID, ExecutionID: executionID, ApplicationID: applicationID, ActorID: actorID}
	if conversation, err := uuid.Parse(stream); err == nil {
		run.ConversationUUID = conversation.String()
	}
	meta, err := frozenVersionMeta(input)
	if err != nil {
		return storage.ProposeDismissalRun{}, storage.ErrContentRejected
	}
	run.IsHRAgent = agentworkforce.IsHRAgent(meta)
	return run, nil
}

// ProposeDismissal stores an HR agent's proposal after the guards. A
// refused guard answers its outcome code and stores nothing.
func (s *AgentWorkforce) ProposeDismissal(ctx context.Context, run storage.ProposeDismissalRun, request storage.DismissalRequest) (storage.DismissalOutcome, error) {
	input, err := agentworkforce.CheckProposal(agentworkforce.ProposalInput{
		Stage: agentworkforce.Stage(request.Stage), Rationale: request.Rationale, Narrowing: request.Narrowing,
	})
	if err != nil {
		var refusal *agentworkforce.Refusal
		if errors.As(err, &refusal) {
			return storage.DismissalOutcome{Status: agentworkforce.OutcomeInvalid, Message: refusal.Message}, nil
		}
		return storage.DismissalOutcome{}, err
	}
	var outcome storage.DismissalOutcome
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		proposer, err := readWorkforceAgent(ctx, tx, run.ProjectID, run.ApplicationID, false)
		if err != nil {
			return err
		}
		target, err := readWorkforceAgent(ctx, tx, run.ProjectID, request.ApplicationID, true)
		var refusal *agentworkforce.Refusal
		if errors.As(err, &refusal) && refusal.Code == agentworkforce.NotFoundCode {
			outcome = storage.DismissalOutcome{Status: agentworkforce.OutcomeUnknownAgent, Message: "No such agent in this project."}
			return nil
		}
		if err != nil {
			return err
		}
		hired, err := hiredBy(ctx, tx, run.ProjectID, target.ApplicationID, run.ApplicationID)
		if err != nil {
			return err
		}
		// The run's frozen meta can be an unpublished draft the owner edited:
		// can_review_agents counts only once it is on a published version,
		// i.e. it went through the agent's release path (ADR-0050 5b: every
		// capability is an ADR-0040 change at the agent's tier).
		publishedHR := false
		if run.IsHRAgent {
			if publishedHR, err = publishedVersionIsHR(ctx, tx, run.ProjectID, run.ApplicationID); err != nil {
				return err
			}
		}
		guard := agentworkforce.CheckAgentProposal(agentworkforce.GuardSubject{
			ProposerApplicationID: run.ApplicationID, ProposerTier: proposer.Tier, ProposerIsHR: run.IsHRAgent && publishedHR,
			TargetApplicationID: target.ApplicationID, TargetTier: target.Tier, TargetIsHR: target.IsHRAgent,
			TargetHiredProposer: hired, TargetLegalHold: target.LegalHold, TargetState: target.State,
		}, input.Stage)
		if errors.As(guard, &refusal) {
			outcome = storage.DismissalOutcome{Status: refusal.Code, Message: refusal.Message}
			return s.append(ctx, tx, run.ProjectID, "agent.workforce.guard_refused",
				"agent:"+strconv.FormatInt(run.ApplicationID, 10), strconv.FormatInt(target.ApplicationID, 10),
				run.ExecutionID, "stage:"+string(input.Stage), "outcome:"+refusal.Code)
		}
		proposal, err := s.insertProposal(ctx, tx, target, input, insertSource{
			hrAgent: run.ApplicationID, executionID: run.ExecutionID,
			principalHint: "agent:" + strconv.FormatInt(run.ApplicationID, 10),
		})
		if errors.As(err, &refusal) && refusal.Code == agentworkforce.OpenProposalCode {
			outcome = storage.DismissalOutcome{Status: agentworkforce.OutcomeAlreadyOpen, Message: refusal.Message}
			return errOpenProposal
		}
		if err != nil {
			return err
		}
		outcome = storage.DismissalOutcome{Status: agentworkforce.OutcomeProposed, ProposalID: proposal.ID}
		return nil
	})
	if errors.Is(err, errOpenProposal) {
		return outcome, nil
	}
	if err != nil {
		return storage.DismissalOutcome{}, err
	}
	if outcome.ProposalID > 0 {
		if proposal, readErr := s.GetProposal(ctx, run.ProjectID, outcome.ProposalID); readErr == nil {
			s.notifyManagers(ctx, proposal)
		}
	}
	return outcome, nil
}

// errOpenProposal rolls back the guard-passing transaction whose insert
// met another open proposal (the unique index aborted it).
var errOpenProposal = errors.New("agent workforce: an open proposal exists")

// hiredBy reports whether target proposed (hired) the proposer: an
// agent_proposals row of target that became the proposer.
func hiredBy(ctx context.Context, tx pgx.Tx, projectID, target, proposer int64) (bool, error) {
	exists, err := relationExists(ctx, tx, "elitea_lifecycle.agent_proposals")
	if err != nil || !exists {
		return false, err
	}
	var hired bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_lifecycle.agent_proposals
               WHERE project_id = $1 AND proposer_application_id = $2 AND created_application_id = $3)`,
		projectID, target, proposer).Scan(&hired); err != nil {
		return false, fmt.Errorf("agent workforce: read the hiring record: %w", err)
	}
	return hired, nil
}

// notifyManagers tells the project's administrators (the agent managers by
// default) about an HR agent's proposal. Best effort.
func (s *AgentWorkforce) notifyManagers(ctx context.Context, proposal WorkforceProposal) {
	admins, err := projectAdminIDs(ctx, s.pool, proposal.ProjectID)
	if err != nil {
		return
	}
	for _, id := range admins {
		meta := map[string]any{
			"message": fmt.Sprintf("An HR agent proposed to %s agent %q. Review it in the workforce inbox.",
				proposal.Stage, proposal.ApplicationName),
			"project_id": proposal.ProjectID, "application_id": proposal.ApplicationID, "proposal_id": proposal.ID,
			"stage": proposal.Stage, "event": "proposed",
		}
		_ = insertReviewNotification(ctx, s.pool, fmt.Sprintf("workforce-%d-proposed-%d", proposal.ID, id),
			proposal.ProjectID, id, WorkforceNotificationEventType, meta)
	}
}

// RetiredAgents answers which of the agents are not active (retired or
// deleted): the admission's check. A database without the column answers
// none.
func (s *AgentWorkforce) RetiredAgents(ctx context.Context, projectID int64, applicationIDs []int64) (map[int64]bool, error) {
	return retiredAgents(ctx, s.pool, projectID, applicationIDs)
}

func retiredAgents(ctx context.Context, q lifecycleQuerier, projectID int64, applicationIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if projectID <= 0 || len(applicationIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
SELECT application_id FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = ANY($2::bigint[]) AND lifecycle_state <> 'active'`,
		projectID, applicationIDs)
	if err != nil {
		return nil, fmt.Errorf("agent workforce: read the retired agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// publishedVersionIsHR reports whether a published version of the agent
// sets capabilities.can_review_agents.
func publishedVersionIsHR(ctx context.Context, q lifecycleQuerier, projectID, applicationID int64) (bool, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return false, nil
	}
	var hr bool
	err = q.QueryRow(ctx, fmt.Sprintf(`
SELECT EXISTS (SELECT 1 FROM %s.application_versions
               WHERE application_id = $1 AND status IN ('published', 'embedded')
                 AND meta -> 'capabilities' -> 'can_review_agents' = 'true'::jsonb)`, schema),
		applicationID).Scan(&hr)
	if err != nil {
		return false, fmt.Errorf("agent workforce: read the published capabilities: %w", err)
	}
	return hr, nil
}
