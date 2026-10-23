package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
)

// TestScheduleOccurrenceRepositoryPostgresTakeover is a real PostgreSQL
// service-integration gate. Separate repository values model two Main replicas;
// it covers planning ownership, occurrence takeover, fencing and terminal
// idempotency. It is not a multiprocess restart, load or soak test.
func TestScheduleOccurrenceRepositoryPostgresTakeover(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	first, err := NewScheduleOccurrenceRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewScheduleOccurrenceRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const jobID = "scheduler.postgres.takeover.v1"
	if _, err := pool.Exec(ctx,
		`DELETE FROM elitea_runtime.scheduled_occurrences WHERE job_id = $1`,
		jobID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM elitea_runtime.scheduled_job_cursors WHERE job_id = $1`,
		jobID,
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM elitea_runtime.scheduled_occurrences WHERE job_id = $1`,
			jobID,
		)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM elitea_runtime.scheduled_job_cursors WHERE job_id = $1`,
			jobID,
		)
	})

	now, err := first.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	job := schedulingapp.RegisteredJob{ID: jobID, Revision: "r1"}
	planningA, acquired, err := first.ClaimPlanning(
		ctx, job, "main-a", now, time.Minute, time.Minute,
	)
	if err != nil || !acquired {
		t.Fatalf("first planning claim acquired=%v error=%v", acquired, err)
	}
	if _, acquired, err := second.ClaimPlanning(
		ctx, job, "main-b", now.Add(30*time.Second), time.Minute, time.Minute,
	); err != nil || acquired {
		t.Fatalf("unexpired planning takeover acquired=%v error=%v", acquired, err)
	}
	planningB, acquired, err := second.ClaimPlanning(
		ctx, job, "main-b", now.Add(time.Minute), time.Minute, time.Minute,
	)
	if err != nil || !acquired || planningB.LeaseEpoch <= planningA.LeaseEpoch {
		t.Fatalf("planning takeover=%+v acquired=%v error=%v", planningB, acquired, err)
	}
	due := now.UTC().Truncate(time.Minute)
	if !due.After(planningB.ObservedThrough) {
		due = planningB.ObservedThrough.Add(time.Second)
	}
	seed := schedulingapp.OccurrenceSeed{
		InvocationID:     postgresScheduleInvocationID(),
		JobID:            job.ID,
		ScheduleRevision: job.Revision,
		DueAt:            due,
		Mode:             schedulingapp.ModeDurableAdmission,
	}
	if err := first.MaterializeAndAdvance(ctx, planningA, []schedulingapp.OccurrenceSeed{seed}, due); !errors.Is(err, schedulingapp.ErrStaleFence) {
		t.Fatalf("old planning fence error=%v", err)
	}
	if err := second.MaterializeAndAdvance(ctx, planningB, []schedulingapp.OccurrenceSeed{seed}, due); err != nil {
		t.Fatalf("materialize with current planning fence: %v", err)
	}

	oldClaims, err := first.ClaimPage(ctx, []schedulingapp.RegisteredJob{job}, "main-a", now, time.Minute, 1)
	if err != nil || len(oldClaims) != 1 {
		t.Fatalf("first occurrence claim=%v error=%v", oldClaims, err)
	}
	if claims, err := second.ClaimPage(
		ctx, []schedulingapp.RegisteredJob{job}, "main-b", now.Add(30*time.Second), time.Minute, 1,
	); err != nil || len(claims) != 0 {
		t.Fatalf("unexpired occurrence takeover=%v error=%v", claims, err)
	}
	newClaims, err := second.ClaimPage(
		ctx, []schedulingapp.RegisteredJob{job}, "main-b", now.Add(time.Minute), time.Minute, 1,
	)
	if err != nil || len(newClaims) != 1 || newClaims[0].LeaseEpoch <= oldClaims[0].LeaseEpoch {
		t.Fatalf("occurrence takeover=%v error=%v", newClaims, err)
	}
	if err := first.Complete(ctx, oldClaims[0], schedulingapp.OutcomeDurablyAdmitted); !errors.Is(err, schedulingapp.ErrStaleFence) {
		t.Fatalf("old occurrence fence error=%v", err)
	}
	const retryDelay = 2 * time.Second
	if err := second.ReleaseForRetry(ctx, newClaims[0], "AMBIGUOUS_ADMISSION", retryDelay); err != nil {
		t.Fatalf("release current occurrence for delayed retry: %v", err)
	}
	releasedAt, err := first.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claims, err := first.ClaimPage(
		ctx, []schedulingapp.RegisteredJob{job}, "main-a", releasedAt, time.Minute, 1,
	); err != nil || len(claims) != 0 {
		t.Fatalf("occurrence reclaimed before retry delay=%v error=%v", claims, err)
	}
	finalClaims, err := first.ClaimPage(
		ctx, []schedulingapp.RegisteredJob{job}, "main-a", releasedAt.Add(retryDelay), time.Minute, 1,
	)
	if err != nil || len(finalClaims) != 1 || finalClaims[0].LeaseEpoch <= newClaims[0].LeaseEpoch {
		t.Fatalf("post-delay occurrence takeover=%v error=%v", finalClaims, err)
	}
	if err := second.Complete(ctx, newClaims[0], schedulingapp.OutcomeDurablyAdmitted); !errors.Is(err, schedulingapp.ErrStaleFence) {
		t.Fatalf("released occurrence fence error=%v", err)
	}
	if err := first.Complete(ctx, finalClaims[0], schedulingapp.OutcomeDurablyAdmitted); err != nil {
		t.Fatalf("post-delay occurrence completion: %v", err)
	}
	if claims, err := first.ClaimPage(
		ctx, []schedulingapp.RegisteredJob{job}, "main-a", now.Add(2*time.Minute), time.Minute, 1,
	); err != nil || len(claims) != 0 {
		t.Fatalf("terminal occurrence was reclaimed=%v error=%v", claims, err)
	}

	var state, outcome string
	var attempts int
	if err := pool.QueryRow(ctx, `
SELECT state, outcome, attempt_count
FROM elitea_runtime.scheduled_occurrences
WHERE invocation_id = $1`, seed.InvocationID).Scan(&state, &outcome, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "COMPLETED" || outcome != "durably_admitted" || attempts != 3 {
		t.Fatalf("terminal row state=%s outcome=%s attempts=%d", state, outcome, attempts)
	}
}

func postgresScheduleInvocationID() string {
	return "53d0f89019b9525bf0b2135e304a9f443c848b07f31da065c9758ba477414268"
}
