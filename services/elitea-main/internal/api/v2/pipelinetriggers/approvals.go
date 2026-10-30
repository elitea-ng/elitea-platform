package pipelinetriggers

// Unattended approvals (ADR-0041 decision 5, #1491).
//
// A schedule or a trigger run has no person who can approve a sensitive tool
// or a review node. Such a run never approves on its own. It takes its
// `on_approval_required` action:
//
//   - fail: the run ends with approval_required_unattended;
//   - skip: the step is refused, and the model reads that it was skipped;
//   - notify_and_wait: the run pauses, its owner is notified, and the pause
//     expires as approval_timed_out.
//
// The schedule or the trigger sets the action (elitea_runtime.
// unattended_approval_settings). With no setting, the platform default
// applies (platform config approvals.unattended_default, default fail).
// Admission copies the effective action into elitea_runtime.
// unattended_run_policies before the run starts, so a later change does not
// change a run in flight.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

const (
	approvalTargetSchedule = "schedule"
	approvalTargetTrigger  = "trigger"
)

// errInvalidOnApprovalRequired refuses a body whose on_approval_required is
// not fail, skip or notify_and_wait.
var errInvalidOnApprovalRequired = errors.New("pipelinetriggers: on_approval_required is not valid")

// onApprovalRequiredMessage is the 400 text for errInvalidOnApprovalRequired.
const onApprovalRequiredMessage = "`on_approval_required` must be `fail`, `skip` or `notify_and_wait`"

// parseOnApprovalRequired reads `on_approval_required` from a settings body.
// An absent field (or an absent body) keeps the stored value: it returns "".
func parseOnApprovalRequired(raw []byte) (platformconfig.OnApprovalRequired, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", nil
	}
	field, present := probe["on_approval_required"]
	if !present || string(field) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(field, &value); err != nil {
		return "", errInvalidOnApprovalRequired
	}
	action := platformconfig.OnApprovalRequired(strings.TrimSpace(value))
	if !action.Valid() {
		return "", errInvalidOnApprovalRequired
	}
	return action, nil
}

// saveOnApprovalRequired stores the schedule's or the trigger's action on the
// caller's transaction, the one that writes the schedule or the trigger row:
// both commit or neither does. A database without shared 0211 (a package
// test that applies the tenant corpus only) has nowhere to store it, as
// effectiveOnApprovalRequired reads: the save is skipped. The table is
// probed first, because a failed statement would abort the transaction.
func saveOnApprovalRequired(
	ctx context.Context, tx pgx.Tx, projectID int64, target string, versionID, actorID int64,
	action platformconfig.OnApprovalRequired,
) error {
	if action == "" {
		return nil
	}
	var present bool
	if err := tx.QueryRow(ctx,
		`SELECT to_regclass('elitea_runtime.unattended_approval_settings') IS NOT NULL`).Scan(&present); err != nil {
		return fmt.Errorf("pipelinetriggers: save on_approval_required: %w", err)
	}
	if !present {
		return nil
	}
	var updatedBy any
	if actorID > 0 {
		updatedBy = actorID
	}
	_, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.unattended_approval_settings
    (project_id, target, version_id, on_approval_required, updated_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (project_id, target, version_id) DO UPDATE
SET on_approval_required = EXCLUDED.on_approval_required,
    updated_by = EXCLUDED.updated_by,
    updated_at = clock_timestamp()`,
		projectID, target, versionID, string(action), updatedBy)
	if err != nil {
		return fmt.Errorf("pipelinetriggers: save on_approval_required: %w", err)
	}
	return nil
}

// deleteOnApprovalRequired removes the schedule's action with the schedule.
func (h *Handler) deleteOnApprovalRequired(ctx context.Context, projectID int64, target string, versionID int64) error {
	_, err := h.pool.Exec(ctx, `
DELETE FROM elitea_runtime.unattended_approval_settings
WHERE project_id = $1 AND target = $2 AND version_id = $3`, projectID, target, versionID)
	if err != nil {
		return fmt.Errorf("pipelinetriggers: delete on_approval_required: %w", err)
	}
	return nil
}

// effectiveOnApprovalRequired is the stored action, else the platform
// default. A read failure answers fail, the action that never lets a step
// run unapproved.
func (h *Handler) effectiveOnApprovalRequired(
	ctx context.Context, projectID int64, target string, versionID int64,
) (platformconfig.OnApprovalRequired, error) {
	var stored string
	err := h.pool.QueryRow(ctx, `
SELECT on_approval_required FROM elitea_runtime.unattended_approval_settings
WHERE project_id = $1 AND target = $2 AND version_id = $3`, projectID, target, versionID).Scan(&stored)
	switch {
	case platformconfig.UndefinedTable(err):
		// A database without shared 0211 (a package test that applies the
		// tenant corpus only): no stored setting.
	case err == nil:
		if action := platformconfig.OnApprovalRequired(stored); action.Valid() {
			return action, nil
		}
		return platformconfig.OnApprovalRequiredFail, nil
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return platformconfig.OnApprovalRequiredFail,
			fmt.Errorf("pipelinetriggers: read on_approval_required: %w", err)
	}
	settings, err := platformconfig.LoadApprovalsSettings(ctx, h.pool)
	if err != nil {
		return platformconfig.OnApprovalRequiredFail,
			fmt.Errorf("pipelinetriggers: read approvals settings: %w", err)
	}
	return settings.UnattendedDefault, nil
}

// viewOnApprovalRequired is the value a settings read shows. A read failure
// shows fail, which is also what a run would do.
func (h *Handler) viewOnApprovalRequired(ctx context.Context, projectID int64, target string, versionID int64) string {
	action, err := h.effectiveOnApprovalRequired(ctx, projectID, target, versionID)
	if err != nil {
		h.log().Warn("pipelinetriggers: read on_approval_required", "err", err)
	}
	return string(action)
}

// approvalTargetOf maps a run origin to its settings target.
func approvalTargetOf(origin string) string {
	if origin == OriginSchedule {
		return approvalTargetSchedule
	}
	return approvalTargetTrigger
}

// recordRunApprovalPolicy writes the run's snapshot before the run starts.
// An error refuses the run: a run without a snapshot would fall back to the
// platform default, not to the action its schedule or trigger set.
func (h *Handler) recordRunApprovalPolicy(ctx context.Context, request runRequest, conversationUUID string) error {
	if request.ActorUserID <= 0 {
		// No owner to notify or to continue as: Main applies the platform
		// default, read when the pause happens.
		return nil
	}
	action, err := h.effectiveOnApprovalRequired(ctx, request.ProjectID, approvalTargetOf(request.Origin), request.VersionID)
	if err != nil {
		return err
	}
	_, err = h.pool.Exec(ctx, `
INSERT INTO elitea_runtime.unattended_run_policies
    (project_id, conversation_uuid, trigger_origin, version_id, owner_user_id, on_approval_required)
VALUES ($1, $2::uuid, $3, $4, $5, $6)
ON CONFLICT (project_id, conversation_uuid) DO NOTHING`,
		request.ProjectID, conversationUUID, string(executionTriggerOrigin(request.Origin)),
		request.VersionID, request.ActorUserID, string(action))
	if platformconfig.UndefinedTable(err) {
		// Without shared 0211 there is no snapshot; Main then reads the
		// platform default (fail) when the run pauses.
		return nil
	}
	if err != nil {
		return fmt.Errorf("pipelinetriggers: record run approval policy: %w", err)
	}
	return nil
}
