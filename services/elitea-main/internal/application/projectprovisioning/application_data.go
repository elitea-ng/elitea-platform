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
// THE VECTOR STORE IS THE THIRD APPLICATION, WITH THREE DIFFERENCES (ADR-0031
// decision 4; AppVector, infra/vectorclient):
//
//   - RECORDED ONLY BY A REPLICA THAT INDEXES INTO IT. Under the Python
//     indexing runtime no toolkit index is in elitea-vector, so a replica
//     there does not record the step even when it has a client
//     (WithApplicationDataDrain: it still RUNS a step another replica
//     recorded, which is harmless and correct). A replica whose
//     configuration has the vector store explicitly OFF — Python indexing
//     and no vector target (WithVectorStoreOff) — closes a recorded step with
//     NoteVectorStoreNotConfigured at Warn instead of waiting: the deployment
//     turned the store off after the row was written, and no replica will
//     ever drain it. A replica that indexes into the store but has no client
//     (a rollout, the chart's guard refuses the steady state) leaves it
//     pending, like any unwired application.
//   - A REFUSED CERTIFICATE DOES NOT CLOSE THE STEP. For an engine, a refusal
//     closes the step with NoteRefused: the engine's data is the engine's. The
//     vector store's data is the platform's, and a "done" over vectors still
//     in Qdrant would be a silent leak, so the step stays pending at the
//     journal's LONGEST backoff with an Error log naming the identity
//     problem, and runs once an operator fixes the allowlist or the material.
//   - IT WAITS OUT A CUT-OFF RUN'S TOKEN. A delete refuses a project with
//     active runs, except the one caller that skips the check
//     (SkipActiveWorkCheck). The deciding transaction deletes the project's
//     execution jobs and claims with the row, which revokes a worker's vector
//     claim token at the source (elitea-main's introspection joins the job,
//     the claim and the project row), but elitea-vector CACHES a verified
//     answer for up to five seconds (its worker-claim cap), so a worker that
//     was mid-ingest can land one more upsert inside that window. DropProject
//     in the delete's own run — milliseconds after the decision — could then
//     be followed by vectors nothing will ever delete. So the decision
//     records how many runs it cut off (deletionCleanup.ActiveRuns, DecidedAt)
//     and the vector step waits VectorDropSettle after the decision while
//     that count is positive: the step is left pending, the journal's first
//     retry (a minute later) drops the project. A delete that cut off nothing
//     drops at once.
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
//     CLASS of the reason as its note (cleanupjournal.NoteRefused /
//     NoteInvalidRequest, "failed permanently: ..."), with an Error log that
//     carries the engine's message. The note is stored on the row and reported
//     by DELETE of the project (StepStatus.Msg), so it never carries the raw
//     error, which can hold an address or what the engine refused. An operator
//     finds these rows by the note; nothing else is retried.
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

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/cleanupjournal"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

// ErrApplicationDataNotRemoved reports a delete that removed the project and
// has not yet deleted its data in one application's engine (a DeepWiki wiki
// index, an Inventory graph). The cleanup journal retries it.
var ErrApplicationDataNotRemoved = errors.New("projectprovisioning: application data was not removed")

// The notes of an application step closed without the engine deleting
// anything, shared with the toolkit journal (cleanupjournal): the engine does
// not support the deletion (UNIMPLEMENTED); the engine refused this process's
// certificate; the request was invalid. Class texts, never the engine's
// message.
const (
	NoteUnsupported    = cleanupjournal.NoteUnsupported
	NoteRefused        = cleanupjournal.NoteRefused
	NoteInvalidRequest = cleanupjournal.NoteInvalidRequest
	// NoteVectorStoreNotConfigured closes a recorded vector step on a replica
	// whose configuration has the vector store off (WithVectorStoreOff): the
	// deployment no longer indexes into elitea-vector, so nothing will ever
	// drain the step, and nothing is there to delete under the runtime that
	// replaced it.
	NoteVectorStoreNotConfigured = "vector store not configured"
)

// AppVector is the vector store's application name (infra/vectorclient.App):
// its step is StepVectorData. The provisioner treats it as the package comment
// says.
const AppVector = "vector"

// VectorDropSettle is how long after a delete that cut off active runs the
// vector step waits before DropProject: elitea-vector's longest cache of a
// verified worker claim token (5 s) with room for an upsert already in flight
// and clock drift between replicas.
const VectorDropSettle = 15 * time.Second

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
// `<name>_data`), its remover, and whether a delete decided on this replica
// records its step (WithApplicationData) or the replica only drains steps
// other replicas recorded (WithApplicationDataDrain).
type applicationCleanup struct {
	app      string
	remover  ApplicationDataRemover
	recorded bool
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
	return withApplication(app, remover, true)
}

// WithApplicationDataDrain wires an application's remover for the steps OTHER
// replicas recorded, without recording the step on deletes this replica
// decides: a replica that has an elitea-vector client under the Python
// indexing runtime, whose own projects hold no toolkit index there. A
// WithApplicationData for the same name upgrades it to recorded.
func WithApplicationDataDrain(app string, remover ApplicationDataRemover) Option {
	return withApplication(app, remover, false)
}

func withApplication(app string, remover ApplicationDataRemover, recorded bool) Option {
	return func(p *Provisioner) {
		app = strings.TrimSpace(app)
		if app == "" || remover == nil || cleanupjournal.IsNilPointer(remover) {
			return
		}
		for i := range p.applications {
			if p.applications[i].app == app {
				p.applications[i].remover = remover
				p.applications[i].recorded = p.applications[i].recorded || recorded
				return
			}
		}
		p.applications = append(p.applications, applicationCleanup{app: app, remover: remover, recorded: recorded})
		sort.Slice(p.applications, func(i, j int) bool { return p.applications[i].app < p.applications[j].app })
	}
}

// WithVectorStoreOff declares that this replica's configuration has the vector
// store explicitly off: the Python indexing runtime and no elitea-vector
// target. A recorded vector step is then closed with
// NoteVectorStoreNotConfigured instead of waiting for a replica that will
// never come. It is ignored when a vector remover is wired.
func WithVectorStoreOff() Option {
	return func(p *Provisioner) { p.vectorStoreOff = true }
}

// ApplicationsWired names the applications whose step a delete decided on this
// replica records, sorted. The deciding transaction writes them on the journal
// row; the composition root logs them at startup. A drain-only application
// (WithApplicationDataDrain) is not among them.
func (p *Provisioner) ApplicationsWired() []string {
	names := make([]string, 0, len(p.applications))
	for _, application := range p.applications {
		if application.recorded {
			names = append(names, application.app)
		}
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
// the request is invalid). The note is recorded on the step and reported to
// the caller, so it is one of the class texts above, never the raw error.
type closedWithNote struct{ note string }

func (e closedWithNote) Error() string { return "step closed: " + e.note }

// waitingForRuns is the outcome of the vector step inside VectorDropSettle of a
// decision that cut off active runs: pending, not failed on the store's
// account, retried by the journal's ordinary backoff.
type waitingForRuns struct {
	active int64
	since  time.Duration
}

func (e waitingForRuns) Error() string {
	return fmt.Sprintf("the delete cut off %d active run(s) %s ago; the vector drop waits %s for their claim tokens to lapse in the store",
		e.active, e.since.Round(time.Second), VectorDropSettle)
}

// identityRefused is the outcome of the vector step when the store refused
// this process's certificate: the step stays pending and the run backs the
// row off to the journal's longest pause (runCleanup), because only an
// operator fixes it and polling a store that refuses us is noise.
type identityRefused struct{ err error }

func (e identityRefused) Error() string { return e.err.Error() }
func (e identityRefused) Unwrap() error { return e.err }

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
			if app == AppVector && p.vectorStoreOff {
				p.logger.LogAttrs(ctx, slog.LevelWarn, "project delete cleanup: the journal recorded the vector store but this replica's configuration has it off (Python indexing, no elitea-vector target); the step is closed as not configured",
					attrs...)
				return "", closedWithNote{note: NoteVectorStoreNotConfigured}
			}
			p.logger.LogAttrs(ctx, slog.LevelWarn, "project delete cleanup: this replica has no client for the application the journal recorded; the step is left pending for a replica that has",
				attrs...)
			return "", notWiredError{app: app}
		}
		if app == AppVector && cleanup != nil && cleanup.ActiveRuns > 0 && !cleanup.DecidedAt.IsZero() {
			// See the package comment: a run that was writing vectors when the
			// project was deleted may upsert once more after a DropProject,
			// until the store's cache of its claim token lapses.
			if since := time.Since(cleanup.DecidedAt); since < VectorDropSettle {
				p.logger.LogAttrs(ctx, slog.LevelWarn, "project delete cleanup: the delete cut off active runs; the vector drop waits for their claim tokens to lapse in elitea-vector",
					append(attrs, slog.Int64("active_runs", cleanup.ActiveRuns), slog.Duration("since_decision", since))...)
				return "", waitingForRuns{active: cleanup.ActiveRuns, since: since}
			}
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
		case app == AppVector && errors.Is(err, platformops.ErrNotAllowed):
			// The store refused this process's identity. The vectors are the
			// platform's own, so the step is NOT closed: it waits at the
			// longest backoff for an operator to fix ELITEA_VECTOR_ADMIN_
			// IDENTITIES or the client material (the log says which).
			p.logger.LogAttrs(ctx, slog.LevelError, "project delete cleanup: elitea-vector refused this process's client certificate; the project's vectors stay in Qdrant and the step is retried at the longest backoff until ELITEA_VECTOR_ADMIN_IDENTITIES names this process's identity or its client material is fixed",
				append(attrs, slog.Any("err", err))...)
			return "", identityRefused{err: fmt.Errorf("%s: %w", app, err)}
		case errors.Is(err, platformops.ErrNotAllowed), errors.Is(err, platformops.ErrConfig):
			// The reason is logged here, with the project id; the note (stored,
			// and reported by DELETE of the project) is the class only.
			note := NoteInvalidRequest
			if errors.Is(err, platformops.ErrNotAllowed) {
				note = NoteRefused
			}
			p.logger.LogAttrs(ctx, slog.LevelError, "project delete cleanup FAILED PERMANENTLY: the engine refused this process or the request is invalid; the project's data stays in the engine until an operator intervenes",
				append(attrs, slog.String("note", note), slog.Any("err", err))...)
			return "", closedWithNote{note: note}
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
	remaining := time.Until(deadline) - cleanupjournal.BookkeepingTimeout
	if remaining <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, remaining/time.Duration(max(stepsLeft, 1)))
}
