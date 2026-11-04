package agentexecution

// A schedule's or a webhook trigger's turn on a handed-off workspace thread
// (ADR-0045 decision 9, handoffcontinue.StartThreadTurn).
//
// Two facts of such a turn are written by its own admission transaction,
// after the CLOUD_IDLE fence:
//
//   - The run's on_approval_required snapshot (ADR-0041 decision 5). The
//     thread keeps one unattended_run_policies row, and each started turn
//     writes its own snapshot over the previous one. An occurrence that the
//     fence refuses writes nothing, so it cannot change the snapshot of a
//     turn that runs; and a turn that starts has its snapshot from its first
//     moment.
//   - The question's `trigger_id` or `schedule_id` meta, which the trigger's
//     run limit counts (pipelinetriggers, threadTriggerAdmissible).

import (
	"encoding/json"

	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// ThreadRun is what a schedule's or a trigger's turn on a handed-off thread
// records with its admission. Set only by handoffcontinue; never from a
// request body.
type ThreadRun struct {
	// TriggerID or ScheduleID is the row that started the turn; the other
	// is 0.
	TriggerID  int64
	ScheduleID int64
	// Policy is the run's on_approval_required snapshot; nil leaves the
	// thread's row as it is (the platform default applies when none).
	Policy *RunPolicy
}

// RunPolicy is one unattended_run_policies row.
type RunPolicy struct {
	VersionID          int64
	OwnerUserID        int64
	OnApprovalRequired platformconfig.OnApprovalRequired
}

// UnattendedRunPolicy is the snapshot an admission writes
// (SubmitRequest.RunPolicy).
type UnattendedRunPolicy struct {
	ProjectID          int64
	ConversationUUID   string
	TriggerOrigin      string
	VersionID          int64
	OwnerUserID        int64
	OnApprovalRequired string
}

// Clone copies the snapshot.
func (p *UnattendedRunPolicy) Clone() *UnattendedRunPolicy {
	if p == nil {
		return nil
	}
	clone := *p
	return &clone
}

// valid: a thread run belongs to a schedule or a webhook, names its row, and
// a policy names its owner and a known action.
func (r *ThreadRun) valid(origin executiondomain.TriggerOrigin) bool {
	if r == nil {
		return true
	}
	if (origin != executiondomain.TriggerOriginSchedule && origin != executiondomain.TriggerOriginWebhook) ||
		r.TriggerID < 0 || r.ScheduleID < 0 || (r.TriggerID > 0) == (r.ScheduleID > 0) {
		return false
	}
	return r.Policy == nil || (r.Policy.OwnerUserID > 0 && r.Policy.VersionID >= 0 && r.Policy.OnApprovalRequired.Valid())
}

// snapshot is the admission's policy row for the thread.
func (r *ThreadRun) snapshot(
	projectID int64, conversationUUID string, origin executiondomain.TriggerOrigin,
) *UnattendedRunPolicy {
	if r == nil || r.Policy == nil {
		return nil
	}
	return &UnattendedRunPolicy{
		ProjectID: projectID, ConversationUUID: conversationUUID, TriggerOrigin: string(origin.Stored()),
		VersionID: r.Policy.VersionID, OwnerUserID: r.Policy.OwnerUserID,
		OnApprovalRequired: string(r.Policy.OnApprovalRequired),
	}
}

// questionMeta adds the row that started the turn to the question's meta.
func (r *ThreadRun) questionMeta(meta json.RawMessage) json.RawMessage {
	if r == nil {
		return meta
	}
	fields := map[string]any{}
	if err := json.Unmarshal(meta, &fields); err != nil {
		fields = map[string]any{}
	}
	if r.TriggerID > 0 {
		fields["trigger_id"] = r.TriggerID
	}
	if r.ScheduleID > 0 {
		fields["schedule_id"] = r.ScheduleID
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return meta
	}
	return encoded
}
