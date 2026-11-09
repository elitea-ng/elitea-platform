package repos

// The agent workforce (shared 0245, ADR-0050 decisions 5b, 5g-5j and 5l,
// issue 1616): dismissal and demotion proposals, their decision by a person,
// the ladder stages they apply (agent_workforce_ladder.go), the appeal and
// the reinstatement. Every change is a security audit event in the change's
// own transaction (TypeAgentWorkforceChanged), with the actor (a person, or
// the HR agent that proposed) and the evidence digest.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// WorkforceNotificationEventType is the bell notice of a workforce change.
const WorkforceNotificationEventType = "agent_workforce_changed"

// AgentWorkforce is the workforce store.
type AgentWorkforce struct {
	pool  *pgxpool.Pool
	audit securityaudit.Appender
	now   func() time.Time
	// suspensionChanged is the kill switch gate's invalidation, told every
	// suspension the ladder wrote or resumed.
	suspensionChanged func(id int64)
}

// NewAgentWorkforce builds the store. A nil appender is refused.
func NewAgentWorkforce(pool *pgxpool.Pool, audit securityaudit.Appender) (*AgentWorkforce, error) {
	if pool == nil || audit == nil {
		return nil, errors.New("agent workforce: pool and audit appender are required")
	}
	return &AgentWorkforce{pool: pool, audit: audit, now: time.Now}, nil
}

// ComposeAgentWorkforce builds the store with the platform appender.
func ComposeAgentWorkforce(pool *pgxpool.Pool, suspensionChanged func(id int64)) (*AgentWorkforce, error) {
	appender, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, err
	}
	store, err := NewAgentWorkforce(pool, appender)
	if err != nil {
		return nil, err
	}
	store.suspensionChanged = suspensionChanged
	return store, nil
}

// WithClock sets the clock (tests).
func (s *AgentWorkforce) WithClock(now func() time.Time) *AgentWorkforce {
	if now != nil {
		s.now = now
	}
	return s
}

func (s *AgentWorkforce) notify(ids ...int64) {
	if s.suspensionChanged == nil {
		return
	}
	for _, id := range ids {
		if id > 0 {
			s.suspensionChanged(id)
		}
	}
}

// WorkforceActor is a person acting on the workforce.
type WorkforceActor struct {
	UserID int64
	// Manager: the person holds agents.workforce.manage in the project.
	Manager bool
}

// WorkforceProposal is one proposal as the inbox and the agent's page read
// it.
type WorkforceProposal struct {
	ID                    int64                    `json:"id"`
	ProjectID             int64                    `json:"project_id"`
	ApplicationID         int64                    `json:"application_id"`
	ApplicationName       string                   `json:"application_name"`
	Kind                  string                   `json:"kind"`
	Stage                 string                   `json:"stage"`
	Source                string                   `json:"source"`
	ProposedByUserID      *int64                   `json:"proposed_by_user_id"`
	ProposerApplicationID *int64                   `json:"proposer_application_id"`
	ExecutionID           *string                  `json:"execution_id"`
	Rationale             string                   `json:"rationale"`
	Narrowing             agentworkforce.Narrowing `json:"narrowing"`
	Evidence              map[string]any           `json:"evidence"`
	EvidenceDigest        string                   `json:"evidence_digest"`
	Status                string                   `json:"status"`
	OwnerComment          *string                  `json:"owner_comment"`
	DecidedByUserID       *int64                   `json:"decided_by_user_id"`
	DecidedAt             *time.Time               `json:"decided_at"`
	DecisionNote          *string                  `json:"decision_note"`
	Applied               map[string]any           `json:"applied"`
	AppealState           *string                  `json:"appeal_state"`
	AppealReason          *string                  `json:"appeal_reason"`
	AppealedByUserID      *int64                   `json:"appealed_by_user_id"`
	AppealedAt            *time.Time               `json:"appealed_at"`
	AppealDecidedByUserID *int64                   `json:"appeal_decided_by_user_id"`
	AppealDecidedAt       *time.Time               `json:"appeal_decided_at"`
	AppealNote            *string                  `json:"appeal_note"`
	CreatedAt             time.Time                `json:"created_at"`
}

const workforceProposalColumns = `
p.id, p.project_id, p.application_id, p.kind, p.stage, p.source, p.proposed_by_user_id,
p.proposer_application_id, p.execution_id, p.rationale, p.narrowing, p.evidence, p.evidence_digest,
p.status, p.owner_comment, p.decided_by_user_id, p.decided_at, p.decision_note, p.applied,
p.appeal_state, p.appeal_reason, p.appealed_by_user_id, p.appealed_at, p.appeal_decided_by_user_id,
p.appeal_decided_at, p.appeal_note, p.created_at`

func scanWorkforceProposal(row pgx.Row) (WorkforceProposal, error) {
	var (
		p                            WorkforceProposal
		narrowing, evidence, applied []byte
	)
	if err := row.Scan(&p.ID, &p.ProjectID, &p.ApplicationID, &p.Kind, &p.Stage, &p.Source, &p.ProposedByUserID,
		&p.ProposerApplicationID, &p.ExecutionID, &p.Rationale, &narrowing, &evidence, &p.EvidenceDigest,
		&p.Status, &p.OwnerComment, &p.DecidedByUserID, &p.DecidedAt, &p.DecisionNote, &applied,
		&p.AppealState, &p.AppealReason, &p.AppealedByUserID, &p.AppealedAt, &p.AppealDecidedByUserID,
		&p.AppealDecidedAt, &p.AppealNote, &p.CreatedAt); err != nil {
		return WorkforceProposal{}, err
	}
	if err := json.Unmarshal(narrowing, &p.Narrowing); err != nil {
		return WorkforceProposal{}, fmt.Errorf("agent workforce: decode the narrowing: %w", err)
	}
	if err := json.Unmarshal(evidence, &p.Evidence); err != nil {
		return WorkforceProposal{}, fmt.Errorf("agent workforce: decode the evidence: %w", err)
	}
	if len(applied) > 0 {
		if err := json.Unmarshal(applied, &p.Applied); err != nil {
			return WorkforceProposal{}, fmt.Errorf("agent workforce: decode the applied record: %w", err)
		}
	}
	return p, nil
}

// WorkforceFilter narrows a list.
type WorkforceFilter struct {
	// Status: "" is the open ones (proposed, asked_owner); all is every one.
	Status        string
	ApplicationID int64
	Limit, Offset int
}

// ListProposals answers a project's proposals, newest first.
func (s *AgentWorkforce) ListProposals(ctx context.Context, projectID int64, filter WorkforceFilter) ([]WorkforceProposal, int, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	where := `p.project_id = $1 AND ($2::bigint = 0 OR p.application_id = $2)`
	switch filter.Status {
	case "":
		where += ` AND p.status IN ('proposed', 'asked_owner')`
	case "all":
	default:
		where += ` AND p.status = $5`
	}
	args := []any{projectID, filter.ApplicationID, limit, filter.Offset}
	if filter.Status != "" && filter.Status != "all" {
		args = append(args, filter.Status)
	}
	rows, err := s.pool.Query(ctx, `
SELECT `+workforceProposalColumns+`, count(*) OVER ()
FROM elitea_lifecycle.agent_workforce_proposals AS p
WHERE `+where+`
ORDER BY p.created_at DESC, p.id DESC
LIMIT $3 OFFSET $4`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("agent workforce: list: %w", err)
	}
	defer rows.Close()
	out := []WorkforceProposal{}
	total := 0
	for rows.Next() {
		proposal, err := scanWorkforceProposal(countedRow{row: rows, count: &total})
		if err != nil {
			return nil, 0, err
		}
		out = append(out, proposal)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("agent workforce: list: %w", err)
	}
	s.nameProposals(ctx, projectID, out)
	return out, total, nil
}

type countedRow struct {
	row   pgx.Row
	count *int
}

func (r countedRow) Scan(dest ...any) error { return r.row.Scan(append(dest, r.count)...) }

func (s *AgentWorkforce) nameProposals(ctx context.Context, projectID int64, proposals []WorkforceProposal) {
	if len(proposals) == 0 {
		return
	}
	ids := make([]int64, 0, len(proposals))
	for _, p := range proposals {
		ids = append(ids, p.ApplicationID)
	}
	names := applicationNames(ctx, s.pool, projectID, ids)
	for i := range proposals {
		proposals[i].ApplicationName = names[proposals[i].ApplicationID]
	}
}

func applicationNames(ctx context.Context, q lifecycleQuerier, projectID int64, ids []int64) map[int64]string {
	names := map[int64]string{}
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return names
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT id, name FROM %s.applications WHERE id = ANY($1::bigint[])`, schema), ids)
	if err != nil {
		return names
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if rows.Scan(&id, &name) == nil {
			names[id] = name
		}
	}
	return names
}

// GetProposal answers one proposal of the project.
func (s *AgentWorkforce) GetProposal(ctx context.Context, projectID, proposalID int64) (WorkforceProposal, error) {
	proposal, err := readWorkforceProposal(ctx, s.pool, projectID, proposalID, false)
	if err != nil {
		return WorkforceProposal{}, err
	}
	list := []WorkforceProposal{proposal}
	s.nameProposals(ctx, projectID, list)
	return list[0], nil
}

func readWorkforceProposal(ctx context.Context, q lifecycleQuerier, projectID, proposalID int64, lock bool) (WorkforceProposal, error) {
	statement := `SELECT ` + workforceProposalColumns + ` FROM elitea_lifecycle.agent_workforce_proposals AS p
WHERE p.project_id = $1 AND p.id = $2`
	if lock {
		statement += ` FOR UPDATE`
	}
	proposal, err := scanWorkforceProposal(q.QueryRow(ctx, statement, projectID, proposalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkforceProposal{}, agentworkforce.Refuse(agentworkforce.NotFoundCode, "No such proposal in this project.")
	}
	if err != nil {
		return WorkforceProposal{}, fmt.Errorf("agent workforce: read the proposal: %w", err)
	}
	return proposal, nil
}

// WorkforceAgent is what the workforce knows about one agent: its registry
// record's workforce columns, its owners and whether a legal hold covers it.
type WorkforceAgent struct {
	ProjectID        int64      `json:"project_id"`
	ApplicationID    int64      `json:"application_id"`
	ApplicationName  string     `json:"application_name"`
	Tier             string     `json:"risk_tier"`
	BusinessOwnerID  int64      `json:"business_owner_id"`
	TechnicalOwnerID int64      `json:"technical_owner_id"`
	UnderReview      bool       `json:"under_review"`
	UnderReviewSince *time.Time `json:"under_review_since"`
	State            string     `json:"lifecycle_state"`
	RetiredAt        *time.Time `json:"retired_at"`
	RetiredBy        *int64     `json:"retired_by"`
	ReinstateUntil   *time.Time `json:"reinstate_until"`
	PurgeAfter       *time.Time `json:"purge_after"`
	SuspensionID     int64      `json:"suspension_id"`
	SuspensionSource string     `json:"suspension_source"`
	LegalHold        bool       `json:"legal_hold"`
	IsHRAgent        bool       `json:"is_hr_agent"`
}

// IsOwner reports whether userID owns the agent.
func (a WorkforceAgent) IsOwner(userID int64) bool {
	return userID > 0 && (userID == a.BusinessOwnerID || userID == a.TechnicalOwnerID)
}

// readWorkforceAgent reads the agent's registry row (creating it, as every
// registry read does) and the facts the guards need. lock takes the row
// lock: every ladder change of one agent serialises on it.
func readWorkforceAgent(ctx context.Context, q lifecycleQuerier, projectID, applicationID int64, lock bool) (WorkforceAgent, error) {
	if err := ensureRegistry(ctx, q, projectID, applicationID); err != nil {
		return WorkforceAgent{}, workforceRefusal(err)
	}
	statement := `
SELECT tenant_project_id, application_id, risk_tier, COALESCE(business_owner_id, 0), COALESCE(technical_owner_id, 0),
       under_review, under_review_since, lifecycle_state, retired_at, retired_by, reinstate_until, purge_after
FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`
	if lock {
		statement += ` FOR UPDATE`
	}
	var agent WorkforceAgent
	err := q.QueryRow(ctx, statement, projectID, applicationID).Scan(&agent.ProjectID, &agent.ApplicationID, &agent.Tier,
		&agent.BusinessOwnerID, &agent.TechnicalOwnerID, &agent.UnderReview, &agent.UnderReviewSince, &agent.State,
		&agent.RetiredAt, &agent.RetiredBy, &agent.ReinstateUntil, &agent.PurgeAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkforceAgent{}, agentworkforce.Refuse(agentworkforce.NotFoundCode, "The agent does not exist.")
	}
	if err != nil {
		return WorkforceAgent{}, fmt.Errorf("agent workforce: read the registry row: %w", err)
	}
	var source *string
	if err := q.QueryRow(ctx, `
SELECT id, source FROM elitea_runtime.execution_suspensions
WHERE scope = 'agent' AND project_id = $1 AND scope_key = $2 AND resumed_at IS NULL`,
		projectID, strconv.FormatInt(applicationID, 10)).Scan(&agent.SuspensionID, &source); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return WorkforceAgent{}, fmt.Errorf("agent workforce: read the suspension: %w", err)
	}
	if source != nil {
		agent.SuspensionSource = *source
	}
	if agent.LegalHold, err = legalHold(ctx, q, projectID, applicationID); err != nil {
		return WorkforceAgent{}, err
	}
	if agent.IsHRAgent, err = anyVersionIsHR(ctx, q, projectID, applicationID); err != nil {
		return WorkforceAgent{}, err
	}
	agent.ApplicationName = applicationNames(ctx, q, projectID, []int64{applicationID})[applicationID]
	return agent, nil
}

func workforceRefusal(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return agentworkforce.Refuse(agentworkforce.NotFoundCode, "The agent does not exist.")
	}
	if refusal, ok := agentlifecycle.AsRefusal(err); ok {
		code := agentworkforce.InvalidCode
		if refusal.Code == agentlifecycle.NotFoundCode {
			code = agentworkforce.NotFoundCode
		}
		return agentworkforce.Refuse(code, refusal.Message)
	}
	return err
}

// legalHold reports whether an active legal hold covers the agent or its
// project (shared 0195).
func legalHold(ctx context.Context, q lifecycleQuerier, projectID, applicationID int64) (bool, error) {
	var held bool
	err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_runtime.legal_holds
               WHERE released_at IS NULL AND project_id = $1
                 AND (scope = 'project' OR (scope = 'agent' AND agent_id = $2)))`, projectID, applicationID).Scan(&held)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000") {
			return false, nil
		}
		return false, fmt.Errorf("agent workforce: read the legal holds: %w", err)
	}
	return held, nil
}

// anyVersionIsHR reports whether any version of the agent sets
// capabilities.can_review_agents: an agent that may become an HR agent on
// its next publish is one already.
func anyVersionIsHR(ctx context.Context, q lifecycleQuerier, projectID, applicationID int64) (bool, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return false, nil
	}
	var hr bool
	err = q.QueryRow(ctx, fmt.Sprintf(`
SELECT EXISTS (SELECT 1 FROM %s.application_versions
               WHERE application_id = $1 AND meta -> 'capabilities' -> 'can_review_agents' = 'true'::jsonb)`, schema),
		applicationID).Scan(&hr)
	if err != nil {
		return false, fmt.Errorf("agent workforce: read the agent's capabilities: %w", err)
	}
	return hr, nil
}

// Agent answers the workforce view of one agent.
func (s *AgentWorkforce) Agent(ctx context.Context, projectID, applicationID int64) (WorkforceAgent, error) {
	return readWorkforceAgent(ctx, s.pool, projectID, applicationID, false)
}

// NewWorkforceProposal is a proposal from a person.
type NewWorkforceProposal struct {
	ProjectID     int64
	ApplicationID int64
	Input         agentworkforce.ProposalInput
	Actor         WorkforceActor
}

// ProposeByPerson stores a person's proposal. Only an agent manager or the
// agent's owner may propose.
func (s *AgentWorkforce) ProposeByPerson(ctx context.Context, request NewWorkforceProposal) (WorkforceProposal, error) {
	input, err := agentworkforce.CheckProposal(request.Input)
	if err != nil {
		return WorkforceProposal{}, err
	}
	var created WorkforceProposal
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		agent, err := readWorkforceAgent(ctx, tx, request.ProjectID, request.ApplicationID, true)
		if err != nil {
			return err
		}
		if !request.Actor.Manager && !agent.IsOwner(request.Actor.UserID) {
			return agentworkforce.Refuse(agentworkforce.ForbiddenCode, "Only an agent manager or the agent's owner can propose this.")
		}
		if err := stageAllowedFor(agent, input.Stage); err != nil {
			return err
		}
		created, err = s.insertProposal(ctx, tx, agent, input, insertSource{userID: request.Actor.UserID})
		return err
	})
	return created, err
}

// stageAllowedFor refuses a stage the agent's state makes meaningless.
func stageAllowedFor(agent WorkforceAgent, stage agentworkforce.Stage) error {
	switch agent.State {
	case agentworkforce.StateDeleted:
		return agentworkforce.Refuse(agentworkforce.StateConflictCode, "The agent was deleted.")
	case agentworkforce.StateRetired:
		if stage != agentworkforce.StageDelete {
			return agentworkforce.Refuse(agentworkforce.StateConflictCode, "The agent is retired: reinstate it, or propose deleting it.")
		}
	default:
		if stage == agentworkforce.StageDelete {
			return agentworkforce.Refuse(agentworkforce.StateConflictCode, "Only a retired agent can be deleted. Retire it first.")
		}
	}
	return nil
}

type insertSource struct {
	userID        int64
	hrAgent       int64
	executionID   string
	principalHint string
}

func (s *AgentWorkforce) insertProposal(ctx context.Context, tx pgx.Tx, agent WorkforceAgent, input agentworkforce.ProposalInput, source insertSource) (WorkforceProposal, error) {
	card, err := latestScorecard(ctx, tx, agent.ProjectID, agent.ApplicationID, s.now().UTC())
	if err != nil {
		return WorkforceProposal{}, err
	}
	evidence := map[string]any{"scorecard": card, "risk_tier": agent.Tier, "lifecycle_state": agent.State}
	digest := agentworkforce.EvidenceDigest(evidence)
	encodedEvidence, err := json.Marshal(evidence)
	if err != nil {
		return WorkforceProposal{}, fmt.Errorf("agent workforce: encode the evidence: %w", err)
	}
	narrowing, err := json.Marshal(input.Narrowing)
	if err != nil {
		return WorkforceProposal{}, fmt.Errorf("agent workforce: encode the narrowing: %w", err)
	}
	kind := agentworkforce.SourcePerson
	var execution *string
	if source.hrAgent > 0 {
		kind = agentworkforce.SourceHRAgent
		execution = &source.executionID
	}
	proposal, err := scanWorkforceProposal(tx.QueryRow(ctx, `
INSERT INTO elitea_lifecycle.agent_workforce_proposals AS p
    (project_id, application_id, kind, stage, source, proposed_by_user_id, proposer_application_id, execution_id,
     rationale, narrowing, evidence, evidence_digest)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12)
RETURNING `+workforceProposalColumns,
		agent.ProjectID, agent.ApplicationID, input.Stage.Kind(), string(input.Stage), kind,
		nullablePositive(source.userID), nullablePositive(source.hrAgent), execution, input.Rationale,
		string(narrowing), string(encodedEvidence), digest))
	if isUniqueViolation(err, "agent_workforce_proposals_one_open") {
		return WorkforceProposal{}, agentworkforce.Refuse(agentworkforce.OpenProposalCode,
			"The agent already has an open proposal. Decide it first.")
	}
	if err != nil {
		return WorkforceProposal{}, fmt.Errorf("agent workforce: store the proposal: %w", err)
	}
	principal := source.principalHint
	if principal == "" {
		principal = personRef(source.userID)
	}
	if err := s.append(ctx, tx, agent.ProjectID, "agent.workforce.proposed", principal, proposalRef(proposal.ID),
		source.executionID, "stage:"+string(input.Stage), "evidence:"+digest); err != nil {
		return WorkforceProposal{}, err
	}
	return proposal, nil
}

// Decision is a person's decision on an open proposal.
type Decision struct {
	ProjectID  int64
	ProposalID int64
	Actor      WorkforceActor
	Note       string
	// ConfirmNested confirms retiring an agent that other agents nest.
	ConfirmNested bool
}

// decide checks that the actor may decide the proposal: an agent manager, or
// the owner of the agent the proposal is about.
func mayDecide(actor WorkforceActor, agent WorkforceAgent) error {
	if actor.UserID <= 0 {
		return agentworkforce.Refuse(agentworkforce.ForbiddenCode, "A decision needs a signed-in person.")
	}
	if !actor.Manager && !agent.IsOwner(actor.UserID) {
		return agentworkforce.Refuse(agentworkforce.ForbiddenCode, "Only an agent manager or the agent's owner decides this.")
	}
	return nil
}

// Approve applies the proposal's stage, in the decision's transaction.
func (s *AgentWorkforce) Approve(ctx context.Context, decision Decision) (WorkforceProposal, error) {
	note, err := agentworkforce.CheckNote(decision.Note, agentworkforce.MaxNoteBytes)
	if err != nil {
		return WorkforceProposal{}, err
	}
	var (
		decided  WorkforceProposal
		affected []int64
	)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		proposal, err := readWorkforceProposal(ctx, tx, decision.ProjectID, decision.ProposalID, true)
		if err != nil {
			return err
		}
		if proposal.Status != agentworkforce.StatusProposed && proposal.Status != agentworkforce.StatusAskedOwner {
			return agentworkforce.Refuse(agentworkforce.StateConflictCode, "The proposal was already decided.")
		}
		agent, err := readWorkforceAgent(ctx, tx, decision.ProjectID, proposal.ApplicationID, true)
		if err != nil {
			return err
		}
		if err := mayDecide(decision.Actor, agent); err != nil {
			return err
		}
		stage := agentworkforce.Stage(proposal.Stage)
		if err := stageAllowedFor(agent, stage); err != nil {
			return err
		}
		applied, suspensions, err := s.applyStage(ctx, tx, agent, stage, proposal, decision)
		if err != nil {
			return err
		}
		affected = suspensions
		encoded, err := json.Marshal(applied)
		if err != nil {
			return fmt.Errorf("agent workforce: encode the applied record: %w", err)
		}
		decided, err = scanWorkforceProposal(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals AS p
SET status = 'approved', decided_by_user_id = $3, decided_at = clock_timestamp(), decision_note = NULLIF($4, ''),
    applied = $5::jsonb, updated_at = clock_timestamp()
WHERE p.project_id = $1 AND p.id = $2
RETURNING `+workforceProposalColumns, decision.ProjectID, proposal.ID, decision.Actor.UserID, note, string(encoded)))
		if err != nil {
			return fmt.Errorf("agent workforce: record the decision: %w", err)
		}
		return s.append(ctx, tx, decision.ProjectID, "agent.workforce.approved", personRef(decision.Actor.UserID),
			proposalRef(proposal.ID), "", "stage:"+proposal.Stage, "evidence:"+proposal.EvidenceDigest,
			"agent:"+strconv.FormatInt(proposal.ApplicationID, 10))
	})
	if err != nil {
		return WorkforceProposal{}, err
	}
	s.notify(affected...)
	s.notifyOwners(ctx, decided, "approved")
	return decided, nil
}

// Reject keeps the agent.
func (s *AgentWorkforce) Reject(ctx context.Context, decision Decision) (WorkforceProposal, error) {
	return s.close(ctx, decision, agentworkforce.StatusRejected, "agent.workforce.rejected")
}

// AskOwner routes an open proposal to the owner for comment.
func (s *AgentWorkforce) AskOwner(ctx context.Context, decision Decision) (WorkforceProposal, error) {
	var updated WorkforceProposal
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		proposal, err := readWorkforceProposal(ctx, tx, decision.ProjectID, decision.ProposalID, true)
		if err != nil {
			return err
		}
		if proposal.Status != agentworkforce.StatusProposed {
			return agentworkforce.Refuse(agentworkforce.StateConflictCode, "Only a new proposal can be routed to the owner.")
		}
		if !decision.Actor.Manager {
			return agentworkforce.Refuse(agentworkforce.ForbiddenCode, "Only an agent manager routes a proposal to the owner.")
		}
		updated, err = scanWorkforceProposal(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals AS p
SET status = 'asked_owner', updated_at = clock_timestamp()
WHERE p.project_id = $1 AND p.id = $2
RETURNING `+workforceProposalColumns, decision.ProjectID, proposal.ID))
		if err != nil {
			return fmt.Errorf("agent workforce: route the proposal: %w", err)
		}
		return s.append(ctx, tx, decision.ProjectID, "agent.workforce.asked_owner", personRef(decision.Actor.UserID),
			proposalRef(proposal.ID), "", "stage:"+proposal.Stage)
	})
	if err != nil {
		return WorkforceProposal{}, err
	}
	s.notifyOwners(ctx, updated, "asked_owner")
	return updated, nil
}

// OwnerComment records the owner's comment on a proposal routed to them.
func (s *AgentWorkforce) OwnerComment(ctx context.Context, projectID, proposalID, userID int64, comment string) (WorkforceProposal, error) {
	comment, err := agentworkforce.CheckNote(comment, agentworkforce.MaxRationaleBytes)
	if err != nil || comment == "" {
		return WorkforceProposal{}, agentworkforce.Refuse(agentworkforce.InvalidCode, "A comment of at most 4096 bytes is required.")
	}
	var updated WorkforceProposal
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		proposal, err := readWorkforceProposal(ctx, tx, projectID, proposalID, true)
		if err != nil {
			return err
		}
		if proposal.Status != agentworkforce.StatusAskedOwner {
			return agentworkforce.Refuse(agentworkforce.StateConflictCode, "The proposal is not waiting for the owner.")
		}
		agent, err := readWorkforceAgent(ctx, tx, projectID, proposal.ApplicationID, false)
		if err != nil {
			return err
		}
		if !agent.IsOwner(userID) {
			return agentworkforce.Refuse(agentworkforce.ForbiddenCode, "Only the agent's owner comments here.")
		}
		updated, err = scanWorkforceProposal(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals AS p
SET owner_comment = $3, updated_at = clock_timestamp()
WHERE p.project_id = $1 AND p.id = $2
RETURNING `+workforceProposalColumns, projectID, proposal.ID, comment))
		if err != nil {
			return fmt.Errorf("agent workforce: record the comment: %w", err)
		}
		return s.append(ctx, tx, projectID, "agent.workforce.owner_commented", personRef(userID), proposalRef(proposal.ID), "")
	})
	return updated, err
}

func (s *AgentWorkforce) close(ctx context.Context, decision Decision, status, action string) (WorkforceProposal, error) {
	note, err := agentworkforce.CheckNote(decision.Note, agentworkforce.MaxNoteBytes)
	if err != nil {
		return WorkforceProposal{}, err
	}
	var updated WorkforceProposal
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		proposal, err := readWorkforceProposal(ctx, tx, decision.ProjectID, decision.ProposalID, true)
		if err != nil {
			return err
		}
		if proposal.Status != agentworkforce.StatusProposed && proposal.Status != agentworkforce.StatusAskedOwner {
			return agentworkforce.Refuse(agentworkforce.StateConflictCode, "The proposal was already decided.")
		}
		agent, err := readWorkforceAgent(ctx, tx, decision.ProjectID, proposal.ApplicationID, false)
		if err != nil {
			return err
		}
		if err := mayDecide(decision.Actor, agent); err != nil {
			return err
		}
		updated, err = scanWorkforceProposal(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals AS p
SET status = $3, decided_by_user_id = $4, decided_at = clock_timestamp(), decision_note = NULLIF($5, ''),
    updated_at = clock_timestamp()
WHERE p.project_id = $1 AND p.id = $2
RETURNING `+workforceProposalColumns, decision.ProjectID, proposal.ID, status, decision.Actor.UserID, note))
		if err != nil {
			return fmt.Errorf("agent workforce: record the decision: %w", err)
		}
		return s.append(ctx, tx, decision.ProjectID, action, personRef(decision.Actor.UserID), proposalRef(proposal.ID), "",
			"stage:"+proposal.Stage, "evidence:"+proposal.EvidenceDigest)
	})
	return updated, err
}

// Appeal is the owner's appeal of an approved proposal, within the
// reinstate window of the decision.
func (s *AgentWorkforce) Appeal(ctx context.Context, projectID, proposalID, userID int64, reason string) (WorkforceProposal, error) {
	reason, err := agentworkforce.CheckNote(reason, agentworkforce.MaxRationaleBytes)
	if err != nil || reason == "" {
		return WorkforceProposal{}, agentworkforce.Refuse(agentworkforce.InvalidCode, "An appeal says why, in at most 4096 bytes.")
	}
	var updated WorkforceProposal
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		proposal, err := readWorkforceProposal(ctx, tx, projectID, proposalID, true)
		if err != nil {
			return err
		}
		agent, err := readWorkforceAgent(ctx, tx, projectID, proposal.ApplicationID, false)
		if err != nil {
			return err
		}
		if !agent.IsOwner(userID) {
			return agentworkforce.Refuse(agentworkforce.ForbiddenCode, "Only the agent's owner can appeal.")
		}
		if proposal.Status != agentworkforce.StatusApproved || proposal.AppealState != nil {
			return agentworkforce.Refuse(agentworkforce.AppealClosedCode, "Only an approved decision can be appealed, once.")
		}
		if proposal.Stage == string(agentworkforce.StageDelete) {
			return agentworkforce.Refuse(agentworkforce.AppealClosedCode, "A deletion cannot be appealed.")
		}
		if proposal.DecidedAt == nil || s.now().After(proposal.DecidedAt.Add(agentworkforce.ReinstateWindow)) {
			return agentworkforce.Refuse(agentworkforce.AppealClosedCode, "The 30-day appeal window has closed.")
		}
		updated, err = scanWorkforceProposal(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals AS p
SET appeal_state = 'open', appeal_reason = $3, appealed_by_user_id = $4, appealed_at = clock_timestamp(),
    updated_at = clock_timestamp()
WHERE p.project_id = $1 AND p.id = $2
RETURNING `+workforceProposalColumns, projectID, proposal.ID, reason, userID))
		if err != nil {
			return fmt.Errorf("agent workforce: record the appeal: %w", err)
		}
		return s.append(ctx, tx, projectID, "agent.workforce.appealed", personRef(userID), proposalRef(proposal.ID), "",
			"stage:"+proposal.Stage)
	})
	if err != nil {
		return WorkforceProposal{}, err
	}
	s.notifyOwners(ctx, updated, "appealed")
	return updated, nil
}

// AppealDecision is an agent manager's review of an open appeal. The
// reviewer differs from the manager who decided (in code and in the CHECK).
type AppealDecision struct {
	ProjectID  int64
	ProposalID int64
	Actor      WorkforceActor
	Grant      bool
	Note       string
}

// DecideAppeal grants or upholds an open appeal. Granting reverses the
// stage: reinstates a retired agent, resumes a dismissal suspension, clears
// the watch flag. A demotion is a change; granting its appeal records the
// decision and leaves restoring the tools to a new change by the owner.
func (s *AgentWorkforce) DecideAppeal(ctx context.Context, decision AppealDecision) (WorkforceProposal, error) {
	note, err := agentworkforce.CheckNote(decision.Note, agentworkforce.MaxNoteBytes)
	if err != nil {
		return WorkforceProposal{}, err
	}
	var (
		updated WorkforceProposal
		resumed []int64
	)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		proposal, err := readWorkforceProposal(ctx, tx, decision.ProjectID, decision.ProposalID, true)
		if err != nil {
			return err
		}
		if proposal.AppealState == nil || *proposal.AppealState != agentworkforce.AppealOpen {
			return agentworkforce.Refuse(agentworkforce.AppealClosedCode, "There is no open appeal on this proposal.")
		}
		if !decision.Actor.Manager {
			return agentworkforce.Refuse(agentworkforce.ForbiddenCode, "Only an agent manager reviews an appeal.")
		}
		if proposal.DecidedByUserID != nil && *proposal.DecidedByUserID == decision.Actor.UserID {
			return agentworkforce.Refuse(agentworkforce.ForbiddenCode, "An appeal is reviewed by an agent manager other than the one who decided.")
		}
		agent, err := readWorkforceAgent(ctx, tx, decision.ProjectID, proposal.ApplicationID, true)
		if err != nil {
			return err
		}
		if agent.IsOwner(decision.Actor.UserID) {
			return agentworkforce.Refuse(agentworkforce.ForbiddenCode, "The owner cannot review their own appeal.")
		}
		state := agentworkforce.AppealUpheld
		if decision.Grant {
			state = agentworkforce.AppealGranted
			if resumed, err = s.reverseStage(ctx, tx, agent, proposal, decision.Actor.UserID); err != nil {
				return err
			}
		}
		updated, err = scanWorkforceProposal(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.agent_workforce_proposals AS p
SET appeal_state = $3, appeal_decided_by_user_id = $4, appeal_decided_at = clock_timestamp(),
    appeal_note = NULLIF($5, ''), updated_at = clock_timestamp()
WHERE p.project_id = $1 AND p.id = $2
RETURNING `+workforceProposalColumns, decision.ProjectID, proposal.ID, state, decision.Actor.UserID, note))
		if err != nil {
			return fmt.Errorf("agent workforce: record the appeal decision: %w", err)
		}
		return s.append(ctx, tx, decision.ProjectID, "agent.workforce.appeal_"+state, personRef(decision.Actor.UserID),
			proposalRef(proposal.ID), "", "stage:"+proposal.Stage, "evidence:"+proposal.EvidenceDigest)
	})
	if err != nil {
		return WorkforceProposal{}, err
	}
	s.notify(resumed...)
	s.notifyOwners(ctx, updated, "appeal_decided")
	return updated, nil
}

// reverseStage undoes what a stage did, for a granted appeal.
func (s *AgentWorkforce) reverseStage(ctx context.Context, tx pgx.Tx, agent WorkforceAgent, proposal WorkforceProposal, actorID int64) ([]int64, error) {
	switch agentworkforce.Stage(proposal.Stage) {
	case agentworkforce.StageWatch:
		return nil, clearWatch(ctx, tx, agent)
	case agentworkforce.StageSuspend:
		return resumeDismissalSuspension(ctx, tx, agent, actorID, "Appeal granted.")
	case agentworkforce.StageRetire:
		if agent.State != agentworkforce.StateRetired {
			return nil, nil
		}
		return s.reinstate(ctx, tx, agent, actorID, "appeal")
	}
	return nil, nil
}

func (s *AgentWorkforce) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("agent workforce: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("agent workforce: commit: %w", err)
	}
	return nil
}

func personRef(userID int64) string {
	if userID <= 0 {
		return "system"
	}
	return "user:" + strconv.FormatInt(userID, 10)
}

func proposalRef(id int64) string { return strconv.FormatInt(id, 10) }

func (s *AgentWorkforce) append(ctx context.Context, tx pgx.Tx, projectID int64, action, principal, resource, executionID string, reasons ...string) error {
	if reasons == nil {
		reasons = []string{}
	}
	event := securityaudit.SecurityEvent{
		Type: securityaudit.TypeAgentWorkforceChanged, TenantProjectID: projectID, Action: action,
		Reasons: reasons, PrincipalRef: principal, ResourceType: "agent_workforce", ResourceID: resource,
		ExecutionID: executionID, Time: time.Now().UTC(),
	}
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("agent workforce: append the audit event %s: %w", action, err)
	}
	return nil
}

// notifyOwners tells the agent's owners about a change. Best effort.
func (s *AgentWorkforce) notifyOwners(ctx context.Context, proposal WorkforceProposal, what string) {
	agent, err := readWorkforceAgent(ctx, s.pool, proposal.ProjectID, proposal.ApplicationID, false)
	if err != nil {
		return
	}
	seen := map[int64]bool{}
	for _, id := range []int64{agent.BusinessOwnerID, agent.TechnicalOwnerID} {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		meta := map[string]any{
			"message": fmt.Sprintf("Workforce: the %s proposal for agent %q was %s.", proposal.Stage, agent.ApplicationName,
				what),
			"project_id": proposal.ProjectID, "application_id": proposal.ApplicationID, "proposal_id": proposal.ID,
			"stage": proposal.Stage, "event": what,
		}
		key := fmt.Sprintf("workforce-%d-%s-%d", proposal.ID, what, id)
		_ = insertReviewNotification(ctx, s.pool, key, proposal.ProjectID, id, WorkforceNotificationEventType, meta)
	}
}
