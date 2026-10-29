// Package safety is the kill switch (ADR-0041 decision 1, issue #1493): one
// suspension model with four scopes, the admission check every run start
// passes through, and the cancel of the runs a new suspension covers.
//
//	platform      every run (the kill switch)
//	project       every run of one project
//	agent         every run of one agent (project id + application id)
//	toolkit_type  every call of one toolkit type, refused by the worker
//
// The rows live in elitea_runtime.execution_suspensions (shared 0194). The
// admission read is cached per replica (Gate) and invalidated over core NATS
// (elitea.rt.v1.safety.suspension), the same pattern as the security
// invalidation fan-out of ADR-0039.
//
// It is separate from maintenance mode. Maintenance closes the API to
// non-admins and pauses scheduled dispatch; it does not stop a run in
// flight. A suspension refuses new runs and cancels the runs in flight it
// covers.
package safety

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/guardrails"
)

// Scope is what a suspension covers.
type Scope string

const (
	ScopePlatform    Scope = "platform"
	ScopeProject     Scope = "project"
	ScopeAgent       Scope = "agent"
	ScopeToolkitType Scope = "toolkit_type"
)

// Valid reports whether s is one of the four scopes.
func (s Scope) Valid() bool {
	switch s {
	case ScopePlatform, ScopeProject, ScopeAgent, ScopeToolkitType:
		return true
	}
	return false
}

// ProjectScoped reports whether a project admin may set and resume s.
func (s Scope) ProjectScoped() bool { return s == ScopeProject || s == ScopeAgent }

// ResumeRequirement says who may resume a suspension.
type ResumeRequirement string

const (
	// ResumeAnyAdmin: an administrator who may suspend the scope.
	ResumeAnyAdmin ResumeRequirement = "any_admin"
	// ResumeGovernanceRole: a platform administrator only.
	ResumeGovernanceRole ResumeRequirement = "governance_role"
	// ResumeTwoPerson: a platform administrator who did not suspend it.
	ResumeTwoPerson ResumeRequirement = "two_person"
)

// Valid reports whether r is a known requirement.
func (r ResumeRequirement) Valid() bool {
	switch r {
	case ResumeAnyAdmin, ResumeGovernanceRole, ResumeTwoPerson:
		return true
	}
	return false
}

// Source says who wrote a suspension.
type Source string

const (
	SourceManual        Source = "manual"
	SourceBreaker       Source = "breaker"
	SourceCertification Source = "certification"
	SourceSelfTest      Source = "self_test"
)

// Permission is the grant behind every suspend and resume route: central
// (administration mode) for the admin routes, project-resolved (default
// mode) for the project routes. Shared 0194 seeds it.
const Permission = "safety.suspend"

// ErrorCode is the typed refusal a covered run start answers.
const ErrorCode = "execution_suspended"

// PlatformStopConfirmation is the typed confirmation a platform suspension
// needs, in the UI and on the wire.
const PlatformStopConfirmation = "STOP"

// Limits of the free text.
const (
	MaxReasonBytes = 1024
	maxKeyBytes    = 128
)

// Target names what a new suspension covers.
type Target struct {
	Scope Scope
	// ProjectID is the project of a project or agent scope; 0 otherwise.
	ProjectID int64
	// ApplicationID is the agent of an agent scope; 0 otherwise. The
	// self-test's synthetic agent is project 0, application 0.
	ApplicationID int64
	// ToolkitType is the toolkit type of a toolkit_type scope, raw or
	// canonical; Normalize canonicalises it.
	ToolkitType string
}

// ErrInvalidTarget is a malformed scope, key or reason.
var ErrInvalidTarget = errors.New("invalid suspension target")

// MaxTargetID is the largest project or application id a target may name:
// both are PostgreSQL integers (execution_suspensions.project_id, the tenant
// application ids), so a larger id names nothing and must not be narrowed
// into one that does.
const MaxTargetID = math.MaxInt32

// Normalize validates t and returns it with the canonical toolkit key.
func (t Target) Normalize() (Target, error) {
	if t.ProjectID > MaxTargetID || t.ApplicationID > MaxTargetID {
		return Target{}, fmt.Errorf("%w: project_id and application_id are at most %d", ErrInvalidTarget, MaxTargetID)
	}
	switch t.Scope {
	case ScopePlatform:
		if t.ProjectID != 0 || t.ApplicationID != 0 || t.ToolkitType != "" {
			return Target{}, fmt.Errorf("%w: a platform suspension names no key", ErrInvalidTarget)
		}
	case ScopeProject:
		if t.ProjectID <= 0 || t.ApplicationID != 0 || t.ToolkitType != "" {
			return Target{}, fmt.Errorf("%w: a project suspension names one project", ErrInvalidTarget)
		}
	case ScopeAgent:
		if t.ProjectID < 0 || t.ApplicationID < 0 || t.ToolkitType != "" ||
			(t.ProjectID == 0) != (t.ApplicationID == 0) {
			return Target{}, fmt.Errorf("%w: an agent suspension names a project and an agent", ErrInvalidTarget)
		}
	case ScopeToolkitType:
		if t.ProjectID != 0 || t.ApplicationID != 0 {
			return Target{}, fmt.Errorf("%w: a toolkit type suspension names one type", ErrInvalidTarget)
		}
		key := guardrails.CanonicalKey(t.ToolkitType)
		if key == "" || len(key) > maxKeyBytes {
			return Target{}, fmt.Errorf("%w: the toolkit type is empty or too long", ErrInvalidTarget)
		}
		t.ToolkitType = key
	default:
		return Target{}, fmt.Errorf("%w: unknown scope", ErrInvalidTarget)
	}
	return t, nil
}

// Key is the stored scope_key: NULL ("") for platform, the project id, the
// application id, or the canonical toolkit type.
func (t Target) Key() string {
	switch t.Scope {
	case ScopeProject:
		return strconv.FormatInt(t.ProjectID, 10)
	case ScopeAgent:
		return strconv.FormatInt(t.ApplicationID, 10)
	case ScopeToolkitType:
		return t.ToolkitType
	}
	return ""
}

// Suspension is one row of elitea_runtime.execution_suspensions.
type Suspension struct {
	ID             int64
	Target         Target
	Reason         string
	ActorID        int64
	Source         Source
	ResumeRequires ResumeRequirement
	CreatedAt      time.Time
	// CancelledRuns is how many in-flight runs the suspension cancelled when
	// it was created.
	CancelledRuns int
	ResumedAt     *time.Time
	ResumedBy     *int64
	ResumeReason  string
}

// Active reports whether the suspension is still in force.
func (s Suspension) Active() bool { return s.ResumedAt == nil }

// Subject is a run asking to start: its project, its agent (0 for an ad hoc
// chat, an index run or a toolkit run) and its toolkit type (empty unless the
// run is one toolkit's tool call).
type Subject struct {
	ProjectID     int64
	ApplicationID int64
	ToolkitType   string
}

// Covers reports whether the suspension covers the subject. Only active
// suspensions are passed here.
func (s Suspension) Covers(subject Subject) bool {
	t := s.Target
	switch t.Scope {
	case ScopePlatform:
		return true
	case ScopeProject:
		return subject.ProjectID > 0 && subject.ProjectID == t.ProjectID
	case ScopeAgent:
		return subject.ApplicationID == t.ApplicationID && subject.ProjectID == t.ProjectID &&
			(subject.ApplicationID > 0 || t.ApplicationID == 0)
	case ScopeToolkitType:
		return subject.ToolkitType != "" && guardrails.CanonicalKey(subject.ToolkitType) == t.ToolkitType
	}
	return false
}

// ErrExecutionSuspended is the sentinel every SuspendedError matches.
var ErrExecutionSuspended = errors.New(ErrorCode)

// ErrStateUnavailable means the suspension state could not be read at all
// (cold start and the database refused). Admission fails closed on it.
var ErrStateUnavailable = errors.New("suspension state unavailable")

// SuspendedError is the typed refusal of a covered run. It names the scope
// and the key, never the reason text (an operator's note, not for callers).
type SuspendedError struct {
	SuspensionID int64
	Scope        Scope
	ProjectID    int64
	Key          string
}

func (e *SuspendedError) Error() string {
	var b strings.Builder
	b.WriteString("execution suspended: scope ")
	b.WriteString(string(e.Scope))
	if e.Key != "" {
		b.WriteString(" ")
		b.WriteString(e.Key)
	}
	return b.String()
}

// Is lets errors.Is(err, ErrExecutionSuspended) match.
func (e *SuspendedError) Is(target error) bool { return target == ErrExecutionSuspended }

// Message is the caller-safe sentence of the refusal.
func (e *SuspendedError) Message() string {
	switch e.Scope {
	case ScopePlatform:
		return "An administrator stopped all agent execution on this platform."
	case ScopeProject:
		return "An administrator stopped agent execution in this project."
	case ScopeAgent:
		return "An administrator stopped this agent."
	case ScopeToolkitType:
		return "An administrator stopped this toolkit type."
	}
	return "An administrator stopped this execution."
}

// Body is the JSON error body of a refused start: {"error", "code",
// "message", "scope", "scope_key", "suspension_id"}.
func (e *SuspendedError) Body() map[string]any {
	body := map[string]any{
		"error":         e.Message(),
		"code":          ErrorCode,
		"message":       e.Message(),
		"scope":         string(e.Scope),
		"suspension_id": e.SuspensionID,
	}
	if e.Key != "" {
		body["scope_key"] = e.Key
	}
	return body
}

// AsSuspended returns the SuspendedError in err's chain.
func AsSuspended(err error) (*SuspendedError, bool) {
	var suspended *SuspendedError
	if errors.As(err, &suspended) {
		return suspended, true
	}
	return nil, false
}

// HTTPRefusal maps a refused start to its HTTP answer: 409 with the typed
// execution_suspended body for a covered run, 503 when the suspension state
// could not be read at all (admission fails closed). ok is false for every
// other error.
func HTTPRefusal(err error) (status int, body map[string]any, ok bool) {
	if suspended, found := AsSuspended(err); found {
		return http.StatusConflict, suspended.Body(), true
	}
	if errors.Is(err, ErrStateUnavailable) {
		return http.StatusServiceUnavailable, map[string]any{
			"error":   "suspension_state_unavailable",
			"code":    "suspension_state_unavailable",
			"message": "The platform cannot confirm that execution is allowed right now. Try again shortly.",
		}, true
	}
	return 0, nil, false
}

func refusal(s Suspension) *SuspendedError {
	return &SuspendedError{SuspensionID: s.ID, Scope: s.Target.Scope, ProjectID: s.Target.ProjectID, Key: s.Target.Key()}
}
