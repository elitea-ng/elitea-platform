package admin

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// The approvals section (ADR-0041 decision 5, #1491). It is LIVE: a schedule
// or trigger run reads it when it is admitted, and the projection of a pause
// reads it for a run that has no stored choice. Nothing caches it, so a save
// applies to the next run.

// approvalsSection holds what an unattended run does at a step that needs an
// approval. It uses the permission of the other runtime governance sections,
// so no migration seeds a new one.
func approvalsSection() map[string]any {
	const section = platformconfig.SectionApprovals
	defaults := platformconfig.DefaultApprovalsSettings()
	return map[string]any{
		"id":    section,
		"title": "Approvals",
		"description": "What a schedule or trigger run does when a step needs an approval and no person is " +
			"there. It never approves the step. fail ends the run with approval_required_unattended. skip " +
			"refuses the step, and the model reads that it was skipped. notify_and_wait pauses the run, " +
			"notifies its owner and ends the run with approval_timed_out when nobody answers in time. A " +
			"schedule or trigger can set its own action. Every pause, decision and timeout is recorded in " +
			"the audit trail as agent.approval.*.",
		"order":               7,
		"icon":                "approval",
		"always_visible":      true,
		"required_permission": "configuration.governance",
		"fields": []map[string]any{
			{
				"key":   platformconfig.KeyApprovalsUnattendedDefault,
				"type":  "string",
				"title": "Unattended Default",
				"description": "The action of a schedule or trigger that sets none. fail (the default) is the " +
					"safe choice for a regulated deployment.",
				"section": section,
				"default": string(defaults.UnattendedDefault),
				"enum": []string{
					string(platformconfig.OnApprovalRequiredFail),
					string(platformconfig.OnApprovalRequiredSkip),
					string(platformconfig.OnApprovalRequiredNotifyAndWait),
				},
			},
			{
				"key":   platformconfig.KeyApprovalsUnattendedWaitHours,
				"type":  "integer",
				"title": "Unattended Wait (hours)",
				"description": fmt.Sprintf("How long notify_and_wait waits for the owner before the run ends "+
					"with approval_timed_out. From %d to %d; the default is %d.",
					platformconfig.MinApprovalsUnattendedWaitHours, platformconfig.MaxApprovalsUnattendedWaitHours,
					platformconfig.DefaultApprovalsUnattendedWaitHours),
				"section": section,
				"default": platformconfig.DefaultApprovalsUnattendedWaitHours,
				"minimum": platformconfig.MinApprovalsUnattendedWaitHours,
				"maximum": platformconfig.MaxApprovalsUnattendedWaitHours,
			},
		},
	}
}

// validateApprovalsValues applies the action vocabulary and the wait range
// the schema cannot enforce (JSON numbers arrive as float64, so integrality
// too).
func validateApprovalsValues(section string, values map[string]any) string {
	if section != platformconfig.SectionApprovals {
		return ""
	}
	if raw, present := values[platformconfig.KeyApprovalsUnattendedDefault]; present {
		action, _ := raw.(string)
		if !platformconfig.OnApprovalRequired(action).Valid() {
			return fmt.Sprintf("%q must be fail, skip or notify_and_wait", platformconfig.KeyApprovalsUnattendedDefault)
		}
	}
	if raw, present := values[platformconfig.KeyApprovalsUnattendedWaitHours]; present &&
		!platformconfig.ValidApprovalsWaitHours(raw) {
		return fmt.Sprintf("%q must be a whole number from %d to %d", platformconfig.KeyApprovalsUnattendedWaitHours,
			platformconfig.MinApprovalsUnattendedWaitHours, platformconfig.MaxApprovalsUnattendedWaitHours)
	}
	return ""
}
