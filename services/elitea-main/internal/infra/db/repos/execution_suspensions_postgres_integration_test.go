package repos

// The kill switch's durable half against a REAL migrated database (shared
// 0194): a new suspension cancels exactly the in-flight runs it covers, in
// the same transaction; one active suspension per target; the resume rules
// the CHECK constraints hold.

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
)

func newSuspensionRepo(t *testing.T) (*ExecutionSuspensionRepository, *pgxpool.Pool) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	repo, err := NewExecutionSuspensionRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO centry.project (id, create_success, suspended) VALUES (2, TRUE, FALSE) ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed project 2: %v", err)
	}
	return repo, pool
}

// seedAgentRun writes an agent job of an agent: the job, its request entry
// and its binding with the application id admission records since 0194.
func seedAgentRun(t *testing.T, pool *pgxpool.Pool, executionID string, projectID, applicationID int64) {
	t.Helper()
	seedExecutionJob(t, pool, executionID, "agent.execute.application.v1", "RUNNING", "RUNNING", projectID)
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.input_bundle_entries (
    input_bundle_id, entry_id, entry_version, semantic_role, media_type,
    content_digest, content_size, content_reference, classification,
    required_grant_audience, content_bytes
) VALUES ('bundle-' || $1, 'request', '1', 'request', 'application/vnd.elitea.agent-execution-input.v1+protobuf',
          sha256('c'::bytea), 1, 'ref', 'tenant-confidential', 'aud', '\x00'::bytea)`, executionID); err != nil {
		t.Fatalf("seed request entry: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.agent_execution_jobs (
    execution_id, generation, capability_id, input_bundle_id, request_entry_id,
    client_stream_id, client_message_id, client_execution_generation, sio_event,
    application_id
) VALUES ($1, 1, 'agent.execute.application.v1', 'bundle-' || $1, 'request',
          'stream-' || $1, 'message-' || $1, 'g1', 'chat_predict', $2)`, executionID, applicationID); err != nil {
		t.Fatalf("seed agent binding: %v", err)
	}
}

func desiredState(t *testing.T, pool *pgxpool.Pool, executionID string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(),
		`SELECT desired_state FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, executionID).Scan(&state); err != nil {
		t.Fatalf("read desired state: %v", err)
	}
	return state
}

func TestExecutionSuspensions_AgentScopeCancelsOnlyThatAgentsRuns(t *testing.T) {
	repo, pool := newSuspensionRepo(t)
	seedAgentRun(t, pool, "susp-agent-a", 1, 31)
	seedAgentRun(t, pool, "susp-agent-b", 1, 32)
	// Application ids are per tenant schema: agent 31 of project 2 is another agent.
	seedAgentRun(t, pool, "susp-agent-other-project", 2, 31)
	seedExecutionJob(t, pool, "susp-agent-done", "agent.execute.application.v1", "SUCCEEDED", "RUNNING", 1)

	created, err := repo.CreateSuspension(t.Context(), safety.NewSuspension{
		Target: safety.Target{Scope: safety.ScopeAgent, ProjectID: 1, ApplicationID: 31},
		Reason: "loops", ActorID: 7, Source: safety.SourceManual, ResumeRequires: safety.ResumeAnyAdmin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.CancelledRuns != 1 {
		t.Fatalf("cancelled = %d, want 1", created.CancelledRuns)
	}
	for id, want := range map[string]string{
		"susp-agent-a": "CANCELLED", "susp-agent-b": "RUNNING", "susp-agent-other-project": "RUNNING",
		"susp-agent-done": "RUNNING",
	} {
		if got := desiredState(t, pool, id); got != want {
			t.Errorf("%s desired = %s, want %s", id, got, want)
		}
	}

	var already *safety.AlreadySuspendedError
	if _, err := repo.CreateSuspension(t.Context(), safety.NewSuspension{
		Target: safety.Target{Scope: safety.ScopeAgent, ProjectID: 1, ApplicationID: 31},
		Reason: "again", ActorID: 8, Source: safety.SourceManual, ResumeRequires: safety.ResumeAnyAdmin,
	}); !errors.As(err, &already) || already.Existing.ID != created.ID {
		t.Fatalf("err = %v, want the existing suspension", err)
	}
}

func TestExecutionSuspensions_ProjectAndPlatformScopesCancelEverythingTheyCover(t *testing.T) {
	repo, pool := newSuspensionRepo(t)
	seedExecutionJob(t, pool, "susp-p1-index", "index.ingest.v1", "RUNNING", "RUNNING", 1)
	seedExecutionJob(t, pool, "susp-p2-tool", "toolkit.call_tool.v1", "PENDING", "RUNNING", 2)

	project, err := repo.CreateSuspension(t.Context(), safety.NewSuspension{
		Target: safety.Target{Scope: safety.ScopeProject, ProjectID: 1},
		Reason: "incident", ActorID: 7, Source: safety.SourceManual, ResumeRequires: safety.ResumeGovernanceRole,
	})
	if err != nil {
		t.Fatal(err)
	}
	if project.CancelledRuns < 1 || desiredState(t, pool, "susp-p1-index") != "CANCELLED" ||
		desiredState(t, pool, "susp-p2-tool") != "RUNNING" {
		t.Fatalf("project scope cancelled %d; p1=%s p2=%s", project.CancelledRuns,
			desiredState(t, pool, "susp-p1-index"), desiredState(t, pool, "susp-p2-tool"))
	}

	platform, err := repo.CreateSuspension(t.Context(), safety.NewSuspension{
		Target: safety.Target{Scope: safety.ScopePlatform},
		Reason: "kill switch", ActorID: 7, Source: safety.SourceManual, ResumeRequires: safety.ResumeTwoPerson,
	})
	if err != nil {
		t.Fatal(err)
	}
	if platform.CancelledRuns < 1 || desiredState(t, pool, "susp-p2-tool") != "CANCELLED" {
		t.Fatalf("platform scope cancelled %d", platform.CancelledRuns)
	}

	active, err := repo.LoadActiveSuspensions(t.Context())
	if err != nil || len(active) < 2 {
		t.Fatalf("active = %v, %v", active, err)
	}

	// two_person: the database refuses the suspender as the resumer even if
	// the service were bypassed.
	if _, err := repo.ResumeSuspension(t.Context(), safety.ResumeRecord{ID: platform.ID, ResumedBy: 7}); !errors.Is(err, safety.ErrResumeForbidden) {
		t.Fatalf("self-resume of a two-person stop: %v", err)
	}
	resumed, err := repo.ResumeSuspension(t.Context(), safety.ResumeRecord{ID: platform.ID, ResumedBy: 8, Reason: "clear"})
	if err != nil || resumed.Active() || resumed.ResumeReason != "clear" {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
	if _, err := repo.ResumeSuspension(t.Context(), safety.ResumeRecord{ID: platform.ID, ResumedBy: 9}); !errors.Is(err, safety.ErrNotActive) {
		t.Fatalf("second resume: %v", err)
	}
	listed, err := repo.ListSuspensions(t.Context(), safety.ListFilter{ProjectID: 1, Limit: 10})
	if err != nil || len(listed) != 1 || listed[0].ID != project.ID {
		t.Fatalf("project listing = %+v, %v", listed, err)
	}
}

func TestExecutionSuspensions_ToolkitTypeReachesTheGuardrailRead(t *testing.T) {
	repo, pool := newSuspensionRepo(t)
	if _, err := repo.CreateSuspension(t.Context(), safety.NewSuspension{
		Target: safety.Target{Scope: safety.ScopeToolkitType, ToolkitType: "github"},
		Reason: "token leak", ActorID: 7, Source: safety.SourceManual, ResumeRequires: safety.ResumeAnyAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := pool.QueryRow(t.Context(), `
SELECT scope_key FROM elitea_runtime.execution_suspensions
WHERE scope = 'toolkit_type' AND resumed_at IS NULL`).Scan(&key); err != nil || key != "github" {
		t.Fatalf("key = %q, %v", key, err)
	}
	// A non-canonical key is refused by the CHECK.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.execution_suspensions (scope, scope_key, reason, actor_id)
VALUES ('toolkit_type', 'Git Hub', 'x', 1)`); err == nil {
		t.Fatal("a non-canonical toolkit key must be refused")
	}
}

// An id past the integer column is refused by the database, never narrowed:
// int32(1<<32 + 1) is project 1, and the old cast suspended it.
func TestExecutionSuspensions_OutOfRangeProjectIsRefusedNotNarrowed(t *testing.T) {
	repo, pool := newSuspensionRepo(t)
	seedExecutionJob(t, pool, "susp-narrow", "index.ingest.v1", "RUNNING", "RUNNING", 1)
	for _, target := range []safety.Target{
		{Scope: safety.ScopeProject, ProjectID: 1<<32 + 1},
		{Scope: safety.ScopeAgent, ProjectID: 1<<32 + 1, ApplicationID: 31},
	} {
		if _, err := repo.CreateSuspension(t.Context(), safety.NewSuspension{
			Target: target, Reason: "x", ActorID: 7, Source: safety.SourceManual, ResumeRequires: safety.ResumeAnyAdmin,
		}); err == nil {
			t.Fatalf("%+v: an out-of-range project was stored", target)
		}
	}
	if got := desiredState(t, pool, "susp-narrow"); got != "RUNNING" {
		t.Fatalf("project 1's run was cancelled by an out-of-range id: %s", got)
	}
	active, err := repo.LoadActiveSuspensions(t.Context())
	if err != nil || len(active) != 0 {
		t.Fatalf("active = %+v, %v", active, err)
	}
	if listed, err := repo.ListSuspensions(t.Context(), safety.ListFilter{ProjectID: 1<<32 + 1}); err != nil || len(listed) != 0 {
		t.Fatalf("list of an out-of-range project = %+v, %v", listed, err)
	}
}
