package projectprovisioning

// The application-data steps of a project delete (issues #1243 and #1244).
//
// A DeepWiki wiki's search index and an Inventory toolkit's graph live in the
// engine's own store, not in the project's tenant schema or its PgVector
// database, so none of the journal's other steps reaches them. Each application
// wired on the replica that DECIDES the delete contributes ONE journal step,
// `<app>_data` (StepDeepWikiData, StepInventoryData), that asks the engine to
// delete everything it holds for the project over its platform gRPC service
// (internal/providerhost/platformops).
//
// THE ROW RECORDS THE STEPS. The applications are written into the journal row
// (deletionCleanup.Apps) by the deciding transaction, from the configuration of
// the replica that took the delete; every later run — the delete's own, a
// reconciler's on any replica — takes its step list FROM THE ROW, not from the
// running process. A replica that has no client for a recorded application
// leaves that step PENDING (logged) rather than completing the row: the work
// waits for a replica that can do it. Without this, a replica with fewer
// engines wired would close a row whose engine data it never touched.
//
// HOW THE ENGINE'S ANSWERS ARE READ:
//
//   - success: the step is done, with the engine's summary as its note.
//   - ErrRetryLater (the engine is still stopping a run, is unreachable, or
//     deleted only part): the step fails and the journal retries it with its
//     backoff. A partial deletion's summary is KEPT as the step's note, so the
//     row shows what went while the rest is still owed.
//   - ErrNotSupported (UNIMPLEMENTED): the engine keeps nothing of this kind.
//     The step is marked done with the note NoteUnsupported, at Warn.
//   - ErrNotAllowed, ErrConfig: the engine refused this process's certificate,
//     or the request is invalid. Retrying cannot fix either, so the step is
//     closed the way #1245 closes a drop it gave up on: marked done with the
//     reason in its note ("failed permanently: ..."), with an Error log. An
//     operator finds these rows by the note; nothing else is retried.
//   - anything else: the step fails and the journal retries it.
//
// THE CALL FITS THE RUN. A run has one bound (cleanupTimeout, 5 minutes); the
// client bounds one call at the facade's timeout or two minutes, whichever is
// longer; two engines could therefore outlast the run and the second step would
// be "not reached" on every run, for ever. So each application step bounds its
// call to its SHARE of what is left of the run — (remaining − bookkeeping) /
// (application steps still to run, this one included) — and the client's own
// timeout applies as the smaller of the two. Every step gets a slot in every
// run; a slow engine shortens its own call, not the next engine's.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

// ErrApplicationDataNotRemoved reports a delete that removed the project and
// has not yet deleted its data in one application's engine (a DeepWiki wiki
// index, an Inventory graph). The cleanup journal retries it.
var ErrApplicationDataNotRemoved = errors.New("projectprovisioning: application data was not removed")

// NoteUnsupported is the note of an application step closed because the engine
// does not support the deletion (UNIMPLEMENTED).
const NoteUnsupported = "engine does not support deletion"

// ApplicationDataRemover deletes everything one application's engine holds for
// a project. It is satisfied by *platformops.Client.
type ApplicationDataRemover interface {
	// DeleteProjectData removes the project's data in the engine and returns a
	// one-line summary of what went ("deleted 2 wiki index(es)", "nothing to
	// delete"), recorded as the step's note. It must be idempotent. The errors
	// are platformops's: ErrRetryLater (possibly WITH a summary of a partial
	// deletion), ErrNotSupported, ErrNotAllowed, ErrConfig, or the engine's
	// failure.
	DeleteProjectData(ctx context.Context, projectID int64) (summary string, err error)
}

// applicationCleanup is one wired application: its name (the step is
// `<name>_data`) and its remover.
type applicationCleanup struct {
	app     string
	remover ApplicationDataRemover
}

// ApplicationDataStep is the journal step name of one application's data:
// `<app>_data`.
func ApplicationDataStep(app string) string { return app + "_data" }

// WithApplicationData wires one application's engine into the delete. app is
// the application's name as the platform service knows it ("deepwiki",
// "inventory"); a second option for the same name replaces the first. A nil
// remover (or a typed nil) is ignored, for the reason WithVectorStore ignores
// one: the composition root declares the interface type, and a typed nil would
// pass the nil checks and then panic inside a journal run.
func WithApplicationData(app string, remover ApplicationDataRemover) Option {
	return func(p *Provisioner) {
		app = strings.TrimSpace(app)
		if app == "" || remover == nil || isNilPointer(remover) {
			return
		}
		for i := range p.applications {
			if p.applications[i].app == app {
				p.applications[i].remover = remover
				return
			}
		}
		p.applications = append(p.applications, applicationCleanup{app: app, remover: remover})
		sort.Slice(p.applications, func(i, j int) bool { return p.applications[i].app < p.applications[j].app })
	}
}

// ApplicationsWired names the applications whose engine this provisioner can
// delete from, sorted. The deciding transaction records them on the journal
// row; the composition root logs them at startup.
func (p *Provisioner) ApplicationsWired() []string {
	names := make([]string, 0, len(p.applications))
	for _, application := range p.applications {
		names = append(names, application.app)
	}
	return names
}

// applicationRemover is the remover wired for app on THIS replica, or nil.
func (p *Provisioner) applicationRemover(app string) ApplicationDataRemover {
	for _, application := range p.applications {
		if application.app == app {
			return application.remover
		}
	}
	return nil
}

// notWiredError is the outcome of an application step on a replica that has no
// client for the application the row recorded. runCleanup leaves the step
// pending (not done, not failed on the engine's account) and the row backs off
// for a replica that has the client.
type notWiredError struct{ app string }

func (e notWiredError) Error() string {
	return "this replica has no " + e.app + " client; the step waits for one that has"
}

// closedWithNote is the outcome of an application step that is marked DONE
// without the engine deleting anything, for a reason retrying cannot change
// (the engine does not support the deletion; the engine refuses this process;
// the request is invalid). The note is recorded on the step.
type closedWithNote struct{ note string }

func (e closedWithNote) Error() string { return "step closed: " + e.note }

// applicationSteps are the journal steps of the applications a row recorded,
// in the row's order, each calling the remover this replica has for it.
func applicationSteps(apps []string) []cleanupStep {
	steps := make([]cleanupStep, 0, len(apps))
	for i, app := range apps {
		steps = append(steps, cleanupStep{
			name:    ApplicationDataStep(app),
			notDone: ErrApplicationDataNotRemoved,
			run:     runApplicationStep(app, apps[i:]),
		})
	}
	return steps
}

// runApplicationStep is the run of one application step. following are the
// applications from this one on, for the share of the run bound this call may
// take (applicationCallContext).
func runApplicationStep(app string, following []string) func(context.Context, *Provisioner, int64, *deletionCleanup) (string, error) {
	return func(ctx context.Context, p *Provisioner, projectID int64, cleanup *deletionCleanup) (string, error) {
		attrs := []slog.Attr{slog.String("app", app), slog.Int64("project_id", projectID)}
		remover := p.applicationRemover(app)
		if remover == nil {
			p.logger.LogAttrs(ctx, slog.LevelWarn, "project delete cleanup: this replica has no client for the application the journal recorded; the step is left pending for a replica that has",
				attrs...)
			return "", notWiredError{app: app}
		}
		callCtx, cancel := applicationCallContext(ctx, stepsStillToRun(following, cleanup))
		defer cancel()
		summary, err := remover.DeleteProjectData(callCtx, projectID)
		switch {
		case err == nil:
			p.logger.LogAttrs(ctx, slog.LevelInfo, "project delete: application data removed",
				append(attrs, slog.String("result", summary))...)
			return summary, nil
		case errors.Is(err, platformops.ErrNotSupported):
			p.logger.LogAttrs(ctx, slog.LevelWarn, "project delete cleanup: the engine does not support deleting a project's data; the step is closed as unsupported",
				attrs...)
			return "", closedWithNote{note: NoteUnsupported}
		case errors.Is(err, platformops.ErrNotAllowed), errors.Is(err, platformops.ErrConfig):
			p.logger.LogAttrs(ctx, slog.LevelError, "project delete cleanup FAILED PERMANENTLY: the engine refused this process or the request is invalid; the project's data stays in the engine until an operator intervenes",
				append(attrs, slog.Any("err", err))...)
			return "", closedWithNote{note: "failed permanently: " + err.Error()}
		}
		// ErrRetryLater (a partial deletion carries its summary, kept as the
		// note) or the engine's failure: the journal retries the step.
		return summary, fmt.Errorf("%s: %w", app, err)
	}
}

// stepsStillToRun counts, among the applications from the current one on,
// those whose step is not done: the calls that still have to fit in the run.
func stepsStillToRun(following []string, cleanup *deletionCleanup) int {
	count := 0
	for _, app := range following {
		if cleanup == nil || !cleanup.Done[ApplicationDataStep(app)] {
			count++
		}
	}
	return max(count, 1)
}

// applicationCallContext bounds one engine call to its share of what is left of
// the run: (time to the run's deadline − a bookkeeping margin) / steps still to
// run. With no deadline on ctx the call is bounded by the client alone. A share
// that is already spent is returned as is: the call fails at once with the
// run's deadline and the journal retries the step.
func applicationCallContext(ctx context.Context, stepsLeft int) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	remaining := time.Until(deadline) - bookkeepingTimeout
	if remaining <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, remaining/time.Duration(max(stepsLeft, 1)))
}
