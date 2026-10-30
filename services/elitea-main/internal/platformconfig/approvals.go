package platformconfig

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SectionApprovals holds the platform's approval settings for unattended runs
// (ADR-0041 decision 5, #1491). A schedule or a trigger run has no person to
// approve a sensitive tool or a review node. It never approves on its own:
// it takes its on_approval_required action, which the schedule or trigger
// sets, else unattended_default.
const SectionApprovals = "approvals"

// Field keys of SectionApprovals.
const (
	KeyApprovalsUnattendedDefault   = "unattended_default"
	KeyApprovalsUnattendedWaitHours = "unattended_wait_hours"
)

// OnApprovalRequired is what an unattended run does at a step that needs an
// approval. There is no auto-approve value.
type OnApprovalRequired string

const (
	// OnApprovalRequiredFail ends the run with approval_required_unattended.
	OnApprovalRequiredFail OnApprovalRequired = "fail"
	// OnApprovalRequiredSkip refuses the step: the model reads a tool result
	// that says the step was skipped because no approver was there.
	OnApprovalRequiredSkip OnApprovalRequired = "skip"
	// OnApprovalRequiredNotifyAndWait pauses the run, notifies its owner and
	// ends it with approval_timed_out when nobody answers in time.
	OnApprovalRequiredNotifyAndWait OnApprovalRequired = "notify_and_wait"
)

// Valid reports whether o is one of the three actions.
func (o OnApprovalRequired) Valid() bool {
	switch o {
	case OnApprovalRequiredFail, OnApprovalRequiredSkip, OnApprovalRequiredNotifyAndWait:
		return true
	default:
		return false
	}
}

// Bounds of KeyApprovalsUnattendedWaitHours: how long notify_and_wait waits.
const (
	DefaultApprovalsUnattendedWaitHours = 24
	MinApprovalsUnattendedWaitHours     = 1
	MaxApprovalsUnattendedWaitHours     = 720
)

// ApprovalsSettings is the section.
type ApprovalsSettings struct {
	UnattendedDefault OnApprovalRequired
	UnattendedWait    time.Duration
}

// DefaultApprovalsSettings is a deployment that never saved the section:
// unattended runs fail at a step that needs an approval.
func DefaultApprovalsSettings() ApprovalsSettings {
	return ApprovalsSettings{
		UnattendedDefault: OnApprovalRequiredFail,
		UnattendedWait:    DefaultApprovalsUnattendedWaitHours * time.Hour,
	}
}

// ApprovalsSettingsFromValues is the overlay. An unknown action is fail, the
// safe default; a wait out of range is clamped into range.
func ApprovalsSettingsFromValues(values Values) ApprovalsSettings {
	settings := DefaultApprovalsSettings()
	if raw, present := values[KeyApprovalsUnattendedDefault]; present {
		action, _ := raw.(string)
		if parsed := OnApprovalRequired(strings.TrimSpace(action)); parsed.Valid() {
			settings.UnattendedDefault = parsed
		}
	}
	if raw, present := values[KeyApprovalsUnattendedWaitHours]; present {
		hours, ok := raw.(float64)
		switch {
		case !ok || math.IsNaN(hours):
			hours = DefaultApprovalsUnattendedWaitHours
		case hours < MinApprovalsUnattendedWaitHours:
			hours = MinApprovalsUnattendedWaitHours
		case hours > MaxApprovalsUnattendedWaitHours:
			hours = MaxApprovalsUnattendedWaitHours
		}
		settings.UnattendedWait = time.Duration(math.Trunc(hours)) * time.Hour
	}
	return settings
}

// ValidApprovalsWaitHours reports whether a saved value is a whole number of
// hours from MinApprovalsUnattendedWaitHours to MaxApprovalsUnattendedWaitHours.
func ValidApprovalsWaitHours(raw any) bool {
	hours, ok := raw.(float64)
	return ok && hours == math.Trunc(hours) &&
		hours >= MinApprovalsUnattendedWaitHours && hours <= MaxApprovalsUnattendedWaitHours
}

// LoadApprovalsSettings reads the section. A database without the table
// never saved it and answers the defaults.
func LoadApprovalsSettings(ctx context.Context, pool *pgxpool.Pool) (ApprovalsSettings, error) {
	values, err := Load(ctx, pool, SectionApprovals)
	if UndefinedTable(err) {
		return DefaultApprovalsSettings(), nil
	}
	if err != nil {
		return DefaultApprovalsSettings(), err
	}
	return ApprovalsSettingsFromValues(values), nil
}
