package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentproposal"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// AgentProposals stores elitea_lifecycle.agent_proposals (shared/0234,
// ADR-0050 decision 5a, #1615): the propose_agent tool writes a proposal
// (storage.ProposeAgentSink), the registry inbox lists and decides them, and
// Copy to project copies chosen memory entries between two agents. Every
// change is a security audit event in the change's transaction
// (TypeAgentProposalChanged).
type AgentProposals struct {
	pool  *pgxpool.Pool
	audit securityaudit.Appender
	now   func() time.Time
}

var _ storage.ProposeAgentSink = (*AgentProposals)(nil)

// ErrProposalNotFound is a proposal that is not in the project.
var ErrProposalNotFound = errors.New("agent proposal not found")

// ErrProposalDecided is a decision on a proposal that is no longer open.
var ErrProposalDecided = errors.New("agent proposal already decided")

// NewAgentProposals builds the store. A nil appender is refused.
func NewAgentProposals(pool *pgxpool.Pool, audit securityaudit.Appender) (*AgentProposals, error) {
	if pool == nil || audit == nil {
		return nil, errors.New("agent proposals: pool and audit appender are required")
	}
	return &AgentProposals{pool: pool, audit: audit, now: time.Now}, nil
}

// ComposeAgentProposals builds the store with the platform appender.
func ComposeAgentProposals(pool *pgxpool.Pool) (*AgentProposals, error) {
	appender, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, err
	}
	return NewAgentProposals(pool, appender)
}

// WithClock sets the clock the suppression window is measured on (tests).
func (s *AgentProposals) WithClock(now func() time.Time) *AgentProposals {
	if now != nil {
		s.now = now
	}
	return s
}

// proposeRunSQL reads what a propose_agent call needs off the claimed run:
// its agent, actor, conversation and frozen input (for the version's
// capabilities).
const proposeRunSQL = `
SELECT COALESCE(a.application_id, 0), j.actor_id, COALESCE(a.client_stream_id, ''), e.content_bytes
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = j.input_bundle_id AND e.entry_id = a.request_entry_id
 AND e.semantic_role = '` + executiondomain.AgentExecutionRequestRole + `'
WHERE j.execution_id = $1 AND j.generation = $2 AND j.resource_project_id = $3`

// ResolveProposeAgentRun reads the claimed run. The caller has authorized
// the claim; projectID is the project it resolved.
func (s *AgentProposals) ResolveProposeAgentRun(ctx context.Context, projectID int64, executionID string, generation uint64) (storage.ProposeAgentRun, error) {
	if generation == 0 || generation > 1<<63-1 {
		return storage.ProposeAgentRun{}, storage.ErrContentUnauthorized
	}
	var (
		applicationID int64
		actor, stream string
		input         []byte
	)
	err := s.pool.QueryRow(ctx, proposeRunSQL, executionID, int64(generation), projectID).
		Scan(&applicationID, &actor, &stream, &input)
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.ProposeAgentRun{}, storage.ErrContentUnauthorized
	}
	if err != nil {
		return storage.ProposeAgentRun{}, fmt.Errorf("agent proposals: read the run: %w", err)
	}
	actorID, err := strconv.ParseInt(actor, 10, 64)
	if err != nil || actorID <= 0 {
		return storage.ProposeAgentRun{}, storage.ErrContentRejected
	}
	run := storage.ProposeAgentRun{
		ProjectID: projectID, ExecutionID: executionID, ApplicationID: applicationID, ActorID: actorID,
	}
	if conversation, err := uuid.Parse(stream); err == nil {
		run.ConversationUUID = conversation.String()
	}
	meta, err := frozenVersionMeta(input)
	if err != nil {
		return storage.ProposeAgentRun{}, storage.ErrContentRejected
	}
	run.CanPropose = agentproposal.CanPropose(meta)
	return run, nil
}

// frozenVersionMeta reads version_details.meta of the frozen input.
func frozenVersionMeta(encoded []byte) (map[string]any, error) {
	var input runtimev1.AgentExecutionInputV1
	if len(encoded) == 0 || len(encoded) > executiondomain.MaxAgentExecutionInputBytes || proto.Unmarshal(encoded, &input) != nil {
		return nil, errors.New("agent proposals: the frozen input is not readable")
	}
	application := input.GetApplication()
	if len(application) == 0 {
		return nil, nil
	}
	var decoded struct {
		VersionDetails struct {
			Meta map[string]any `json:"meta"`
		} `json:"version_details"`
	}
	if err := json.Unmarshal(application, &decoded); err != nil {
		return nil, err
	}
	return decoded.VersionDetails.Meta, nil
}

// lockProposer serialises the proposals of one agent: the limit counts rows
// the other writers add.
func lockProposer(ctx context.Context, tx pgx.Tx, projectID, applicationID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`,
		fmt.Sprintf("elitea_agent_proposals:%d:%d", projectID, applicationID))
	return err
}

// Propose stores one proposal, unless the spawn is already proposed, the
// agent has MaxOpenPerAgent open ones, or a person dismissed the same spec
// less than SuppressFor ago.
func (s *AgentProposals) Propose(ctx context.Context, run storage.ProposeAgentRun, write storage.ProposalWrite) (storage.ProposalOutcome, error) {
	draft, err := json.Marshal(write.Draft)
	if err != nil {
		return storage.ProposalOutcome{}, fmt.Errorf("agent proposals: encode the draft: %w", err)
	}
	seeds := write.Request.MemorySeeds
	if seeds == nil {
		seeds = []agentproposal.Seed{}
	}
	encodedSeeds, err := json.Marshal(seeds)
	if err != nil {
		return storage.ProposalOutcome{}, fmt.Errorf("agent proposals: encode the seeds: %w", err)
	}
	var conversation *string
	if run.ConversationUUID != "" {
		conversation = &run.ConversationUUID
	}
	var outcome storage.ProposalOutcome
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockProposer(ctx, tx, run.ProjectID, run.ApplicationID); err != nil {
			return err
		}
		var existing int64
		err := tx.QueryRow(ctx, `
SELECT id FROM elitea_lifecycle.agent_proposals
WHERE project_id = $1 AND execution_id = $2 AND ephemeral_agent_id = $3`,
			run.ProjectID, run.ExecutionID, write.Request.EphemeralAgentID).Scan(&existing)
		if err == nil {
			outcome = storage.ProposalOutcome{Status: agentproposal.OutcomeDuplicate, ProposalID: existing}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var open int
		var suppressed bool
		if err := tx.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE status = 'proposed'),
       COALESCE(bool_or(status = 'dismissed' AND spec_digest = $3 AND decided_at > $4), false)
FROM elitea_lifecycle.agent_proposals
WHERE project_id = $1 AND proposer_application_id = $2`,
			run.ProjectID, run.ApplicationID, write.SpecDigest, s.now().UTC().Add(-agentproposal.SuppressFor)).
			Scan(&open, &suppressed); err != nil {
			return err
		}
		if suppressed {
			outcome = storage.ProposalOutcome{Status: agentproposal.OutcomeSuppressed}
			return nil
		}
		if open >= agentproposal.MaxOpenPerAgent {
			outcome = storage.ProposalOutcome{Status: agentproposal.OutcomeLimit}
			return nil
		}
		var id int64
		if err := tx.QueryRow(ctx, `
INSERT INTO elitea_lifecycle.agent_proposals
    (project_id, proposer_application_id, proposed_for_user_id, execution_id, conversation_uuid,
     ephemeral_agent_id, spec_digest, rationale, draft, memory_seeds)
VALUES ($1, $2, $3, $4, $5::uuid, $6, $7, $8, $9::jsonb, $10::jsonb)
RETURNING id`,
			run.ProjectID, run.ApplicationID, run.ActorID, run.ExecutionID, conversation,
			write.Request.EphemeralAgentID, write.SpecDigest, write.Request.Rationale, string(draft), string(encodedSeeds)).
			Scan(&id); err != nil {
			return err
		}
		outcome = storage.ProposalOutcome{Status: agentproposal.OutcomeProposed, ProposalID: id}
		return s.append(ctx, tx, run.ProjectID, "agent.proposal.proposed", "agent:"+strconv.FormatInt(run.ApplicationID, 10),
			strconv.FormatInt(id, 10), run.ExecutionID)
	})
	if err != nil {
		return storage.ProposalOutcome{}, fmt.Errorf("agent proposals: propose: %w", err)
	}
	return outcome, nil
}

func (s *AgentProposals) append(ctx context.Context, tx pgx.Tx, projectID int64, action, principal, resource, executionID string) error {
	event := securityaudit.SecurityEvent{
		Type: securityaudit.TypeAgentProposalChanged, TenantProjectID: projectID, Action: action,
		Reasons: []string{}, PrincipalRef: principal, ResourceType: "agent_proposal", ResourceID: resource,
		ExecutionID: executionID, Time: time.Now().UTC(),
	}
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("agent proposals: append the audit event %s: %w", action, err)
	}
	return nil
}

// AgentProposal is one proposal as the inbox and the review form read it.
type AgentProposal struct {
	ID                    int64                        `json:"id"`
	ProjectID             int64                        `json:"project_id"`
	ProposerApplicationID int64                        `json:"proposer_application_id"`
	ProposerName          string                       `json:"proposer_name"`
	ProposedForUserID     int64                        `json:"proposed_for_user_id"`
	ExecutionID           string                       `json:"execution_id"`
	ConversationUUID      *string                      `json:"conversation_uuid"`
	EphemeralAgentID      string                       `json:"ephemeral_agent_id"`
	SpecDigest            string                       `json:"spec_digest"`
	Rationale             string                       `json:"rationale"`
	Draft                 map[string]any               `json:"draft"`
	MemorySeeds           []agentproposal.Seed         `json:"memory_seeds"`
	ToolCategories        []agentproposal.ToolCategory `json:"tool_categories"`
	Status                string                       `json:"status"`
	DecidedByUserID       *int64                       `json:"decided_by_user_id"`
	DecidedAt             *time.Time                   `json:"decided_at"`
	CreatedApplicationID  *int64                       `json:"created_application_id"`
	CreatedAt             time.Time                    `json:"created_at"`
	// Evidence is the spawn's record, when the run reported one with the
	// same profile digest.
	Evidence *AgentProposalEvidence `json:"evidence"`
}

// AgentProposalEvidence is the promoted spawn's ephemeral_agents row: how
// it ended and what it spent. Never its soul, brief text or task.
type AgentProposalEvidence struct {
	Status       string     `json:"status"`
	Steps        int64      `json:"steps"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
	ErrorCode    *string    `json:"error_code"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
}

// AgentProposalFilter narrows a list.
type AgentProposalFilter struct {
	// Status is empty for open proposals only.
	Status           string
	ConversationUUID string
	Limit, Offset    int
}

const proposalColumns = `
p.id, p.project_id, p.proposer_application_id, p.proposed_for_user_id, p.execution_id,
p.conversation_uuid::text, p.ephemeral_agent_id, p.spec_digest, p.rationale, p.draft, p.memory_seeds,
p.status, p.decided_by_user_id, p.decided_at, p.created_application_id, p.created_at,
e.status, e.steps, e.input_tokens, e.output_tokens, e.error_code, e.started_at, e.finished_at`

const proposalEvidenceJoin = `
LEFT JOIN elitea_runtime.ephemeral_agents AS e
  ON e.execution_id = p.execution_id AND e.agent_id = p.ephemeral_agent_id
 AND e.project_id = p.project_id AND e.profile_digest = p.spec_digest`

func scanProposal(row pgx.Row) (AgentProposal, error) {
	var (
		proposal                  AgentProposal
		draft, seeds              []byte
		evidenceStatus, errorCode *string
		steps                     *int64
		inputTokens, outputTokens *int64
		startedAt, finishedAt     *time.Time
		decidedBy, createdApp     *int64
		decidedAt                 *time.Time
		conversationUUID          *string
	)
	if err := row.Scan(&proposal.ID, &proposal.ProjectID, &proposal.ProposerApplicationID, &proposal.ProposedForUserID,
		&proposal.ExecutionID, &conversationUUID, &proposal.EphemeralAgentID, &proposal.SpecDigest, &proposal.Rationale,
		&draft, &seeds, &proposal.Status, &decidedBy, &decidedAt, &createdApp, &proposal.CreatedAt,
		&evidenceStatus, &steps, &inputTokens, &outputTokens, &errorCode, &startedAt, &finishedAt); err != nil {
		return AgentProposal{}, err
	}
	proposal.ConversationUUID, proposal.DecidedByUserID, proposal.DecidedAt, proposal.CreatedApplicationID =
		conversationUUID, decidedBy, decidedAt, createdApp
	if err := json.Unmarshal(draft, &proposal.Draft); err != nil {
		return AgentProposal{}, fmt.Errorf("agent proposals: decode the draft: %w", err)
	}
	if err := json.Unmarshal(seeds, &proposal.MemorySeeds); err != nil {
		return AgentProposal{}, fmt.Errorf("agent proposals: decode the seeds: %w", err)
	}
	if proposal.MemorySeeds == nil {
		proposal.MemorySeeds = []agentproposal.Seed{}
	}
	proposal.ToolCategories = draftToolCategories(proposal.Draft)
	if evidenceStatus != nil && startedAt != nil {
		proposal.Evidence = &AgentProposalEvidence{
			Status: *evidenceStatus, StartedAt: *startedAt, FinishedAt: finishedAt, ErrorCode: errorCode,
		}
		if steps != nil {
			proposal.Evidence.Steps = *steps
		}
		if inputTokens != nil {
			proposal.Evidence.InputTokens = *inputTokens
		}
		if outputTokens != nil {
			proposal.Evidence.OutputTokens = *outputTokens
		}
	}
	return proposal, nil
}

// draftToolCategories reads the categories stored with the draft's tools.
func draftToolCategories(draft map[string]any) []agentproposal.ToolCategory {
	out := []agentproposal.ToolCategory{}
	tools, _ := draft["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		key, _ := tool["key"].(string)
		if key == "" {
			continue
		}
		toolkitType, _ := tool["toolkit_type"].(string)
		category, _ := tool["category"].(string)
		out = append(out, agentproposal.ToolCategory{Key: key, ToolkitType: toolkitType, Category: category})
	}
	return out
}

// ListProposals lists a project's proposals, newest first, with the
// proposing agent's name, and the total.
func (s *AgentProposals) ListProposals(ctx context.Context, projectID int64, filter AgentProposalFilter) ([]AgentProposal, int, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return nil, 0, err
	}
	status := filter.Status
	if status == "" {
		status = string(agentproposal.StatusProposed)
	}
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var conversation *string
	if filter.ConversationUUID != "" {
		conversation = &filter.ConversationUUID
	}
	var total int
	if err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_lifecycle.agent_proposals
WHERE project_id = $1 AND ($2 = 'all' OR status = $2)
  AND ($3::uuid IS NULL OR conversation_uuid = $3::uuid)`, projectID, status, conversation).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("agent proposals: count: %w", err)
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT %[2]s, COALESCE(app.name, '')
FROM elitea_lifecycle.agent_proposals AS p
%[3]s
LEFT JOIN %[1]s.applications AS app ON app.id = p.proposer_application_id
WHERE p.project_id = $1 AND ($2 = 'all' OR p.status = $2)
  AND ($3::uuid IS NULL OR p.conversation_uuid = $3::uuid)
ORDER BY p.created_at DESC, p.id DESC
LIMIT $4 OFFSET $5`, schema, proposalColumns, proposalEvidenceJoin), projectID, status, conversation, limit, max(filter.Offset, 0))
	if err != nil {
		return nil, 0, fmt.Errorf("agent proposals: list: %w", err)
	}
	defer rows.Close()
	out := []AgentProposal{}
	for rows.Next() {
		proposal, err := scanProposalWithName(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, proposal)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("agent proposals: list: %w", err)
	}
	return out, total, nil
}

// namedRow appends the proposer's name to a proposal row.
type namedRow struct {
	row  pgx.Row
	name *string
}

func (r namedRow) Scan(dest ...any) error { return r.row.Scan(append(dest, r.name)...) }

func scanProposalWithName(row pgx.Row) (AgentProposal, error) {
	var name string
	proposal, err := scanProposal(namedRow{row: row, name: &name})
	if err != nil {
		return AgentProposal{}, fmt.Errorf("agent proposals: read a row: %w", err)
	}
	proposal.ProposerName = name
	return proposal, nil
}

// GetProposal reads one proposal of the project.
func (s *AgentProposals) GetProposal(ctx context.Context, projectID, proposalID int64) (AgentProposal, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return AgentProposal{}, err
	}
	proposal, err := scanProposalWithName(s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT %[2]s, COALESCE(app.name, '')
FROM elitea_lifecycle.agent_proposals AS p
%[3]s
LEFT JOIN %[1]s.applications AS app ON app.id = p.proposer_application_id
WHERE p.project_id = $1 AND p.id = $2`, schema, proposalColumns, proposalEvidenceJoin), projectID, proposalID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentProposal{}, ErrProposalNotFound
	}
	return proposal, err
}

// DismissProposal dismisses an open proposal. Its spec is suppressed for
// the proposing agent for SuppressFor from now.
func (s *AgentProposals) DismissProposal(ctx context.Context, projectID, proposalID, userID int64) error {
	return s.decide(ctx, projectID, proposalID, userID, func(tx pgx.Tx) (pgconn.CommandTag, error) {
		return tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_proposals
SET status = 'dismissed', decided_by_user_id = $3, decided_at = $4, updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2 AND status = 'proposed'`, projectID, proposalID, userID, s.now().UTC())
	}, "agent.proposal.dismissed")
}

// MarkProposalCreated records the agent a person created from an open
// proposal. The application must exist in the project's schema: the create
// route made it, and this only links it. The new agent's registry row is
// the create route's own (tier low); nothing here sets a tier.
func (s *AgentProposals) MarkProposalCreated(ctx context.Context, projectID, proposalID, userID, applicationID int64) error {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return err
	}
	return s.decide(ctx, projectID, proposalID, userID, func(tx pgx.Tx) (pgconn.CommandTag, error) {
		var exists bool
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.applications WHERE id = $1)`, schema),
			applicationID).Scan(&exists); err != nil {
			return pgconn.CommandTag{}, err
		}
		if !exists {
			return pgconn.CommandTag{}, ErrProposalNotFound
		}
		return tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_proposals
SET status = 'created', decided_by_user_id = $3, decided_at = $4, created_application_id = $5,
    updated_at = clock_timestamp()
WHERE project_id = $1 AND id = $2 AND status = 'proposed'`, projectID, proposalID, userID, s.now().UTC(), applicationID)
	}, "agent.proposal.created")
}

func (s *AgentProposals) decide(ctx context.Context, projectID, proposalID, userID int64, update func(pgx.Tx) (pgconn.CommandTag, error), action string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `
SELECT status FROM elitea_lifecycle.agent_proposals WHERE project_id = $1 AND id = $2 FOR UPDATE`,
			projectID, proposalID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProposalNotFound
		}
		if err != nil {
			return err
		}
		if status != string(agentproposal.StatusProposed) {
			return ErrProposalDecided
		}
		tag, err := update(tx)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrProposalDecided
		}
		return s.append(ctx, tx, projectID, action, "user:"+strconv.FormatInt(userID, 10), strconv.FormatInt(proposalID, 10), "")
	})
}

// CopiedMemory is one memory entry Copy to project copies.
type CopiedMemory struct {
	Kind    string
	Content string
	Pinned  bool
}

// SeedAgentMemory writes entries into an agent's memory as a person's
// entries: active, written_by person, no conversation and no subject. It
// serves the review form's accepted seeds and Copy to project's chosen
// entries; both are a person's decision, so nothing waits for review. The
// agent memory limit applies as for any write.
func (s *AgentProposals) SeedAgentMemory(ctx context.Context, projectID, applicationID, userID int64, entries []CopiedMemory, action, resource string) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return 0, err
	}
	written := 0
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.applications WHERE id = $1)`, schema),
			applicationID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrProposalNotFound
		}
		if err := lockAgent(ctx, tx, projectID, applicationID); err != nil {
			return err
		}
		for _, entry := range entries {
			kind := agentmemory.Kind(entry.Kind)
			if !kind.AgentWritable() {
				continue
			}
			content, err := agentmemory.CheckContent(entry.Content)
			if err != nil {
				continue
			}
			var id int64
			if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.agent_memory_entries
    (application_id, kind, content, pinned, status, written_by_kind, written_by_user_id)
VALUES ($1, $2, $3, $4, 'active', 'person', $5)
RETURNING id`, schema), applicationID, string(kind), content, entry.Pinned, userID).Scan(&id); err != nil {
				return err
			}
			if err := recordVersion(ctx, tx, schema, id); err != nil {
				return err
			}
			written++
		}
		if written == 0 {
			return nil
		}
		return s.append(ctx, tx, projectID, action, "user:"+strconv.FormatInt(userID, 10), resource, "")
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// MemoryEntriesForCopy reads the chosen ACTIVE entries of an agent that are
// about nobody: an entry about a person stays with that person and the
// agent it was written for. Entries not found are left out.
func (s *AgentProposals) MemoryEntriesForCopy(ctx context.Context, projectID, applicationID int64, ids []int64) ([]CopiedMemory, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT kind, content, pinned FROM %s.agent_memory_entries
WHERE application_id = $1 AND id = ANY($2) AND status = 'active' AND subject_user_id IS NULL
  AND kind <> 'cursor'
ORDER BY id`, schema), applicationID, ids)
	if err != nil {
		return nil, fmt.Errorf("agent proposals: read the memory to copy: %w", err)
	}
	defer rows.Close()
	out := []CopiedMemory{}
	for rows.Next() {
		var entry CopiedMemory
		if err := rows.Scan(&entry.Kind, &entry.Content, &entry.Pinned); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}
