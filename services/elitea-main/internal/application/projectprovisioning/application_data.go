package projectprovisioning

// The application-data steps of a project delete (issues #1243 and #1244).
//
// A DeepWiki wiki's search index and an Inventory toolkit's graph live in the
// engine's own store, not in the project's tenant schema or its PgVector
// database, so none of the journal's other steps reaches them. Each enabled
// application contributes ONE journal step, `<app>_data` (StepDeepWikiData,
// StepInventoryData), that asks the engine to delete everything it holds for
// the project over its platform gRPC service (internal/providerhost/platformops).
//
// The step has the journal's semantics and nothing of its own: it is
// idempotent on the engine's side (a project with nothing left answers an empty
// success), it runs after the project row is gone (so no new generation or
// ingest can start for the project), and a failure — the engine still stopping
// a run, unreachable, or a deletion it could only partly do — leaves the step
// pending for the next run, with the journal's backoff. An application that is
// NOT configured on this process contributes no step: there is nothing to call,
// and a step that always "skips" would make every journal row look as though
// the data had been considered. The composition root logs which applications
// are wired (and which are not) at startup.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// ErrApplicationDataNotRemoved reports a delete that removed the project and
// has not yet deleted its data in one application's engine (a DeepWiki wiki
// index, an Inventory graph). The cleanup journal retries it.
var ErrApplicationDataNotRemoved = errors.New("projectprovisioning: application data was not removed")

// ApplicationDataRemover deletes everything one application's engine holds for
// a project. It is satisfied by *platformops.Client.
type ApplicationDataRemover interface {
	// DeleteProjectData removes the project's data in the engine and returns a
	// one-line summary of what went ("deleted 2 wiki index(es)", "nothing to
	// delete"), recorded as the step's note. It must be idempotent. Any error
	// means the step is not done and is retried; an error that also returns a
	// summary (a deletion the engine could only partly do) is still a failure.
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

// ApplicationsWired names the applications whose engine this provisioner
// deletes from, sorted. For the composition root's startup log and for tests.
func (p *Provisioner) ApplicationsWired() []string {
	names := make([]string, 0, len(p.applications))
	for _, application := range p.applications {
		names = append(names, application.app)
	}
	return names
}

// applicationSteps are the journal steps of the wired applications, in name
// order, each calling its remover.
func (p *Provisioner) applicationSteps() []cleanupStep {
	steps := make([]cleanupStep, 0, len(p.applications))
	for _, application := range p.applications {
		steps = append(steps, cleanupStep{
			name:    ApplicationDataStep(application.app),
			notDone: ErrApplicationDataNotRemoved,
			run: func(ctx context.Context, p *Provisioner, projectID int64, _ *deletionCleanup) (string, error) {
				summary, err := application.remover.DeleteProjectData(ctx, projectID)
				if err != nil {
					return "", fmt.Errorf("%s: %w", application.app, err)
				}
				p.logger.LogAttrs(ctx, slog.LevelInfo, "project delete: application data removed",
					slog.String("app", application.app), slog.Int64("project_id", projectID), slog.String("result", summary))
				return summary, nil
			},
		})
	}
	return steps
}
