package delegationnotice

import (
	"context"
	"sync"
	"time"
)

const (
	// reviewTTL bounds how long a stream or a request keeps a viewer's Review:
	// a stream outlives a role change, so the answer is asked again after it.
	reviewTTL = 30 * time.Second
	// failureTTL keeps a failed lookup from being retried by every frame.
	failureTTL = 5 * time.Second
	// maxMemo bounds the per-viewer permission memo.
	maxMemo = 1024
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
	now         func() time.Time

	mu   sync.Mutex
	memo map[[2]int64]memoed
}

type memoed struct {
	allowed bool
	until   time.Time
}

// NewService builds the Reviewer. It answers nil when either side is missing,
// so a composition without them serves notices unstamped (every client then
// shows the notice without Review delegation). Assign the result to a Reviewer
// only when it is not nil.
func NewService(permissions PermissionChecker, editors EditorLister) *Service {
	if permissions == nil || editors == nil {
		return nil
	}
	return &Service{permissions: permissions, editors: editors, now: time.Now, memo: map[[2]int64]memoed{}}
}

// allowed is the project's edit permission of one viewer, remembered briefly:
// it is the same for every calling agent of a page.
func (s *Service) allowed(ctx context.Context, projectID, userID int64) (bool, error) {
	key := [2]int64{projectID, userID}
	s.mu.Lock()
	hit, ok := s.memo[key]
	s.mu.Unlock()
	if ok && s.now().Before(hit.until) {
		return hit.allowed, nil
	}
	allowed, err := s.permissions.HasProjectPermission(ctx, userID, projectID, EditPermission)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	if len(s.memo) >= maxMemo {
		s.memo = map[[2]int64]memoed{}
	}
	s.memo[key] = memoed{allowed: allowed, until: s.now().Add(reviewTTL)}
	s.mu.Unlock()
	return allowed, nil
}

// DelegationReview implements Reviewer. A caller with no saved identity (an
// ad hoc root) has no grant to review. A viewer who may edit agents may
// review; a viewer who may not is told who to ask. Looking the names up is
// best effort: a failed lookup shows the notice without them.
func (s *Service) DelegationReview(ctx context.Context, projectID, userID, callerID int64) (Review, error) {
	if callerID <= 0 {
		return Review{}, nil
	}
	allowed, err := s.allowed(ctx, projectID, userID)
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

// cached remembers each caller's Review for reviewTTL (and a failure for
// failureTTL), so one stream or request asks once per calling agent.
type cached struct {
	inner Reviewer
	now   func() time.Time

	mu   sync.Mutex
	seen map[int64]cachedReview
}

type cachedReview struct {
	review Review
	err    error
	until  time.Time
}

// Cached wraps a Reviewer so one stream or request asks once per calling
// agent for a while. It answers nil for a nil Reviewer.
func Cached(inner Reviewer) Reviewer {
	if inner == nil {
		return nil
	}
	return &cached{inner: inner, now: time.Now, seen: map[int64]cachedReview{}}
}

func (c *cached) DelegationReview(ctx context.Context, projectID, userID, callerID int64) (Review, error) {
	c.mu.Lock()
	hit, ok := c.seen[callerID]
	c.mu.Unlock()
	if ok && c.now().Before(hit.until) {
		return hit.review, hit.err
	}
	review, err := c.inner.DelegationReview(ctx, projectID, userID, callerID)
	ttl := reviewTTL
	if err != nil {
		review, ttl = Review{}, failureTTL
	}
	c.mu.Lock()
	c.seen[callerID] = cachedReview{review: review, err: err, until: c.now().Add(ttl)}
	c.mu.Unlock()
	return review, err
}
