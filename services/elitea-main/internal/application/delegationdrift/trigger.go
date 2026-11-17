package delegationdrift

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultDelay is how long a project's checks are held back so a burst of
// writes (a version publish writes the version, its tools and its tags) runs
// one check, after the last of them.
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

	mu      sync.Mutex
	pending map[int64]*time.Timer
	// background parents the checks; cancelling it (Stop) ends them.
	background context.Context
	cancel     context.CancelFunc
}

// NewTrigger builds the trigger. A delay of zero or less runs the check at
// once (tests).
func NewTrigger(detector *Detector, delay time.Duration) *Trigger {
	ctx, cancel := context.WithCancel(context.Background())
	return &Trigger{detector: detector, delay: delay, pending: map[int64]*time.Timer{}, background: ctx, cancel: cancel}
}

// Stop cancels the checks that have not run and the ones in flight.
func (t *Trigger) Stop() {
	t.cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, timer := range t.pending {
		timer.Stop()
		delete(t.pending, id)
	}
}

// ProjectChanged asks for a drift check of the project. A check already waiting
// for the project absorbs the request.
func (t *Trigger) ProjectChanged(projectID int64) {
	if t == nil || projectID <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, waiting := t.pending[projectID]; waiting {
		return
	}
	t.pending[projectID] = time.AfterFunc(t.delay, func() {
		t.mu.Lock()
		delete(t.pending, projectID)
		t.mu.Unlock()
		t.run(projectID)
	})
}

func (t *Trigger) run(projectID int64) {
	ctx, cancel := context.WithTimeout(t.background, checkTimeout)
	defer cancel()
	if _, err := t.detector.CheckProject(ctx, projectID); err != nil && ctx.Err() == nil {
		slog.WarnContext(ctx, "delegation drift: the check failed", "project_id", projectID, "error", err)
	}
}

// CheckCallerQuietly is the check run when a caller's grants are read. It never fails
// the read: a failure is logged and the grants are served as stored.
func (d *Detector) CheckCallerQuietly(ctx context.Context, projectID, callerID int64) {
	if _, err := d.CheckCaller(ctx, projectID, callerID); err != nil {
		slog.WarnContext(ctx, "delegation drift: the check on read failed", "project_id", projectID, "caller_application_id", callerID, "error", err)
	}
}
