// Package retention is the retention policy per data class and the legal hold
// of ADR-0042 decision 4 (issue #1495).
//
// A PLATFORM policy per data class sets the bounds: MinAgeDays is the shortest
// maximum a project override may choose, MaxAgeDays is the platform's maximum
// age and the ceiling of every override (nil = keep forever). A PROJECT
// override sets only its own MaxAgeDays, inside those bounds, and only for a
// class whose platform row allows overrides.
//
// The rules spanning two rows live here, not in a database CHECK: the
// elitea_runtime.retention_policy table (shared 0195) holds what one row can
// state on its own.
//
// What enforces each class:
//
//   - conversation: elitea-main's conversation retention routine
//     (internal/application/conversationretention), by last message time.
//   - attachment: no separate sweep. Attachments are deleted with their
//     conversation. A non-null attachment maximum is refused until a sweep
//     enforces it, so the admin page cannot show a limit nothing applies.
//   - gateway_request_log: the LLM gateway's hourly prune reads the platform
//     row (services/elitea-llm-gateway/internal/requestlog).
//   - admin_audit, security_audit: elitea-scheduler's audit sweepers, once an
//     administrator has saved the row.
//   - step_content, step_record: reserved; refused until their data exists.
package retention

import (
	"errors"
	"fmt"
	"time"
)

// DataClass names one kind of retained data.
type DataClass string

// The data classes. The order of Classes is the admin page's order.
const (
	ClassConversation      DataClass = "conversation"
	ClassAttachment        DataClass = "attachment"
	ClassStepContent       DataClass = "step_content"
	ClassStepRecord        DataClass = "step_record"
	ClassGatewayRequestLog DataClass = "gateway_request_log"
	ClassAdminAudit        DataClass = "admin_audit"
	ClassSecurityAudit     DataClass = "security_audit"
)

// Classes are the data classes a policy can be stored for today. The reserved
// step classes are not in it.
var Classes = []DataClass{
	ClassConversation, ClassAttachment, ClassGatewayRequestLog, ClassAdminAudit, ClassSecurityAudit,
}

// MaxAgeCeilingDays bounds every age at 100 years: a larger number is a typo,
// and it would overflow nothing but would read as "forever" while not being
// NULL.
const MaxAgeCeilingDays = 36500

// classRules is what each class allows beyond the common bounds.
type classRules struct {
	// floorDays is the shortest maximum the enforcing sweeper accepts. Below
	// it the sweeper refuses (the audit sweepers' own floors).
	floorDays int
	// forever reports whether a NULL maximum (keep forever) is allowed.
	forever bool
	// overridable reports whether a project override may exist at all.
	overridable bool
	// enforced reports whether anything enforces a maximum yet.
	enforced bool
}

var rules = map[DataClass]classRules{
	ClassConversation:      {floorDays: 1, forever: true, overridable: true, enforced: true},
	ClassAttachment:        {floorDays: 1, forever: true, overridable: false, enforced: false},
	ClassGatewayRequestLog: {floorDays: 1, forever: false, overridable: false, enforced: true},
	ClassAdminAudit:        {floorDays: 7, forever: true, overridable: false, enforced: true},
	ClassSecurityAudit:     {floorDays: 1, forever: true, overridable: false, enforced: true},
}

// Validation errors. ErrInvalid wraps every refusal so a handler can map the
// family to 400 with one check.
var (
	ErrInvalid        = errors.New("invalid retention policy")
	ErrUnknownClass   = fmt.Errorf("%w: unknown data class", ErrInvalid)
	ErrOutOfBounds    = fmt.Errorf("%w: outside the platform bounds", ErrInvalid)
	ErrNoOverride     = fmt.Errorf("%w: this data class takes no project override", ErrInvalid)
	ErrNotEnforced    = fmt.Errorf("%w: no sweep enforces a maximum for this data class yet", ErrInvalid)
	ErrForeverRefused = fmt.Errorf("%w: this data class must have a maximum age", ErrInvalid)
)

// Policy is one row of elitea_runtime.retention_policy.
type Policy struct {
	Class                DataClass
	ProjectID            *int64 // nil on the platform row
	MinAgeDays           int
	MaxAgeDays           *int // nil = keep forever
	AllowProjectOverride bool
	UpdatedBy            *int64 // nil = seeded, never edited
	UpdatedAt            time.Time
}

// Known reports whether class can carry a stored policy today.
func Known(class DataClass) bool {
	_, ok := rules[class]
	return ok
}

// Overridable reports whether class takes project overrides at all.
func Overridable(class DataClass) bool { return rules[class].overridable }

// FloorDays is the shortest maximum class accepts.
func FloorDays(class DataClass) int { return rules[class].floorDays }

// ValidatePlatform checks a platform row before it is stored.
func ValidatePlatform(p Policy) error {
	r, ok := rules[p.Class]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownClass, p.Class)
	}
	if p.ProjectID != nil {
		return fmt.Errorf("%w: a platform policy has no project", ErrInvalid)
	}
	if p.MinAgeDays < 0 || p.MinAgeDays > MaxAgeCeilingDays {
		return fmt.Errorf("%w: min_age_days must be between 0 and %d", ErrInvalid, MaxAgeCeilingDays)
	}
	if p.AllowProjectOverride && !r.overridable {
		return fmt.Errorf("%w: %s", ErrNoOverride, p.Class)
	}
	if p.MaxAgeDays == nil {
		if !r.forever {
			return fmt.Errorf("%w: %s", ErrForeverRefused, p.Class)
		}
		return nil
	}
	if !r.enforced {
		return fmt.Errorf("%w: %s (attachments are deleted with their conversation)", ErrNotEnforced, p.Class)
	}
	maxDays := *p.MaxAgeDays
	if maxDays < r.floorDays || maxDays > MaxAgeCeilingDays {
		return fmt.Errorf("%w: max_age_days for %s must be between %d and %d",
			ErrInvalid, p.Class, r.floorDays, MaxAgeCeilingDays)
	}
	if maxDays < p.MinAgeDays {
		return fmt.Errorf("%w: max_age_days is below min_age_days", ErrInvalid)
	}
	return nil
}

// ValidateOverride checks a project override against its platform row.
// override.MaxAgeDays nil means "keep forever", which only fits a platform
// row that also keeps forever.
func ValidateOverride(platform, override Policy) error {
	r, ok := rules[override.Class]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownClass, override.Class)
	}
	if override.ProjectID == nil || *override.ProjectID <= 0 {
		return fmt.Errorf("%w: a project override needs a project", ErrInvalid)
	}
	if platform.Class != override.Class || platform.ProjectID != nil {
		return fmt.Errorf("%w: override checked against the wrong platform row", ErrInvalid)
	}
	if !r.overridable || !platform.AllowProjectOverride {
		return fmt.Errorf("%w: %s", ErrNoOverride, override.Class)
	}
	if override.MaxAgeDays == nil {
		if platform.MaxAgeDays != nil {
			return fmt.Errorf("%w: keep forever exceeds the platform maximum of %d days",
				ErrOutOfBounds, *platform.MaxAgeDays)
		}
		return nil
	}
	maxDays := *override.MaxAgeDays
	floor := max(platform.MinAgeDays, r.floorDays)
	if maxDays < floor {
		return fmt.Errorf("%w: %d days is below the platform minimum of %d", ErrOutOfBounds, maxDays, floor)
	}
	if platform.MaxAgeDays != nil && maxDays > *platform.MaxAgeDays {
		return fmt.Errorf("%w: %d days exceeds the platform maximum of %d", ErrOutOfBounds, maxDays, *platform.MaxAgeDays)
	}
	if maxDays > MaxAgeCeilingDays {
		return fmt.Errorf("%w: max_age_days must be at most %d", ErrInvalid, MaxAgeCeilingDays)
	}
	return nil
}

// Effective is the maximum age that applies to one project: the override when
// one is stored and still inside the platform bounds, else the platform's.
// nil means keep forever.
//
// An override that a later platform edit left outside the bounds is CLAMPED
// to them here rather than trusted: the platform administrator's latest word
// wins, so tightening the platform maximum takes effect for every project
// without editing each override.
func Effective(platform Policy, override *Policy) *int {
	if override == nil || !platform.AllowProjectOverride || !Overridable(platform.Class) {
		return platform.MaxAgeDays
	}
	if override.MaxAgeDays == nil {
		return platform.MaxAgeDays
	}
	days := *override.MaxAgeDays
	floor := max(platform.MinAgeDays, FloorDays(platform.Class))
	if days < floor {
		days = floor
	}
	if platform.MaxAgeDays != nil && days > *platform.MaxAgeDays {
		days = *platform.MaxAgeDays
	}
	return &days
}

// HoldScope is what a legal hold covers.
type HoldScope string

// The hold scopes.
const (
	HoldProject HoldScope = "project"
	HoldAgent   HoldScope = "agent"
	HoldUser    HoldScope = "user"
)

// Hold is one row of elitea_runtime.legal_holds.
type Hold struct {
	ID            int64
	Scope         HoldScope
	ProjectID     *int64
	AgentID       *int64
	UserID        *int64
	Reason        string
	CreatedBy     int64
	CreatedAt     time.Time
	ReleasedBy    *int64
	ReleasedAt    *time.Time
	ReleaseReason *string
}

// Active reports whether the hold still suspends deletion.
func (h Hold) Active() bool { return h.ReleasedAt == nil }

// ValidateHold checks a new hold before it is stored.
func ValidateHold(h Hold) error {
	if len(h.Reason) == 0 || len(h.Reason) > 2000 {
		return fmt.Errorf("%w: a hold needs a reason of 1 to 2000 characters", ErrInvalid)
	}
	positive := func(v *int64) bool { return v != nil && *v > 0 }
	switch h.Scope {
	case HoldProject:
		if !positive(h.ProjectID) || h.AgentID != nil || h.UserID != nil {
			return fmt.Errorf("%w: a project hold names project_id only", ErrInvalid)
		}
	case HoldAgent:
		if !positive(h.ProjectID) || !positive(h.AgentID) || h.UserID != nil {
			return fmt.Errorf("%w: an agent hold names project_id and agent_id", ErrInvalid)
		}
	case HoldUser:
		if !positive(h.UserID) || h.ProjectID != nil || h.AgentID != nil {
			return fmt.Errorf("%w: a user hold names user_id only", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: hold scope must be project, agent or user", ErrInvalid)
	}
	return nil
}
