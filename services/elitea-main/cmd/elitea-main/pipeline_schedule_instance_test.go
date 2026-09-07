package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	v2pipelinetriggers "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	schedulingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/scheduling"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPipelineScheduleRunnerStartsWithoutAnIndexSchedulerName is the regression
// test for the boot that stopped every stack-backed journey.
//
// It calls the composition root's own function with the value every compose
// file in deploy/ actually produces: an EMPTY scheduler instance name, because
// runtimecomposition fills that field only when index scheduling is enabled and
// no stack enables it. Before the fix this returned
// "construct pipeline schedule runner: invalid scheduler configuration:
// instance ID must be a bounded canonical identifier", and main.go turned that
// into `return fmt.Errorf("compose pipeline schedule job: …")` — process exit 1
// before the HTTP listener ever bound.
//
// No database is needed and none is used: pgxpool.New does not dial, the
// repository only wraps the pool, and the runner's first tick happens on the
// goroutine this test cancels. The point of the test is the COMPOSITION, which
// is where the defect was.
func TestPipelineScheduleRunnerStartsWithoutAnIndexSchedulerName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A syntactically valid DSN that is never dialled during composition.
	pool, err := pgxpool.New(ctx, "postgres://elitea:elitea@127.0.0.1:1/elitea?sslmode=disable")
	if err != nil {
		t.Fatalf("build a pool for the composition: %v", err)
	}
	defer pool.Close()

	handler := v2pipelinetriggers.NewHandler(pool)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := startPipelineScheduleRunner(ctx, pool, handler, "", logger); err != nil {
		t.Fatalf("the pipeline schedule runner must compose without an index scheduler name: %v", err)
	}
}

// TestPipelineScheduleInstanceIDIsAcceptedByTheScheduler pins the contract the
// derived name has to satisfy, against the scheduler's OWN validation rather
// than against a copy of its regular expression.
func TestPipelineScheduleInstanceIDIsAcceptedByTheScheduler(t *testing.T) {
	for name, configured := range map[string]string{
		"absent":              "",
		"separators only":     "---",
		"operator set":        "elitea-main-pov-1",
		"upper case host":     "Elitea-Main-0.pod",
		"kubernetes pod name": "elitea-main-7d9f8c6b45-x2kqz",
	} {
		t.Run(name, func(t *testing.T) {
			identifier := pipelineScheduleInstanceID(configured)
			if identifier == "" {
				t.Fatal("the derived instance name must not be empty")
			}
			config := schedulingapp.Config{
				InstanceID:      identifier,
				LeaseDuration:   2 * time.Minute,
				MaxParallel:     1,
				PageSize:        1,
				MaxPagesPerTick: 2,
			}
			registry, err := schedulingapp.NewRegistry(config.LeaseDuration, schedulingapp.Job{
				ID:       v2pipelinetriggers.ScheduleJobID,
				Revision: v2pipelinetriggers.ScheduleJobRevision,
				Mode:     schedulingapp.ModeDurableAdmission,
				Schedule: mustParsePipelineCadence(t),
				Timeout:  v2pipelinetriggers.ScheduleJobTimeout,
				Handler:  mustBuildPipelineScheduleJob(t),
			})
			if err != nil {
				t.Fatalf("register the pipeline schedule job: %v", err)
			}
			if _, err := schedulingapp.NewRunner(
				stubScheduleStore{}, registry, config, nil,
			); err != nil {
				t.Fatalf("the scheduler rejected instance name %q: %v", identifier, err)
			}
		})
	}
}

// TestCanonicalScheduleInstanceIDRejectsWhatItCannotProject documents the one
// answer that matters to the caller: "" means "nothing usable here", so the
// fallback runs instead of a name the scheduler would refuse.
func TestCanonicalScheduleInstanceIDRejectsWhatItCannotProject(t *testing.T) {
	for _, raw := range []string{"", "   ", "///", "-", "..."} {
		if got := canonicalScheduleInstanceID(raw); got != "" {
			t.Fatalf("canonicalScheduleInstanceID(%q) = %q, want the empty answer", raw, got)
		}
	}
	if got := canonicalScheduleInstanceID("Pod-7/Main"); got != "pod-7main" {
		t.Fatalf("canonicalScheduleInstanceID = %q, want %q", got, "pod-7main")
	}
}

func mustParsePipelineCadence(t *testing.T) schedulingapp.Schedule {
	t.Helper()
	cadence, err := schedulingapp.ParseCron(v2pipelinetriggers.ScheduleJobCadence)
	if err != nil {
		t.Fatalf("parse the pipeline schedule cadence: %v", err)
	}
	return cadence
}

func mustBuildPipelineScheduleJob(t *testing.T) schedulingapp.Handler {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://elitea:elitea@127.0.0.1:1/elitea?sslmode=disable")
	if err != nil {
		t.Fatalf("build a pool for the job: %v", err)
	}
	t.Cleanup(pool.Close)
	job, err := v2pipelinetriggers.NewScheduleJob(v2pipelinetriggers.NewHandler(pool))
	if err != nil {
		t.Fatalf("construct the pipeline schedule job: %v", err)
	}
	return job
}

// stubScheduleStore satisfies the scheduler's Store without a database. The
// runner is never started here, so no method is called.
type stubScheduleStore struct{ schedulingapp.Store }
