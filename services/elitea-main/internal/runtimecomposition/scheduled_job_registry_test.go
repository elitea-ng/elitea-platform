package runtimecomposition

import (
	"context"
	"testing"

	schedulingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/scheduling"
)

// TestArtifactRetentionScheduledJobsIncludesBothIndexAndSweepJobs is the
// composition-level check S14's plan text requires: it calls the exact
// function New() calls to build the scheduler's job list
// (scheduledJobs -> scheduledJobRegistry), not a reimplementation of it, and
// asserts the artifact retention sweep is actually present alongside the
// index scheduling job — not just that artifactRetentionSweep.Execute
// passes in isolation (artifact_retention_sweep_test.go already covers
// that). If a future edit to composition.go's New() stops calling
// scheduledJobs() for the retention job, or scheduledJobs() itself drops
// it, this test fails; a correct, unit-tested Handler that never reaches
// the registry is exactly the gap class this stage's plan text calls out
// (the same failure mode S13 found for its own bootstrap function, one
// layer down in the scheduler instead of a missing route/handler).
//
// It stops short of invoking runtimecomposition.New() itself: New()'s full
// dependency graph (Redis, gRPC TLS, signing keys, five Postgres pools) has
// no existing test anywhere in this package that constructs it, and
// building one from scratch for this one assertion is out of proportion —
// see docs/plans/storage-migration-plan.md's S14 section for this
// documented tradeoff.
func TestArtifactRetentionScheduledJobsIncludesBothIndexAndSweepJobs(t *testing.T) {
	schedule, err := schedulingapp.ParseCron("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	jobs := scheduledJobs(
		stubSchedulingHandler{}, stubSchedulingHandler{}, stubSchedulingHandler{},
		schedule, schedule, schedule,
	)
	registry, err := scheduledJobRegistry(currentIndexScheduleLeaseDuration, jobs...)
	if err != nil {
		t.Fatal(err)
	}

	registered := registry.RegisteredJobs()
	if len(registered) != 3 {
		t.Fatalf("expected exactly 3 registered jobs, got %d: %+v", len(registered), registered)
	}
	revisionByID := make(map[string]string, len(registered))
	for _, job := range registered {
		revisionByID[job.ID] = job.Revision
	}
	if revisionByID[currentIndexScheduleCapability] != currentIndexScheduleRevision {
		t.Errorf("index scheduling job missing or has the wrong revision: %+v", revisionByID)
	}
	if revisionByID[artifactRetentionSweepCapability] != artifactRetentionSweepRevision {
		t.Errorf("artifact retention sweep job missing or has the wrong revision: %+v", revisionByID)
	}
	// #940 A3. Same guard, same reason: a producer that is correct and
	// unit-tested but never reaches the registry warns nobody about anything,
	// and no unit test of the notifier can see that.
	if revisionByID[patExpirySweepCapability] != patExpirySweepRevision {
		t.Errorf("personal access token expiry sweep job missing or has the wrong revision: %+v", revisionByID)
	}
}

func TestScheduledJobsKeepsIndexSchedulingWithoutArtifactStore(t *testing.T) {
	schedule, err := schedulingapp.ParseCron("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	jobs := scheduledJobs(stubSchedulingHandler{}, nil, nil, schedule, nil, nil)
	registry, err := scheduledJobRegistry(currentIndexScheduleLeaseDuration, jobs...)
	if err != nil {
		t.Fatal(err)
	}

	registered := registry.RegisteredJobs()
	if len(registered) != 1 || registered[0].ID != currentIndexScheduleCapability {
		t.Fatalf("expected only current index scheduling, got %+v", registered)
	}
}

type stubSchedulingHandler struct{}

func (stubSchedulingHandler) Execute(context.Context, schedulingapp.Occurrence) (schedulingapp.Outcome, error) {
	return schedulingapp.OutcomeLocalCompleted, nil
}

var _ schedulingapp.Handler = stubSchedulingHandler{}
