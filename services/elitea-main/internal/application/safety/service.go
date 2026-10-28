package safety

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// NewSuspension is what the store writes.
type NewSuspension struct {
	Target         Target
	Reason         string
	ActorID        int64
	Source         Source
	ResumeRequires ResumeRequirement
}

// ResumeRecord is what the store writes on resume.
type ResumeRecord struct {
	ID        int64
	ResumedBy int64
	Reason    string
}

// ListFilter narrows a listing. ProjectID > 0 keeps the project and agent
// suspensions of that project.
type ListFilter struct {
	ActiveOnly bool
	ProjectID  int64
	Limit      int
}

// Store is the durable half (repos.ExecutionSuspensionRepository).
type Store interface {
	ActiveLoader
	// CreateSuspension inserts the row and, in the same transaction, cancels
	// every in-flight run it covers through the durable desired-state cancel
	// (execution_jobs.desired_state = CANCELLED). It answers
	// *AlreadySuspendedError when an active suspension has the same target.
	CreateSuspension(ctx context.Context, s NewSuspension) (Suspension, error)
	// ResumeSuspension stamps resumed_at on an ACTIVE row; ErrNotActive
	// otherwise.
	ResumeSuspension(ctx context.Context, r ResumeRecord) (Suspension, error)
	GetSuspension(ctx context.Context, id int64) (Suspension, error)
	ListSuspensions(ctx context.Context, f ListFilter) ([]Suspension, error)
}

var (
	// ErrNotFound: no such suspension (or not one the caller's surface
	// reaches).
	ErrNotFound = errors.New("suspension not found")
	// ErrNotActive: the suspension was already resumed.
	ErrNotActive = errors.New("suspension is not active")
	// ErrResumeForbidden: the caller may not resume this suspension.
	ErrResumeForbidden = errors.New("the caller may not resume this suspension")
	// ErrScopeForbidden: the caller's surface may not set this scope.
	ErrScopeForbidden = errors.New("the caller may not suspend this scope")
	// ErrConfirmationRequired: a platform suspension needs the typed "STOP".
	ErrConfirmationRequired = errors.New("a platform stop needs the typed confirmation")
)

// AlreadySuspendedError: an active suspension already covers the target.
type AlreadySuspendedError struct{ Existing Suspension }

func (e *AlreadySuspendedError) Error() string {
	return fmt.Sprintf("an active suspension (%d) already covers this target", e.Existing.ID)
}

// Actor is the caller of a suspend or resume.
type Actor struct {
	UserID int64
	// PlatformAdmin is true on the admin (administration-mode) surface,
	// false on the project surface. The route gate proved the caller holds
	// safety.suspend on that surface.
	PlatformAdmin bool
	// ProjectID is the project of the project surface; 0 on the admin one.
	ProjectID int64
}

// CreateRequest is one suspend.
type CreateRequest struct {
	Target         Target
	Reason         string
	ResumeRequires ResumeRequirement
	// Confirmation must be "STOP" for a platform suspension.
	Confirmation string
	Source       Source
}

// ResumeRequest is one resume.
type ResumeRequest struct {
	ID     int64
	Reason string
}

// Service sets and resumes suspensions.
type Service struct {
	store Store
	gate  *Gate
}

// NewService builds the service. gate may be nil (no runtime plane on this
// replica): the rows and the cancel are still written.
func NewService(store Store, gate *Gate) (*Service, error) {
	if store == nil {
		return nil, errors.New("safety: the suspension store is required")
	}
	return &Service{store: store, gate: gate}, nil
}

// Create suspends a target and cancels the runs in flight it covers.
func (s *Service) Create(ctx context.Context, actor Actor, request CreateRequest) (Suspension, error) {
	target, err := request.Target.Normalize()
	if err != nil {
		return Suspension{}, err
	}
	if !actor.PlatformAdmin {
		if !target.Scope.ProjectScoped() || actor.ProjectID <= 0 || target.ProjectID != actor.ProjectID {
			return Suspension{}, ErrScopeForbidden
		}
	}
	if target.Scope == ScopePlatform && request.Confirmation != PlatformStopConfirmation {
		return Suspension{}, ErrConfirmationRequired
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" || len(reason) > MaxReasonBytes || !utf8.ValidString(reason) {
		return Suspension{}, fmt.Errorf("%w: a reason of 1 to %d bytes is required", ErrInvalidTarget, MaxReasonBytes)
	}
	requires := request.ResumeRequires
	if requires == "" {
		requires = ResumeAnyAdmin
	}
	if !requires.Valid() {
		return Suspension{}, fmt.Errorf("%w: unknown resume requirement", ErrInvalidTarget)
	}
	source := request.Source
	if source == "" {
		source = SourceManual
	}
	if source != SourceManual && source != SourceSelfTest {
		return Suspension{}, fmt.Errorf("%w: only manual suspensions are written here", ErrInvalidTarget)
	}
	if actor.UserID < 0 || (actor.UserID == 0 && source != SourceSelfTest) {
		return Suspension{}, ErrScopeForbidden
	}
	created, err := s.store.CreateSuspension(ctx, NewSuspension{
		Target: target, Reason: reason, ActorID: actor.UserID, Source: source, ResumeRequires: requires,
	})
	if err != nil {
		return Suspension{}, err
	}
	s.gate.Notify(created.ID)
	return created, nil
}

// MayResume reports whether actor may resume s. It is the one rule the
// routes, the UI hint and the tests share.
func MayResume(actor Actor, s Suspension) error {
	if !actor.PlatformAdmin {
		if !s.Target.Scope.ProjectScoped() || actor.ProjectID <= 0 || s.Target.ProjectID != actor.ProjectID {
			return ErrNotFound
		}
	}
	switch s.ResumeRequires {
	case ResumeAnyAdmin:
		return nil
	case ResumeGovernanceRole:
		if !actor.PlatformAdmin {
			return ErrResumeForbidden
		}
		return nil
	case ResumeTwoPerson:
		if !actor.PlatformAdmin || actor.UserID == s.ActorID {
			return ErrResumeForbidden
		}
		return nil
	}
	return ErrResumeForbidden
}

// Resume ends a suspension, if the actor may.
func (s *Service) Resume(ctx context.Context, actor Actor, request ResumeRequest) (Suspension, error) {
	if request.ID <= 0 {
		return Suspension{}, ErrNotFound
	}
	reason := strings.TrimSpace(request.Reason)
	if len(reason) > MaxReasonBytes || !utf8.ValidString(reason) {
		return Suspension{}, fmt.Errorf("%w: the resume note is too long", ErrInvalidTarget)
	}
	current, err := s.store.GetSuspension(ctx, request.ID)
	if err != nil {
		return Suspension{}, err
	}
	if err := MayResume(actor, current); err != nil {
		return Suspension{}, err
	}
	if !current.Active() {
		return Suspension{}, ErrNotActive
	}
	if actor.UserID < 0 || (actor.UserID == 0 && current.Source != SourceSelfTest) {
		return Suspension{}, ErrResumeForbidden
	}
	resumed, err := s.store.ResumeSuspension(ctx, ResumeRecord{ID: current.ID, ResumedBy: actor.UserID, Reason: reason})
	if err != nil {
		return Suspension{}, err
	}
	s.gate.Notify(resumed.ID)
	return resumed, nil
}

// List lists suspensions the actor's surface reaches.
func (s *Service) List(ctx context.Context, actor Actor, filter ListFilter) ([]Suspension, error) {
	if !actor.PlatformAdmin {
		if actor.ProjectID <= 0 {
			return nil, ErrScopeForbidden
		}
		filter.ProjectID = actor.ProjectID
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 200
	}
	return s.store.ListSuspensions(ctx, filter)
}

// Get reads one suspension the actor's surface reaches.
func (s *Service) Get(ctx context.Context, actor Actor, id int64) (Suspension, error) {
	found, err := s.store.GetSuspension(ctx, id)
	if err != nil {
		return Suspension{}, err
	}
	if !actor.PlatformAdmin && (!found.Target.Scope.ProjectScoped() || found.Target.ProjectID != actor.ProjectID) {
		return Suspension{}, ErrNotFound
	}
	return found, nil
}
