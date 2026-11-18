package delegationnotice

import (
	"context"
	"sync"
)

// PermissionChecker answers whether a person holds a project permission.
type PermissionChecker interface {
	HasProjectPermission(ctx context.Context, userID, projectID int64, permission string) (bool, error)
}

// EditorLister names the people to ask about one agent: its registry owners,
// else its owner.
type EditorLister interface {
	EditorNames(ctx context.Context, projectID, applicationID int64, limit int) ([]string, error)
}

// Service is the Reviewer over the project's permissions and the agent's owners.
type Service struct {
	permissions PermissionChecker
	editors     EditorLister
}

// NewService builds the Reviewer. It answers nil when either side is missing,
// so a composition without them serves notices unstamped (every client then
// shows the notice without Review delegation).
func NewService(permissions PermissionChecker, editors EditorLister) *Service {
	if permissions == nil || editors == nil {
		return nil
	}
	return &Service{permissions: permissions, editors: editors}
}

// DelegationReview implements Reviewer. A caller with no saved identity (an
// ad hoc root) has no grant to review. A viewer who may edit agents may
// review; a viewer who may not is told who to ask. Looking the names up is
// best effort: a failed lookup shows the notice without them.
func (s *Service) DelegationReview(ctx context.Context, projectID, userID, callerID int64) (Review, error) {
	if callerID <= 0 {
		return Review{}, nil
	}
	allowed, err := s.permissions.HasProjectPermission(ctx, userID, projectID, EditPermission)
	if err != nil {
		return Review{}, err
	}
	if allowed {
		return Review{CanReview: true}, nil
	}
	names, err := s.editors.EditorNames(ctx, projectID, callerID, MaxEditors)
	if err != nil {
		return Review{}, nil
	}
	return Review{Editors: names}, nil
}

// cached remembers each caller's Review for the life of one stream or request.
type cached struct {
	inner Reviewer
	mu    sync.Mutex
	seen  map[int64]Review
}

// Cached wraps a Reviewer so one stream or request asks once per calling
// agent. It answers nil for a nil Reviewer.
func Cached(inner Reviewer) Reviewer {
	if inner == nil {
		return nil
	}
	return &cached{inner: inner, seen: map[int64]Review{}}
}

func (c *cached) DelegationReview(ctx context.Context, projectID, userID, callerID int64) (Review, error) {
	c.mu.Lock()
	review, ok := c.seen[callerID]
	c.mu.Unlock()
	if ok {
		return review, nil
	}
	review, err := c.inner.DelegationReview(ctx, projectID, userID, callerID)
	if err != nil {
		return Review{}, err
	}
	c.mu.Lock()
	c.seen[callerID] = review
	c.mu.Unlock()
	return review, nil
}
