package repos

// The admin Tasks union, against a REAL migrated database.
//
// The handler tests beside this one run on a double, so they prove which store
// each kind reaches. They cannot prove the union: that the three sources are
// all in it, that `kind` is derived from capability_id rather than guessed,
// that a job asked to stop reads as CANCELLING rather than as RUNNING, and
// that the cancel actually changes a row. Every one of those is SQL, and SQL
// that returns the wrong rows returns them with a 200.

import (
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func newBackgroundJobsRepo(t *testing.T) (*AdminBackgroundJobsRepository, *pgxpool.Pool) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	repo, err := NewAdminBackgroundJobsRepository(pool)
	if err != nil {
		t.Fatalf("build the admin background jobs repository: %v", err)
	}
	return repo, pool
}

// seedExecutionJob writes one runtime-plane job with the given capability and
// state. It writes the input bundle the foreign key needs first.
func seedExecutionJob(
	t *testing.T, pool *pgxpool.Pool,
	executionID, capabilityID, state, desiredState string, projectID int64,
) {
	t.Helper()
	bundleID := "bundle-" + executionID
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.input_bundles (
    input_bundle_id, immutable_version, media_type, resource_project_id,
    manifest_digest, manifest_size, manifest_bytes, created_by
) VALUES ($1, '1', 'application/x-protobuf', $2,
          sha256('m'::bytea), 1, '\x00'::bytea, 'test')
ON CONFLICT (input_bundle_id) DO NOTHING`, bundleID, projectID); err != nil {
		t.Fatalf("seed input bundle: %v", err)
	}
	// The eight configuration columns stay NULL. `execution_jobs_capability_payload`
	// (shared/0115) requires exactly that for every capability except
	// `configuration.validate.v1`, so filling them would make this seed refuse
	// the very rows the page is about.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state, admitted_at
) VALUES (
    $1, 1, 'cmd-' || $1, $7, $2, $2, '42', 'user:42', $3,
    '1', $4, sha256('r'::bytea), 'scope', 'key-' || $1, $5, $6, now()
)`, executionID, projectID, capabilityID, bundleID, state, desiredState,
		strconv.FormatInt(projectID, 10)); err != nil {
		t.Fatalf("seed execution job: %v", err)
	}
}

func seedScheduledOccurrence(t *testing.T, pool *pgxpool.Pool, invocationID string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_runtime.scheduled_occurrences (
    invocation_id, job_id, schedule_revision, due_at, outcome_mode, state,
    next_attempt_at
) VALUES ($1, 'index.schedule.scan.v1', 'rev-1', now(), 'local_bounded',
          'PENDING', now())`, invocationID); err != nil {
		t.Fatalf("seed scheduled occurrence: %v", err)
	}
}

func findJob(rows []BackgroundJobRow, taskID string) (BackgroundJobRow, bool) {
	for _, row := range rows {
		if row.TaskID == taskID {
			return row, true
		}
	}
	return BackgroundJobRow{}, false
}

func TestAdminBackgroundJobs_UnionsEverySourceAndDerivesTheKind(t *testing.T) {
	repo, pool := newBackgroundJobsRepo(t)
	seedExecutionJob(t, pool, "exec-index", "index.ingest.v1", "RUNNING", "RUNNING", 1)
	seedExecutionJob(t, pool, "exec-agent", "agent.execute.application.v1", "SUCCEEDED", "RUNNING", 1)
	seedExecutionJob(t, pool, "exec-tool", "toolkit.call_tool.v1", "PENDING", "RUNNING", 1)
	seedScheduledOccurrence(t, pool, "0000000000000000000000000000000000000000000000000000000000000001")

	page, err := repo.ListBackgroundJobs(t.Context(), BackgroundJobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListBackgroundJobs: %v", err)
	}

	for taskID, wantKind := range map[string]string{
		"exec-index": BackgroundJobKindIndex,
		"exec-agent": BackgroundJobKindAgent,
		"exec-tool":  BackgroundJobKindToolkit,
		"0000000000000000000000000000000000000000000000000000000000000001": BackgroundJobKindSchedule,
	} {
		row, found := findJob(page.Rows, taskID)
		if !found {
			t.Fatalf("%s is missing from the union: %+v", taskID, page.Rows)
		}
		if row.Kind != wantKind {
			t.Errorf("%s kind = %q, want %q", taskID, row.Kind, wantKind)
		}
	}
	if page.Total < 4 {
		t.Errorf("total = %d, want at least the four seeded rows", page.Total)
	}
}

// A finished job is not cancellable; a live one is; a scheduled occurrence
// never is. The page renders the control from this flag, so a wrong value is a
// button that does nothing or a missing button on a runaway job.
func TestAdminBackgroundJobs_CancellableFollowsTheRowState(t *testing.T) {
	repo, pool := newBackgroundJobsRepo(t)
	seedExecutionJob(t, pool, "exec-live", "index.ingest.v1", "RUNNING", "RUNNING", 1)
	seedExecutionJob(t, pool, "exec-done", "index.ingest.v1", "SUCCEEDED", "RUNNING", 1)
	seedScheduledOccurrence(t, pool, "0000000000000000000000000000000000000000000000000000000000000002")

	page, err := repo.ListBackgroundJobs(t.Context(), BackgroundJobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListBackgroundJobs: %v", err)
	}
	for taskID, want := range map[string]bool{
		"exec-live": true,
		"exec-done": false,
		"0000000000000000000000000000000000000000000000000000000000000002": false,
	} {
		row, found := findJob(page.Rows, taskID)
		if !found {
			t.Fatalf("%s is missing", taskID)
		}
		if row.Cancellable != want {
			t.Errorf("%s cancellable = %v, want %v", taskID, row.Cancellable, want)
		}
	}
}

// CANCELLING is not a state any table holds. It is the pair
// (state, desired_state), named — and without it a job that has been asked to
// stop reads as RUNNING and the operator presses cancel again.
func TestAdminBackgroundJobs_StoppingJobReadsAsCancelling(t *testing.T) {
	repo, pool := newBackgroundJobsRepo(t)
	seedExecutionJob(t, pool, "exec-stopping", "index.ingest.v1", "RUNNING", "CANCELLED", 1)

	page, err := repo.ListBackgroundJobs(t.Context(), BackgroundJobFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListBackgroundJobs: %v", err)
	}
	row, found := findJob(page.Rows, "exec-stopping")
	if !found {
		t.Fatal("the stopping job is missing")
	}
	if row.Status != "CANCELLING" {
		t.Errorf("status = %q, want CANCELLING", row.Status)
	}
	if row.Cancellable {
		t.Error("a job that is already stopping was offered a cancel")
	}
}

func TestAdminBackgroundJobs_FiltersByKindStatusAndProject(t *testing.T) {
	repo, pool := newBackgroundJobsRepo(t)
	seedExecutionJob(t, pool, "exec-a", "index.ingest.v1", "RUNNING", "RUNNING", 1)
	seedExecutionJob(t, pool, "exec-b", "agent.execute.adhoc.v1", "SUCCEEDED", "RUNNING", 1)
	seedScheduledOccurrence(t, pool, "0000000000000000000000000000000000000000000000000000000000000003")

	byKind, err := repo.ListBackgroundJobs(t.Context(), BackgroundJobFilter{
		Kind: BackgroundJobKindIndex, Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListBackgroundJobs: %v", err)
	}
	for _, row := range byKind.Rows {
		if row.Kind != BackgroundJobKindIndex {
			t.Fatalf("the kind filter let %q through", row.Kind)
		}
	}
	if _, found := findJob(byKind.Rows, "exec-a"); !found {
		t.Error("the kind filter dropped the matching row")
	}

	byStatus, err := repo.ListBackgroundJobs(t.Context(), BackgroundJobFilter{
		Status: "SUCCEEDED", Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListBackgroundJobs: %v", err)
	}
	if _, found := findJob(byStatus.Rows, "exec-b"); !found {
		t.Error("the status filter dropped the matching row")
	}
	if _, found := findJob(byStatus.Rows, "exec-a"); found {
		t.Error("the status filter let a RUNNING row through")
	}

	// The project filter must also EXCLUDE the rows that have no project at
	// all — a scheduled occurrence belongs to none, and `project_id = 1` is
	// not true of NULL.
	projectID := int64(1)
	byProject, err := repo.ListBackgroundJobs(t.Context(), BackgroundJobFilter{
		ProjectID: &projectID, Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListBackgroundJobs: %v", err)
	}
	for _, row := range byProject.Rows {
		if row.ProjectID == nil || *row.ProjectID != 1 {
			t.Fatalf("the project filter let %+v through", row)
		}
	}
}

func TestAdminBackgroundJobs_PaginatesWithinTheWindow(t *testing.T) {
	repo, pool := newBackgroundJobsRepo(t)
	for _, id := range []string{"exec-p1", "exec-p2", "exec-p3"} {
		seedExecutionJob(t, pool, id, "index.ingest.v1", "RUNNING", "RUNNING", 1)
		// Distinct admitted_at values, so the ORDER BY is deterministic and a
		// page boundary means something.
		time.Sleep(2 * time.Millisecond)
	}

	first, err := repo.ListBackgroundJobs(t.Context(), BackgroundJobFilter{
		Kind: BackgroundJobKindIndex, Limit: 2, Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListBackgroundJobs: %v", err)
	}
	second, err := repo.ListBackgroundJobs(t.Context(), BackgroundJobFilter{
		Kind: BackgroundJobKindIndex, Limit: 2, Offset: 2,
	})
	if err != nil {
		t.Fatalf("ListBackgroundJobs: %v", err)
	}
	if len(first.Rows) != 2 || len(second.Rows) != 1 {
		t.Fatalf("pages = %d and %d, want 2 and 1", len(first.Rows), len(second.Rows))
	}
	// `total` is the FILTERED count, not the page size — a paginator that reads
	// len(rows) shows one page and stops.
	if first.Total != 3 {
		t.Errorf("total = %d, want 3", first.Total)
	}
	// And the two pages are disjoint: an offset that does not move is the
	// classic way a listing repeats its first page for ever.
	for _, row := range second.Rows {
		if _, repeated := findJob(first.Rows, row.TaskID); repeated {
			t.Errorf("%s appears on both pages", row.TaskID)
		}
	}
}

func TestAdminBackgroundJobs_CancelFlipsTheDesiredStateOnce(t *testing.T) {
	repo, pool := newBackgroundJobsRepo(t)
	seedExecutionJob(t, pool, "exec-cancel", "index.ingest.v1", "RUNNING", "RUNNING", 1)

	if err := repo.CancelRuntimeJob(t.Context(), "exec-cancel"); err != nil {
		t.Fatalf("CancelRuntimeJob: %v", err)
	}
	var desired string
	if err := pool.QueryRow(t.Context(), `
SELECT desired_state FROM elitea_runtime.execution_jobs
WHERE execution_id = 'exec-cancel'`).Scan(&desired); err != nil {
		t.Fatalf("read desired_state: %v", err)
	}
	if desired != "CANCELLED" {
		t.Fatalf("desired_state = %q, want CANCELLED", desired)
	}

	// A second cancel refuses rather than reporting success over a row it did
	// not change.
	if err := repo.CancelRuntimeJob(t.Context(), "exec-cancel"); err != ErrBackgroundJobNotCancellable {
		t.Errorf("second cancel returned %v, want ErrBackgroundJobNotCancellable", err)
	}
}

func TestAdminBackgroundJobs_CancelRefusesASettledJob(t *testing.T) {
	repo, pool := newBackgroundJobsRepo(t)
	seedExecutionJob(t, pool, "exec-settled", "index.ingest.v1", "SUCCEEDED", "RUNNING", 1)

	if err := repo.CancelRuntimeJob(t.Context(), "exec-settled"); err != ErrBackgroundJobNotCancellable {
		t.Errorf("cancel of a settled job returned %v, want ErrBackgroundJobNotCancellable", err)
	}
}

func TestAdminBackgroundJobs_CancelOfAnUnknownJobRefuses(t *testing.T) {
	repo, _ := newBackgroundJobsRepo(t)
	if err := repo.CancelRuntimeJob(t.Context(), "nothing-here"); err != ErrBackgroundJobNotCancellable {
		t.Errorf("cancel of an unknown job returned %v, want ErrBackgroundJobNotCancellable", err)
	}
}

// The evaluation leg is the one the union BUILDS rather than writes: eval_runs
// is a tenant table, so there is one leg per project schema. A test that only
// seeded the two shared tables would pass with that whole branch missing.
func TestAdminBackgroundJobs_IncludesEveryProjectsEvaluationRuns(t *testing.T) {
	repo, pool := newBackgroundJobsRepo(t)

	var datasetID int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO p_1.eval_datasets (name, description)
VALUES ('admin tasks fixture', '') RETURNING id`).Scan(&datasetID); err != nil {
		t.Fatalf("seed eval dataset: %v", err)
	}
	var runID int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO p_1.eval_runs (dataset_id, status, created_by)
VALUES ($1, 'running', 42) RETURNING id`, datasetID).Scan(&runID); err != nil {
		t.Fatalf("seed eval run: %v", err)
	}

	page, err := repo.ListBackgroundJobs(t.Context(), BackgroundJobFilter{
		Kind: BackgroundJobKindEval, Limit: 100,
	})
	if err != nil {
		t.Fatalf("ListBackgroundJobs: %v", err)
	}
	row, found := findJob(page.Rows, strconv.FormatInt(runID, 10))
	if !found {
		t.Fatalf("the evaluation run is missing from the union: %+v", page.Rows)
	}
	if row.Kind != BackgroundJobKindEval || row.Status != "running" {
		t.Errorf("row = %+v, want an eval row in status running", row)
	}
	// The project HAS to travel with the row: a run id is unique only inside
	// its own schema, so the cancel route cannot find it without this.
	if row.ProjectID == nil || *row.ProjectID != 1 {
		t.Errorf("project = %v, want 1", row.ProjectID)
	}
	if !row.Cancellable {
		t.Error("a running evaluation was not offered a cancel")
	}
	if row.Principal != "42" {
		t.Errorf("principal = %q, want 42", row.Principal)
	}
}
