package repos

// The approval decision ledger (ADR-0041 decision 5, issue #1491; shared
// 0211).
//
// Every path that raises or decides an approval writes one
// elitea_runtime.approval_ledger row in the SAME transaction as its state
// change, and mirrors the row to the security audit outbox as
// ai.elitea.security.v1.agent.approval (action agent.approval.<outcome>).
// A failed append fails the change: an approval without its evidence does
// not happen.
//
//   - root pauses: the terminal projection of agent_hitl_interrupt writes
//     the pause rows beside meta.hitl_interrupts, which is the UI projection
//     of these rows; the HITL continuation writes the decision rows in the
//     admission transaction that consumes the pending set.
//   - per-interrupt cards (0157): the raise, the decide compare-and-set and
//     every close.
//   - desktop remote toolkit confirmations (0155): the confirmation that is
//     asked for and the one that is consumed.
//   - Stop, regenerate and a refused governed continuation close the open
//     pauses of their response.
//
// A row holds references and digests only. The arguments are digested
// (argument_digest), never stored; a comment, an answer or an edit is never
// stored.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Ledger paths, phases and outcomes (0211's CHECKs).
const (
	approvalPathRoot      = "root"
	approvalPathInterrupt = "interrupt"
	approvalPathDesktop   = "desktop"

	approvalPhasePause    = "pause"
	approvalPhaseNotice   = "notice"
	approvalPhaseDecision = "decision"

	ApprovalOutcomePaused            = "paused"
	ApprovalOutcomeUnattendedWaiting = "unattended_waiting"
	ApprovalOutcomeApproved          = "approved"
	ApprovalOutcomeRejected          = "rejected"
	ApprovalOutcomeEdited            = "edited"
	ApprovalOutcomeAnswered          = "answered"
	ApprovalOutcomeOther             = "other"
	ApprovalOutcomeTimedOut          = "timed_out"
	ApprovalOutcomeCancelled         = "cancelled"
	ApprovalOutcomeSuperseded        = "superseded"
	ApprovalOutcomeUnattendedFailed  = "unattended_failed"
	ApprovalOutcomeUnattendedSkipped = "unattended_skipped"
)

// The typed reasons of the system outcomes.
const (
	// ApprovalRequiredUnattendedCode is the typed error of an unattended run
	// that needed an approval and was set to fail (or could not skip).
	ApprovalRequiredUnattendedCode = "approval_required_unattended"
	// ApprovalTimedOutCode is the typed error of an unattended run whose
	// owner did not answer before the wait expired.
	ApprovalTimedOutCode = "approval_timed_out"
	// approvalSkippedReason is the reason of an unattended skip.
	approvalSkippedReason = "approval_skipped_unattended"
	// approvalRunEndedReason closes a pause whose run ended.
	approvalRunEndedReason = "run_ended"
)

// approvalCall names the call a pause is about. Every field is optional:
// a review node or a question names no tool.
type approvalCall struct {
	GuardrailType  string
	ToolName       string
	ToolkitName    string
	ToolkitType    string
	ArgumentDigest string
}

// approvalEntry is one ledger row.
type approvalEntry struct {
	ProjectID         int64
	ExecutionID       string
	ResponseMessageID string
	ConversationUUID  string
	InterruptID       string
	InterruptKey      string
	Path              string
	Phase             string
	Outcome           string
	Action            string
	// ActorUserID is the person who decided; 0 is Main (a system row).
	ActorUserID   int64
	TriggerOrigin string
	Call          approvalCall
	Reason        string
}

var (
	approvalAppenderOnce sync.Once
	approvalAppender     securityaudit.Appender
	approvalAppenderErr  error

	argumentDigestShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	approvalActionShape = regexp.MustCompile(`^[a-z_]{1,32}$`)
)

// approvalAuditAppender is the PostgresAppender every approval row is
// mirrored with.
func approvalAuditAppender() (securityaudit.Appender, error) {
	approvalAppenderOnce.Do(func() {
		approvalAppender, approvalAppenderErr = securityaudit.NewPostgresAppender("elitea-main")
	})
	return approvalAppender, approvalAppenderErr
}

// approvalTx returns the pgx transaction under a repository executor. The
// audit appender writes through it, so the row and its event commit
// together. A caller with no real transaction cannot record an approval, and
// the change it records fails with it.
// approvalLedgerless is implemented ONLY by unit-test executors that script
// a fixed query sequence and carry no ledger. Every production executor is a
// pgx transaction, and the ledger writes there or the caller fails.
type approvalLedgerless interface{ approvalLedgerless() }

// skipApprovalLedger reports whether executor is a test double without a
// ledger.
func skipApprovalLedger(executor sqlExecutor) bool {
	_, ok := executor.(approvalLedgerless)
	return ok
}

func approvalTx(executor sqlExecutor) (pgx.Tx, error) {
	if wrapped, ok := executor.(pgxExecutor); ok {
		if tx, ok := wrapped.queryer.(pgx.Tx); ok {
			return tx, nil
		}
	}
	return nil, errors.New("approval ledger requires a database transaction")
}

// recordApproval writes one ledger row and its audit event. It answers false,
// and writes nothing, when the wait already has a row of this phase: a
// replayed write is a no-op, and a second decision is the caller's conflict.
func recordApproval(ctx context.Context, executor sqlExecutor, entry approvalEntry) (bool, error) {
	if entry.ProjectID <= 0 || entry.ExecutionID == "" || entry.InterruptID == "" ||
		entry.Path == "" || entry.Phase == "" || entry.Outcome == "" {
		return false, errors.New("approval ledger entry is incomplete")
	}
	tx, err := approvalTx(executor)
	if err != nil {
		return false, err
	}
	// A stream id that is not a conversation uuid (a test or a foreign
	// client) is stored as NULL rather than refusing the projection.
	if _, parseErr := uuid.Parse(entry.ConversationUUID); parseErr != nil {
		entry.ConversationUUID = ""
	}
	appender, err := approvalAuditAppender()
	if err != nil {
		return false, err
	}
	eventID, err := uuid.NewV7()
	if err != nil {
		return false, fmt.Errorf("approval ledger event id: %w", err)
	}
	origin := entry.TriggerOrigin
	if origin == "" {
		origin = "manual"
	}
	actorKind := "user"
	var actor any
	if entry.ActorUserID > 0 {
		actor = entry.ActorUserID
	} else {
		actorKind = "system"
	}
	action := entry.Action
	if !approvalActionShape.MatchString(action) {
		action = ""
	}
	digest := entry.Call.ArgumentDigest
	if !argumentDigestShape.MatchString(digest) {
		digest = ""
	}
	var id int64
	err = tx.QueryRow(ctx, `
INSERT INTO elitea_runtime.approval_ledger (
    project_id, execution_id, response_message_id, conversation_uuid, interrupt_id, interrupt_key,
    path, phase, outcome, action, actor_kind, actor_user_id, trigger_origin, guardrail_type,
    tool_name, toolkit_name, toolkit_type, argument_digest, reason, audit_event_id)
VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, '')::uuid, $5, NULLIF($6, ''),
        $7, $8, $9, NULLIF($10, ''), $11, $12, $13, NULLIF($14, ''),
        NULLIF($15, ''), NULLIF($16, ''), NULLIF($17, ''), NULLIF($18, ''), NULLIF($19, ''), $20::uuid)
ON CONFLICT (execution_id, interrupt_id, phase) DO NOTHING
RETURNING id`,
		entry.ProjectID, entry.ExecutionID, entry.ResponseMessageID, entry.ConversationUUID,
		entry.InterruptID, entry.InterruptKey, entry.Path, entry.Phase, entry.Outcome, action,
		actorKind, actor, origin, boundedText(entry.Call.GuardrailType, 64),
		boundedText(entry.Call.ToolName, 512), boundedText(entry.Call.ToolkitName, 512),
		boundedText(entry.Call.ToolkitType, 128), digest, entry.Reason, eventID.String()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record approval: %w", err)
	}
	principal := "system:elitea-main"
	fidelity := securityaudit.FidelityNone
	if entry.ActorUserID > 0 {
		principal = auditUserRef(strconv.FormatInt(entry.ActorUserID, 10))
		fidelity = securityaudit.FidelityAttributed
	}
	reasons := []string{"path:" + entry.Path, "origin:" + origin}
	if guardrail := reasonPart(entry.Call.GuardrailType); guardrail != "" {
		reasons = append(reasons, "guardrail:"+guardrail)
	}
	if action != "" {
		reasons = append(reasons, "action:"+action)
	}
	if entry.Reason != "" {
		reasons = append(reasons, entry.Reason)
	}
	responseRef := entry.ResponseMessageID
	resourceType := "chat_message"
	if responseRef == "" {
		responseRef, resourceType = entry.ExecutionID, "local_turn"
	}
	if err := appender.Append(ctx, tx, securityaudit.SecurityEvent{
		ID:              eventID.String(),
		Type:            securityaudit.TypeAgentApproval,
		TenantProjectID: entry.ProjectID,
		Action:          "agent.approval." + entry.Outcome,
		Effect:          approvalEffect(entry.Outcome),
		Reasons:         reasons,
		PrincipalRef:    principal,
		ActorFidelity:   fidelity,
		ExecutionID:     auditText(entry.ExecutionID),
		InvocationID:    auditText(entry.InterruptID),
		DecisionID:      strconv.FormatInt(id, 10),
		ResourceType:    resourceType,
		ResourceID:      auditText(responseRef),
		ToolName:        auditText(entry.Call.ToolName),
		ToolkitName:     auditText(firstNonEmpty(entry.Call.ToolkitName, entry.Call.ToolkitType)),
		ArgumentsDigest: digest,
	}); err != nil {
		return false, fmt.Errorf("mirror approval to the security audit outbox: %w", err)
	}
	return true, nil
}

// approvalEffect is the event effect of an outcome: allow when the call may
// run, deny when it may not, none for a wait.
func approvalEffect(outcome string) securityaudit.Effect {
	switch outcome {
	case ApprovalOutcomeApproved, ApprovalOutcomeEdited, ApprovalOutcomeAnswered:
		return securityaudit.EffectAllow
	case ApprovalOutcomeRejected, ApprovalOutcomeTimedOut, ApprovalOutcomeCancelled, ApprovalOutcomeSuperseded,
		ApprovalOutcomeUnattendedFailed, ApprovalOutcomeUnattendedSkipped:
		return securityaudit.EffectDeny
	default:
		return ""
	}
}

// approvalOutcomeOfAction maps a decided action to its outcome.
func approvalOutcomeOfAction(action string) string {
	switch action {
	case "approve", "continue", "authorize":
		return ApprovalOutcomeApproved
	case "reject", "block_with_comment", "skip":
		return ApprovalOutcomeRejected
	case "edit":
		return ApprovalOutcomeEdited
	case "answer":
		return ApprovalOutcomeAnswered
	default:
		return ApprovalOutcomeOther
	}
}

// approvalCallOf reads the call of a pause: a root interrupt carries the
// fields at the top level and its arguments as `tool_args`; a 0157 card
// carries them under `display` and its arguments as `tool_args_json`.
func approvalCallOf(pause map[string]any) approvalCall {
	source := pause
	if display, ok := pause["display"].(map[string]any); ok {
		source = display
	}
	text := func(key string) string {
		value, _ := source[key].(string)
		if value == "" {
			value, _ = pause[key].(string)
		}
		return value
	}
	call := approvalCall{
		GuardrailType: text("guardrail_type"),
		ToolName:      text("tool_name"),
		ToolkitName:   text("toolkit_name"),
		ToolkitType:   text("toolkit_type"),
	}
	// The worker's digest covers the arguments before masking, so it is the
	// stronger evidence; without it the arguments the card shows are
	// digested in their canonical form.
	if digest := text("argument_digest"); argumentDigestShape.MatchString(digest) {
		call.ArgumentDigest = digest
		return call
	}
	if args, ok := source["tool_args"]; ok && args != nil {
		call.ArgumentDigest = canonicalArgumentDigest(args)
	} else if raw := text("tool_args_json"); raw != "" {
		var decoded any
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&decoded) == nil {
			call.ArgumentDigest = canonicalArgumentDigest(decoded)
		} else {
			sum := sha256.Sum256([]byte(raw))
			call.ArgumentDigest = "sha256:" + hex.EncodeToString(sum[:])
		}
	}
	return call
}

// canonicalArgumentDigest is "sha256:" and the SHA-256 of the canonical JSON
// of value: object keys sorted, no insignificant space.
func canonicalArgumentDigest(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	// Re-decode with UseNumber so a number keeps its written form.
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if decoder.Decode(&normalized) == nil {
		if again, err := json.Marshal(normalized); err == nil {
			encoded = again
		}
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// decodeApprovalPauses reads the pauses of a root pause projection
// (meta.hitl_interrupts, else meta.hitl_interrupt) by interrupt id.
func decodeApprovalPauses(raw []byte) (map[string]map[string]any, []string) {
	var metadata struct {
		Interrupts []map[string]any `json:"hitl_interrupts"`
		Interrupt  map[string]any   `json:"hitl_interrupt"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&metadata) != nil {
		return map[string]map[string]any{}, nil
	}
	pauses := metadata.Interrupts
	if len(pauses) == 0 && metadata.Interrupt != nil {
		pauses = []map[string]any{metadata.Interrupt}
	}
	byID := make(map[string]map[string]any, len(pauses))
	order := make([]string, 0, len(pauses))
	for _, pause := range pauses {
		id, _ := pause["interrupt_id"].(string)
		if id == "" {
			continue
		}
		if _, seen := byID[id]; !seen {
			order = append(order, id)
		}
		byID[id] = pause
	}
	return byID, order
}

// decodeApprovalPauseList reads a JSON array of root interrupts.
func decodeApprovalPauseList(raw []byte) (map[string]map[string]any, []string) {
	wrapped := append(append([]byte(`{"hitl_interrupts":`), raw...), '}')
	return decodeApprovalPauses(wrapped)
}

// executionTriggerOrigin reads how an execution started (shared 0140).
// An execution with no row is manual.
func executionTriggerOriginOf(ctx context.Context, tx sqlExecutor, executionID string) (string, error) {
	var origin string
	err := tx.QueryRow(ctx, `
SELECT trigger_origin FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 ORDER BY generation DESC LIMIT 1`, executionID).Scan(&origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return "manual", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the run's trigger origin: %w", err)
	}
	return origin, nil
}

// closeApprovalPauses records how the open pauses of one response ended
// without a decision: a Stop (cancelled), a regenerate or a new turn
// (superseded), a run that ended. A pause that already has a decision is
// left as it is. actorUserID 0 is Main.
func closeApprovalPauses(ctx context.Context, tx sqlExecutor, responseMessageID, outcome string, actorUserID int64, reason string) error {
	return closeApprovalPausesOn(ctx, tx, responseMessageID, "", outcome, actorUserID, reason)
}

// closeApprovalPausesOn is closeApprovalPauses for the pauses of one path
// only ("" is every path). The card ledger closes its own cards and never a
// root pause the response may also hold.
func closeApprovalPausesOn(ctx context.Context, tx sqlExecutor, responseMessageID, path, outcome string, actorUserID int64, reason string) error {
	if responseMessageID == "" || skipApprovalLedger(tx) {
		return nil
	}
	rows, err := tx.Query(ctx, `
SELECT pause.project_id, pause.execution_id, COALESCE(pause.conversation_uuid::text, ''), pause.interrupt_id,
       COALESCE(pause.interrupt_key, ''), pause.path, pause.trigger_origin, COALESCE(pause.guardrail_type, ''),
       COALESCE(pause.tool_name, ''), COALESCE(pause.toolkit_name, ''), COALESCE(pause.toolkit_type, ''),
       COALESCE(pause.argument_digest, '')
FROM elitea_runtime.approval_ledger AS pause
WHERE pause.response_message_id = $1 AND pause.phase = 'pause' AND ($2::text = '' OR pause.path = $2::text)
  AND NOT EXISTS (
      SELECT 1 FROM elitea_runtime.approval_ledger AS decided
      WHERE decided.execution_id = pause.execution_id AND decided.interrupt_id = pause.interrupt_id
        AND decided.phase = 'decision')
ORDER BY pause.id`, responseMessageID, path)
	if err != nil {
		return fmt.Errorf("read open approval pauses: %w", err)
	}
	var open []approvalEntry
	for rows.Next() {
		entry := approvalEntry{ResponseMessageID: responseMessageID}
		if err := rows.Scan(&entry.ProjectID, &entry.ExecutionID, &entry.ConversationUUID, &entry.InterruptID,
			&entry.InterruptKey, &entry.Path, &entry.TriggerOrigin, &entry.Call.GuardrailType, &entry.Call.ToolName,
			&entry.Call.ToolkitName, &entry.Call.ToolkitType, &entry.Call.ArgumentDigest); err != nil {
			rows.Close()
			return fmt.Errorf("read open approval pauses: %w", err)
		}
		open = append(open, entry)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read open approval pauses: %w", err)
	}
	rows.Close()
	for _, entry := range open {
		entry.Phase, entry.Outcome, entry.ActorUserID, entry.Reason = approvalPhaseDecision, outcome, actorUserID, reason
		if _, err := recordApproval(ctx, tx, entry); err != nil {
			return err
		}
		if err := finishUnattendedAction(ctx, tx, entry.ExecutionID, entry.InterruptID, "DONE"); err != nil {
			return err
		}
	}
	return nil
}

// finishUnattendedAction ends the open unattended intent of one wait.
func finishUnattendedAction(ctx context.Context, tx sqlExecutor, executionID, interruptID, state string) error {
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.unattended_approval_actions
SET state = $3, updated_at = clock_timestamp()
WHERE execution_id = $1 AND interrupt_id = $2 AND state IN ('QUEUED', 'WAITING')`,
		executionID, interruptID, state); err != nil {
		return fmt.Errorf("finish the unattended approval action: %w", err)
	}
	return nil
}

// boundedText answers value when it fits the column, else "".
func boundedText(value string, limit int) string {
	if len(value) > limit || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00") {
		return ""
	}
	return value
}

// auditText fits value into a security audit string field (256 bytes, no
// control characters). A longer or unprintable value is its digest.
func auditText(value string) string {
	if value == "" {
		return ""
	}
	clean := len(value) <= 256 && utf8.ValidString(value)
	if clean {
		for _, r := range value {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				clean = false
				break
			}
		}
	}
	if clean && !strings.HasPrefix(strings.ToLower(value), "bearer ") {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// reasonPart lower-cases value into a reason code fragment, or "".
func reasonPart(value string) string {
	value = strings.ToLower(value)
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '.' && r != '-' {
			return ""
		}
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// recordRootApprovalDecisions writes the decision rows of a root HITL
// continuation in its admission transaction, after the statement that
// consumed the pending set. A decision whose wait already has one is the
// racing approver that lost: the whole admission is refused with the typed
// conflict, and nothing of it commits.
//
// A continuation Main admitted for an unattended skip carries the run's
// automated trigger origin (a person's carries manual or api): its rows name
// Main and the outcome unattended_skipped.
func recordRootApprovalDecisions(
	ctx context.Context, tx sqlExecutor, turn agentexecutionapp.CurrentContinueTurn,
	row sqlcgen.ResumeCurrentAgentHITLRow, triggerOrigin string,
) error {
	var decisions []agentexecutionapp.CurrentHITLDecision
	if err := json.Unmarshal(turn.HITLDecisions, &decisions); err != nil {
		return fmt.Errorf("read the HITL decisions: %w", err)
	}
	pauses, _ := decodeApprovalPauses([]byte(row.PreviousMetadataJson))
	pausedExecution := ""
	if row.PreviousTaskID != nil {
		pausedExecution = *row.PreviousTaskID
	}
	if pausedExecution == "" {
		pausedExecution = "response:" + turn.ResponseMessageID
	}
	origin, err := executionTriggerOriginOf(ctx, tx, pausedExecution)
	if err != nil {
		return err
	}
	system := unattendedOrigin(triggerOrigin)
	for _, decision := range decisions {
		entry := approvalEntry{
			ProjectID: turn.ProjectID, ExecutionID: pausedExecution, ResponseMessageID: turn.ResponseMessageID,
			ConversationUUID: turn.ConversationUUID, InterruptID: decision.InterruptID, Path: approvalPathRoot,
			Phase: approvalPhaseDecision, Outcome: approvalOutcomeOfAction(decision.Action),
			Action: decision.Action, ActorUserID: turn.ActorUserID, TriggerOrigin: origin,
			Call: approvalCallOf(pauses[decision.InterruptID]),
		}
		if system {
			entry.Outcome, entry.ActorUserID, entry.Reason = ApprovalOutcomeUnattendedSkipped, 0, approvalSkippedReason
		}
		inserted, err := recordApproval(ctx, tx, entry)
		if err != nil {
			return err
		}
		if !inserted {
			return agentexecutionapp.ErrCurrentAgentHITLAlreadyResolved
		}
		if err := finishUnattendedAction(ctx, tx, pausedExecution, decision.InterruptID, "DONE"); err != nil {
			return err
		}
	}
	return nil
}
