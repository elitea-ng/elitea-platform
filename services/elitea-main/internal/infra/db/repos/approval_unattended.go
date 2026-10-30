package repos

// Unattended runs and approvals (ADR-0041 decision 5, issue #1491; shared
// 0211).
//
// A run a schedule or an inbound trigger started (execution_jobs.
// trigger_origin schedule or webhook) has no person to approve a step. When
// it pauses for an approval, Main never approves the step. It takes the
// run's on_approval_required action, snapshotted at admission in
// unattended_run_policies (else the platform default approvals.
// unattended_default, whose default is fail):
//
//   - fail: the pause is recorded and ended at once as unattended_failed,
//     and the run ends with the typed error approval_required_unattended.
//   - skip: the pause is recorded and a QUEUED intent asks the unattended
//     approval worker to answer it with a refusal (block_with_comment with
//     the skip text when the pause offers it, else reject). The model reads
//     a tool result that says the step was skipped. A pause that offers no
//     refusal fails instead.
//   - notify_and_wait: the pause stays open, its owner gets a notification,
//     and a WAITING intent expires the pause as timed_out after
//     approvals.unattended_wait_hours.
//
// Each outcome is a ledger row and a security audit event.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// ApprovalNotificationEventType is the notification an unattended run's owner
// gets when the run waits for an approval.
const ApprovalNotificationEventType = "agent_approval_required"

// approvalSkipMessage is what the model reads for a skipped step.
const approvalSkipMessage = "Skipped: this run is unattended (started by a schedule or a trigger) and no " +
	"approver is available. The step was not run. Continue without it."

// approvalFailedMessage and approvalTimedOutMessage are the readable errors.
const (
	approvalFailedMessage = "The run stopped: a step needs an approval, and this run is unattended " +
		"(started by a schedule or a trigger). Its approval action is fail. (" + ApprovalRequiredUnattendedCode + ")"
	approvalTimedOutMessage = "The run stopped: a step waited for an approval, and nobody answered before the " +
		"wait expired. (" + ApprovalTimedOutCode + ")"
)

// unattendedPolicy is what one unattended run does at a step that needs an
// approval.
type unattendedPolicy struct {
	Action        platformconfig.OnApprovalRequired
	OwnerUserID   int64
	TriggerOrigin string
	Wait          time.Duration
}

// unattendedOrigin reports whether a trigger origin has no person behind it.
func unattendedOrigin(origin string) bool {
	return origin == "schedule" || origin == "webhook"
}

// loadApprovalsSettingsTx reads the approvals section on the caller's
// transaction. A database without the platform_config table (a corpus-only
// test database) answers the defaults without aborting the transaction.
func loadApprovalsSettingsTx(ctx context.Context, tx sqlExecutor) (platformconfig.ApprovalsSettings, error) {
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('centry.platform_config') IS NOT NULL`).Scan(&present); err != nil {
		return platformconfig.DefaultApprovalsSettings(), fmt.Errorf("read the approvals section: %w", err)
	}
	if !present {
		return platformconfig.DefaultApprovalsSettings(), nil
	}
	rows, err := tx.Query(ctx, `SELECT key, value FROM centry.platform_config WHERE section = $1`,
		platformconfig.SectionApprovals)
	if err != nil {
		return platformconfig.DefaultApprovalsSettings(), fmt.Errorf("read the approvals section: %w", err)
	}
	defer rows.Close()
	values := platformconfig.Values{}
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return platformconfig.DefaultApprovalsSettings(), fmt.Errorf("read the approvals section: %w", err)
		}
		var decoded any
		if json.Unmarshal(raw, &decoded) == nil {
			values[key] = decoded
		}
	}
	if err := rows.Err(); err != nil {
		return platformconfig.DefaultApprovalsSettings(), fmt.Errorf("read the approvals section: %w", err)
	}
	return platformconfig.ApprovalsSettingsFromValues(values), nil
}

// resolveUnattendedPolicy answers the policy of an execution, and false when
// a person started it (or the continuation a person sent).
func resolveUnattendedPolicy(
	ctx context.Context, tx sqlExecutor, projectID int64, executionID, conversationUUID string,
) (unattendedPolicy, bool, error) {
	var origin, actor string
	err := tx.QueryRow(ctx, `
SELECT trigger_origin, actor_id FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 ORDER BY generation DESC LIMIT 1`, executionID).Scan(&origin, &actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return unattendedPolicy{}, false, nil
	}
	if err != nil {
		return unattendedPolicy{}, false, fmt.Errorf("read the run's trigger origin: %w", err)
	}
	if !unattendedOrigin(origin) {
		return unattendedPolicy{}, false, nil
	}
	settings, err := loadApprovalsSettingsTx(ctx, tx)
	if err != nil {
		return unattendedPolicy{}, false, err
	}
	policy := unattendedPolicy{Action: settings.UnattendedDefault, TriggerOrigin: origin, Wait: settings.UnattendedWait}
	policy.OwnerUserID, _ = strconv.ParseInt(actor, 10, 64)
	if _, parseErr := uuid.Parse(conversationUUID); parseErr == nil {
		var action string
		var owner int64
		err = tx.QueryRow(ctx, `
SELECT on_approval_required, owner_user_id FROM elitea_runtime.unattended_run_policies
WHERE project_id = $1 AND conversation_uuid = $2::uuid`, projectID, conversationUUID).Scan(&action, &owner)
		switch {
		case err == nil:
			if parsed := platformconfig.OnApprovalRequired(action); parsed.Valid() {
				policy.Action = parsed
			}
			if owner > 0 {
				policy.OwnerUserID = owner
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return unattendedPolicy{}, false, fmt.Errorf("read the run's approval policy: %w", err)
		}
	}
	if !policy.Action.Valid() {
		policy.Action = platformconfig.OnApprovalRequiredFail
	}
	return policy, true, nil
}

// rootPauseOutcome tells the terminal projection how to finish a root pause.
type rootPauseOutcome int

const (
	// rootPauseWaits persists the pause as it always has.
	rootPauseWaits rootPauseOutcome = iota
	// rootPauseFailed ends the response with approval_required_unattended.
	rootPauseFailed
)

// projectRootApprovalPause records the pause rows of a root HITL pause on the
// terminal transaction, and applies the run's unattended action. mixed is a
// pause that also waits for an MCP sign-in, which only a person can give: an
// unattended mixed pause fails.
func projectRootApprovalPause(
	ctx context.Context, tx sqlExecutor, expected outputapp.ExpectedAgentExecution,
	interrupts json.RawMessage, mixed bool,
) (rootPauseOutcome, error) {
	if skipApprovalLedger(tx) {
		return rootPauseWaits, nil
	}
	projectID, err := parseProjectID(expected.ProjectionProjectID)
	if err != nil {
		return rootPauseWaits, outputapp.ErrAgentExecutionResultMismatch
	}
	pauses, order := decodeApprovalPauseList(interrupts)
	if len(order) == 0 {
		return rootPauseWaits, nil
	}
	policy, unattended, err := resolveUnattendedPolicy(ctx, tx, projectID, expected.ExecutionID, expected.ClientStreamID)
	if err != nil {
		return rootPauseWaits, err
	}
	origin := policy.TriggerOrigin
	if !unattended {
		if origin, err = executionTriggerOriginOf(ctx, tx, expected.ExecutionID); err != nil {
			return rootPauseWaits, err
		}
	}
	entries := make([]approvalEntry, 0, len(order))
	for _, id := range order {
		entry := approvalEntry{
			ProjectID: projectID, ExecutionID: expected.ExecutionID, ResponseMessageID: expected.ClientMessageID,
			ConversationUUID: expected.ClientStreamID, InterruptID: id, Path: approvalPathRoot,
			Phase: approvalPhasePause, Outcome: ApprovalOutcomePaused, TriggerOrigin: origin,
			Call: approvalCallOf(pauses[id]),
		}
		if _, err := recordApproval(ctx, tx, entry); err != nil {
			return rootPauseWaits, err
		}
		entries = append(entries, entry)
	}
	if !unattended {
		return rootPauseWaits, nil
	}
	action := policy.Action
	skipActions := make(map[string]string, len(order))
	if action == platformconfig.OnApprovalRequiredSkip {
		for _, id := range order {
			refusal := skipRefusal(availableActions(pauses[id]))
			if refusal == "" || mixed {
				action = platformconfig.OnApprovalRequiredFail // nothing to answer the pause with
				break
			}
			skipActions[id] = refusal
		}
	}
	if mixed {
		action = platformconfig.OnApprovalRequiredFail
	}
	switch action {
	case platformconfig.OnApprovalRequiredSkip:
		for _, entry := range entries {
			if err := insertUnattendedAction(ctx, tx, entry, policy, "skip", skipActions[entry.InterruptID]); err != nil {
				return rootPauseWaits, err
			}
		}
		return rootPauseWaits, nil
	case platformconfig.OnApprovalRequiredNotifyAndWait:
		for _, entry := range entries {
			notice := entry
			notice.Phase, notice.Outcome = approvalPhaseNotice, ApprovalOutcomeUnattendedWaiting
			if _, err := recordApproval(ctx, tx, notice); err != nil {
				return rootPauseWaits, err
			}
			if err := insertUnattendedAction(ctx, tx, entry, policy, "notify_and_wait", ""); err != nil {
				return rootPauseWaits, err
			}
		}
		return rootPauseWaits, notifyApprovalOwner(ctx, tx, entries[0], policy)
	default:
		for _, entry := range entries {
			failed := entry
			failed.Phase, failed.Outcome, failed.Reason = approvalPhaseDecision, ApprovalOutcomeUnattendedFailed,
				ApprovalRequiredUnattendedCode
			if _, err := recordApproval(ctx, tx, failed); err != nil {
				return rootPauseWaits, err
			}
		}
		return rootPauseFailed, nil
	}
}

// finalizeUnattendedApprovalFailure ends a root response with the typed
// error instead of the pause: nothing can answer it.
func finalizeUnattendedApprovalFailure(
	ctx context.Context, tx sqlExecutor, messageGroupID int64, threadID string, invokedSkills []byte,
	skipped json.RawMessage,
) error {
	tag, err := tx.Exec(ctx, `
UPDATE chat_message_group
SET is_streaming = FALSE,
    meta = (meta - 'hitl_interrupt' - 'hitl_interrupts' - 'authorization_requests')
        || jsonb_build_object(
            'thread_id', $2::text,
            'is_error', TRUE,
            'error', $3::text,
            'error_code', $4::text,
            'invoked_skills', $5::jsonb)
        || CASE WHEN jsonb_typeof($6::jsonb) = 'object'
            THEN jsonb_build_object('skipped_capabilities', $6::jsonb) ELSE '{}'::jsonb END,
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
WHERE id = $1`, messageGroupID, threadID, approvalFailedMessage, ApprovalRequiredUnattendedCode,
		string(invokedSkills), string(cloneJSONOrDefault(skipped, []byte("null"))))
	if err != nil || tag.RowsAffected() != 1 {
		return fmt.Errorf("end the unattended response that needs an approval: %w", terminalWriteError(err))
	}
	return nil
}

// availableActions reads a pause's available actions.
func availableActions(pause map[string]any) []string {
	raw, _ := pause["available_actions"].([]any)
	actions := make([]string, 0, len(raw))
	for _, value := range raw {
		if action, ok := value.(string); ok {
			actions = append(actions, action)
		}
	}
	return actions
}

// skipRefusal is the action an unattended skip answers a pause with:
// block_with_comment (the model reads why), else reject, else none.
func skipRefusal(actions []string) string {
	switch {
	case slices.Contains(actions, "block_with_comment"):
		return "block_with_comment"
	case slices.Contains(actions, "reject"):
		return "reject"
	default:
		return ""
	}
}

// insertUnattendedAction records the intent of a skip or a wait.
func insertUnattendedAction(
	ctx context.Context, tx sqlExecutor, entry approvalEntry, policy unattendedPolicy, action, skipAction string,
) error {
	if _, err := uuid.Parse(entry.ConversationUUID); policy.OwnerUserID <= 0 || err != nil || entry.ResponseMessageID == "" {
		return errors.New("unattended approval intent has no owner or response")
	}
	state, waitSeconds := "QUEUED", any(nil)
	var refusal any
	if action == "notify_and_wait" {
		state, waitSeconds = "WAITING", int64(math.Max(policy.Wait.Seconds(), 3600))
	} else {
		refusal = skipAction
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.unattended_approval_actions (
    execution_id, interrupt_id, project_id, response_message_id, conversation_uuid, path, action,
    owner_user_id, trigger_origin, skip_action, state, expires_at)
VALUES ($1, $2, $3, $4, $5::uuid, $6, $7, $8, $9, $10, $11,
        CASE WHEN $12::bigint IS NULL THEN NULL ELSE clock_timestamp() + $12::bigint * interval '1 second' END)
ON CONFLICT (execution_id, interrupt_id) DO NOTHING`,
		entry.ExecutionID, entry.InterruptID, entry.ProjectID, entry.ResponseMessageID, entry.ConversationUUID,
		entry.Path, action, policy.OwnerUserID, policy.TriggerOrigin, refusal, state, waitSeconds); err != nil {
		return fmt.Errorf("record the unattended approval intent: %w", err)
	}
	return nil
}

// notifyApprovalOwner tells the run's owner that the run waits for them,
// through the notification bell (centry.notifications), in the pause's
// transaction. The uuid is derived from the wait, so a replay writes one.
func notifyApprovalOwner(ctx context.Context, tx sqlExecutor, entry approvalEntry, policy unattendedPolicy) error {
	if policy.OwnerUserID <= 0 || policy.OwnerUserID > math.MaxInt32 || entry.ProjectID > math.MaxInt32 {
		return nil
	}
	meta, err := json.Marshal(map[string]any{
		"conversation_uuid": entry.ConversationUUID,
		"message_id":        entry.ResponseMessageID,
		"execution_id":      entry.ExecutionID,
		"tool_name":         entry.Call.ToolName,
		"toolkit_name":      firstNonEmpty(entry.Call.ToolkitName, entry.Call.ToolkitType),
		"trigger_origin":    policy.TriggerOrigin,
		"wait_hours":        int64(policy.Wait / time.Hour),
		"message": "A " + policy.TriggerOrigin + " run waits for your approval. Open the conversation to approve " +
			"or reject the step before the wait expires.",
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type, created_at, updated_at)
VALUES ($1::uuid, FALSE, $2, $3, $4::jsonb, $5, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')
ON CONFLICT (uuid) DO NOTHING`, approvalNotificationUUID(entry.ExecutionID+"\x00"+entry.InterruptID),
		int32(entry.ProjectID), int32(policy.OwnerUserID), string(meta), ApprovalNotificationEventType); err != nil {
		return fmt.Errorf("notify the unattended run's owner: %w", err)
	}
	return nil
}

// approvalNotificationUUID derives a UUIDv4-shaped id from key, as
// reviewNotificationUUID does.
func approvalNotificationUUID(key string) string {
	digest := sha256.Sum256([]byte("elitea.approval-notification.v1\x00" + key))
	value := append([]byte(nil), digest[:16]...)
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// projectInterruptApprovalRaise records the pause of a raised per-interrupt
// card (shared 0157) and applies the run's unattended action, on the
// transaction that accepted the card's frame.
func projectInterruptApprovalRaise(
	ctx context.Context, tx sqlExecutor, input domain.RaiseInput, conversationUUID string,
) error {
	card, _, err := domain.ParseCard(input.Card)
	if err != nil {
		return err
	}
	var decoded map[string]any
	if err := json.Unmarshal(input.Card, &decoded); err != nil {
		return domain.ErrInvalidCard
	}
	policy, unattended, err := resolveUnattendedPolicy(ctx, tx, input.ProjectID, input.ExecutionID, conversationUUID)
	if err != nil {
		return err
	}
	origin := policy.TriggerOrigin
	if !unattended {
		if origin, err = executionTriggerOriginOf(ctx, tx, input.ExecutionID); err != nil {
			return err
		}
	}
	entry := approvalEntry{
		ProjectID: input.ProjectID, ExecutionID: input.ExecutionID, ResponseMessageID: input.RootResponseID,
		ConversationUUID: conversationUUID, InterruptID: card.InterruptID, InterruptKey: card.InterruptKey,
		Path: approvalPathInterrupt, Phase: approvalPhasePause, Outcome: ApprovalOutcomePaused,
		TriggerOrigin: origin, Call: approvalCallOf(decoded),
	}
	if _, err := recordApproval(ctx, tx, entry); err != nil || !unattended {
		return err
	}
	actions := make([]string, len(card.AvailableActions))
	for index, action := range card.AvailableActions {
		actions[index] = string(action)
	}
	action := policy.Action
	refusal := skipRefusal(actions)
	if action == platformconfig.OnApprovalRequiredSkip && refusal == "" {
		action = platformconfig.OnApprovalRequiredFail
	}
	switch action {
	case platformconfig.OnApprovalRequiredNotifyAndWait:
		notice := entry
		notice.Phase, notice.Outcome = approvalPhaseNotice, ApprovalOutcomeUnattendedWaiting
		if _, err := recordApproval(ctx, tx, notice); err != nil {
			return err
		}
		if err := insertUnattendedAction(ctx, tx, entry, policy, "notify_and_wait", ""); err != nil {
			return err
		}
		return notifyApprovalOwner(ctx, tx, entry, policy)
	case platformconfig.OnApprovalRequiredSkip:
		if err := systemDecideCard(ctx, tx, input.RootResponseID, card.InterruptKey, card.InterruptID, refusal,
			policy.OwnerUserID); err != nil {
			return err
		}
		skipped := entry
		skipped.Phase, skipped.Outcome, skipped.Action, skipped.Reason = approvalPhaseDecision,
			ApprovalOutcomeUnattendedSkipped, refusal, approvalSkippedReason
		_, err := recordApproval(ctx, tx, skipped)
		return err
	default:
		// The card stays open until the cancelled run closes it; the
		// decision row is written now, so that close changes nothing in
		// the ledger.
		failed := entry
		failed.Phase, failed.Outcome, failed.Reason = approvalPhaseDecision, ApprovalOutcomeUnattendedFailed,
			ApprovalRequiredUnattendedCode
		if _, err := recordApproval(ctx, tx, failed); err != nil {
			return err
		}
		return cancelRunForApproval(ctx, tx, input.ExecutionID, input.Generation, ApprovalRequiredUnattendedCode)
	}
}

// systemDecideCard decides a PENDING card with a refusal in the raise
// transaction (an unattended skip). The 0157 columns name a user, so
// decided_by is the run's owner, the identity the run executes as; the
// ledger row names Main (actor_kind system).
func systemDecideCard(ctx context.Context, tx sqlExecutor, rootResponseID, interruptKey, interruptID, action string, ownerUserID int64) error {
	if ownerUserID <= 0 {
		return errors.New("unattended skip has no run owner")
	}
	value := ""
	if action == "block_with_comment" {
		value = approvalSkipMessage
	}
	requestSum := sha256.Sum256([]byte("elitea.unattended-skip.v1\x00" + rootResponseID + "\x00" + interruptKey))
	body, err := json.Marshal(map[string]any{
		"request_id": hex.EncodeToString(requestSum[:]), "expected_revision": 1, "action": action, "value": value,
	})
	if err != nil {
		return err
	}
	decision, canonical, err := domain.ParseDecisionRequest(body)
	if err != nil {
		return err
	}
	var revision int64
	err = tx.QueryRow(ctx, `
WITH decided AS (
  UPDATE elitea_runtime.execution_interrupts
  SET state = 'DECIDED', revision = revision + 1, request_id = $3, decision_json = $4,
      decided_by = $5, decided_at = clock_timestamp(), updated_at = clock_timestamp()
  WHERE root_response_id = $1::uuid AND interrupt_key = $2 AND state = 'PENDING' AND revision = 1
  RETURNING root_response_id, interrupt_key, revision
), bumped AS (
  UPDATE elitea_runtime.execution_interrupt_responses response
  SET decision_revision = response.decision_revision + 1, updated_at = clock_timestamp()
  FROM decided WHERE response.root_response_id = decided.root_response_id
  RETURNING response.decision_revision
), audited AS (
  INSERT INTO elitea_runtime.execution_interrupt_audit (root_response_id, interrupt_key, transition, revision, actor_id)
  SELECT root_response_id, interrupt_key, 'DECIDED', revision, $5 FROM decided
)
SELECT decided.revision FROM decided, bumped`,
		rootResponseID, interruptKey, decision.RequestID, canonical, ownerUserID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrAlreadyResolved
	}
	if err != nil {
		return fmt.Errorf("decide the unattended card: %w", err)
	}
	projectID, err := interruptResponseProject(ctx, tx, rootResponseID)
	if err != nil {
		return err
	}
	decided := domain.Action(action)
	// The frame-accepting transaction has no project search_path, and the
	// resolved frame reads the conversation's readers from the tenant
	// tables: the project's path is installed for this one write only.
	return withProjectSearchPath(ctx, tx, projectID, func() error {
		_, emitErr := emitInterruptResolved(ctx, tx, projectID, rootResponseID, []domain.Resolved{{
			InterruptKey: interruptKey, InterruptID: interruptID, State: domain.StateDecided, Revision: revision,
			Action: &decided,
		}})
		return emitErr
	})
}

// withProjectSearchPath runs fn with the transaction-local search_path of
// the project (p_<id>), then restores the path the transaction had.
func withProjectSearchPath(ctx context.Context, tx sqlExecutor, projectID int64, fn func() error) error {
	schema, err := currentProjectSchema(projectID)
	if err != nil {
		return err
	}
	var previous, installed string
	if err := tx.QueryRow(ctx, `SELECT current_setting('search_path'), set_config('search_path', $1, true)`,
		schema).Scan(&previous, &installed); err != nil {
		return fmt.Errorf("install the project search_path: %w", err)
	}
	fnErr := fn()
	if fnErr != nil {
		// The transaction is failing; it rolls back with its settings.
		return fnErr
	}
	if err := tx.QueryRow(ctx, `SELECT set_config('search_path', $1, true)`, previous).Scan(&installed); err != nil {
		return fmt.Errorf("restore the search_path: %w", err)
	}
	return nil
}

// recordInterruptApprovalDecision writes the decision row of a per-interrupt
// card a person decided, in the Decide transaction. A row that already
// exists is the racing approver that lost: domain.ErrAlreadyResolved.
func recordInterruptApprovalDecision(
	ctx context.Context, tx sqlExecutor, projectID int64, rootResponseID, interruptKey, action string, actorUserID int64,
) error {
	var executionID, interruptID, origin string
	var conversationUUID *string
	var card []byte
	// One statement: the card, the pause's conversation and the run's
	// trigger origin (no job row is manual). The decision path has a
	// statement budget.
	err := tx.QueryRow(ctx, `
SELECT interrupt.execution_id, interrupt.interrupt_id, interrupt.card_json,
       (SELECT pause.conversation_uuid::text FROM elitea_runtime.approval_ledger AS pause
        WHERE pause.execution_id = interrupt.execution_id AND pause.interrupt_id = interrupt.interrupt_id
          AND pause.phase = 'pause'),
       COALESCE((SELECT job.trigger_origin FROM elitea_runtime.execution_jobs AS job
        WHERE job.execution_id = interrupt.execution_id ORDER BY job.generation DESC LIMIT 1), 'manual')
FROM elitea_runtime.execution_interrupts AS interrupt
WHERE interrupt.root_response_id = $1::uuid AND interrupt.interrupt_key = $2`,
		rootResponseID, interruptKey).Scan(&executionID, &interruptID, &card, &conversationUUID, &origin)
	if err != nil {
		return fmt.Errorf("read the decided card: %w", err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(card, &decoded)
	entry := approvalEntry{
		ProjectID: projectID, ExecutionID: executionID, ResponseMessageID: rootResponseID,
		InterruptID: interruptID, InterruptKey: interruptKey, Path: approvalPathInterrupt,
		Phase: approvalPhaseDecision, Outcome: approvalOutcomeOfAction(action), Action: action,
		ActorUserID: actorUserID, TriggerOrigin: origin, Call: approvalCallOf(decoded),
	}
	if conversationUUID != nil {
		entry.ConversationUUID = *conversationUUID
	}
	inserted, err := recordApproval(ctx, tx, entry)
	if err != nil {
		return err
	}
	if !inserted {
		return domain.ErrAlreadyResolved
	}
	return finishUnattendedAction(ctx, tx, executionID, interruptID, "DONE")
}

func interruptResponseProject(ctx context.Context, tx sqlExecutor, rootResponseID string) (int64, error) {
	var projectID int64
	if err := tx.QueryRow(ctx, `SELECT project_id FROM elitea_runtime.execution_interrupt_responses
WHERE root_response_id = $1::uuid`, rootResponseID).Scan(&projectID); err != nil {
		return 0, fmt.Errorf("read the card's project: %w", err)
	}
	return projectID, nil
}

// cancelRunForApproval stops a running unattended run with a typed reason
// (execution_jobs.cancel_reason, shared 0211). The worker observes it on its
// next lease renewal; the cancellation projection shows the typed error.
func cancelRunForApproval(ctx context.Context, tx sqlExecutor, executionID string, generation int64, reason string) error {
	if _, err := tx.Exec(ctx, `
UPDATE elitea_runtime.execution_jobs
SET desired_state = 'CANCELLED', cancel_reason = $3
WHERE execution_id = $1 AND ($2::bigint = 0 OR generation = $2)
  AND desired_state = 'RUNNING'
  AND state IN ('PENDING', 'DISPATCHED', 'CLAIMED', 'RUNNING', 'SETTLING')`,
		executionID, generation, reason); err != nil {
		return fmt.Errorf("stop the unattended run: %w", err)
	}
	return nil
}

// approvalCancelReason reads why Main cancelled an execution, when the
// reason is an approval outcome.
func approvalCancelReason(ctx context.Context, tx sqlExecutor, executionID string, generation uint64) (string, error) {
	var reason *string
	err := tx.QueryRow(ctx, `
SELECT cancel_reason FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = $2`, executionID, int64(generation)).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the run's cancel reason: %w", err)
	}
	if reason == nil || (*reason != ApprovalRequiredUnattendedCode && *reason != ApprovalTimedOutCode) {
		return "", nil
	}
	return *reason, nil
}

// approvalCancelBrowserData is the replay event of a run stopped for an
// approval.
func approvalCancelBrowserData(code string) ([]byte, error) {
	return json.Marshal(struct {
		Code        string `json:"code"`
		SafeMessage string `json:"safe_message"`
		Retryable   bool   `json:"retryable"`
	}{Code: code, SafeMessage: approvalMessageOf(code)})
}

func approvalMessageOf(code string) string {
	if code == ApprovalTimedOutCode {
		return approvalTimedOutMessage
	}
	return approvalFailedMessage
}

// presentApprovalCancelledAgentResponse marks the agent response of a run
// Main stopped for an approval, as presentCapReachedAgentResponse does for a
// cap.
func presentApprovalCancelledAgentResponse(ctx context.Context, tx sqlExecutor, record outputRecord, code string) error {
	schema, err := currentProjectSchema(record.ProjectionProjectID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s AS message_group
SET is_streaming = FALSE,
    meta = (message_group.meta - 'fanout_parked_v1') || jsonb_build_object(
        'is_error', TRUE,
        'error', $3::text,
        'error_code', $4::text
    ),
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
FROM elitea_runtime.agent_execution_jobs AS agent,
     %s AS conversation
WHERE agent.execution_id = $1
  AND agent.generation = $2
  AND message_group.uuid::text = agent.client_message_id
  AND conversation.id = message_group.conversation_id
  AND conversation.uuid::text = agent.client_stream_id`,
		schema+".chat_message_group",
		schema+".chat_conversations",
	), record.ExecutionID, int64(record.Generation), approvalMessageOf(code), code); err != nil {
		return fmt.Errorf("mark the agent response stopped for an approval: %w", err)
	}
	return nil
}
