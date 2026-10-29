package repos

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// RunSteps is the step record store of ADR-0042 (shared 0205,
// internal/application/runevidence): the persist path of ReportRunSteps, the
// anchor into the tenant's security audit chain, the evidence reads and the
// step retention sweeps.
type RunSteps struct {
	pool     *pgxpool.Pool
	appender securityaudit.Appender
}

var (
	_ runevidence.Store          = (*RunSteps)(nil)
	_ runevidence.RetentionStore = (*RunSteps)(nil)
)

// ErrRunNotFound is a run (execution) with no step records in the project.
var ErrRunNotFound = errors.New("run has no step records in this project")

// NewRunSteps builds the store. appender writes the anchor events; it is the
// same PostgresAppender every security audit writer uses, and works whatever
// the credential governance profile (the 0183 outbox is a shared migration).
func NewRunSteps(pool *pgxpool.Pool, appender securityaudit.Appender) (*RunSteps, error) {
	if pool == nil || appender == nil {
		return nil, errors.New("run steps: a pool and an appender are required")
	}
	return &RunSteps{pool: pool, appender: appender}, nil
}

// ComposeRunSteps builds the store with elitea-main's security audit
// appender.
func ComposeRunSteps(pool *pgxpool.Pool) (*RunSteps, error) {
	appender, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, err
	}
	return NewRunSteps(pool, appender)
}

// runContextSQL reads what Main knows of a claimed generation. The release
// and its definition digest come from admission (0199); the actor is the
// active agent identity binding of a governed run (its admission pin is
// governed and the agent has an active binding for the released version),
// else the user the run was admitted for.
const runContextSQL = `
SELECT j.resource_project_id::bigint, j.actor_id, a.application_id, a.release_id,
       r.definition_digest, a.client_stream_id, a.client_message_id,
       (SELECT b.id::text
          FROM elitea_runtime.execution_admission_profiles p
          JOIN elitea_identity.agent_identity_bindings b
            ON b.tenant_project_id = j.resource_project_id
           AND b.application_id = a.application_id
           AND b.state = 'active'
           AND (r.application_version_id IS NULL OR b.application_version_id = r.application_version_id)
         WHERE p.execution_id = j.execution_id
           AND p.profile = 'elitea.credential-profile.governed.v1'
         ORDER BY b.application_version_id DESC
         LIMIT 1)
  FROM elitea_runtime.execution_jobs j
  LEFT JOIN elitea_runtime.agent_execution_jobs a
    ON a.execution_id = j.execution_id AND a.generation = j.generation
  LEFT JOIN elitea_lifecycle.application_releases r ON r.id = a.release_id
 WHERE j.execution_id = $1 AND j.generation = $2`

// RunContext implements runevidence.Store.
func (s *RunSteps) RunContext(ctx context.Context, executionID string, generation uint64) (runevidence.RunContext, error) {
	var (
		run                      runevidence.RunContext
		actorID                  string
		applicationID, releaseID *int64
		digest                   []byte
		stream, message, binding *string
	)
	if err := s.pool.QueryRow(ctx, runContextSQL, executionID, int64(generation)).Scan(
		&run.ProjectID, &actorID, &applicationID, &releaseID, &digest, &stream, &message, &binding); err != nil {
		return runevidence.RunContext{}, fmt.Errorf("run steps: read the run: %w", err)
	}
	run.RunID, run.Generation = executionID, int64(generation)
	run.ApplicationID, run.ReleaseID, run.DefinitionDigest = applicationID, releaseID, digest
	if user, err := strconv.ParseInt(actorID, 10, 64); err == nil && user > 0 {
		run.ActorUserID = &user
	}
	switch {
	case binding != nil:
		run.Actor = "agent:" + *binding
	case run.ActorUserID != nil:
		run.Actor = "user:" + strconv.FormatInt(*run.ActorUserID, 10)
	default:
		run.Actor = "user:" + truncateEvidence(actorID, 200)
	}
	if stream != nil {
		if parsed, err := uuid.Parse(*stream); err == nil {
			run.ConversationUUID = parsed.String()
		}
	}
	if message != nil {
		run.MessageID = *message
	}
	return run, nil
}

// EvidenceFields implements runevidence.Store: the toolkit's
// settings.evidence_fields. A toolkit id that is not a saved toolkit of the
// project (an internal toolkit) sets none. A value that is not a list of
// strings keeps nothing: minimisation fails closed.
func (s *RunSteps) EvidenceFields(ctx context.Context, projectID int64, toolkitID string) ([]string, bool, error) {
	id, err := strconv.ParseInt(toolkitID, 10, 32)
	if err != nil || id <= 0 {
		return nil, false, nil
	}
	project := strconv.FormatInt(projectID, 10)
	var present bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "p_"+project+".elitea_tools").Scan(&present); err != nil {
		return nil, false, fmt.Errorf("run steps: toolkit lookup: %w", err)
	}
	if !present {
		return nil, false, nil
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT settings->'evidence_fields' FROM %s.elitea_tools WHERE id = $1`, schema(project)), id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (raw == nil || string(raw) == "null")) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("run steps: read evidence_fields: %w", err)
	}
	var fields []string
	if json.Unmarshal(raw, &fields) != nil {
		return []string{}, true, nil
	}
	return fields, true, nil
}

// StoredStepIDs implements runevidence.Store.
func (s *RunSteps) StoredStepIDs(ctx context.Context, runID string, ids []string) (map[string]bool, error) {
	return storedStepIDs(ctx, s.pool, runID, ids)
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func storedStepIDs(ctx context.Context, q queryer, runID string, ids []string) (map[string]bool, error) {
	rows, err := q.Query(ctx, `
SELECT step_id::text FROM elitea_runtime.run_steps WHERE run_id = $1 AND step_id = ANY($2::uuid[])`, runID, ids)
	if err != nil {
		return nil, fmt.Errorf("run steps: read stored steps: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("run steps: read stored steps: %w", err)
	}
	out := make(map[string]bool, len(found))
	for _, id := range found {
		out[id] = true
	}
	return out, nil
}

// lockHeadSQL creates the run's head on its first report and locks it. ON
// CONFLICT DO UPDATE takes the row lock, so two first reports of a run
// serialise here instead of both reading last_seq 0.
const lockHeadSQL = `
INSERT INTO elitea_runtime.run_step_heads AS h
    (run_id, project_id, application_id, actor_user_id, conversation_uuid, message_id)
VALUES ($1, $2, $3, $4, $5::uuid, $6)
ON CONFLICT (run_id) DO UPDATE SET run_id = h.run_id
RETURNING h.project_id, h.last_seq, h.last_hash`

const insertStepSQL = `
INSERT INTO elitea_runtime.run_steps (
    run_id, seq, step_id, generation, project_id, application_id, release_id, definition_digest,
    kind, actor, actor_user_id, conversation_uuid, node_id, name, toolkit_id, started_at, ended_at,
    summary, retrieval, cited_step_ids, cited_document_ids,
    inputs_digest, inputs_size, inputs_original_size, inputs_truncated, inputs_filtered, inputs_media_type, inputs_object_key,
    outputs_digest, outputs_size, outputs_original_size, outputs_truncated, outputs_filtered, outputs_media_type, outputs_object_key,
    prev_hash, hash
) VALUES (
    $1, $2, $3::uuid, $4, $5, $6, $7, $8,
    $9, $10, $11, $12::uuid, $13, $14, $15, $16, $17,
    $18::text::jsonb, $19::text::jsonb, $20, $21,
    $22, $23, $24, $25, $26, $27, $28,
    $29, $30, $31, $32, $33, $34, $35,
    $36, $37
)`

// Append implements runevidence.Store: one transaction under the head lock.
func (s *RunSteps) Append(ctx context.Context, run runevidence.RunContext, records []runevidence.Record) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("run steps: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var (
		headProject int64
		lastSeq     int64
		lastHash    []byte
	)
	if err := tx.QueryRow(ctx, lockHeadSQL, run.RunID, run.ProjectID, run.ApplicationID, run.ActorUserID,
		nullableString(run.ConversationUUID), nullableString(run.MessageID)).Scan(&headProject, &lastSeq, &lastHash); err != nil {
		return 0, fmt.Errorf("run steps: lock the run head: %w", err)
	}
	if headProject != run.ProjectID {
		return 0, fmt.Errorf("run steps: run %s belongs to another project", run.RunID)
	}
	ids := make([]string, len(records))
	for i, r := range records {
		ids[i] = r.StepID
	}
	// Under the lock: a concurrent retry of the same report may have stored
	// some of these since the caller looked.
	stored, err := storedStepIDs(ctx, tx, run.RunID, ids)
	if err != nil {
		return 0, err
	}
	accepted := 0
	for _, r := range records {
		if stored[r.StepID] {
			continue
		}
		stored[r.StepID] = true
		r.RunID, r.ProjectID = run.RunID, run.ProjectID
		r.Seq, r.PrevHash = lastSeq+1, lastHash
		if r.Hash, err = runevidence.ChainHash(r.PrevHash, r); err != nil {
			return 0, fmt.Errorf("run steps: hash step %s: %w", r.StepID, err)
		}
		if _, err := tx.Exec(ctx, insertStepSQL, recordArgs(r)...); err != nil {
			return 0, fmt.Errorf("run steps: insert step: %w", err)
		}
		lastSeq, lastHash = r.Seq, r.Hash
		accepted++
	}
	if accepted == 0 {
		return 0, nil
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.run_step_heads SET last_seq = $2, last_hash = $3, updated_at = clock_timestamp()
 WHERE run_id = $1`, run.RunID, lastSeq, lastHash); err != nil {
		return 0, fmt.Errorf("run steps: move the run head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("run steps: commit: %w", err)
	}
	return accepted, nil
}

func recordArgs(r runevidence.Record) []any {
	var retrieval *string
	if r.Retrieval != nil {
		value := string(r.Retrieval)
		retrieval = &value
	}
	return []any{
		r.RunID, r.Seq, r.StepID, r.Generation, r.ProjectID, r.ApplicationID, r.ReleaseID, r.DefinitionDigest,
		string(r.Kind), r.Actor, r.ActorUserID, nullableString(r.ConversationUUID), r.NodeID, r.Name, r.ToolkitID,
		r.StartedAt, r.EndedAt, string(r.Summary), retrieval, r.CitedStepIDs, r.CitedDocumentIDs,
		r.Inputs.Digest, r.Inputs.Size, r.Inputs.OriginalSize, r.Inputs.Truncated, r.Inputs.Filtered, r.Inputs.MediaType, nullableString(r.Inputs.ObjectKey),
		r.Outputs.Digest, r.Outputs.Size, r.Outputs.OriginalSize, r.Outputs.Truncated, r.Outputs.Filtered, r.Outputs.MediaType, nullableString(r.Outputs.ObjectKey),
		r.PrevHash, r.Hash,
	}
}

// Anchor implements runevidence.Store: when the head moved since the last
// anchor, append ai.elitea.security.v1.run.steps.anchored (the head hash and
// step count) to the tenant's security audit chain, in the same transaction
// that records the anchor on the head. The appender writes the 0183 outbox,
// which every install has: the governed profile changes who EMITS governance
// events and whether the relay starts, not whether an append works, and
// elitea-main now starts the relay on every install (cmd/elitea-main).
func (s *RunSteps) Anchor(ctx context.Context, runID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("run steps: begin anchor: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var (
		project, lastSeq, anchoredSeq int64
		lastHash                      []byte
	)
	err = tx.QueryRow(ctx, `
SELECT project_id, last_seq, last_hash, anchored_seq FROM elitea_runtime.run_step_heads
 WHERE run_id = $1 FOR UPDATE`, runID).Scan(&project, &lastSeq, &lastHash, &anchoredSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // the run reported no steps
	}
	if err != nil {
		return fmt.Errorf("run steps: lock the run head: %w", err)
	}
	if lastSeq == anchoredSeq {
		return nil
	}
	if err := s.appender.Append(ctx, tx, securityaudit.SecurityEvent{
		Type:            securityaudit.TypeRunStepsAnchored,
		TenantProjectID: project,
		Action:          "run.steps.anchored",
		ExecutionID:     truncateEvidence(runID, 256),
		ResourceType:    "run",
		ResourceID:      truncateEvidence(runID, 256),
		ChainHead:       hex.EncodeToString(lastHash),
		ChainLength:     lastSeq,
	}); err != nil {
		return fmt.Errorf("run steps: anchor: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.run_step_heads SET anchored_seq = last_seq, anchored_at = clock_timestamp()
 WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("run steps: record the anchor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("run steps: commit anchor: %w", err)
	}
	return nil
}

// AnchorPending implements runevidence.RetentionStore: settled runs (no
// generation still live) whose head moved since the last anchor.
func (s *RunSteps) AnchorPending(ctx context.Context, limit int) (int, error) {
	rows, err := s.pool.Query(ctx, `
SELECT h.run_id FROM elitea_runtime.run_step_heads h
 WHERE h.anchored_seq < h.last_seq
   AND NOT EXISTS (
       SELECT 1 FROM elitea_runtime.execution_jobs j
        WHERE j.execution_id = h.run_id
          AND j.state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED'))
 ORDER BY h.updated_at
 LIMIT $1`, limit)
	if err != nil {
		return 0, fmt.Errorf("run steps: pending anchors: %w", err)
	}
	runs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("run steps: pending anchors: %w", err)
	}
	anchored := 0
	var errs []error
	for _, run := range runs {
		if err := s.Anchor(ctx, run); err != nil {
			errs = append(errs, err)
			continue
		}
		anchored++
	}
	return anchored, errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Reads

// RunHead is a run's head row.
type RunHead struct {
	RunID            string
	ProjectID        int64
	ApplicationID    *int64
	ActorUserID      *int64
	ConversationUUID string
	MessageID        string
	LastSeq          int64
	LastHash         []byte
	AnchoredSeq      int64
	AnchoredAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

const headColumns = `run_id, project_id, application_id, actor_user_id, conversation_uuid::text, message_id,
       last_seq, last_hash, anchored_seq, anchored_at, created_at, updated_at`

func scanHead(row pgx.Row) (RunHead, error) {
	var (
		h                     RunHead
		conversation, message *string
	)
	err := row.Scan(&h.RunID, &h.ProjectID, &h.ApplicationID, &h.ActorUserID, &conversation, &message,
		&h.LastSeq, &h.LastHash, &h.AnchoredSeq, &h.AnchoredAt, &h.CreatedAt, &h.UpdatedAt)
	h.ConversationUUID, h.MessageID = valueOrEmpty(conversation), valueOrEmpty(message)
	return h, err
}

// Head reads a run's head in a project.
func (s *RunSteps) Head(ctx context.Context, projectID int64, runID string) (RunHead, error) {
	h, err := scanHead(s.pool.QueryRow(ctx, `SELECT `+headColumns+`
  FROM elitea_runtime.run_step_heads WHERE run_id = $1 AND project_id = $2`, runID, projectID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunHead{}, ErrRunNotFound
	}
	if err != nil {
		return RunHead{}, fmt.Errorf("run steps: read the run head: %w", err)
	}
	return h, nil
}

const stepColumns = `id, run_id, seq, step_id::text, generation, project_id, application_id, release_id, definition_digest,
       kind, actor, actor_user_id, conversation_uuid::text, node_id, name, toolkit_id, started_at, ended_at,
       summary::text, retrieval::text, cited_step_ids, cited_document_ids,
       inputs_digest, inputs_size, inputs_original_size, inputs_truncated, inputs_filtered, inputs_media_type, inputs_object_key,
       outputs_digest, outputs_size, outputs_original_size, outputs_truncated, outputs_filtered, outputs_media_type, outputs_object_key,
       prev_hash, hash, content_expired_at, content_expired_policy, recorded_at, export_txid`

func scanStep(row pgx.Row) (runevidence.Record, error) {
	var (
		r                                    runevidence.Record
		kind                                 string
		conversation, retrieval              *string
		summary                              string
		inputsKey, outputsKey, expiredPolicy *string
	)
	err := row.Scan(&r.ID, &r.RunID, &r.Seq, &r.StepID, &r.Generation, &r.ProjectID, &r.ApplicationID, &r.ReleaseID, &r.DefinitionDigest,
		&kind, &r.Actor, &r.ActorUserID, &conversation, &r.NodeID, &r.Name, &r.ToolkitID, &r.StartedAt, &r.EndedAt,
		&summary, &retrieval, &r.CitedStepIDs, &r.CitedDocumentIDs,
		&r.Inputs.Digest, &r.Inputs.Size, &r.Inputs.OriginalSize, &r.Inputs.Truncated, &r.Inputs.Filtered, &r.Inputs.MediaType, &inputsKey,
		&r.Outputs.Digest, &r.Outputs.Size, &r.Outputs.OriginalSize, &r.Outputs.Truncated, &r.Outputs.Filtered, &r.Outputs.MediaType, &outputsKey,
		&r.PrevHash, &r.Hash, &r.ContentExpiredAt, &expiredPolicy, &r.RecordedAt, &r.ExportTxID)
	if err != nil {
		return runevidence.Record{}, err
	}
	r.Kind = runevidence.Kind(kind)
	r.ConversationUUID, r.Summary = valueOrEmpty(conversation), []byte(summary)
	if retrieval != nil {
		r.Retrieval = []byte(*retrieval)
	}
	r.Inputs.ObjectKey, r.Outputs.ObjectKey, r.ContentExpiredPolicy = valueOrEmpty(inputsKey), valueOrEmpty(outputsKey), valueOrEmpty(expiredPolicy)
	if r.CitedStepIDs == nil {
		r.CitedStepIDs = []string{}
	}
	if r.CitedDocumentIDs == nil {
		r.CitedDocumentIDs = []string{}
	}
	return r, nil
}

// MaxStepsPerRunRead bounds one read of a run's steps (and the verifier's).
const MaxStepsPerRunRead = 100000

// Steps reads a run's steps in seq order.
func (s *RunSteps) Steps(ctx context.Context, projectID int64, runID string) ([]runevidence.Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+stepColumns+`
  FROM elitea_runtime.run_steps WHERE run_id = $1 AND project_id = $2 ORDER BY seq LIMIT $3`, runID, projectID, MaxStepsPerRunRead)
	if err != nil {
		return nil, fmt.Errorf("run steps: read steps: %w", err)
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (runevidence.Record, error) { return scanStep(row) })
	if err != nil {
		return nil, fmt.Errorf("run steps: read steps: %w", err)
	}
	return records, nil
}

// StepsPage reads up to limit of a run's steps after afterSeq, in seq order.
func (s *RunSteps) StepsPage(ctx context.Context, projectID int64, runID string, afterSeq int64, limit int) ([]runevidence.Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+stepColumns+`
  FROM elitea_runtime.run_steps WHERE run_id = $1 AND project_id = $2 AND seq > $3 ORDER BY seq LIMIT $4`,
		runID, projectID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("run steps: read steps: %w", err)
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (runevidence.Record, error) { return scanStep(row) })
	if err != nil {
		return nil, fmt.Errorf("run steps: read steps: %w", err)
	}
	return records, nil
}

// Step reads one step of a run.
func (s *RunSteps) Step(ctx context.Context, projectID int64, runID, stepID string) (runevidence.Record, error) {
	if _, err := uuid.Parse(stepID); err != nil {
		return runevidence.Record{}, ErrRunNotFound
	}
	r, err := scanStep(s.pool.QueryRow(ctx, `SELECT `+stepColumns+`
  FROM elitea_runtime.run_steps WHERE run_id = $1 AND project_id = $2 AND step_id = $3::uuid`, runID, projectID, stepID))
	if errors.Is(err, pgx.ErrNoRows) {
		return runevidence.Record{}, ErrRunNotFound
	}
	if err != nil {
		return runevidence.Record{}, fmt.Errorf("run steps: read step: %w", err)
	}
	return r, nil
}

// RunHeld reports whether an active legal hold covers the run.
func (s *RunSteps) RunHeld(ctx context.Context, runID string) (bool, error) {
	var held bool
	if err := s.pool.QueryRow(ctx, `SELECT `+runHeldSQL+` FROM elitea_runtime.run_step_heads h WHERE h.run_id = $1`, runID).Scan(&held); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("run steps: legal hold lookup: %w", err)
	}
	return held, nil
}

// ConversationRun is one run of a conversation with its step count.
type ConversationRun struct {
	RunID     string
	MessageID string
	Steps     int64
	StartedAt time.Time
	UpdatedAt time.Time
	Anchored  bool
}

// MaxConversationRuns bounds one conversation's run list.
const MaxConversationRuns = 1000

// ConversationRuns lists a conversation's runs, newest first.
func (s *RunSteps) ConversationRuns(ctx context.Context, projectID int64, conversationUUID string) ([]ConversationRun, error) {
	if _, err := uuid.Parse(conversationUUID); err != nil {
		return []ConversationRun{}, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT run_id, coalesce(message_id, ''), last_seq, created_at, updated_at, anchored_seq = last_seq
  FROM elitea_runtime.run_step_heads
 WHERE project_id = $1 AND conversation_uuid = $2::uuid
 ORDER BY created_at DESC, run_id
 LIMIT $3`, projectID, conversationUUID, MaxConversationRuns)
	if err != nil {
		return nil, fmt.Errorf("run steps: conversation runs: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ConversationRun, error) {
		var r ConversationRun
		err := row.Scan(&r.RunID, &r.MessageID, &r.Steps, &r.StartedAt, &r.UpdatedAt, &r.Anchored)
		return r, err
	})
}

// RecordContentRead writes the centry.audit_events row of one evidence
// content read (who read which run's content), synchronously: a read the
// trail cannot record is refused by the caller.
func (s *RunSteps) RecordContentRead(ctx context.Context, userID, projectID int64, runID, stepID string, side runevidence.Side, route string) error {
	name := truncateEvidence(runID+"/"+stepID+"/"+string(side), 256)
	if _, err := s.pool.Exec(ctx, `
INSERT INTO centry.audit_events
    (timestamp, user_id, project_id, event_type, action, http_method, http_route, status_code, duration_ms, is_error, entity_type, entity_name)
VALUES (clock_timestamp(), $1, $2, 'api', 'GET run step content', 'GET', $3, 200, 0, false, 'run_step_content', $4)`,
		int32(userID), int32(projectID), truncateEvidence(route, 512), name); err != nil {
		return fmt.Errorf("run steps: record the content read: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Retention (runevidence.RetentionStore)

// runHeldSQL is true when an active legal hold covers run head h: a hold on
// its project, on the user it ran for, or on its agent.
const runHeldSQL = `EXISTS (
	SELECT 1 FROM elitea_runtime.legal_holds lh
	 WHERE lh.released_at IS NULL
	   AND (   (lh.scope = 'project' AND lh.project_id = h.project_id)
	        OR (lh.scope = 'user' AND lh.user_id = h.actor_user_id)
	        OR (lh.scope = 'agent' AND lh.project_id = h.project_id AND lh.agent_id = h.application_id)))`

// EvidenceProjects implements runevidence.RetentionStore.
func (s *RunSteps) EvidenceProjects(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT project_id FROM elitea_runtime.run_step_heads ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("run steps: projects: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// ExpireContent implements runevidence.RetentionStore.
func (s *RunSteps) ExpireContent(ctx context.Context, projectID int64, cutoff time.Time, policy string, limit int) (int, error) {
	tag, err := s.pool.Exec(ctx, `
WITH doomed AS (
    SELECT st.id FROM elitea_runtime.run_steps st
      JOIN elitea_runtime.run_step_heads h ON h.run_id = st.run_id
     WHERE st.project_id = $1 AND st.recorded_at < $2
       AND st.content_expired_at IS NULL
       AND (st.inputs_object_key IS NOT NULL OR st.outputs_object_key IS NOT NULL)
       AND NOT `+runHeldSQL+`
     ORDER BY st.recorded_at, st.id
     LIMIT $4
       FOR UPDATE OF st SKIP LOCKED
)
UPDATE elitea_runtime.run_steps st
   SET content_expired_at = clock_timestamp(), content_expired_policy = $3
  FROM doomed WHERE st.id = doomed.id`, projectID, cutoff.UTC(), policy, limit)
	if err != nil {
		return 0, fmt.Errorf("run steps: expire content: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// HeldContent implements runevidence.RetentionStore.
func (s *RunSteps) HeldContent(ctx context.Context, projectID int64, cutoff time.Time) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.run_steps st
  JOIN elitea_runtime.run_step_heads h ON h.run_id = st.run_id
 WHERE st.project_id = $1 AND st.recorded_at < $2 AND st.content_expired_at IS NULL
   AND (st.inputs_object_key IS NOT NULL OR st.outputs_object_key IS NOT NULL)
   AND `+runHeldSQL, projectID, cutoff.UTC()).Scan(&n); err != nil {
		return 0, fmt.Errorf("run steps: held content: %w", err)
	}
	return n, nil
}

// ClaimPurges implements runevidence.RetentionStore. Content a legal hold
// covers is not purged even when it was expired before the hold was placed;
// its objects stay until the hold is released.
func (s *RunSteps) ClaimPurges(ctx context.Context, limit int) ([]runevidence.Purge, error) {
	rows, err := s.pool.Query(ctx, `
SELECT st.id, st.inputs_object_key, st.outputs_object_key FROM elitea_runtime.run_steps st
  JOIN elitea_runtime.run_step_heads h ON h.run_id = st.run_id
 WHERE st.content_expired_at IS NOT NULL
   AND (st.inputs_object_key IS NOT NULL OR st.outputs_object_key IS NOT NULL)
   AND NOT `+runHeldSQL+`
 ORDER BY st.content_expired_at, st.id
 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("run steps: claim purges: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (runevidence.Purge, error) {
		var (
			p      runevidence.Purge
			in, ou *string
		)
		if err := row.Scan(&p.ID, &in, &ou); err != nil {
			return p, err
		}
		for _, key := range []*string{in, ou} {
			if key != nil {
				p.Keys = append(p.Keys, *key)
			}
		}
		return p, nil
	})
}

// CompletePurges implements runevidence.RetentionStore.
func (s *RunSteps) CompletePurges(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `
UPDATE elitea_runtime.run_steps SET inputs_object_key = NULL, outputs_object_key = NULL
 WHERE id = ANY($1) AND content_expired_at IS NOT NULL`, ids); err != nil {
		return fmt.Errorf("run steps: complete purges: %w", err)
	}
	return nil
}

// exportPassedSQL is true when every run_steps export sink has passed every
// step of run h, or no sink exists: retention never outruns the SIEM export
// (the rule elitea-scheduler's audit sweeper keeps for admin_audit).
const exportPassedSQL = `NOT EXISTS (
	SELECT 1 FROM elitea_runtime.audit_export_cursors c
	  JOIN elitea_runtime.run_steps st ON st.run_id = h.run_id
	 WHERE c.stream = 'run_steps' AND (st.export_txid, st.id) > (c.last_txid, c.last_id))`

// ExpiredRuns implements runevidence.RetentionStore.
func (s *RunSteps) ExpiredRuns(ctx context.Context, projectID int64, cutoff time.Time, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
SELECT h.run_id FROM elitea_runtime.run_step_heads h
 WHERE h.project_id = $1 AND h.updated_at < $2
   AND NOT `+runHeldSQL+`
   AND `+exportPassedSQL+`
 ORDER BY h.updated_at, h.run_id
 LIMIT $3`, projectID, cutoff.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("run steps: expired runs: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// HeldRuns implements runevidence.RetentionStore.
func (s *RunSteps) HeldRuns(ctx context.Context, projectID int64, cutoff time.Time) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_runtime.run_step_heads h
 WHERE h.project_id = $1 AND h.updated_at < $2 AND `+runHeldSQL, projectID, cutoff.UTC()).Scan(&n); err != nil {
		return 0, fmt.Errorf("run steps: held runs: %w", err)
	}
	return n, nil
}

// ExpireRunContent implements runevidence.RetentionStore.
func (s *RunSteps) ExpireRunContent(ctx context.Context, runID, policy string) error {
	if _, err := s.pool.Exec(ctx, `
UPDATE elitea_runtime.run_steps SET content_expired_at = clock_timestamp(), content_expired_policy = $2
 WHERE run_id = $1 AND content_expired_at IS NULL
   AND (inputs_object_key IS NOT NULL OR outputs_object_key IS NOT NULL)`, runID, policy); err != nil {
		return fmt.Errorf("run steps: expire run content: %w", err)
	}
	return nil
}

// DeleteRun implements runevidence.RetentionStore: under the head's row lock
// it re-checks age, hold and that no content object is still stored, then
// deletes the steps and the head.
func (s *RunSteps) DeleteRun(ctx context.Context, runID string, cutoff time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("run steps: begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var eligible bool
	err = tx.QueryRow(ctx, `
SELECT h.updated_at < $2 AND NOT `+runHeldSQL+` AND `+exportPassedSQL+`
       AND NOT EXISTS (SELECT 1 FROM elitea_runtime.run_steps st
                        WHERE st.run_id = h.run_id
                          AND (st.inputs_object_key IS NOT NULL OR st.outputs_object_key IS NOT NULL))
  FROM elitea_runtime.run_step_heads h WHERE h.run_id = $1
   FOR UPDATE OF h SKIP LOCKED`, runID, cutoff.UTC()).Scan(&eligible)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !eligible) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("run steps: lock run: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM elitea_runtime.run_steps WHERE run_id = $1`, runID); err != nil {
		return false, fmt.Errorf("run steps: delete steps: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM elitea_runtime.run_step_heads WHERE run_id = $1`, runID); err != nil {
		return false, fmt.Errorf("run steps: delete head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("run steps: commit delete: %w", err)
	}
	return true, nil
}

// expireConversationStepContent marks the content of a deleted
// conversation's runs expired (policy conversation_deleted), in the
// conversation delete's transaction: row first, and the step_content sweep's
// drain deletes the objects. The step records stay, governed by the
// step_record policy: they hold digests, not content. A run a legal hold
// covers keeps its content. Guarded by to_regclass
// because repository fixtures build tenant tables without the shared schema.
func expireConversationStepContent(ctx context.Context, tx pgx.Tx, projectID string, conversationUUID string) error {
	project, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || conversationUUID == "" {
		return nil
	}
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('elitea_runtime.run_steps') IS NOT NULL`).Scan(&present); err != nil {
		return fmt.Errorf("conversations: step content lookup: %w", err)
	}
	if !present {
		return nil
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.run_steps st
   SET content_expired_at = clock_timestamp(), content_expired_policy = $3
  FROM elitea_runtime.run_step_heads h
 WHERE h.run_id = st.run_id
   AND st.project_id = $1 AND st.conversation_uuid = $2::uuid AND st.content_expired_at IS NULL
   AND (st.inputs_object_key IS NOT NULL OR st.outputs_object_key IS NOT NULL)
   AND NOT `+runHeldSQL,
		project, conversationUUID, runevidence.PolicyConversationDeleted); err != nil {
		return fmt.Errorf("conversations: expire step content: %w", err)
	}
	return nil
}

func truncateEvidence(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
