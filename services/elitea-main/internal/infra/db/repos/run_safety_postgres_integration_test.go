package repos

// Run safety against a REAL migrated database (shared 0201): the
// toolkit_type kill switch cancels the in-flight runs whose recorded toolkit
// set has the type (and reads it for a run admitted before 0201), and the
// whole-run wall cap cancels a run past the cap from its original start.

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
)

func setToolkitTypes(t *testing.T, pool *pgxpool.Pool, executionID string, types []string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`UPDATE elitea_runtime.execution_jobs SET toolkit_types = $2::text[] WHERE execution_id = $1`,
		executionID, types); err != nil {
		t.Fatalf("record toolkit types: %v", err)
	}
}

func TestRunSafety_ToolkitTypeSuspensionCancelsRunsThatHoldTheType(t *testing.T) {
	repo, pool := newSuspensionRepo(t)
	seedAgentRun(t, pool, "tk-agent-github", 1, 31)
	setToolkitTypes(t, pool, "tk-agent-github", []string{"github", "jira"})
	seedAgentRun(t, pool, "tk-agent-jira", 1, 32)
	setToolkitTypes(t, pool, "tk-agent-jira", []string{"jira"})
	seedExecutionJob(t, pool, "tk-call-github", "toolkit.call_tool.v1", "RUNNING", "RUNNING", 2)
	setToolkitTypes(t, pool, "tk-call-github", []string{"github"})
	seedExecutionJob(t, pool, "tk-done-github", "agent.execute.application.v1", "SUCCEEDED", "RUNNING", 1)
	setToolkitTypes(t, pool, "tk-done-github", []string{"github"})

	// An agent run admitted before 0201: no record; its stored input is read.
	seedAgentRun(t, pool, "tk-legacy-github", 1, 33)
	legacy, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{
		Application: []byte(`{"version_details":{"tools":[{"id":4,"type":"GitHub","settings":{}}]}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE elitea_runtime.input_bundle_entries
SET content_bytes = $2::bytea, content_size = length($2::bytea), content_digest = sha256($2::bytea)
WHERE input_bundle_id = 'bundle-' || $1 AND entry_id = 'request'`, "tk-legacy-github", legacy); err != nil {
		t.Fatalf("store the legacy input: %v", err)
	}
	// One whose input cannot be read is recorded as no toolkit, not guessed.
	seedAgentRun(t, pool, "tk-legacy-unreadable", 1, 34)

	created, err := repo.CreateSuspension(t.Context(), safety.NewSuspension{
		Target: safety.Target{Scope: safety.ScopeToolkitType, ToolkitType: "github"},
		Reason: "token leak", ActorID: 7, Source: safety.SourceManual, ResumeRequires: safety.ResumeAnyAdmin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.CancelledRuns != 3 {
		t.Fatalf("cancelled = %d, want 3", created.CancelledRuns)
	}
	for id, want := range map[string]string{
		"tk-agent-github": "CANCELLED", "tk-call-github": "CANCELLED", "tk-legacy-github": "CANCELLED",
		"tk-agent-jira": "RUNNING", "tk-done-github": "RUNNING", "tk-legacy-unreadable": "RUNNING",
	} {
		if got := desiredState(t, pool, id); got != want {
			t.Errorf("%s desired = %s, want %s", id, got, want)
		}
	}
	var recorded []string
	if err := pool.QueryRow(t.Context(), `SELECT toolkit_types FROM elitea_runtime.execution_jobs
WHERE execution_id = 'tk-legacy-unreadable'`).Scan(&recorded); err != nil || recorded == nil || len(recorded) != 0 {
		t.Fatalf("unreadable legacy input recorded %v, %v; want an empty record", recorded, err)
	}
}

func TestRunSafety_WallCapCancelsFromTheOriginalStart(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	walls, err := NewRunWallCapRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	seedAgentRun(t, pool, "cap-old", 1, 31)
	seedAgentRun(t, pool, "cap-young", 1, 31)
	seedAgentRun(t, pool, "cap-resumed", 1, 31)
	seedExecutionJob(t, pool, "cap-index", "index.ingest.v1", "RUNNING", "RUNNING", 1)
	if _, err := pool.Exec(t.Context(), `
UPDATE elitea_runtime.execution_jobs SET admitted_at = clock_timestamp() - interval '2 hours'
WHERE execution_id IN ('cap-old', 'cap-index');
UPDATE elitea_runtime.execution_jobs SET run_started_at = clock_timestamp() - interval '2 hours'
WHERE execution_id = 'cap-resumed'`); err != nil {
		t.Fatalf("age the runs: %v", err)
	}

	for id, want := range map[string]bool{"cap-old": true, "cap-young": false, "cap-resumed": true, "cap-index": false} {
		got, err := walls.EnforceWallCap(t.Context(), id, 1, time.Hour)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got != want {
			t.Errorf("%s cancelled = %t, want %t", id, got, want)
		}
	}
	var reason *string
	if err := pool.QueryRow(t.Context(), `SELECT cancel_reason FROM elitea_runtime.execution_jobs
WHERE execution_id = 'cap-old'`).Scan(&reason); err != nil || reason == nil || *reason != CapReasonWallCap {
		t.Fatalf("cancel reason = %v, %v", reason, err)
	}
	if desiredState(t, pool, "cap-young") != "RUNNING" {
		t.Fatal("a run inside the cap was cancelled")
	}
	// Idempotent: the second renewal reports no new cancel.
	if again, err := walls.EnforceWallCap(t.Context(), "cap-old", 1, time.Hour); err != nil || again {
		t.Fatalf("second enforce = %t, %v", again, err)
	}
}
