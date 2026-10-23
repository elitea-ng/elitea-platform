package executions

import (
	"context"
	"errors"
	"strconv"
	"sync"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

const (
	defaultMaxSSEAuthorizations             = 4
	defaultMaxSSEAuthorizationsPerPrincipal = 1
	defaultMaxActiveSSEStreams              = 16
	defaultMaxActiveSSEStreamsPerPrincipal  = 4
	defaultMaxActiveSSEStreamsPerProject    = 8
)

// sseAuthorizationGate bounds only authorization queries against the dedicated
// replay pool. It never waits: a saturated caller must retry instead of
// accumulating request goroutines or consuming a long-lived stream slot.
type sseAuthorizationGate struct {
	mu             sync.Mutex
	active         int
	globalLimit    int
	principalLimit int
	byPrincipal    map[string]int
	waiting        int
	changed        chan struct{}
}

func newSSEAuthorizationGate(globalLimit, principalLimit int) *sseAuthorizationGate {
	if globalLimit <= 0 || principalLimit <= 0 || principalLimit > globalLimit {
		return nil
	}
	return &sseAuthorizationGate{
		globalLimit:    globalLimit,
		principalLimit: principalLimit,
		byPrincipal:    make(map[string]int),
		changed:        make(chan struct{}),
	}
}

func (g *sseAuthorizationGate) acquire(principalID string) (func(), bool) {
	if g == nil || principalID == "" {
		return nil, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// Established streams waiting to reauthorize take precedence over new
	// initial requests. This prevents initial request churn from starving a
	// stream until its fail-closed continuation deadline.
	if g.waiting > 0 || !g.capacityAvailableLocked(principalID) {
		return nil, false
	}
	return g.acquireLocked(principalID), true
}

func (g *sseAuthorizationGate) acquireContext(
	ctx context.Context,
	principalID string,
) (func(), error) {
	if ctx == nil {
		return nil, errors.New("SSE authorization wait context is required")
	}
	if g == nil || principalID == "" {
		return nil, errSSEAuthorizationBusy
	}
	g.mu.Lock()
	g.waiting++
	for {
		if g.capacityAvailableLocked(principalID) {
			g.waiting--
			release := g.acquireLocked(principalID)
			g.mu.Unlock()
			return release, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.waiting--
			g.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
			g.mu.Lock()
		}
	}
}

func (g *sseAuthorizationGate) capacityAvailableLocked(principalID string) bool {
	return g.active < g.globalLimit &&
		g.byPrincipal[principalID] < g.principalLimit
}

func (g *sseAuthorizationGate) acquireLocked(principalID string) func() {
	g.active++
	g.byPrincipal[principalID]++

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			decrementSSEAdmissionCount(g.byPrincipal, principalID)
			close(g.changed)
			g.changed = make(chan struct{})
			g.mu.Unlock()
		})
	}
}

// sseAdmissionGate bounds long-lived replay loops independently of ordinary
// request rate limiting. It is intentionally process-local: every replica has
// a fixed resource ceiling, while the dedicated replay pool protects output
// settlement from aggregate cross-replica SSE load.
type sseAdmissionGate struct {
	mu             sync.Mutex
	active         int
	globalLimit    int
	principalLimit int
	projectLimit   int
	byPrincipal    map[string]int
	byProject      map[string]int
}

func newSSEAdmissionGate(globalLimit, principalLimit, projectLimit int) *sseAdmissionGate {
	if globalLimit <= 0 || principalLimit <= 0 || projectLimit <= 0 || principalLimit > globalLimit || projectLimit > globalLimit {
		return nil
	}
	return &sseAdmissionGate{
		globalLimit:    globalLimit,
		principalLimit: principalLimit,
		projectLimit:   projectLimit,
		byPrincipal:    make(map[string]int),
		byProject:      make(map[string]int),
	}
}

func (g *sseAdmissionGate) acquire(principalID, projectID string) (func(), bool) {
	if g == nil || principalID == "" || projectID == "" {
		return nil, false
	}
	g.mu.Lock()
	if g.active >= g.globalLimit || g.byPrincipal[principalID] >= g.principalLimit || g.byProject[projectID] >= g.projectLimit {
		g.mu.Unlock()
		return nil, false
	}
	g.active++
	g.byPrincipal[principalID]++
	g.byProject[projectID]++
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			decrementSSEAdmissionCount(g.byPrincipal, principalID)
			decrementSSEAdmissionCount(g.byProject, projectID)
			g.mu.Unlock()
		})
	}, true
}

func decrementSSEAdmissionCount(counts map[string]int, key string) {
	if counts[key] <= 1 {
		delete(counts, key)
		return
	}
	counts[key]--
}

func sseOwningPrincipalID(ctx context.Context) (string, bool) {
	principal, ok := auth.RuntimePrincipalFromContext(ctx)
	if !ok {
		return "", false
	}
	ownerID, ok := principal.OwningUserID()
	if !ok {
		return "", false
	}
	return strconv.FormatInt(ownerID, 10), true
}
