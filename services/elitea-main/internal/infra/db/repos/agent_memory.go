package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// AgentMemory stores an agent's own memory (tenant/0163, ADR-0050 decision
// 1b, #1607). It serves the worker's memory tools (storage.AgentMemorySink),
// the recall at turn start and the Memory routes. Every change is a security
// audit event in the change's transaction (TypeAgentMemoryChanged).
type AgentMemory struct {
	pool        *pgxpool.Pool
	audit       securityaudit.Appender
	activeLimit int
}

var _ storage.AgentMemorySink = (*AgentMemory)(nil)

// NewAgentMemory builds the store. A nil appender is refused: a memory change
// without its evidence does not happen.
func NewAgentMemory(pool *pgxpool.Pool, audit securityaudit.Appender) (*AgentMemory, error) {
	if pool == nil || audit == nil {
		return nil, errors.New("agent memory: pool and audit appender are required")
	}
	return &AgentMemory{pool: pool, audit: audit, activeLimit: agentmemory.DefaultActiveLimit}, nil
}

// ComposeAgentMemory builds the store with the platform appender.
func ComposeAgentMemory(pool *pgxpool.Pool) (*AgentMemory, error) {
	appender, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, err
	}
	return NewAgentMemory(pool, appender)
}

// WithActiveLimit sets the per-agent active entry limit (tests).
func (s *AgentMemory) WithActiveLimit(limit int) *AgentMemory {
	if limit > 0 {
		s.activeLimit = limit
	}
	return s
}

// agentMemoryRunSQL reads what a memory call needs off the claimed
// execution: its agent, actor, conversation, response message, trigger
// origin, whether it is a continuation of an unattended chain or an
// agent-to-agent turn, whether the
// project is personal and whether the conversation is tainted, and the
// frozen input (for the version's memory block).
const agentMemoryRunSQL = `
SELECT COALESCE(a.application_id, 0), j.actor_id, a.client_stream_id, a.client_message_id,
       j.trigger_origin,
       EXISTS (SELECT 1 FROM elitea_runtime.run_chain_links AS link
               WHERE link.execution_id = j.execution_id AND link.kind <> 'root')
       -- An agent-to-agent turn (ADR-0050 decision 7d, #1611): another
       -- agent asked, nobody watches it, so it is not attended either.
       OR EXISTS (SELECT 1 FROM elitea_runtime.agent_floor_turns AS floor_turn
                  WHERE floor_turn.execution_id = j.execution_id AND floor_turn.cause = 'agent_mention'),
       COALESCE((SELECT project.name = $4 || project.owner_id::text
                 FROM centry.project AS project WHERE project.id = j.resource_project_id), false),
       EXISTS (SELECT 1 FROM elitea_runtime.memory_conversation_taints AS taint
               WHERE taint.project_id = j.resource_project_id
                 AND taint.conversation_uuid::text = a.client_stream_id),
       e.content_bytes
FROM elitea_runtime.execution_jobs AS j
JOIN elitea_runtime.agent_execution_jobs AS a
  ON a.execution_id = j.execution_id AND a.generation = j.generation
JOIN elitea_runtime.input_bundle_entries AS e
  ON e.input_bundle_id = j.input_bundle_id AND e.entry_id = a.request_entry_id
 AND e.semantic_role = '` + executiondomain.AgentExecutionRequestRole + `'
WHERE j.execution_id = $1 AND j.generation = $2 AND j.resource_project_id = $3`

// ResolveAgentMemoryRun reads the claimed run. The caller has authorized the
// claim; projectID is the project it resolved.
func (s *AgentMemory) ResolveAgentMemoryRun(ctx context.Context, projectID int64, executionID string, generation uint64) (storage.AgentMemoryRun, error) {
	if generation == 0 || generation > 1<<63-1 {
		return storage.AgentMemoryRun{}, storage.ErrContentUnauthorized
	}
	var (
		applicationID                   int64
		actor, stream, message, origin  string
		continuation, personal, tainted bool
		input                           []byte
	)
	err := s.pool.QueryRow(ctx, agentMemoryRunSQL, executionID, int64(generation), projectID, personalproject.NamePrefix).
		Scan(&applicationID, &actor, &stream, &message, &origin, &continuation, &personal, &tainted, &input)
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.AgentMemoryRun{}, storage.ErrContentUnauthorized
	}
	if err != nil {
		return storage.AgentMemoryRun{}, fmt.Errorf("agent memory: read the run: %w", err)
	}
	actorID, _ := strconv.ParseInt(actor, 10, 64)
	conversation, err := uuid.Parse(stream)
	if err != nil {
		// No conversation: a run memory cannot attribute.
		return storage.AgentMemoryRun{}, storage.ErrContentRejected
	}
	settings, err := frozenMemorySettings(input)
	if err != nil {
		return storage.AgentMemoryRun{}, storage.ErrContentRejected
	}
	run := storage.AgentMemoryRun{
		ProjectID: projectID, ApplicationID: applicationID, ActorID: actorID,
		ConversationUUID: conversation.String(), MessageID: message, ExecutionID: executionID,
		Settings: settings, Attended: origin == string(executiondomain.TriggerOriginManual) && !continuation,
		PersonalProject: personal, Tainted: tainted,
	}
	run.Present, err = s.conversationPeople(ctx, projectID, run.ConversationUUID)
	if err != nil {
		return storage.AgentMemoryRun{}, err
	}
	if !run.Tainted {
		// The taint table records personal memory read through the memory
		// toolkit. Personal memory recalled into a turn's instructions
		// (#870) is marked on the response message instead
		// (meta.memories_used), and the conversation's history carries it
		// into every later turn: either taints the conversation.
		if run.Tainted, err = s.personalRecallInConversation(ctx, projectID, run.ConversationUUID); err != nil {
			return storage.AgentMemoryRun{}, err
		}
	}
	return run, nil
}

// personalRecallInConversation reports whether any turn of the conversation
// had personal memories recalled into its instructions.
func (s *AgentMemory) personalRecallInConversation(ctx context.Context, projectID int64, conversationUUID string) (bool, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return false, err
	}
	var recalled bool
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT EXISTS (
    SELECT 1 FROM %[1]s.chat_message_group AS message
    JOIN %[1]s.chat_conversations AS conversation ON conversation.id = message.conversation_id
    WHERE conversation.uuid = $1::uuid
      AND COALESCE((message.meta ->> 'memories_used') ~ '^[1-9][0-9]*$', false))`, schema), conversationUUID).Scan(&recalled)
	if err != nil {
		return false, fmt.Errorf("agent memory: read the conversation's personal recall: %w", err)
	}
	return recalled, nil
}

// lockAgent serialises the writes of one agent's memory: the active limit
// counts rows the other writers change.
func lockAgent(ctx context.Context, tx pgx.Tx, projectID, applicationID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('elitea_agent_memory:' || $1::text || ':' || $2::text, 0))`,
		projectID, applicationID)
	return err
}

// frozenMemorySettings reads the memory block of the version the run
// executes, from its frozen input: what the run was admitted with, not the
// version as it is now.
func frozenMemorySettings(encoded []byte) (agentmemory.Settings, error) {
	var input runtimev1.AgentExecutionInputV1
	if len(encoded) == 0 || len(encoded) > executiondomain.MaxAgentExecutionInputBytes || proto.Unmarshal(encoded, &input) != nil {
		return agentmemory.Settings{}, errors.New("agent memory: the frozen input is not readable")
	}
	if len(input.Application) == 0 {
		return agentmemory.Off, nil
	}
	var application struct {
		VersionDetails struct {
			Meta map[string]any `json:"meta"`
		} `json:"version_details"`
	}
	if err := json.Unmarshal(input.Application, &application); err != nil {
		return agentmemory.Settings{}, err
	}
	return agentmemory.Parse(application.VersionDetails.Meta[agentmemory.MetaKey])
}

// conversationPeople answers the people in a conversation with their names.
func (s *AgentMemory) conversationPeople(ctx context.Context, projectID int64, conversationUUID string) (map[int64]string, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT DISTINCT (participant.entity_meta ->> 'id')::bigint,
       COALESCE(NULLIF(btrim(participant.meta ->> 'user_name'), ''),
                NULLIF(btrim(participant.entity_meta ->> 'user_name'), ''), '')
FROM %[1]s.chat_conversations AS conversation
JOIN %[1]s.chat_participant_mapping AS mapping ON mapping.conversation_id = conversation.id
JOIN %[1]s.chat_participants AS participant
  ON participant.id = mapping.participant_id AND participant.entity_name = 'user'
WHERE conversation.uuid = $1::uuid
  AND participant.entity_meta ->> 'id' ~ '^[1-9][0-9]{0,9}$'`, schema), conversationUUID)
	if err != nil {
		return nil, fmt.Errorf("agent memory: read the conversation's people: %w", err)
	}
	defer rows.Close()
	people := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("agent memory: scan a person: %w", err)
		}
		people[id] = name
	}
	return people, rows.Err()
}

// InsertAgentMemory stores one agent write, archives the oldest unpinned
// episodes past the active limit, and audits it, in one transaction.
func (s *AgentMemory) InsertAgentMemory(ctx context.Context, run storage.AgentMemoryRun, write storage.AgentMemoryWrite) (int64, error) {
	schema, err := tenantschema.QuoteInt(run.ProjectID)
	if err != nil {
		return 0, err
	}
	var id int64
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// One writer per agent at a time: the limit count and the archive
		// must see each other's rows.
		if err := lockAgent(ctx, tx, run.ProjectID, run.ApplicationID); err != nil {
			return err
		}
		if write.SubjectUserID > 0 {
			var optedOut bool
			if err := tx.QueryRow(ctx, fmt.Sprintf(
				`SELECT EXISTS (SELECT 1 FROM %s.agent_memory_subject_optouts WHERE application_id = $1 AND user_id = $2)`, schema),
				run.ApplicationID, write.SubjectUserID).Scan(&optedOut); err != nil {
				return err
			}
			if optedOut {
				return storage.ErrAgentMemorySubjectOptedOut
			}
		}
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.agent_memory_entries
    (application_id, kind, content, subject_user_id, status, conversation_uuid, message_id, execution_id, written_by_kind)
VALUES ($1, $2, $3, NULLIF($4, 0), $5, $6::uuid, NULLIF($7, ''), NULLIF($8, ''), 'agent')
RETURNING id`, schema),
			run.ApplicationID, string(write.Kind), write.Content, write.SubjectUserID, string(write.Status),
			run.ConversationUUID, run.MessageID, run.ExecutionID).Scan(&id); err != nil {
			return err
		}
		if err := recordVersion(ctx, tx, schema, id); err != nil {
			return err
		}
		if write.Status == agentmemory.StatusActive {
			if err := s.enforceLimit(ctx, tx, schema, run.ApplicationID); err != nil {
				return err
			}
		}
		return s.append(ctx, tx, run.ProjectID, "agent.memory.remembered", 0, run.ApplicationID, id, string(write.Status), run.ExecutionID)
	})
	if err != nil {
		if errors.Is(err, storage.ErrAgentMemorySubjectOptedOut) || errors.Is(err, storage.ErrContentRejected) {
			return 0, err
		}
		return 0, fmt.Errorf("agent memory: insert: %w", err)
	}
	return id, nil
}

// recordVersion copies the entry's current state into its history.
func recordVersion(ctx context.Context, tx pgx.Tx, schema string, entryID int64) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.agent_memory_entry_versions
    (entry_id, version, content, kind, status, pinned, written_by_kind, written_by_user_id)
SELECT id, version, content, kind, status, pinned, written_by_kind, written_by_user_id
FROM %[1]s.agent_memory_entries WHERE id = $1
ON CONFLICT (entry_id, version) DO NOTHING`, schema), entryID)
	return err
}

// enforceLimit archives the oldest unpinned episodes past the active limit.
// When only facts and preferences are left over the limit, they stay: the
// limit archives episodes first, and a person decides about the rest.
func (s *AgentMemory) enforceLimit(ctx context.Context, tx pgx.Tx, schema string, applicationID int64) error {
	_, err := tx.Exec(ctx, fmt.Sprintf(`
WITH over AS (
    SELECT GREATEST(count(*) - $2, 0) AS n
    FROM %[1]s.agent_memory_entries WHERE application_id = $1 AND status = 'active'
), oldest AS (
    SELECT id FROM %[1]s.agent_memory_entries
    WHERE application_id = $1 AND status = 'active' AND kind = 'episode' AND NOT pinned
    ORDER BY updated_at, id
    LIMIT (SELECT n FROM over)
)
UPDATE %[1]s.agent_memory_entries AS entry
SET status = 'archived', updated_at = clock_timestamp()
FROM oldest WHERE entry.id = oldest.id`, schema), applicationID, s.activeLimit)
	return err
}

// ActiveAgentMemory answers the recall pool of one agent: its pinned active
// entries and its newest active ones, at most agentmemory.RecallPool.
func (s *AgentMemory) ActiveAgentMemory(ctx context.Context, projectID, applicationID int64) ([]agentmemory.Candidate, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT id, kind, content, COALESCE(subject_user_id, 0), pinned, updated_at
FROM %s.agent_memory_entries
WHERE application_id = $1 AND status = 'active'
ORDER BY pinned DESC, updated_at DESC, id DESC
LIMIT %d`, schema, agentmemory.RecallPool), applicationID)
	if err != nil {
		return nil, fmt.Errorf("agent memory: recall: %w", err)
	}
	defer rows.Close()
	var candidates []agentmemory.Candidate
	for rows.Next() {
		var candidate agentmemory.Candidate
		var kind string
		if err := rows.Scan(&candidate.ID, &kind, &candidate.Content, &candidate.SubjectUserID, &candidate.Pinned, &candidate.UpdatedAt); err != nil {
			return nil, fmt.Errorf("agent memory: scan: %w", err)
		}
		candidate.Kind = agentmemory.Kind(kind)
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// ForgetAgentMemory deletes one entry of the run's agent. An entry about a
// person not in the conversation is out of reach.
func (s *AgentMemory) ForgetAgentMemory(ctx context.Context, run storage.AgentMemoryRun, entryID int64) (bool, error) {
	schema, err := tenantschema.QuoteInt(run.ProjectID)
	if err != nil {
		return false, err
	}
	present := make([]int64, 0, len(run.Present))
	for id := range run.Present {
		present = append(present, id)
	}
	var forgotten bool
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, fmt.Sprintf(`
DELETE FROM %s.agent_memory_entries
WHERE id = $1 AND application_id = $2 AND kind <> 'cursor'
  AND (subject_user_id IS NULL OR subject_user_id = ANY($3::bigint[]))`, schema),
			entryID, run.ApplicationID, present)
		if err != nil {
			return err
		}
		forgotten = tag.RowsAffected() == 1
		if !forgotten {
			return nil
		}
		return s.append(ctx, tx, run.ProjectID, "agent.memory.deleted", 0, run.ApplicationID, entryID, "agent", run.ExecutionID)
	})
	if err != nil {
		return false, fmt.Errorf("agent memory: forget: %w", err)
	}
	return forgotten, nil
}

// append writes one audit event. actorID 0 is the agent.
func (s *AgentMemory) append(ctx context.Context, tx pgx.Tx, projectID int64, action string, actorID, applicationID, entryID int64, reason, executionID string) error {
	principal := "agent:" + strconv.FormatInt(applicationID, 10)
	if actorID > 0 {
		principal = "user:" + strconv.FormatInt(actorID, 10)
	}
	resource := strconv.FormatInt(applicationID, 10)
	if entryID > 0 {
		resource += ":" + strconv.FormatInt(entryID, 10)
	}
	reasons := []string{}
	if reason != "" {
		reasons = append(reasons, reason)
	}
	event := securityaudit.SecurityEvent{
		Type: securityaudit.TypeAgentMemoryChanged, TenantProjectID: projectID, Action: action,
		Reasons: reasons, PrincipalRef: principal, ResourceType: "agent_memory_entry", ResourceID: resource,
		ExecutionID: executionID, Time: time.Now().UTC(),
	}
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("agent memory: append the audit event %s: %w", action, err)
	}
	return nil
}

// ResolveAgentMemoryRecall is the recall at turn start
// (agentexecution.AgentMemoryRecallResolver).
func (s *AgentMemory) ResolveAgentMemoryRecall(ctx context.Context, request agentexecutionapp.AgentMemoryRecallRequest) (string, error) {
	if request.ProjectID <= 0 || request.ApplicationID <= 0 || uuid.Validate(request.ConversationUUID) != nil {
		return "", nil
	}
	candidates, err := s.ActiveAgentMemory(ctx, request.ProjectID, request.ApplicationID)
	if err != nil || len(candidates) == 0 {
		return "", err
	}
	people, err := s.conversationPeople(ctx, request.ProjectID, request.ConversationUUID)
	if err != nil {
		return "", err
	}
	present := make(map[int64]bool, len(people))
	for id := range people {
		present[id] = true
	}
	return agentmemory.Render(agentmemory.Select(candidates, present, request.UserInput, request.Settings), people), nil
}

// AgentMemoryEntry is one entry as the Memory routes show it.
type AgentMemoryEntry struct {
	ID               int64                `json:"id"`
	ApplicationID    int64                `json:"application_id"`
	Kind             string               `json:"kind"`
	Content          string               `json:"content"`
	SubjectUserID    *int64               `json:"subject_user_id"`
	Pinned           bool                 `json:"pinned"`
	Status           string               `json:"status"`
	Version          int                  `json:"version"`
	ConversationUUID *string              `json:"conversation_uuid"`
	MessageID        *string              `json:"message_id"`
	ExecutionID      *string              `json:"execution_id"`
	WrittenByKind    string               `json:"written_by_kind"`
	WrittenByUserID  *int64               `json:"written_by_user_id"`
	CreatedAt        time.Time            `json:"created_at"`
	UpdatedAt        time.Time            `json:"updated_at"`
	History          []AgentMemoryVersion `json:"history,omitempty"`
}

// AgentMemoryVersion is one stored version of an entry.
type AgentMemoryVersion struct {
	Version         int       `json:"version"`
	Kind            string    `json:"kind"`
	Content         string    `json:"content"`
	Status          string    `json:"status"`
	Pinned          bool      `json:"pinned"`
	WrittenByKind   string    `json:"written_by_kind"`
	WrittenByUserID *int64    `json:"written_by_user_id"`
	CreatedAt       time.Time `json:"created_at"`
}

// AgentMemoryViewer is who reads: an entry about another person is visible only
// to that person and to an agent manager (an owner or a holder of the
// memory edit permissions).
type AgentMemoryViewer struct {
	UserID  int64
	Manager bool
}

// AgentMemoryFilter narrows a list.
type AgentMemoryFilter struct {
	Kind          string
	Status        string
	SubjectUserID int64
	Query         string
	// Source is agent or person: who wrote the current version.
	Source string
	// CreatedAfter and CreatedBefore bound created_at; zero is no bound.
	CreatedAfter, CreatedBefore time.Time
	Limit, Offset               int
}

const agentMemoryColumns = `id, application_id, kind, content, subject_user_id, pinned, status, version,
       conversation_uuid::text, message_id, execution_id, written_by_kind, written_by_user_id, created_at, updated_at`

func scanAgentMemoryEntry(row pgx.Row) (AgentMemoryEntry, error) {
	var entry AgentMemoryEntry
	var subject, writer *int32
	err := row.Scan(&entry.ID, &entry.ApplicationID, &entry.Kind, &entry.Content, &subject, &entry.Pinned,
		&entry.Status, &entry.Version, &entry.ConversationUUID, &entry.MessageID, &entry.ExecutionID,
		&entry.WrittenByKind, &writer, &entry.CreatedAt, &entry.UpdatedAt)
	if subject != nil {
		value := int64(*subject)
		entry.SubjectUserID = &value
	}
	if writer != nil {
		value := int64(*writer)
		entry.WrittenByUserID = &value
	}
	return entry, err
}

// ErrAgentMemoryNotFound is an entry that does not exist or is not visible.
var ErrAgentMemoryNotFound = errors.New("agent memory entry not found")

// ListMemory lists one agent's entries the viewer may see, newest first.
func (s *AgentMemory) ListMemory(ctx context.Context, projectID, applicationID int64, viewer AgentMemoryViewer, filter AgentMemoryFilter) ([]AgentMemoryEntry, int, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return nil, 0, err
	}
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}
	where := `application_id = $1
  AND ($2::boolean OR subject_user_id IS NULL OR subject_user_id = $3)
  AND ($4 = '' OR kind = $4) AND ($5 = '' OR status = $5)
  AND ($6::bigint = 0 OR subject_user_id = $6)
  AND ($7 = '' OR content ILIKE '%' || $7 || '%')
  AND ($8 = '' OR written_by_kind = $8)
  AND ($9::timestamptz IS NULL OR created_at >= $9) AND ($10::timestamptz IS NULL OR created_at < $10)`
	args := []any{applicationID, viewer.Manager, viewer.UserID, filter.Kind, filter.Status, filter.SubjectUserID, likeEscape(filter.Query),
		filter.Source, optionalTime(filter.CreatedAfter), optionalTime(filter.CreatedBefore)}
	var total int
	// The schema is spliced with + and never through a format string: the
	// WHERE text carries the ILIKE wildcards ('%'), which fmt would read as
	// verbs and turn into SQL that does not parse.
	table := schema + `.agent_memory_entries`
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("agent memory: count: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+agentMemoryColumns+` FROM `+table+` WHERE `+where+
		fmt.Sprintf(`
ORDER BY pinned DESC, updated_at DESC, id DESC LIMIT %d OFFSET %d`, filter.Limit, max(filter.Offset, 0)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("agent memory: list: %w", err)
	}
	defer rows.Close()
	entries := []AgentMemoryEntry{}
	for rows.Next() {
		entry, err := scanAgentMemoryEntry(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("agent memory: scan: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, total, rows.Err()
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

// AgentMemoryPerson names a person an entry is about or was written by.
type AgentMemoryPerson struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// PeopleNames answers the display names of the given users, in id order. A
// user with no row or no name is left out (the email is never a fallback:
// the list is read by every project member, and an address is not theirs to
// see), and so is every user where the user table is absent (it is another
// corpus's).
func (s *AgentMemory) PeopleNames(ctx context.Context, ids []int64) ([]AgentMemoryPerson, error) {
	people := []AgentMemoryPerson{}
	if len(ids) == 0 {
		return people, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT u.id, btrim(u.name)
FROM public.auth_core__user AS u
WHERE u.id = ANY($1) AND btrim(COALESCE(u.name, '')) <> ''
ORDER BY u.id`, ids)
	if err != nil {
		if missingRelation(err) {
			return people, nil
		}
		return nil, fmt.Errorf("agent memory: people: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var person AgentMemoryPerson
		if err := rows.Scan(&person.ID, &person.Name); err != nil {
			return nil, fmt.Errorf("agent memory: people scan: %w", err)
		}
		people = append(people, person)
	}
	return people, rows.Err()
}

func likeEscape(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// GetMemory reads one visible entry with its history.
func (s *AgentMemory) GetMemory(ctx context.Context, projectID, applicationID, entryID int64, viewer AgentMemoryViewer) (AgentMemoryEntry, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return AgentMemoryEntry{}, err
	}
	entry, err := s.visibleEntry(ctx, s.pool, schema, applicationID, entryID, viewer, false)
	if err != nil {
		return AgentMemoryEntry{}, err
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT version, kind, content, status, pinned, written_by_kind, written_by_user_id, created_at
FROM %s.agent_memory_entry_versions WHERE entry_id = $1 ORDER BY version DESC`, schema), entryID)
	if err != nil {
		return AgentMemoryEntry{}, fmt.Errorf("agent memory: history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version AgentMemoryVersion
		var writer *int32
		if err := rows.Scan(&version.Version, &version.Kind, &version.Content, &version.Status, &version.Pinned,
			&version.WrittenByKind, &writer, &version.CreatedAt); err != nil {
			return AgentMemoryEntry{}, err
		}
		if writer != nil {
			value := int64(*writer)
			version.WrittenByUserID = &value
		}
		entry.History = append(entry.History, version)
	}
	return entry, rows.Err()
}

type agentMemoryQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *AgentMemory) visibleEntry(ctx context.Context, q agentMemoryQuerier, schema string, applicationID, entryID int64, viewer AgentMemoryViewer, lock bool) (AgentMemoryEntry, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	entry, err := scanAgentMemoryEntry(q.QueryRow(ctx, fmt.Sprintf(`SELECT `+agentMemoryColumns+`
FROM %s.agent_memory_entries WHERE id = $1 AND application_id = $2`+suffix, schema), entryID, applicationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentMemoryEntry{}, ErrAgentMemoryNotFound
	}
	if err != nil {
		return AgentMemoryEntry{}, fmt.Errorf("agent memory: read the entry: %w", err)
	}
	if entry.SubjectUserID != nil && *entry.SubjectUserID != viewer.UserID && !viewer.Manager {
		return AgentMemoryEntry{}, ErrAgentMemoryNotFound
	}
	return entry, nil
}

// AgentMemoryUpdate is a person's change of one entry; nil fields stay.
type AgentMemoryUpdate struct {
	Content *string
	Kind    *string
	Pinned  *bool
	// Status: approve (active), reject or archive (archived).
	Status *string
}

// ErrAgentMemoryForbidden is a change the viewer may not make.
var ErrAgentMemoryForbidden = errors.New("agent memory change not allowed")

// ErrAgentMemoryInvalid is a change that is not valid; it wraps the reason.
var ErrAgentMemoryInvalid = errors.New("invalid agent memory change")

// UpdateMemory applies a person's change as a new version. A non-manager may
// only correct the content of an entry about themself. approver says whether
// the viewer may move a pending entry.
func (s *AgentMemory) UpdateMemory(ctx context.Context, projectID, applicationID, entryID int64, viewer AgentMemoryViewer, approver bool, update AgentMemoryUpdate) (AgentMemoryEntry, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return AgentMemoryEntry{}, err
	}
	var result AgentMemoryEntry
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockAgent(ctx, tx, projectID, applicationID); err != nil {
			return err
		}
		entry, err := s.visibleEntry(ctx, tx, schema, applicationID, entryID, viewer, true)
		if err != nil {
			return err
		}
		own := entry.SubjectUserID != nil && *entry.SubjectUserID == viewer.UserID
		if !viewer.Manager && (!own || update.Kind != nil || update.Pinned != nil || update.Status != nil) {
			return ErrAgentMemoryForbidden
		}
		action := "agent.memory.edited"
		content, kind, pinned, status := entry.Content, entry.Kind, entry.Pinned, entry.Status
		if update.Content != nil {
			if content, err = agentmemory.CheckContent(*update.Content); err != nil {
				return fmt.Errorf("%w: %w", ErrAgentMemoryInvalid, err)
			}
		}
		if update.Kind != nil {
			if !agentmemory.Kind(*update.Kind).Valid() || (*update.Kind == string(agentmemory.KindCursor)) != (entry.Kind == string(agentmemory.KindCursor)) {
				return fmt.Errorf("%w: kind must be fact, preference or episode", ErrAgentMemoryInvalid)
			}
			kind = *update.Kind
		}
		if update.Pinned != nil {
			pinned = *update.Pinned
		}
		if update.Status != nil {
			next := agentmemory.Status(*update.Status)
			if !next.Valid() {
				return fmt.Errorf("%w: status must be pending, active or archived", ErrAgentMemoryInvalid)
			}
			if entry.Status == string(agentmemory.StatusPending) && next != agentmemory.StatusPending {
				if !approver {
					return ErrAgentMemoryForbidden
				}
				action = "agent.memory.approved"
				if next == agentmemory.StatusArchived {
					action = "agent.memory.rejected"
				}
			} else if next == agentmemory.StatusPending && entry.Status != string(agentmemory.StatusPending) {
				return fmt.Errorf("%w: an entry cannot go back to pending", ErrAgentMemoryInvalid)
			} else if next == agentmemory.StatusArchived {
				action = "agent.memory.archived"
			}
			status = string(next)
		}
		result, err = scanAgentMemoryEntry(tx.QueryRow(ctx, fmt.Sprintf(`
UPDATE %s.agent_memory_entries
SET content = $3, kind = $4, pinned = $5, status = $6, version = version + 1,
    written_by_kind = 'person', written_by_user_id = $7, updated_at = clock_timestamp()
WHERE id = $1 AND application_id = $2
RETURNING `+agentMemoryColumns, schema), entryID, applicationID, content, kind, pinned, status, viewer.UserID))
		if err != nil {
			return err
		}
		if err := recordVersion(ctx, tx, schema, entryID); err != nil {
			return err
		}
		if status == string(agentmemory.StatusActive) && entry.Status != status {
			if err := s.enforceLimit(ctx, tx, schema, applicationID); err != nil {
				return err
			}
		}
		return s.append(ctx, tx, projectID, action, viewer.UserID, applicationID, entryID, "", "")
	})
	return result, err
}

// DeleteMemory deletes one entry: a manager any, a person their own.
func (s *AgentMemory) DeleteMemory(ctx context.Context, projectID, applicationID, entryID int64, viewer AgentMemoryViewer) error {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockAgent(ctx, tx, projectID, applicationID); err != nil {
			return err
		}
		entry, err := s.visibleEntry(ctx, tx, schema, applicationID, entryID, viewer, true)
		if err != nil {
			return err
		}
		own := entry.SubjectUserID != nil && *entry.SubjectUserID == viewer.UserID
		if !viewer.Manager && !own {
			return ErrAgentMemoryForbidden
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.agent_memory_entries WHERE id = $1`, schema), entryID); err != nil {
			return err
		}
		return s.append(ctx, tx, projectID, "agent.memory.deleted", viewer.UserID, applicationID, entryID, "", "")
	})
}

// ClearMemory deletes every entry of one agent (managers only) and answers
// how many.
func (s *AgentMemory) ClearMemory(ctx context.Context, projectID, applicationID, actorID int64) (int64, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return 0, err
	}
	var cleared int64
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockAgent(ctx, tx, projectID, applicationID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.agent_memory_entries WHERE application_id = $1`, schema), applicationID)
		if err != nil {
			return err
		}
		cleared = tag.RowsAffected()
		return s.append(ctx, tx, projectID, "agent.memory.cleared", actorID, applicationID, 0, "", "")
	})
	return cleared, err
}

// SetSubjectOptOut turns "remember things about me" off (optOut true) or on
// for one person and one agent. Turning it off also deletes what the agent
// already holds about that person.
func (s *AgentMemory) SetSubjectOptOut(ctx context.Context, projectID, applicationID, userID int64, optOut bool) error {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if !optOut {
			_, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.agent_memory_subject_optouts WHERE application_id = $1 AND user_id = $2`, schema), applicationID, userID)
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.agent_memory_subject_optouts (application_id, user_id) VALUES ($1, $2)
ON CONFLICT DO NOTHING`, schema), applicationID, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.agent_memory_entries WHERE application_id = $1 AND subject_user_id = $2`, schema), applicationID, userID); err != nil {
			return err
		}
		return s.append(ctx, tx, projectID, "agent.memory.opted_out", userID, applicationID, 0, "", "")
	})
}

// SubjectOptedOut reports a person's opt-out for one agent.
func (s *AgentMemory) SubjectOptedOut(ctx context.Context, projectID, applicationID, userID int64) (bool, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return false, err
	}
	var opted bool
	err = s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.agent_memory_subject_optouts WHERE application_id = $1 AND user_id = $2)`, schema), applicationID, userID).Scan(&opted)
	return opted, err
}

// IsAgentOwner reports whether userID is a registry owner of the agent.
func (s *AgentMemory) IsAgentOwner(ctx context.Context, projectID, applicationID, userID int64) (bool, error) {
	var owner bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_lifecycle.agent_registry
               WHERE tenant_project_id = $1 AND application_id = $2
                 AND $3 IN (business_owner_id, technical_owner_id))`, projectID, applicationID, userID).Scan(&owner)
	return owner, err
}
