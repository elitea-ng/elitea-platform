package repos

// The index registry against a real PostgreSQL (migration shared/0162).
// Requires ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexregistryapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexregistry"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/jackc/pgx/v5/pgxpool"
)

func registryRun(n int, name string) indexingapp.RegistryInitialRun {
	return indexingapp.RegistryInitialRun{
		ProjectID: 1, ToolkitID: 19, IndexName: name,
		MetaID: fmt.Sprintf("%s-meta-%d", name, n), ExecutionID: fmt.Sprintf("%s-exec-%d", name, n),
		CorrelationID: fmt.Sprintf("%s-corr-%d", name, n),
		Generation:    uint64(n), IndexGeneration: uint64(n),
		Configuration: []byte(`{"index_name":"` + name + `","progress_step":10}`),
		AdmittedAt:    time.Now().UTC().Add(-time.Minute),
	}
}

func registrySummary(dim uint32, model string) outputapp.IndexIngestSummary {
	return outputapp.IndexIngestSummary{
		Status: outputapp.IndexIngestStatusOK, Message: "ok", TerminalState: outputapp.IndexIngestTerminalCompleted,
		IndexedDocuments: 5, IndexedChunks: 20, FailedChunks: 2, SkippedJSON: `{"too_large":1}`,
		EmbeddingModel: model, EmbeddingDimension: dim,
	}
}

func registryResult(n int, name string, summary outputapp.IndexIngestSummary) indexregistryapp.Result {
	return indexregistryapp.Result{
		ExecutionID: fmt.Sprintf("%s-exec-%d", name, n), Generation: uint64(n),
		OccurredAt: time.Now().UTC(), Summary: summary,
	}
}

// admitRegistryRun admits a real index ingest, so an execution_jobs row exists
// (PENDING), and writes its registry row through the real initializer. It
// returns the execution id the row names. indexGeneration orders runs of the
// same index.
func admitRegistryRun(
	t *testing.T,
	pool *pgxpool.Pool,
	repo *IndexRegistryRepository,
	name, key string,
	indexGeneration uint64,
) (string, error) {
	t.Helper()
	jobs, err := NewIndexIngestJobsRepository(pool, IndexIngestDispatchPolicy{
		StreamName: "elitea:runtime:index:commands", CapabilityVersion: "1", ResourceClass: "indexing",
		IsolationClass: "project", Priority: 1, DeadlineTTL: time.Hour, LimitsRevision: "index-limits-v1", MaxOutstanding: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := postgresIndexSubmitRequest(key, name)
	request.Identity.TenantID = "1"
	admitted, err := newPostgresIndexAdmissionService(t, jobs, key).Submit(context.Background(), request)
	if err != nil || !admitted.Created {
		t.Fatalf("admit %s: %+v err=%v", key, admitted, err)
	}
	initializer, err := indexingapp.NewRegistryIndexMetaInitializer(repo)
	if err != nil {
		t.Fatal(err)
	}
	outcome := indexingapp.AdmissionOutcome{
		AdmissionOutcome: admitted.AdmissionOutcome, Generation: 1, IndexGeneration: indexGeneration,
		IndexMetaID: admitted.IndexMetaID, IndexMetaCorrelationID: request.CorrelationID,
	}
	if outcome.AdmittedAt.IsZero() {
		outcome.AdmittedAt = time.Now().UTC()
	}
	return admitted.ExecutionID, initializer.MaterializeInitialIndexMeta(context.Background(), request, outcome)
}

func setJobState(t *testing.T, pool *pgxpool.Pool, executionID, state string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE elitea_runtime.execution_jobs SET state = $2 WHERE execution_id = $1`, executionID, state); err != nil {
		t.Fatalf("set job %s to %s: %v", executionID, state, err)
	}
}

func newRegistryTestRepo(t *testing.T) (*IndexRegistryRepository, *pgxpool.Pool) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	repo, err := NewIndexRegistryRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repo, pool
}

func mustFind(t *testing.T, repo *IndexRegistryRepository, name string) indexregistryapp.Row {
	t.Helper()
	row, found, err := repo.FindByName(context.Background(), 1, 19, name)
	if err != nil || !found {
		t.Fatalf("find %q: found=%v err=%v", name, found, err)
	}
	return row
}

func TestIndexRegistryInitializeCreatesRetriesAndStartsNextRun(t *testing.T) {
	repo, _ := newRegistryTestRepo(t)
	ctx := context.Background()

	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "docs")); err != nil {
		t.Fatal(err)
	}
	row := mustFind(t, repo, "docs")
	if row.State != indexregistryapp.StateInProgress || row.TaskID == nil || *row.TaskID != "docs-exec-1" ||
		len(row.History) != 2 || row.IndexConf["progress_step"] == nil {
		t.Fatalf("created row = %+v", row)
	}

	// The durable initializer retries until it succeeds; a retry is a no-op.
	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "docs")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again := mustFind(t, repo, "docs"); len(again.History) != 2 || again.IndexID != row.IndexID {
		t.Fatalf("retry changed the row: %+v", again)
	}

	if _, err := repo.ApplyResult(ctx, 1, registryResult(1, "docs", registrySummary(8, "m"))); err != nil {
		t.Fatal(err)
	}
	if err := repo.InitializeRegistryRun(ctx, registryRun(2, "docs")); err != nil {
		t.Fatalf("next run after completion: %v", err)
	}
	next := mustFind(t, repo, "docs")
	if next.IndexID != row.IndexID || next.State != indexregistryapp.StateInProgress || len(next.History) != 3 ||
		next.Dimension != 8 || next.Indexed != 0 {
		t.Fatalf("next run row = %+v", next)
	}
	// A late initializer for the older generation names a superseded run.
	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "docs")); err != nil &&
		!errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
		t.Fatalf("older generation = %v", err)
	}
}

func TestIndexRegistryConcurrentFirstInitializeAdmitsExactlyOneRun(t *testing.T) {
	repo, pool := newRegistryTestRepo(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run := registryRun(1, "race")
			run.MetaID = fmt.Sprintf("race-meta-%d", i)
			run.ExecutionID = fmt.Sprintf("race-exec-%d", i)
			errs[i] = repo.InitializeRegistryRun(ctx, run)
		}()
	}
	wg.Wait()
	winners := 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	var live int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.index_registry WHERE name = 'race' AND deleted_at IS NULL`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if winners != 1 || live != 1 {
		t.Fatalf("winners=%d live rows=%d, want 1 and 1", winners, live)
	}
}

func TestIndexRegistryListIsScopedAndOldestFirst(t *testing.T) {
	repo, _ := newRegistryTestRepo(t)
	ctx := context.Background()
	for _, name := range []string{"b", "a", "c"} {
		if err := repo.InitializeRegistryRun(ctx, registryRun(1, name)); err != nil {
			t.Fatal(err)
		}
	}
	other := registryRun(1, "elsewhere")
	other.ToolkitID = 20
	if err := repo.InitializeRegistryRun(ctx, other); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.List(ctx, 1, 19)
	if err != nil || len(rows) != 3 || rows[0].Name != "b" || rows[1].Name != "a" || rows[2].Name != "c" {
		t.Fatalf("list = %+v err=%v", rows, err)
	}
	empty, err := repo.List(ctx, 2, 19)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("another project's list = %#v err=%v", empty, err)
	}
}

func TestIndexRegistryResultStampsOnceAndFailsAChangedDimension(t *testing.T) {
	repo, pool := newRegistryTestRepo(t)
	ctx := context.Background()
	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "docs")); err != nil {
		t.Fatal(err)
	}
	outcome, err := repo.ApplyResult(ctx, 1, registryResult(1, "docs", registrySummary(1536, "Text-Embedding-3-Small")))
	if err != nil || !outcome.Applied || outcome.Mismatch != nil {
		t.Fatalf("first result: %+v err=%v", outcome, err)
	}
	row := mustFind(t, repo, "docs")
	if row.State != indexregistryapp.StateCompleted || row.Model != "Text-Embedding-3-Small" || row.Dimension != 1536 ||
		row.Collection != "emb_text_embedding_3_small_1536" || row.Indexed != 5 || row.Chunks != 20 ||
		row.Failed != 2 || row.Skipped["too_large"] != 1 {
		t.Fatalf("stamped row = %+v", row)
	}

	if err := repo.InitializeRegistryRun(ctx, registryRun(2, "docs")); err != nil {
		t.Fatal(err)
	}
	outcome, err = repo.ApplyResult(ctx, 1, registryResult(2, "docs", registrySummary(768, "Text-Embedding-3-Small")))
	if err != nil || !outcome.Applied || outcome.Mismatch == nil {
		t.Fatalf("changed dimension: %+v err=%v", outcome, err)
	}
	row = mustFind(t, repo, "docs")
	if row.State != indexregistryapp.StateFailed || row.Error == nil || row.Dimension != 1536 || row.Indexed == 5 && row.Chunks == 99 {
		t.Fatalf("mismatched row = %+v", row)
	}
	var dimension int
	if err := pool.QueryRow(ctx, `SELECT embedding_dimension FROM elitea_runtime.index_registry WHERE name = 'docs'`).Scan(&dimension); err != nil || dimension != 1536 {
		t.Fatalf("stored dimension = %d err=%v", dimension, err)
	}

	// A result for a run the registry never held writes nothing.
	none, err := repo.ApplyResult(ctx, 1, registryResult(9, "ghost", registrySummary(8, "m")))
	if err != nil || none.Applied {
		t.Fatalf("result with no row: %+v err=%v", none, err)
	}
}

func TestIndexRegistryTerminalEffects(t *testing.T) {
	repo, _ := newRegistryTestRepo(t)
	ctx := context.Background()
	terminal := func(n int, name string, state indexingapp.CurrentIndexMetaTerminalState, msg string) indexingapp.RegistryTerminal {
		return indexingapp.RegistryTerminal{ProjectID: 1, CurrentTerminalIndexMeta: indexingapp.CurrentTerminalIndexMeta{
			MetaID: fmt.Sprintf("%s-meta-%d", name, n), ExecutionID: fmt.Sprintf("%s-exec-%d", name, n),
			Generation: uint64(n), IndexGeneration: uint64(n), IndexName: name, ToolkitID: 19,
			State: state, OccurredAt: time.Now().UTC(), SafeError: msg,
		}}
	}
	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "f")); err != nil {
		t.Fatal(err)
	}
	if err := repo.ApplyRegistryTerminal(ctx, terminal(1, "f", indexingapp.CurrentIndexMetaFailed, "worker lost")); err != nil {
		t.Fatal(err)
	}
	if row := mustFind(t, repo, "f"); row.State != indexregistryapp.StateFailed || *row.Error != "worker lost" {
		t.Fatalf("failed row = %+v", row)
	}
	// Idempotent.
	if err := repo.ApplyRegistryTerminal(ctx, terminal(1, "f", indexingapp.CurrentIndexMetaFailed, "worker lost")); err != nil {
		t.Fatal(err)
	}

	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "c")); err != nil {
		t.Fatal(err)
	}
	if err := repo.ApplyRegistryTerminal(ctx, terminal(1, "c", indexingapp.CurrentIndexMetaCancelled, "")); err != nil {
		t.Fatal(err)
	}
	cancelled := mustFind(t, repo, "c")
	if cancelled.State != indexregistryapp.StateCancelled || cancelled.TaskID != nil {
		t.Fatalf("cancelled row = %+v", cancelled)
	}
	stop := indexingapp.RegistryManualStop{ProjectID: 1, CurrentManualStopCleanup: indexingapp.CurrentManualStopCleanup{
		MetaID: "c-meta-1", ExecutionID: "c-exec-1", Generation: 1, IndexGeneration: 1, IndexName: "c", ToolkitID: 19,
	}}
	if id, err := repo.VerifyRegistryManualStop(ctx, stop); err != nil || id != cancelled.IndexID {
		t.Fatalf("verify manual stop = %q err=%v", id, err)
	}

	// An effect for a run no row holds (admitted under python before the mode
	// switched) is acknowledged as a no-op, logged at info, and never requeued;
	// for a deleted index it is superseded, so the reconciler resolves it
	// instead of retrying forever.
	var logs bytes.Buffer
	repo.WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := repo.ApplyRegistryTerminal(ctx, terminal(1, "never", indexingapp.CurrentIndexMetaFailed, "x")); err != nil {
		t.Fatalf("a terminal effect with no registry row = %v, want a no-op", err)
	}
	ghostStop := indexingapp.RegistryManualStop{ProjectID: 1, CurrentManualStopCleanup: indexingapp.CurrentManualStopCleanup{
		MetaID: "never-meta-1", ExecutionID: "never-exec-1", Generation: 1, IndexGeneration: 1, IndexName: "never", ToolkitID: 19,
	}}
	if id, err := repo.VerifyRegistryManualStop(ctx, ghostStop); err != nil || id != "" {
		t.Fatalf("a manual stop with no registry row = %q, %v; want a no-op", id, err)
	}
	if got := logs.String(); strings.Count(got, "level=INFO") != 2 || !strings.Contains(got, "no-op") ||
		!strings.Contains(got, "never-exec-1") {
		t.Fatalf("the two no-ops must each log at info level: %s", got)
	}
	if _, err := repo.MarkDeleted(ctx, 1, 19, cancelled.IndexID); err != nil {
		t.Fatal(err)
	}
	if err := repo.ApplyRegistryTerminal(ctx, terminal(1, "c", indexingapp.CurrentIndexMetaCancelled, "")); !errors.Is(err, indexingapp.ErrCurrentIndexMetaSuperseded) {
		t.Fatalf("terminal of a deleted index = %v, want superseded", err)
	}
}

func TestIndexRegistrySaveConfigurationTouchesNothingElse(t *testing.T) {
	repo, pool := newRegistryTestRepo(t)
	ctx := context.Background()
	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "docs")); err != nil {
		t.Fatal(err)
	}
	before := mustFind(t, repo, "docs")
	if err := repo.SaveConfiguration(ctx, 1, 19, "docs", []byte(`{"index_name":"docs","progress_step":99,"x":null}`)); err != nil {
		t.Fatal(err)
	}
	after := mustFind(t, repo, "docs")
	if fmt.Sprint(after.IndexConf["progress_step"]) != "99" || !after.UpdatedAt.Equal(before.UpdatedAt) ||
		after.State != before.State || len(after.History) != len(before.History) {
		t.Fatalf("save changed more than the configuration: before=%+v after=%+v", before, after)
	}
	if value, present := after.IndexConf["x"]; !present || value != nil {
		t.Fatalf("null was not kept: %v", after.IndexConf)
	}
	if err := repo.SaveConfiguration(ctx, 1, 19, "ghost", []byte(`{}`)); !errors.Is(err, indexregistryapp.ErrNotFound) {
		t.Fatalf("save for an unknown index = %v", err)
	}
	_ = pool
}

func TestIndexRegistryDeleteTombstonesFreesTheNameAndPurges(t *testing.T) {
	repo, pool := newRegistryTestRepo(t)
	ctx := context.Background()
	// A synthetic run: no execution job is recorded for it, so it is not active.
	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "docs")); err != nil {
		t.Fatal(err)
	}
	row := mustFind(t, repo, "docs")
	if row.RunActive {
		t.Fatal("a run whose job does not exist cannot be active")
	}

	if _, err := repo.MarkDeleted(ctx, 1, 19, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, indexregistryapp.ErrNotFound) {
		t.Fatalf("delete of an unknown id = %v", err)
	}
	if _, err := repo.MarkDeleted(ctx, 2, 19, row.IndexID); !errors.Is(err, indexregistryapp.ErrNotFound) {
		t.Fatalf("delete from another project = %v", err)
	}
	if err := repo.UpsertDocuments(ctx, row.IndexID, []indexregistryapp.DocumentVersion{{Key: "a.md", Version: "v1", ChunkCount: 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkDeleted(ctx, 1, 19, row.IndexID); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := repo.FindByName(ctx, 1, 19, "docs"); found {
		t.Fatal("a tombstoned index is still found")
	}
	tombstones, err := repo.ListTombstones(ctx, 10)
	if err != nil || len(tombstones) != 1 || tombstones[0].IndexID != row.IndexID || tombstones[0].Attempts != 0 {
		t.Fatalf("tombstones = %+v err=%v", tombstones, err)
	}
	// The name is free again while the tombstone waits for its vectors.
	if err := repo.InitializeRegistryRun(ctx, registryRun(2, "docs")); err != nil {
		t.Fatalf("recreate the deleted name: %v", err)
	}
	live := mustFind(t, repo, "docs")
	if live.IndexID == row.IndexID {
		t.Fatal("the new index reused the deleted index's id (and so its vector namespace)")
	}
	// Purging a live row is a no-op; purging the tombstone drops its documents.
	if err := repo.PurgeDeleted(ctx, live.IndexID); err != nil {
		t.Fatal(err)
	}
	mustFind(t, repo, "docs")
	if err := repo.PurgeDeleted(ctx, row.IndexID); err != nil {
		t.Fatal(err)
	}
	var documents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.index_registry_documents WHERE index_id = $1::uuid`, row.IndexID).Scan(&documents); err != nil || documents != 0 {
		t.Fatalf("documents of a purged index = %d err=%v", documents, err)
	}
	if left, _ := repo.ListTombstones(ctx, 10); len(left) != 0 {
		t.Fatalf("tombstones after purge = %+v", left)
	}
}

// A run is active exactly while the execution job it names is not terminal.
// updated_at plays no part: a healthy run that reports rarely is neither
// deletable nor restartable, and a run whose job ended is both.
func TestIndexRegistryRunLivenessComesFromTheExecutionJob(t *testing.T) {
	repo, pool := newRegistryTestRepo(t)
	ctx := context.Background()

	execution, err := admitRegistryRun(t, pool, repo, "long", "liveness-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	setJobState(t, pool, execution, "RUNNING")
	if _, err := pool.Exec(ctx,
		`UPDATE elitea_runtime.index_registry SET updated_at = now() - interval '48 hours' WHERE name = 'long'`); err != nil {
		t.Fatal(err)
	}
	row := mustFind(t, repo, "long")
	if !row.RunActive || !row.Active() || row.Abandoned() {
		t.Fatalf("a RUNNING job with a 48h-old updated_at must be an active run: %+v", row)
	}
	if listed, _ := repo.List(ctx, 1, 19); len(listed) != 1 || !listed[0].RunActive {
		t.Fatalf("the list does not read liveness: %+v", listed)
	}
	if _, err := repo.MarkDeleted(ctx, 1, 19, row.IndexID); !errors.Is(err, indexregistryapp.ErrActiveRun) {
		t.Fatalf("delete of a healthy long run = %v, want ErrActiveRun", err)
	}
	if err := repo.InitializeRegistryRun(ctx, registryRun(2, "long")); !errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict) {
		t.Fatalf("starting a run over a healthy one = %v, want conflict", err)
	}
	for _, live := range []string{"PENDING", "DISPATCHED", "CLAIMED", "SETTLING"} {
		setJobState(t, pool, execution, live)
		if got := mustFind(t, repo, "long"); !got.RunActive {
			t.Fatalf("job %s is not terminal, the run is active", live)
		}
	}

	// Every terminal job state ends the run; the index is then deletable.
	for _, terminal := range []string{"SUCCEEDED", "FAILED", "CANCELLED"} {
		setJobState(t, pool, execution, terminal)
		got := mustFind(t, repo, "long")
		if got.RunActive || !got.Abandoned() || got.State != indexregistryapp.StateInProgress {
			t.Fatalf("job %s: the run must be dead and its row still in_progress: %+v", terminal, got)
		}
	}

	// A dead run does not block the next one: it is recorded as abandoned.
	second, err := admitRegistryRun(t, pool, repo, "long", "liveness-2", 2)
	if err != nil {
		t.Fatalf("starting a run after a dead one: %v", err)
	}
	next := mustFind(t, repo, "long")
	if next.State != indexregistryapp.StateInProgress || *next.TaskID != second || !next.RunActive {
		t.Fatalf("the new run is not the row's: %+v", next)
	}
	if len(next.History) < 3 {
		t.Fatalf("history = %+v", next.History)
	}
	previous := next.History[len(next.History)-2]
	if previous["state"] != "failed" || previous["error"] != "abandoned" || previous["execution_id"] != execution {
		t.Fatalf("the dead run is not recorded as abandoned: %+v", previous)
	}

	// ... and the dead run's index can be deleted.
	setJobState(t, pool, second, "FAILED")
	if _, err := repo.MarkDeleted(ctx, 1, 19, next.IndexID); err != nil {
		t.Fatalf("delete of a run whose job is terminal: %v", err)
	}
}

// The tombstone sweeper's claim: due rows only, never the same row twice while
// leased, a failure backs off in the row.
func TestIndexRegistryTombstoneClaimLeaseAndBackoff(t *testing.T) {
	repo, pool := newRegistryTestRepo(t)
	ctx := context.Background()
	var ids []string
	for _, name := range []string{"a", "b", "c"} {
		if err := repo.InitializeRegistryRun(ctx, registryRun(1, name)); err != nil {
			t.Fatal(err)
		}
		row := mustFind(t, repo, name)
		if _, err := repo.MarkDeleted(ctx, 1, 19, row.IndexID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.IndexID)
	}

	first, err := repo.ClaimTombstones(ctx, 2, time.Minute)
	if err != nil || len(first) != 2 || first[0].IndexID != ids[0] || first[1].IndexID != ids[1] {
		t.Fatalf("first claim = %+v err=%v", first, err)
	}
	// The claimed rows are leased: the next claim is the remaining one, then none.
	second, err := repo.ClaimTombstones(ctx, 10, time.Minute)
	if err != nil || len(second) != 1 || second[0].IndexID != ids[2] {
		t.Fatalf("second claim = %+v err=%v", second, err)
	}
	if third, err := repo.ClaimTombstones(ctx, 10, time.Minute); err != nil || len(third) != 0 {
		t.Fatalf("a leased tombstone was claimed again: %+v err=%v", third, err)
	}

	// A failure counts an attempt and waits; a deferral waits without counting.
	if err := repo.RescheduleTombstone(ctx, ids[0], time.Now().Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if err := repo.RescheduleTombstone(ctx, ids[1], time.Now().Add(-time.Second), false); err != nil {
		t.Fatal(err)
	}
	listed, _ := repo.ListTombstones(ctx, 10)
	attempts := map[string]int32{}
	for _, tombstone := range listed {
		attempts[tombstone.IndexID] = tombstone.Attempts
	}
	if attempts[ids[0]] != 1 || attempts[ids[1]] != 0 {
		t.Fatalf("attempts = %v", attempts)
	}
	due, err := repo.ClaimTombstones(ctx, 10, time.Minute)
	if err != nil || len(due) != 1 || due[0].IndexID != ids[1] {
		t.Fatalf("only the deferred, now-due tombstone is claimable: %+v err=%v", due, err)
	}
	// A live row is never rescheduled by id.
	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "live")); err != nil {
		t.Fatal(err)
	}
	live := mustFind(t, repo, "live")
	if err := repo.RescheduleTombstone(ctx, live.IndexID, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	var liveAttempts int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM elitea_runtime.index_registry WHERE index_id = $1::uuid`, live.IndexID).Scan(&liveAttempts); err != nil || liveAttempts != 0 {
		t.Fatalf("a live row was rescheduled: attempts=%d err=%v", liveAttempts, err)
	}
}

type flakyVectorDeleter struct {
	failures int
	calls    []string
}

func (d *flakyVectorDeleter) DeleteIndexVectors(_ context.Context, ns indexingapp.IndexVectorNamespace) error {
	d.calls = append(d.calls, ns.IndexID)
	if d.failures > 0 {
		d.failures--
		return errors.New("vector store unavailable")
	}
	return nil
}

// The sweeper against the real table: a transient failure keeps the tombstone
// and backs it off in the row; once due and successful, the tombstone and its
// documents are purged.
func TestIndexRegistrySweeperRetriesWithBackoffThenPurges(t *testing.T) {
	repo, pool := newRegistryTestRepo(t)
	ctx := context.Background()
	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "gone")); err != nil {
		t.Fatal(err)
	}
	row := mustFind(t, repo, "gone")
	if err := repo.UpsertDocuments(ctx, row.IndexID, []indexregistryapp.DocumentVersion{{Key: "a.md", Version: "v1", ChunkCount: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkDeleted(ctx, 1, 19, row.IndexID); err != nil {
		t.Fatal(err)
	}

	deleter := &flakyVectorDeleter{failures: 1}
	var reported []error
	sweeper, err := indexregistryapp.NewTombstoneSweeper(repo, deleter, func(err error) { reported = append(reported, err) }, 10)
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := sweeper.RunOnce(ctx); err != nil || worked != 1 {
		t.Fatalf("first sweep: worked=%d err=%v", worked, err)
	}
	var attempts int
	var next time.Time
	if err := pool.QueryRow(ctx, `SELECT attempts, next_attempt_at FROM elitea_runtime.index_registry WHERE index_id = $1::uuid`, row.IndexID).Scan(&attempts, &next); err != nil {
		t.Fatalf("the tombstone must survive a failed deletion: %v", err)
	}
	if attempts != 1 || !next.After(time.Now()) || len(reported) != 1 {
		t.Fatalf("attempts=%d next=%v reported=%v", attempts, next, reported)
	}
	// Backing off: an immediate second sweep finds nothing due.
	if worked, err := sweeper.RunOnce(ctx); err != nil || worked != 0 || len(deleter.calls) != 1 {
		t.Fatalf("second sweep inside the backoff: worked=%d calls=%d err=%v", worked, len(deleter.calls), err)
	}
	// Once due, the retry deletes the vectors and purges the tombstone.
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.index_registry SET next_attempt_at = now() - interval '1 second' WHERE index_id = $1::uuid`, row.IndexID); err != nil {
		t.Fatal(err)
	}
	if worked, err := sweeper.RunOnce(ctx); err != nil || worked != 1 {
		t.Fatalf("retry: worked=%d err=%v", worked, err)
	}
	var rows, documents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.index_registry WHERE index_id = $1::uuid`, row.IndexID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("tombstone rows after a successful deletion = %d err=%v", rows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.index_registry_documents WHERE index_id = $1::uuid`, row.IndexID).Scan(&documents); err != nil || documents != 0 {
		t.Fatalf("documents after purge = %d err=%v", documents, err)
	}
}

// The installed placeholder: the tombstone stays exactly as it was, no attempt
// is counted, and the sweeper does not even query.
func TestIndexRegistrySweeperWithTheDeferredDeleterLeavesTombstonesAlone(t *testing.T) {
	repo, pool := newRegistryTestRepo(t)
	ctx := context.Background()
	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "gone")); err != nil {
		t.Fatal(err)
	}
	row := mustFind(t, repo, "gone")
	if _, err := repo.MarkDeleted(ctx, 1, 19, row.IndexID); err != nil {
		t.Fatal(err)
	}
	sweeper, err := indexregistryapp.NewTombstoneSweeper(repo, indexingapp.DeferredIndexVectorDeleter{}, func(error) { t.Fatal("nothing to report") }, 10)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if worked, err := sweeper.RunOnce(ctx); err != nil || worked != 0 {
			t.Fatalf("worked=%d err=%v", worked, err)
		}
	}
	var attempts int
	var next *time.Time
	if err := pool.QueryRow(ctx, `SELECT attempts, next_attempt_at FROM elitea_runtime.index_registry WHERE index_id = $1::uuid`, row.IndexID).Scan(&attempts, &next); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || next != nil {
		t.Fatalf("the deferred sweeper touched the tombstone: attempts=%d next=%v", attempts, next)
	}
}

func TestIndexRegistryDocumentsSupportIncrementalSync(t *testing.T) {
	repo, _ := newRegistryTestRepo(t)
	ctx := context.Background()
	if err := repo.InitializeRegistryRun(ctx, registryRun(1, "docs")); err != nil {
		t.Fatal(err)
	}
	id := mustFind(t, repo, "docs").IndexID
	if err := repo.UpsertDocuments(ctx, id, []indexregistryapp.DocumentVersion{
		{Key: "a.md", Version: "v1", ChunkCount: 2}, {Key: "b.md", Version: "v1", ChunkCount: 4},
	}); err != nil {
		t.Fatal(err)
	}
	// One changed document is rewritten; the other is untouched; one is gone.
	if err := repo.UpsertDocuments(ctx, id, []indexregistryapp.DocumentVersion{{Key: "a.md", Version: "v2", ChunkCount: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteDocuments(ctx, id, []string{"b.md"}); err != nil {
		t.Fatal(err)
	}
	documents, err := repo.ListDocuments(ctx, id)
	if err != nil || len(documents) != 1 || documents[0] != (indexregistryapp.DocumentVersion{Key: "a.md", Version: "v2", ChunkCount: 3}) {
		t.Fatalf("documents = %+v err=%v", documents, err)
	}
	if err := repo.UpsertDocuments(ctx, id, []indexregistryapp.DocumentVersion{{Key: "", Version: "v"}}); err == nil {
		t.Fatal("an empty document key was accepted")
	}
}

func TestIndexRegistryTableRefusesIncoherentRows(t *testing.T) {
	_, pool := newRegistryTestRepo(t)
	ctx := context.Background()
	bad := map[string]string{
		"unknown state":              `INSERT INTO elitea_runtime.index_registry (project_id, toolkit_id, name, state) VALUES (1, 1, 'x', 'scheduled_reindex')`,
		"model without dimension":    `INSERT INTO elitea_runtime.index_registry (project_id, toolkit_id, name, state, embedding_model) VALUES (1, 1, 'x', 'created', 'm')`,
		"empty name":                 `INSERT INTO elitea_runtime.index_registry (project_id, toolkit_id, name, state) VALUES (1, 1, '', 'created')`,
		"history that is not a list": `INSERT INTO elitea_runtime.index_registry (project_id, toolkit_id, name, state, history) VALUES (1, 1, 'x', 'created', '{}')`,
		"execution without its twin": `INSERT INTO elitea_runtime.index_registry (project_id, toolkit_id, name, state, execution_id) VALUES (1, 1, 'x', 'created', 'e')`,
		"zero dimension":             `INSERT INTO elitea_runtime.index_registry (project_id, toolkit_id, name, state, embedding_model, embedding_dimension, collection) VALUES (1, 1, 'x', 'created', 'm', 0, 'c')`,
		"negative count":             `INSERT INTO elitea_runtime.index_registry (project_id, toolkit_id, name, state, indexed_chunks) VALUES (1, 1, 'x', 'created', -1)`,
	}
	for name, statement := range bad {
		if _, err := pool.Exec(ctx, statement); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.index_registry (project_id, toolkit_id, name, state) VALUES (1, 1, 'dup', 'created')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.index_registry (project_id, toolkit_id, name, state) VALUES (1, 1, 'dup', 'created')`); err == nil {
		t.Fatal("two live rows share a name")
	}
}

// The end of the chain: a real admission, the registry initializer, a typed
// result projected through the real output service, and the registry row
// applied in that same transaction. Without WithIndexRegistry the registry is
// not touched at all (python mode).
func TestIndexRegistryResultIsAppliedByTheOutputProjectionOnlyInRustMode(t *testing.T) {
	for _, rust := range []bool{true, false} {
		t.Run(fmt.Sprintf("rust=%v", rust), func(t *testing.T) {
			pool := newMigratedPostgresIntegrationPool(t)
			policy := IndexIngestDispatchPolicy{
				StreamName: "elitea:runtime:index:commands", CapabilityVersion: "1", ResourceClass: "indexing",
				IsolationClass: "project", Priority: 1, DeadlineTTL: time.Hour, LimitsRevision: "index-limits-v1", MaxOutstanding: 1,
			}
			jobs, err := NewIndexIngestJobsRepository(pool, policy)
			if err != nil {
				t.Fatal(err)
			}
			request := postgresIndexSubmitRequest("request-registry", "docs")
			request.Identity.TenantID = "1"
			admitted, err := newPostgresIndexAdmissionService(t, jobs, "registry").Submit(context.Background(), request)
			if err != nil || !admitted.Created {
				t.Fatalf("admit: %+v err=%v", admitted, err)
			}
			registry, err := NewIndexRegistryRepository(pool)
			if err != nil {
				t.Fatal(err)
			}
			initializer, err := indexingapp.NewRegistryIndexMetaInitializer(registry)
			if err != nil {
				t.Fatal(err)
			}
			outcome := indexingapp.AdmissionOutcome{
				AdmissionOutcome:       admitted.AdmissionOutcome,
				Generation:             1,
				IndexGeneration:        1,
				IndexMetaID:            admitted.IndexMetaID,
				IndexMetaCorrelationID: request.CorrelationID,
			}
			if outcome.IndexMetaID == "" {
				t.Fatalf("admission outcome carries no index meta id: %+v", admitted)
			}
			if outcome.AdmittedAt.IsZero() {
				outcome.AdmittedAt = time.Now().UTC()
			}
			if err := initializer.MaterializeInitialIndexMeta(context.Background(), request, outcome); err != nil {
				t.Fatalf("initialize: %v", err)
			}

			results, err := NewIndexIngestResultsRepository(pool, IndexIngestOutputPolicy{
				LimitsRevision: policy.LimitsRevision, ArtifactMediaType: "application/json", MaxArtifactBytes: 1024 * 1024,
			})
			if err != nil {
				t.Fatal(err)
			}
			if rust {
				results.WithIndexRegistry()
			}
			expected, err := results.ExpectedIndexIngest(context.Background(), admitted.ExecutionID, 1)
			if err != nil {
				t.Fatal(err)
			}
			fence := claimPostgresIndexExecution(t, pool, expected)
			frame := postgresInlineIndexOutputFrame(t, expected, fence, registrySummary(1536, "text-embedding-3-small"))
			service := newPostgresIndexOutputService(t, pool, results)
			if _, err := service.IngestIndex(context.Background(), frame); err != nil {
				t.Fatalf("project result: %v", err)
			}

			row := mustFind(t, registry, "docs")
			if rust {
				if row.State != indexregistryapp.StateCompleted || row.Dimension != 1536 || row.Indexed != 5 ||
					row.Collection != "emb_text_embedding_3_small_1536" {
					t.Fatalf("rust mode did not apply the result: %+v", row)
				}
			} else if row.State != indexregistryapp.StateInProgress || row.Stamped() {
				t.Fatalf("python mode touched the registry: %+v", row)
			}
		})
	}
}
