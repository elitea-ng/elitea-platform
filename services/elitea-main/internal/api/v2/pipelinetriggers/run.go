package pipelinetriggers

// The ONE admission path both entry points use.
//
// It is the chat composer's path, assembled the way the MCP server's
// `tools/call` assembles it: a conversation, a user participant for the acting
// identity, an agent participant pinned to the version through the MAPPING's
// `entity_settings.version_id`, and then `StartCurrentApplication`. Everything
// downstream — the frozen version snapshot, the runtime outbox row, budget and
// governance attribution, the message-trace machinery, cancel — is therefore
// identical to a run a person started by typing. A test in this package asserts
// that equality on the dispatch request itself rather than trusting this
// sentence.
//
// Why not admit with no chat rows at all: the seam does not offer it.
// `CurrentApplicationStartRequest.Validate` requires a conversation uuid and a
// participant id, and `ResolveCurrentApplication` re-resolves the target by
// joining `chat_participant_mapping` → `chat_participants` →
// `application_versions`. Reaching past the use case would mean assembling the
// runtime input a second time. See internal/api/v2/mcp/execute.go, which
// records the same decision at length.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/pipelineruns"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// TriggerConversationSource marks the conversations this package creates.
//
// It is a new `chat_conversations.source` value beside `elitea`, `support` and
// `mcp`. The column is free text with no enumeration to extend, and the chat
// listing filters on `source` only when a caller asks for one — so an
// unattended run appears in the ordinary chat list, which is the whole point:
// a run nobody watched must still be findable by the person who owns the
// pipeline.
const TriggerConversationSource = "pipeline_trigger"

// The two participant entity names the turn resolver matches on, as literals.
const (
	applicationEntityName = "application"
	userEntityName        = "user"
)

// maxRunInput bounds the text an inbound caller may put in front of the
// pipeline. It is far below the use case's own 256 KiB ceiling because this is
// a webhook payload summary and not a chat message; a caller with more to say
// should put it behind a URL the pipeline fetches.
const maxRunInput = 16 * 1024

// maxRunConversationName bounds the list entry a person reads.
const maxRunConversationName = 120

var (
	// ErrRuntimeUnavailable is "this deployment cannot run anything".
	ErrRuntimeUnavailable = errors.New("pipelinetriggers: no agent runtime on this deployment")
	// ErrRunForbidden is "the identity this run would use may not run agents
	// in this project any more".
	ErrRunForbidden = errors.New("pipelinetriggers: the run identity lacks the run permission")
	// ErrVersionNotRunnable is "the pipeline version is gone, or is not a
	// pipeline".
	ErrVersionNotRunnable = errors.New("pipelinetriggers: the pipeline version cannot be run")
	// ErrInputTooLarge is a body this package refuses before touching storage.
	ErrInputTooLarge = errors.New("pipelinetriggers: run input is too large")
)

// runTarget is the stored description of what a run starts. Every field comes
// from the DATABASE. Nothing here is read from a request.
type runTarget struct {
	ApplicationID int64
	VersionID     int64
	Name          string
}

// resolveRunTarget confirms the version is a runnable PIPELINE in this project.
//
// The `agent_type = 'pipeline'` predicate is not decoration. Both entry points
// are documented as pipeline entry points, and a trigger row that outlived a
// version edited into an ordinary agent would otherwise become an unattended
// way to run something the operator never issued a credential for.
func (h *Handler) resolveRunTarget(ctx context.Context, schema string, versionID int64) (runTarget, error) {
	var target runTarget
	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT version.application_id, version.id, COALESCE(application.name, version.name)
  FROM %[1]s.application_versions AS version
  JOIN %[1]s.applications AS application ON application.id = version.application_id
 WHERE version.id = $1 AND version.agent_type = 'pipeline'`, schema), versionID,
	).Scan(&target.ApplicationID, &target.VersionID, &target.Name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return runTarget{}, ErrVersionNotRunnable
		}
		return runTarget{}, err
	}
	return target, nil
}

// authorizeRunIdentity RE-RESOLVES the run identity's permission in the
// project, at fire time.
//
// This is rule 4 of the package doc and the answer to "who does a scheduled run
// execute as". It is called on EVERY inbound trigger and EVERY schedule fire,
// never once at save time: a credential or a cron that keeps working after its
// owner lost access is a standing privilege escalation that nobody would see,
// because there is no interactive session to fail.
//
// FAIL CLOSED. No resolver means no decision, and no decision must not mean
// "allowed" for the one capability here that spends money and drives tools.
func (h *Handler) authorizeRunIdentity(ctx context.Context, projectID, userID int64) error {
	if h.permissions == nil {
		return ErrRunForbidden
	}
	resolution, err := h.permissions.ResolvePermissions(
		ctx,
		auth.User{
			ID:     strconv.FormatInt(userID, 10),
			UserID: strconv.FormatInt(userID, 10),
		},
		RunPermissionMode,
		strconv.FormatInt(projectID, 10),
	)
	if err != nil {
		return ErrRunForbidden
	}
	for _, permission := range resolution.Permissions {
		if permission == RunPermission {
			return nil
		}
	}
	return ErrRunForbidden
}

// runOutcome is what an admitted run reports back.
type runOutcome struct {
	ExecutionID       string
	ConversationUUID  string
	ResponseMessageID string
	ApplicationID     int64
	VersionID         int64
}

// runRequest is one unattended start.
type runRequest struct {
	ProjectID int64
	// ActorUserID is the identity the run executes as: the trigger's creator,
	// or the schedule's author. Never a value from the request.
	ActorUserID int64
	VersionID   int64
	// Input is the text placed in front of the pipeline. It may be empty; a
	// pipeline started with no input runs its graph from its own entry node,
	// which is what a webhook with no body means.
	Input string
	// Origin names the entry point in the conversation name and meta, so a
	// person opening the chat list can tell a webhook run from a cron run
	// without opening either.
	Origin string
	// ScheduleID stamps the conversation with the schedule that started it, and
	// is zero for an inbound trigger. It is what the OVERLAP check reads: "is
	// this schedule's previous run still streaming" is answerable from the
	// transcript alone, so no extra column has to be kept in step with the
	// runtime's own state.
	ScheduleID int64
}

// admit runs the whole sequence: authorize, resolve, create the transcript,
// start.
func (h *Handler) admit(ctx context.Context, schema string, request runRequest) (runOutcome, error) {
	if h.pool == nil || h.start == nil {
		return runOutcome{}, ErrRuntimeUnavailable
	}
	if !utf8.ValidString(request.Input) || len(request.Input) > maxRunInput {
		return runOutcome{}, ErrInputTooLarge
	}
	if err := h.authorizeRunIdentity(ctx, request.ProjectID, request.ActorUserID); err != nil {
		return runOutcome{}, err
	}
	target, err := h.resolveRunTarget(ctx, schema, request.VersionID)
	if err != nil {
		return runOutcome{}, err
	}

	conversationUUID, participantID, err := h.prepareRunConversation(ctx, schema, request, target)
	if err != nil {
		return runOutcome{}, err
	}

	outcome, err := h.start.StartCurrentApplication(ctx, agentexecutionapp.CurrentApplicationStartRequest{
		ProjectID:           request.ProjectID,
		ActorUserID:         request.ActorUserID,
		ConversationUUID:    conversationUUID,
		TargetParticipantID: participantID,
		QuestionID:          uuid.NewString(),
		UserInput:           request.Input,
	})
	if err != nil {
		// The turn was never admitted, so the conversation created a moment ago
		// holds nothing and never will. Left behind, a misconfigured webhook
		// retried by its sender fills the chat list with empty transcripts.
		h.discardRunConversation(ctx, schema, conversationUUID)
		return runOutcome{}, fmt.Errorf("pipelinetriggers: start pipeline run: %w", err)
	}

	// #876's second half: pipeline.run.started fires for BOTH entry points
	// (this is the one admission path both use), and schedule.fired
	// additionally fires when a schedule — not an inbound trigger — is what
	// admitted this run. Both are "admitted", not "finished" — see
	// internal/events.EventPipelineRunSucceeded's doc comment for why this
	// package does not also emit the run's eventual outcome.
	if h.events != nil {
		projectID := strconv.FormatInt(request.ProjectID, 10)
		h.events.Emit(ctx, projectID, "pipeline.run.started", map[string]any{
			"execution_id":      outcome.ExecutionID,
			"conversation_uuid": conversationUUID,
			"application_id":    target.ApplicationID,
			"version_id":        target.VersionID,
			"origin":            request.Origin,
		})
		if request.ScheduleID != 0 {
			h.events.Emit(ctx, projectID, "schedule.fired", map[string]any{
				"schedule_id":       request.ScheduleID,
				"execution_id":      outcome.ExecutionID,
				"conversation_uuid": conversationUUID,
				"application_id":    target.ApplicationID,
				"version_id":        target.VersionID,
			})
		}
	}

	// The write half of pipeline.run.succeeded/failed (the SSRF-hardening
	// wave's follow-up to #876's second half): a row this run's EVENTUAL
	// settlement hook (internal/application/pipelineruns.NewSettlementHook)
	// looks up by execution id once the claim-fence machinery decides this
	// run's outcome, minutes from now on a worker's own schedule. Best
	// effort and independent of h.events above: a webhook subscriber only
	// needs THIS row to exist by the time settlement happens, not for the
	// admission events to also be wired.
	if h.runTracker != nil {
		if err := h.runTracker.RecordRunStart(ctx, pipelineruns.Run{
			ExecutionID:      outcome.ExecutionID,
			ProjectID:        strconv.FormatInt(request.ProjectID, 10),
			ApplicationID:    target.ApplicationID,
			VersionID:        target.VersionID,
			ConversationUUID: conversationUUID,
			// request.Origin is already OriginWebhook or OriginSchedule
			// (triggers.go) — the same value the conversation name and meta
			// are stamped with, so a person reading either surface sees one
			// vocabulary.
			Origin: request.Origin,
		}); err != nil {
			// A failure here means the run's EVENTUAL outcome cannot be
			// reported as a webhook event — not that the run itself is in
			// any doubt. The run was already admitted (StartCurrentApplication
			// above succeeded) and this is a secondary, best-effort signal;
			// logging and continuing is the same "degraded, not wrong" choice
			// webhook.NewDispatcher's own doc comment makes for its absent
			// delivery log.
			slog.Error("pipelinetriggers: record pipeline run start", "err", err, "execution_id", outcome.ExecutionID)
		}
	}

	return runOutcome{
		ExecutionID:       outcome.ExecutionID,
		ConversationUUID:  conversationUUID,
		ResponseMessageID: outcome.ResponseMessageID,
		ApplicationID:     target.ApplicationID,
		VersionID:         target.VersionID,
	}, nil
}

func (h *Handler) prepareRunConversation(
	ctx context.Context, schema string, request runRequest, target runTarget,
) (string, int64, error) {
	transaction, err := h.pool.Begin(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("pipelinetriggers: begin run conversation: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	metaFields := map[string]any{
		"conversation_type": TriggerConversationSource,
		"trigger_origin":    request.Origin,
		"started_at":        time.Now().UTC().Format(time.RFC3339),
	}
	if request.ScheduleID > 0 {
		// A STRING, because the overlap check compares it with `->>`, which
		// yields text. A number here would compare `"7"` against `7` and never
		// match, so every scheduled run would look like the first one.
		metaFields["schedule_id"] = strconv.FormatInt(request.ScheduleID, 10)
	}
	meta, err := json.Marshal(metaFields)
	if err != nil {
		return "", 0, err
	}

	var conversationID int64
	var conversationUUID string
	if err := transaction.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_conversations (uuid, name, is_private, author_id, meta, source)
VALUES (gen_random_uuid(), $1, TRUE, $2, $3::jsonb, $4)
RETURNING id, uuid::text`, schema),
		runConversationName(request.Origin, target.Name), request.ActorUserID,
		string(meta), TriggerConversationSource,
	).Scan(&conversationID, &conversationUUID); err != nil {
		return "", 0, fmt.Errorf("pipelinetriggers: create run conversation: %w", err)
	}

	authorMeta, err := json.Marshal(map[string]any{"id": request.ActorUserID})
	if err != nil {
		return "", 0, err
	}
	authorParticipantID, err := findOrCreateParticipant(ctx, transaction, schema, userEntityName, authorMeta)
	if err != nil {
		return "", 0, err
	}
	if err := mapParticipant(ctx, transaction, schema, conversationID, authorParticipantID, []byte(`{}`)); err != nil {
		return "", 0, err
	}

	agentMeta, err := json.Marshal(map[string]any{
		"id":         target.ApplicationID,
		"project_id": request.ProjectID,
	})
	if err != nil {
		return "", 0, err
	}
	agentParticipantID, err := findOrCreateParticipant(ctx, transaction, schema, applicationEntityName, agentMeta)
	if err != nil {
		return "", 0, err
	}
	// `version_id` goes on the MAPPING, not on the participant: the participant
	// is shared across conversations, while the version this run is pinned to
	// belongs to this conversation. ResolveCurrentApplicationTurn reads it from
	// exactly here, which is what makes a revoked-and-reissued trigger against
	// a NEW version run the new one.
	agentSettings, err := json.Marshal(map[string]any{"version_id": target.VersionID})
	if err != nil {
		return "", 0, err
	}
	if err := mapParticipant(ctx, transaction, schema, conversationID, agentParticipantID, agentSettings); err != nil {
		return "", 0, err
	}

	if err := transaction.Commit(ctx); err != nil {
		return "", 0, fmt.Errorf("pipelinetriggers: commit run conversation: %w", err)
	}
	return conversationUUID, agentParticipantID, nil
}

// discardRunConversation removes a conversation whose turn was never admitted.
//
// Guarded on `source` AND on the conversation holding no message group: the
// first means a bug here cannot reach a person's own chat, the second means a
// conversation that somehow did receive a turn is never removed under it.
func (h *Handler) discardRunConversation(ctx context.Context, schema, conversationUUID string) {
	if h.pool == nil || conversationUUID == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	_, _ = h.pool.Exec(ctx, fmt.Sprintf(`
DELETE FROM %[1]s.chat_conversations AS conversation
WHERE conversation.uuid = $1::uuid
  AND conversation.source = $2
  AND NOT EXISTS (
      SELECT 1 FROM %[1]s.chat_message_group AS existing
      WHERE existing.conversation_id = conversation.id
  )`, schema), conversationUUID, TriggerConversationSource)
}

// findOrCreateParticipant is find-then-create. `chat_participants` has no
// unique key on (entity_name, entity_meta), so an INSERT cannot conflict and a
// create-first shape would make a second participant on every run.
func findOrCreateParticipant(
	ctx context.Context, transaction pgx.Tx, schema, entityName string, entityMeta []byte,
) (int64, error) {
	var participantID int64
	err := transaction.QueryRow(ctx, fmt.Sprintf(`
SELECT id FROM %s.chat_participants
WHERE entity_name = $1::text
  AND entity_meta ->> 'id' IS NOT DISTINCT FROM $2::jsonb ->> 'id'
  AND entity_meta ->> 'project_id' IS NOT DISTINCT FROM $2::jsonb ->> 'project_id'
ORDER BY id
LIMIT 1`, schema), entityName, entityMeta).Scan(&participantID)
	if err == nil {
		return participantID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("pipelinetriggers: look up %s participant: %w", entityName, err)
	}
	if err := transaction.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), $1, $2::jsonb, '{}'::json)
RETURNING id`, schema), entityName, entityMeta).Scan(&participantID); err != nil {
		return 0, fmt.Errorf("pipelinetriggers: create %s participant: %w", entityName, err)
	}
	return participantID, nil
}

// mapParticipant attaches a participant to the conversation. ON CONFLICT names
// the COLUMNS and not the constraint, for the reason in repos/conversations.go.
func mapParticipant(
	ctx context.Context, transaction pgx.Tx, schema string, conversationID, participantID int64, settings []byte,
) error {
	if _, err := transaction.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.chat_participant_mapping (conversation_id, participant_id, entity_settings)
VALUES ($1, $2, $3::jsonb)
ON CONFLICT (participant_id, conversation_id) DO NOTHING`, schema),
		conversationID, participantID, settings); err != nil {
		return fmt.Errorf("pipelinetriggers: map participant: %w", err)
	}
	return nil
}

func runConversationName(origin, pipelineName string) string {
	name := origin + ": " + pipelineName
	if utf8.RuneCountInString(name) <= maxRunConversationName {
		return name
	}
	return string([]rune(name)[:maxRunConversationName])
}
