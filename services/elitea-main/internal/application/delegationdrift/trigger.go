package delegationdrift

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultDelay is how long after a write the project's check runs. Writes that
// arrive while one is waiting are absorbed by it, so a burst (a version publish
// writes the version, its tools and its tags) runs one check; a write that
// lands after it runs arms the next.
const DefaultDelay = 2 * time.Second

// checkTimeout bounds one project check.
const checkTimeout = 30 * time.Second

// Trigger runs a project's drift check shortly after a write that can change a
// callee, once per burst. It is in process: the check reads the database as it
// is, so it does not matter which replica ran the write, and a check that is
// missed (a restart inside the delay) is made good by the check that runs when
// the caller's grants are read.
type Trigger struct {
	detector *Detector
	delay    time.Duration

	// sync runs after the drift check, as the person whose write armed it: the
	// grants of the edges a saved composition gained (story S6).
	sync func(ctx context.Context, projectID, actorID int64)

	mu      sync.Mutex
	pending map[int64]*time.Timer
	actors  map[int64]int64
	// background parents the checks; cancelling it (Stop) ends them.
	background context.Context
	cancel     context.CancelFunc
}

// NewTrigger builds the trigger. A delay of zero or less runs the check at
// once (tests).
func NewTrigger(detector *Detector, delay time.Duration) *Trigger {
	ctx, cancel := context.WithCancel(context.Background())
	return &Trigger{detector: detector, delay: delay, pending: map[int64]*time.Timer{}, actors: map[int64]int64{}, background: ctx, cancel: cancel}
}

// WithSync composes what runs after each project check: the sync that gives the
// edges a save created their pending grants. It runs as the person whose write
// armed the check; with none (a write with no person) it is not asked.
func (t *Trigger) WithSync(sync func(ctx context.Context, projectID, actorID int64)) *Trigger {
	t.sync = sync
	return t
}

// Stop cancels the checks that have not run and the ones in flight.
func (t *Trigger) Stop() {
	t.cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, timer := range t.pending {
		timer.Stop()
		delete(t.pending, id)
		delete(t.actors, id)
	}
}

// ProjectChanged asks for a drift check of the project. A check already waiting
// for the project absorbs the request.
func (t *Trigger) ProjectChanged(projectID int64) { t.ProjectChangedBy(projectID, 0) }

// ProjectChangedBy is ProjectChanged with the person who wrote. A check already
// waiting absorbs the request and keeps the first person it was armed by, unless
// it had none.
func (t *Trigger) ProjectChangedBy(projectID, actorID int64) {
	if t == nil || projectID <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, waiting := t.pending[projectID]; waiting {
		if t.actors[projectID] <= 0 && actorID > 0 {
			t.actors[projectID] = actorID
		}
		return
	}
	t.actors[projectID] = actorID
	t.pending[projectID] = time.AfterFunc(t.delay, func() {
		t.mu.Lock()
		delete(t.pending, projectID)
		actor := t.actors[projectID]
		delete(t.actors, projectID)
		t.mu.Unlock()
		t.run(projectID, actor)
	})
}

func (t *Trigger) run(projectID, actorID int64) {
	ctx, cancel := context.WithTimeout(t.background, checkTimeout)
	defer cancel()
	if _, err := t.detector.CheckProject(ctx, projectID); err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "delegation drift: the check failed", "project_id", projectID, "error", err)
	}
	if t.sync != nil && actorID > 0 && ctx.Err() == nil {
		t.sync(ctx, projectID, actorID)
	}
}

// CheckCallerQuietly is the check run when a caller's grants are read. It never fails
// the read: a failure is logged and the grants are served as stored.
func (d *Detector) CheckCallerQuietly(ctx context.Context, projectID, callerID int64) {
	if _, err := d.CheckCaller(ctx, projectID, callerID); err != nil {
		slog.WarnContext(ctx, "delegation drift: the check on read failed", "project_id", projectID, "caller_application_id", callerID, "error", err)
	}
}
