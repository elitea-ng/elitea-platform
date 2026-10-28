package main

// Mandate-bound unattended runs (ADR-0039, issue #1122): pipeline
// schedules, inbound pipeline triggers and index schedules run only under an
// attached mandate with credential governance on.

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	v2mandateattachments "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/mandateattachments"
	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// governedTriggerComposition is what the governed composition root builds
// for unattended runs. Every field is nil with the flag off: the pipeline
// trigger handler then gets no WithGovernedRuns, no attach route mounts and
// the index schedule executor keeps its project-system identity rule.
type governedTriggerComposition struct {
	// Runs is the fire-time mandate check of schedules and triggers.
	Runs *runmandate.Service
	// Attachments serves the attach and detach routes.
	Attachments *v2mandateattachments.Handler
	// IndexMandates and IndexGate make the index schedule executor
	// mandate-bound. Real nils with the flag off, never a nil pointer boxed
	// in the interface.
	IndexMandates indexscheduleapp.ScheduleMandates
	IndexGate     indexscheduleapp.MandateGate
}

// governedTriggersAtBoot builds the composition. With the flag on, a
// construction error refuses boot.
func governedTriggersAtBoot(admission profile.Admission, revision profile.PolicyRevision, pool *pgxpool.Pool) (governedTriggerComposition, error) {
	if !admission.Governed() {
		return governedTriggerComposition{}, nil
	}
	if pool == nil {
		return governedTriggerComposition{}, fmt.Errorf("mandate-bound unattended runs need the platform database")
	}
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return governedTriggerComposition{}, fmt.Errorf("construct unattended run security audit appender: %w", err)
	}
	epochs, err := epoch.NewPostgresStore(pool)
	if err != nil {
		return governedTriggerComposition{}, err
	}
	store := repos.NewUnattendedRunMandates(pool, audit)
	runs, err := runmandate.New(runmandate.Config{
		Mandates: repos.NewMandates(pool, audit),
		Bindings: repos.NewAgentIdentityBindings(pool, audit),
		Epochs:   epochs,
		// The unattended kinds the active revision allows (gate D4): none
		// unless the revision lists them.
		Policy:   revision.Document,
		Refusals: store,
	})
	if err != nil {
		return governedTriggerComposition{}, fmt.Errorf("construct the unattended run mandate check: %w", err)
	}
	attachments, err := v2mandateattachments.New(store, runs)
	if err != nil {
		return governedTriggerComposition{}, err
	}
	return governedTriggerComposition{
		Runs: runs, Attachments: attachments, IndexMandates: store, IndexGate: runs,
	}, nil
}
